package pan123

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/accounting"
)

const (
	maxMemoryUploadSize   = 32 * 1024 * 1024  // 32MB: in-memory threshold
	chunkSize             = 16 * 1024 * 1024  // 16MB: default upload chunk
	maxSliceBufferSize    = 16 * 1024 * 1024  // 16MB: upper bound for buffer
	uploadCompleteMaxWait = 120
	sliceUploadMaxRetries = 5
	sliceUploadTimeout    = 5 * time.Minute
	s3UploadTimeout       = 10 * time.Minute
)

func calculateMD5(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}

// streamMD5 computes MD5 of a reader without buffering the entire file.
// Works for both seekable and non-seekable readers.
// Returns cleanup function for temp file resources (nil if none).
func (f *Fs) streamMD5(ctx context.Context, in io.Reader, filename string, size int64) (etag string, data []byte, reader io.Reader, cleanup func(), err error) {
	unwrapped, wrap := accounting.UnWrap(in)

	// Seekable: read through hasher, seek back, return wrapped reader
	if seeker, canSeek := unwrapped.(io.Seeker); canSeek {
		hasher := md5.New()
		n, copyErr := io.Copy(hasher, in)
		if copyErr != nil {
			err = fmt.Errorf("failed to hash: %w", copyErr)
			return
		}
		if n != size {
			err = fmt.Errorf("size mismatch: expected %d, got %d", size, n)
			return
		}
		if _, seekErr := seeker.Seek(0, io.SeekStart); seekErr != nil {
			err = fmt.Errorf("failed to seek: %w", seekErr)
			return
		}
		return hex.EncodeToString(hasher.Sum(nil)), nil, wrap(unwrapped), nil, nil
	}

	// Non-seekable, small enough for memory
	if size <= maxMemoryUploadSize {
		hasher := md5.New()
		d, copyErr := io.ReadAll(io.TeeReader(in, hasher))
		if copyErr != nil {
			err = fmt.Errorf("failed to read: %w", copyErr)
			return
		}
		if int64(len(d)) != size {
			err = fmt.Errorf("size mismatch: expected %d, got %d", size, len(d))
			return
		}
		return hex.EncodeToString(hasher.Sum(nil)), d, nil, nil, nil
	}

	// Non-seekable, large: buffer to temp file
	fs.Debugf(f, "Buffering non-seekable file %s to temp", filename)
	tmpFile, tmpErr := os.CreateTemp("", "rclone-123pan-upload-*")
	if tmpErr != nil {
		err = fmt.Errorf("failed to create temp file: %w", tmpErr)
		return
	}

	cleanup = func() {
		name := tmpFile.Name()
		_ = tmpFile.Close()
		_ = os.Remove(name)
	}

	hasher := md5.New()
	tee := io.TeeReader(in, hasher)
	n, copyErr := io.Copy(tmpFile, tee)
	if copyErr != nil {
		cleanup()
		cleanup = nil
		err = fmt.Errorf("failed to buffer: %w", copyErr)
		return
	}
	if n != size {
		cleanup()
		cleanup = nil
		err = fmt.Errorf("size mismatch: expected %d, got %d", size, n)
		return
	}
	if _, seekErr := tmpFile.Seek(0, io.SeekStart); seekErr != nil {
		cleanup()
		cleanup = nil
		err = fmt.Errorf("failed to seek temp file: %w", seekErr)
		return
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil, tmpFile, cleanup, nil
}

func (f *Fs) upload(ctx context.Context, in io.Reader, parentID int64, filename string, size int64, options ...fs.OpenOption) (*api.File, error) {
	etag, memData, reader, cleanup, err := f.streamMD5(ctx, in, filename, size)
	if err != nil {
		return nil, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	request := map[string]interface{}{
		"driveId":      0,
		"duplicate":    2,
		"etag":         strings.ToLower(etag),
		"fileName":     filename,
		"parentFileId": parentID,
		"size":         size,
		"type":         0,
	}

	var uploadResp api.UploadRequestResponse
	err = f.callJSONDecode(ctx, "POST", api.UploadRequest, request, &uploadResp)
	if err != nil {
		return nil, fmt.Errorf("upload request failed: %w", err)
	}
	if uploadResp.Code != 0 {
		return nil, fmt.Errorf("upload request error: %s (code %d)", uploadResp.Message, uploadResp.Code)
	}

	fileID := uploadResp.Data.FileID

	if uploadResp.Data.Reuse || uploadResp.Data.Key == "" {
		fs.Debugf(f, "Instant upload succeeded for %s", filename)
	} else if uploadResp.Data.AccessKeyID != "" && uploadResp.Data.SecretAccessKey != "" && uploadResp.Data.SessionToken != "" {
		fs.Debugf(f, "Uploading %s via S3 multipart", filename)
		if memData != nil {
			err = f.uploadS3Multipart(ctx, bytes.NewReader(memData), size, &uploadResp)
		} else {
			err = f.uploadS3Multipart(ctx, reader, size, &uploadResp)
		}
		if err != nil {
			return nil, fmt.Errorf("S3 upload failed: %w", err)
		}
	} else {
		fs.Debugf(f, "Uploading %s via pre-signed URLs", filename)
		if memData != nil {
			fileID, err = f.uploadPreSigned(ctx, bytes.NewReader(memData), size, &uploadResp)
		} else {
			fileID, err = f.uploadPreSigned(ctx, reader, size, &uploadResp)
		}
		if err != nil {
			return nil, fmt.Errorf("pre-signed upload failed: %w", err)
		}
	}

	return &api.File{
		FileID:   fileID,
		FileName: filename,
		Size:     size,
		Etag:     etag,
		Type:     0,
		UpdateAt: time.Now(),
	}, nil
}

func (f *Fs) uploadS3Multipart(ctx context.Context, r io.Reader, size int64, resp *api.UploadRequestResponse) error {
	d := &resp.Data
	numParts := int((size + chunkSize - 1) / chunkSize)

	type partInfo struct {
		PartNumber int    `json:"PartNumber"`
		ETag       string `json:"ETag"`
	}
	parts := make([]partInfo, numParts)

	buf := make([]byte, chunkSize)
	for i := 0; i < numParts; i++ {
		partNum := i + 1
		offset := int64(i) * chunkSize
		partSize := chunkSize
		if offset+int64(partSize) > size {
			partSize = int(size - offset)
		}

		n, readErr := io.ReadFull(r, buf[:partSize])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return fmt.Errorf("failed to read part %d: %w", partNum, readErr)
		}
		partData := buf[:n]
		partMD5 := calculateMD5(partData)

		url := fmt.Sprintf("https://%s/%s/%s?partNumber=%d&uploadId=%s",
			d.EndPoint, d.Bucket, d.Key, partNum, d.UploadID)

		var lastErr error
		for attempt := 0; attempt < sliceUploadMaxRetries; attempt++ {
			if attempt > 0 {
				retryDelay := calculateRetryDelay(attempt-1, baseRetryDelay, maxRetryDelay)
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(retryDelay):
				}
			}

			req, reqErr := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(partData))
			if reqErr != nil {
				lastErr = reqErr
				continue
			}
			req.Header.Set("x-amz-server-side-encryption", "AES256")
			req.Header.Set("x-amz-security-token", d.SessionToken)
			req.Header.Set("Content-MD5", partMD5)

			client := &http.Client{Timeout: s3UploadTimeout}
			httpResp, doErr := client.Do(req)
			if doErr != nil {
				lastErr = doErr
				continue
			}

			if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
				parts[i] = partInfo{PartNumber: partNum, ETag: httpResp.Header.Get("ETag")}
				httpResp.Body.Close()
				break
			}

			body, _ := io.ReadAll(httpResp.Body)
			httpResp.Body.Close()
			lastErr = fmt.Errorf("status %d: %s", httpResp.StatusCode, string(body))
		}

		if parts[i].ETag == "" {
			return fmt.Errorf("part %d upload failed after %d retries: %w", partNum, sliceUploadMaxRetries, lastErr)
		}
		fs.Debugf(f, "Uploaded S3 part %d/%d", partNum, numParts)
	}

	completeReq := map[string]interface{}{
		"bucket":    d.Bucket,
		"key":       d.Key,
		"uploadId":  d.UploadID,
		"partETags": parts,
	}
	if err := f.callJSON(ctx, "POST", api.S3Complete, completeReq); err != nil {
		return fmt.Errorf("S3 complete failed: %w", err)
	}

	uploadCompleteReq := map[string]interface{}{
		"fileId": d.FileID,
	}
	return f.callJSON(ctx, "POST", api.UploadComplete, uploadCompleteReq)
}

func (f *Fs) uploadPreSigned(ctx context.Context, r io.Reader, size int64, resp *api.UploadRequestResponse) (int64, error) {
	d := &resp.Data
	numParts := int((size + chunkSize - 1) / chunkSize)
	lastChunkSize := size % chunkSize
	if lastChunkSize == 0 {
		lastChunkSize = chunkSize
	}

	// Read all data into memory for concurrent part uploads
	partData := make([][]byte, numParts)
	buf := make([]byte, chunkSize)
	for i := 0; i < numParts; i++ {
		partNum := i + 1
		offset := int64(i) * chunkSize
		partSize := chunkSize
		if offset+int64(partSize) > size {
			partSize = int(size - offset)
		}

		n, readErr := io.ReadFull(r, buf[:partSize])
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return 0, fmt.Errorf("failed to read part %d: %w", partNum, readErr)
		}
		partData[i] = make([]byte, n)
		copy(partData[i], buf[:n])
	}

	batchSize := 1
	getUploadURL := f.getS3AuthURLs
	if numParts > 1 {
		batchSize = 10
		getUploadURL = f.getS3PreSignedURLsBatch
	}

	type partETag struct {
		PartNumber int    `json:"PartNumber"`
		ETag       string `json:"ETag"`
	}

	var mu sync.Mutex
	partETags := make([]partETag, numParts)
	thread := f.opt.UploadThread
	sem := make(chan struct{}, thread)
	errCh := make(chan error, numParts)
	var wg sync.WaitGroup

	for start := 1; start <= numParts; start += batchSize {
		end := start + batchSize
		if end > numParts+1 {
			end = numParts + 1
		}

		urlsResp, err := getUploadURL(ctx, resp, start, end)
		if err != nil {
			return 0, fmt.Errorf("failed to get pre-signed URLs for parts %d-%d: %w", start, end-1, err)
		}

		for cur := start; cur < end; cur++ {
			url, ok := urlsResp.Data.PreSignedURLs[strconv.Itoa(cur)]
			if !ok {
				return 0, fmt.Errorf("no pre-signed URL for part %d", cur)
			}

			sem <- struct{}{}
			wg.Add(1)
			go func(partNum int, uploadURL string, data []byte) {
				defer wg.Done()
				defer func() { <-sem }()

				md5Hash := calculateMD5(data)

				err := f.uploadPartWithRetry(ctx, uploadURL, data, partNum)
				if err != nil {
					if isForbidden(err) {
						// Refresh URLs for this batch
						newURLs, refreshErr := getUploadURL(ctx, resp, start, end)
						if refreshErr != nil {
							errCh <- fmt.Errorf("failed to refresh pre-signed URLs after 403: %w", refreshErr)
							return
						}
						newURL, ok := newURLs.Data.PreSignedURLs[strconv.Itoa(partNum)]
						if !ok {
							errCh <- fmt.Errorf("no refreshed pre-signed URL for part %d", partNum)
							return
						}
						// Retry with refreshed URL
						if retryErr := f.uploadPartWithRetry(ctx, newURL, data, partNum); retryErr != nil {
							errCh <- retryErr
							return
						}
					} else {
						errCh <- err
						return
					}
				}

				mu.Lock()
				partETags[partNum-1] = partETag{PartNumber: partNum, ETag: md5Hash}
				mu.Unlock()
				fs.Debugf(f, "Uploaded pre-signed part %d/%d", partNum, numParts)
			}(cur, url, partData[cur-1])
		}
	}

	wg.Wait()
	close(errCh)

	if err := <-errCh; err != nil {
		return 0, err
	}

	completeReq := map[string]interface{}{
		"bucket":    d.Bucket,
		"key":       d.Key,
		"uploadId":  d.UploadID,
		"partETags": partETags,
	}

	var completeResp map[string]interface{}
	err := f.callJSONDecode(ctx, "POST", api.UploadCompleteV2, completeReq, &completeResp)
	if err != nil {
		return 0, fmt.Errorf("upload complete failed: %w", err)
	}

	fileID := d.FileID
	if fid, ok := completeResp["FileId"].(float64); ok {
		fileID = int64(fid)
	}

	return fileID, nil
}

func (f *Fs) getS3AuthURLs(ctx context.Context, d *api.UploadRequestResponse, start, end int) (*api.S3PreSignedURLsResponse, error) {
	urlsReq := map[string]interface{}{
		"StorageNode":     d.Data.StorageNode,
		"bucket":          d.Data.Bucket,
		"key":             d.Data.Key,
		"partNumberStart": start,
		"partNumberEnd":   end,
		"uploadId":        d.Data.UploadID,
	}

	var urlsResp api.S3PreSignedURLsResponse
	err := f.callJSONDecode(ctx, "POST", api.S3Auth, urlsReq, &urlsResp)
	if err != nil {
		return nil, err
	}
	if urlsResp.Code != 0 {
		return nil, fmt.Errorf("S3 auth URLs error: %s (code %d)", urlsResp.Message, urlsResp.Code)
	}
	return &urlsResp, nil
}

func (f *Fs) getS3PreSignedURLsBatch(ctx context.Context, d *api.UploadRequestResponse, start, end int) (*api.S3PreSignedURLsResponse, error) {
	urlsReq := map[string]interface{}{
		"bucket":          d.Data.Bucket,
		"key":             d.Data.Key,
		"partNumberStart": start,
		"partNumberEnd":   end,
		"uploadId":        d.Data.UploadID,
		"StorageNode":     d.Data.StorageNode,
	}

	var urlsResp api.S3PreSignedURLsResponse
	err := f.callJSONDecode(ctx, "POST", api.S3PreSignedURLs, urlsReq, &urlsResp)
	if err != nil {
		return nil, err
	}
	if urlsResp.Code != 0 {
		return nil, fmt.Errorf("S3 pre-signed URLs error: %s (code %d)", urlsResp.Message, urlsResp.Code)
	}
	return &urlsResp, nil
}

func isForbidden(err error) bool {
	return err != nil && strings.Contains(err.Error(), "status 403")
}

func (f *Fs) uploadPartWithRetry(ctx context.Context, url string, data []byte, partNum int) error {
	var lastErr error

	for attempt := 0; attempt < sliceUploadMaxRetries; attempt++ {
		if attempt > 0 {
			retryDelay := calculateRetryDelay(attempt-1, baseRetryDelay, maxRetryDelay)
			fs.Debugf(f, "Retrying part %d, attempt %d/%d after %v", partNum, attempt+1, sliceUploadMaxRetries, retryDelay)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay):
			}
		}

		req, err := http.NewRequestWithContext(ctx, "PUT", url, bytes.NewReader(data))
		if err != nil {
			lastErr = err
			continue
		}

		client := &http.Client{Timeout: sliceUploadTimeout}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			resp.Body.Close()
			return nil
		}

		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode == http.StatusForbidden {
			return fmt.Errorf("part %d upload failed: status 403: %s", partNum, string(body))
		}

		lastErr = fmt.Errorf("part %d upload failed: status %d: %s", partNum, resp.StatusCode, string(body))
	}

	return fmt.Errorf("part %d upload failed after %d retries: %w", partNum, sliceUploadMaxRetries, lastErr)
}
