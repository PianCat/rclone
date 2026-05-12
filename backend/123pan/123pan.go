package pan123

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/backend/123pan/api"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/encoder"
	"github.com/rclone/rclone/lib/pacer"
	"github.com/rclone/rclone/lib/rest"
	"golang.org/x/time/rate"
)

const (
	rootID          = "0"
	minSleep        = 200 * time.Millisecond
	maxSleep        = 5 * time.Second
	rateInterval    = 700 * time.Millisecond
	trashBatchSize  = 100
	maxAPIRetries   = 10
	baseRetryDelay  = time.Second
	maxRetryDelay   = 30 * time.Second
)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "123pan",
		Description: "123 Pan (Web API)",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:      "username",
				Help:      "123 Pan login username or email.",
				Required:  true,
				Sensitive: true,
			},
			{
				Name:      "password",
				Help:      "123 Pan login password.",
				Required:  true,
				Sensitive: true,
			},
			{
				Name:     "upload_thread",
				Help:     "Number of concurrent threads for chunked uploads.",
				Default:  4,
				Advanced: true,
			},
			{
				Name:     "platform",
				Help:     "Platform identifier sent in API headers.",
				Default:  "web",
				Advanced: true,
			},
			{
				Name:     config.ConfigEncoding,
				Help:     config.ConfigEncodingHelp,
				Advanced: true,
				Default: (encoder.Display |
					encoder.EncodeBackSlash |
					encoder.EncodeLeftSpace |
					encoder.EncodeLeftTilde |
					encoder.EncodeRightPeriod |
					encoder.EncodeRightSpace |
					encoder.EncodeWin |
					encoder.EncodeInvalidUtf8),
			},
		},
	})
}

// Options defines the configuration for this backend
type Options struct {
	Username     string               `config:"username"`
	Password     string               `config:"password"`
	UploadThread int                  `config:"upload_thread"`
	Platform     string               `config:"platform"`
	Enc          encoder.MultiEncoder `config:"encoding"`
}

// Fs represents a remote 123Pan Web API server
type Fs struct {
	name             string
	root             string
	opt              Options
	features         *fs.Features
	srv              *rest.Client
	dirCache         *dircache.DirCache
	pacer            *fs.Pacer
	m                configmap.Mapper
	accessToken      string
	apiLimiter       *sync.Map
	httpClient       *http.Client // for download requests
	noRedirectClient *http.Client // for download redirect detection
}

// Object describes a 123Pan object
type Object struct {
	fs          *Fs
	remote      string
	hasMetaData bool
	size        int64
	modTime     time.Time
	id          int64
	etag        string
	s3KeyFlag   string
	parentID    int64
}

// ------------------------------------------------------------
// Helpers
// ------------------------------------------------------------

func parseID(id string) (int64, error) {
	return strconv.ParseInt(id, 10, 64)
}

func formatID(id int64) string {
	return strconv.FormatInt(id, 10)
}

func calculateRetryDelay(attempt int, baseDelay, maxDelay time.Duration) time.Duration {
	delay := baseDelay * time.Duration(1<<uint(attempt))
	if delay > maxDelay {
		delay = maxDelay
	}
	return delay
}

// ------------------------------------------------------------
// Rate limiting
// ------------------------------------------------------------

func (f *Fs) getLimiter(key string) *rate.Limiter {
	if v, ok := f.apiLimiter.Load(key); ok {
		return v.(*rate.Limiter)
	}
	lim := rate.NewLimiter(rate.Every(rateInterval), 1)
	actual, _ := f.apiLimiter.LoadOrStore(key, lim)
	return actual.(*rate.Limiter)
}

// ------------------------------------------------------------
// API helpers
// ------------------------------------------------------------

func (f *Fs) signURL(rawURL string) string {
	return api.GetAPI(rawURL)
}

func (f *Fs) setHeaders(opts *rest.Opts) {
	opts.ExtraHeaders = map[string]string{
		"Authorization": "Bearer " + f.accessToken,
		"platform":      f.opt.Platform,
		"app-version":   "3",
		"origin":        "https://www.123pan.com",
		"referer":       "https://www.123pan.com/",
		"user-agent":    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) rclone-123pan",
	}
}

func (f *Fs) callJSON(ctx context.Context, method, rawURL string, request interface{}) error {
	lim := f.getLimiter(rawURL)
	if err := lim.Wait(ctx); err != nil {
		return err
	}

	rootURL := f.signURL(rawURL)
	opts := rest.Opts{
		Method:  method,
		RootURL: rootURL,
	}
	f.setHeaders(&opts)

	var resp api.BaseResponse
	err := f.pacer.Call(func() (bool, error) {
		httpResp, err := f.srv.CallJSON(ctx, &opts, request, &resp)
		return shouldRetry(ctx, httpResp, err)
	})
	if err != nil {
		return err
	}

	if resp.Code == 401 {
		token, loginErr := f.signIn(ctx)
		if loginErr != nil {
			return loginErr
		}
		f.accessToken = token
		f.setHeaders(&opts)

		var resp2 api.BaseResponse
		err = f.pacer.Call(func() (bool, error) {
			httpResp, err := f.srv.CallJSON(ctx, &opts, request, &resp2)
			return shouldRetry(ctx, httpResp, err)
		})
		if err != nil {
			return err
		}
		if resp2.Code != 0 {
			return fmt.Errorf("API error: %s (code %d)", resp2.Message, resp2.Code)
		}
		return nil
	}

	if resp.Code != 0 {
		return fmt.Errorf("API error: %s (code %d)", resp.Message, resp.Code)
	}

	return nil
}

func (f *Fs) callJSONDecode(ctx context.Context, method, rawURL string, request, response interface{}) error {
	lim := f.getLimiter(rawURL)
	if err := lim.Wait(ctx); err != nil {
		return err
	}

	rootURL := f.signURL(rawURL)
	opts := rest.Opts{
		Method:  method,
		RootURL: rootURL,
	}
	f.setHeaders(&opts)

	err := f.pacer.Call(func() (bool, error) {
		httpResp, err := f.srv.CallJSON(ctx, &opts, request, response)
		return shouldRetry(ctx, httpResp, err)
	})
	if err != nil {
		return err
	}

	if r, ok := response.(api.Response); ok && r.GetCode() == 401 {
		token, loginErr := f.signIn(ctx)
		if loginErr != nil {
			return loginErr
		}
		f.accessToken = token
		f.setHeaders(&opts)

		err = f.pacer.Call(func() (bool, error) {
			httpResp, err := f.srv.CallJSON(ctx, &opts, request, response)
			return shouldRetry(ctx, httpResp, err)
		})
		if err != nil {
			return err
		}
	}

	return nil
}

func (f *Fs) signIn(ctx context.Context) (string, error) {
	var request map[string]interface{}

	if strings.Contains(f.opt.Username, "@") {
		request = map[string]interface{}{
			"mail":     f.opt.Username,
			"password": f.opt.Password,
			"type":     2,
		}
	} else {
		request = map[string]interface{}{
			"passport": f.opt.Username,
			"password": f.opt.Password,
			"remember": true,
		}
	}

	rootURL := api.SignIn
	opts := rest.Opts{
		Method:  "POST",
		RootURL: rootURL,
		ExtraHeaders: map[string]string{
			"origin":      "https://www.123pan.com",
			"referer":     "https://www.123pan.com/",
			"platform":    f.opt.Platform,
			"app-version": "3",
			"user-agent":  "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) rclone-123pan",
		},
	}

	var resp api.SignInResponse
	err := f.pacer.Call(func() (bool, error) {
		httpResp, err := f.srv.CallJSON(ctx, &opts, request, &resp)
		return shouldRetry(ctx, httpResp, err)
	})
	if err != nil {
		return "", fmt.Errorf("sign in failed: %w", err)
	}

	if resp.Code != 200 {
		return "", fmt.Errorf("sign in error: %s (code %d)", resp.Message, resp.Code)
	}

	return resp.Data.Token, nil
}

func (f *Fs) userInfo(ctx context.Context) (*api.UserInfoResponse, error) {
	var resp api.UserInfoResponse
	err := f.callJSONDecode(ctx, "GET", api.UserInfo, nil, &resp)
	if err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("user info error: %s (code %d)", resp.Message, resp.Code)
	}
	return &resp, nil
}

func (f *Fs) listFiles(ctx context.Context, parentID int64, page int) (*api.FileListResponse, error) {
	lim := f.getLimiter("file_list")
	if err := lim.Wait(ctx); err != nil {
		return nil, err
	}

	params := fmt.Sprintf("driveId=0&limit=100&next=0&orderBy=file_id&orderDirection=desc&parentFileId=%d&trashed=false&SearchData=&OnlyLookAbnormalFile=0&event=homeListFile&operateType=4&inDirectSpace=false&Page=%d", parentID, page)
	u := f.signURL(api.FileList + "?" + params)

	opts := rest.Opts{
		Method:  "GET",
		RootURL: u,
	}
	f.setHeaders(&opts)

	var resp api.FileListResponse
	err := f.pacer.Call(func() (bool, error) {
		httpResp, err := f.srv.CallJSON(ctx, &opts, nil, &resp)
		return shouldRetry(ctx, httpResp, err)
	})
	if err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("list files error: %s (code %d)", resp.Message, resp.Code)
	}
	return &resp, nil
}

func (f *Fs) forEachFile(ctx context.Context, parentID int64, fn func(file api.File) (stop bool)) error {
	page := 1
	for {
		resp, err := f.listFiles(ctx, parentID, page)
		if err != nil {
			return err
		}
		for _, file := range resp.Data.InfoList {
			if fn(file) {
				return nil
			}
		}
		if resp.Data.Next == "-1" {
			return nil
		}
		page++
	}
}

func (f *Fs) downloadInfo(ctx context.Context, fileID int64, etag, s3KeyFlag, fileName string, size int64, fileType int) (*api.DownloadInfoResponse, error) {
	request := map[string]interface{}{
		"driveId":   0,
		"etag":      etag,
		"fileId":    fileID,
		"fileName":  fileName,
		"s3keyFlag": s3KeyFlag,
		"size":      size,
		"type":      fileType,
	}

	var resp api.DownloadInfoResponse
	err := f.callJSONDecode(ctx, "POST", api.DownloadInfo, request, &resp)
	if err != nil {
		return nil, err
	}
	if resp.Code != 0 {
		return nil, fmt.Errorf("download info error: %s (code %d)", resp.Message, resp.Code)
	}
	return &resp, nil
}

func (f *Fs) mkdir(ctx context.Context, parentID int64, name string) (int64, error) {
	request := map[string]interface{}{
		"driveId":      0,
		"etag":         "",
		"fileName":     name,
		"parentFileId": parentID,
		"size":         0,
		"type":         1,
	}

	var resp api.UploadRequestResponse
	err := f.callJSONDecode(ctx, "POST", api.Mkdir, request, &resp)
	if err != nil {
		return 0, err
	}
	if resp.Code != 0 {
		return 0, fmt.Errorf("mkdir error: %s (code %d)", resp.Message, resp.Code)
	}
	return resp.Data.FileID, nil
}

func (f *Fs) move(ctx context.Context, fileID, toParentID int64) error {
	request := map[string]interface{}{
		"fileIdList":   []map[string]int64{{"FileId": fileID}},
		"parentFileId": toParentID,
	}

	return f.callJSON(ctx, "POST", api.Move, request)
}

func (f *Fs) rename(ctx context.Context, fileID int64, newName string) error {
	request := map[string]interface{}{
		"driveId":  0,
		"fileId":   fileID,
		"fileName": newName,
	}

	return f.callJSON(ctx, "POST", api.Rename, request)
}

func (f *Fs) trash(ctx context.Context, fileIDs []int64) error {
	if len(fileIDs) == 0 {
		return nil
	}

	type trashInfo struct {
		FileID      int64  `json:"FileId"`
		FileName    string `json:"FileName"`
		Size        int64  `json:"Size"`
		Type        int    `json:"Type"`
		Etag        string `json:"Etag"`
		S3KeyFlag   string `json:"S3KeyFlag"`
		DownloadURL string `json:"DownloadUrl"`
	}
	infos := make([]trashInfo, len(fileIDs))
	for i, id := range fileIDs {
		infos[i] = trashInfo{FileID: id}
	}

	request := map[string]interface{}{
		"driveId":           0,
		"operation":         true,
		"fileTrashInfoList": infos,
	}

	return f.callJSON(ctx, "POST", api.Trash, request)
}

func (f *Fs) trashSingle(ctx context.Context, fileID int64) error {
	return f.trash(ctx, []int64{fileID})
}

func (f *Fs) trashBatch(ctx context.Context, fileIDs []int64) error {
	for i := 0; i < len(fileIDs); i += trashBatchSize {
		end := i + trashBatchSize
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		if err := f.trash(ctx, fileIDs[i:end]); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------
// Fs interface methods
// ------------------------------------------------------------

func (f *Fs) Name() string {
	return f.name
}

func (f *Fs) Root() string {
	return f.root
}

func (f *Fs) String() string {
	return fmt.Sprintf("123pan root '%s'", f.root)
}

func (f *Fs) Features() *fs.Features {
	return f.features
}

func (f *Fs) Precision() time.Duration {
	return fs.ModTimeNotSupported
}

func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.MD5)
}

// NewFs constructs an Fs from the path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	root = strings.Trim(root, "/")

	f := &Fs{
		name: name,
		root: root,
		opt:  *opt,
		m:    m,
		pacer: fs.NewPacer(ctx, pacer.NewDefault(
			pacer.MinSleep(minSleep),
			pacer.MaxSleep(maxSleep),
			pacer.DecayConstant(2),
		)),
		apiLimiter: new(sync.Map),
		httpClient: fshttp.NewClient(ctx),
		noRedirectClient: fshttp.NewClient(ctx),
	}
	f.noRedirectClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}

	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
		CaseInsensitive:         false,
	}).Fill(ctx, f)

	f.srv = rest.NewClient(f.httpClient)

	token, err := f.signIn(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to authenticate: %w", err)
	}
	f.accessToken = token

	f.dirCache = dircache.New(root, rootID, f)

	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, rootID, &tempF)
		tempF.root = newRoot

		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			return f, nil
		}

		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			if err == fs.ErrorObjectNotFound {
				return f, nil
			}
			return nil, err
		}

		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}

	return f, nil
}

// FindLeaf finds a directory by name
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	parentID, err := parseID(pathID)
	if err != nil {
		return "", false, err
	}

	err = f.forEachFile(ctx, parentID, func(file api.File) bool {
		standardName := f.opt.Enc.ToStandardName(file.FileName)
		if standardName == leaf && file.Type == 1 {
			pathIDOut = formatID(file.FileID)
			found = true
			return true
		}
		return false
	})

	return pathIDOut, found, err
}

// CreateDir makes a directory
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	parentID, err := parseID(pathID)
	if err != nil {
		return "", err
	}

	fileID, err := f.mkdir(ctx, parentID, f.opt.Enc.FromStandardName(leaf))
	if err != nil {
		return "", err
	}

	return formatID(fileID), nil
}

func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return nil, err
	}

	parentID, err := parseID(directoryID)
	if err != nil {
		return nil, err
	}

	err = f.forEachFile(ctx, parentID, func(file api.File) bool {
		remote := path.Join(dir, f.opt.Enc.ToStandardName(file.FileName))

		if file.Type == 1 {
			d := fs.NewDir(remote, file.UpdateAt).SetID(formatID(file.FileID))
			entries = append(entries, d)
			f.dirCache.Put(remote, formatID(file.FileID))
		} else {
			o := &Object{
				fs:          f,
				remote:      remote,
				hasMetaData: true,
				size:        file.Size,
				modTime:     file.UpdateAt,
				id:          file.FileID,
				etag:        file.Etag,
				s3KeyFlag:   file.S3KeyFlag,
				parentID:    parentID,
			}
			entries = append(entries, o)
		}
		return false
	})

	return entries, err
}

func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	return f.newObjectWithInfo(ctx, remote, nil, 0)
}

func (f *Fs) newObjectWithInfo(ctx context.Context, remote string, info *api.File, parentID int64) (fs.Object, error) {
	o := &Object{
		fs:       f,
		remote:   remote,
		parentID: parentID,
	}

	if info != nil {
		o.setMetaData(info)
	} else {
		err := o.readMetaData(ctx)
		if err != nil {
			return nil, err
		}
	}

	return o, nil
}

func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	existingObj, err := f.NewObject(ctx, src.Remote())
	switch err {
	case nil:
		return existingObj, existingObj.Update(ctx, in, src, options...)
	case fs.ErrorObjectNotFound:
		return f.PutUnchecked(ctx, in, src, options...)
	default:
		return nil, err
	}
}

func (f *Fs) PutUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()

	if size < 0 {
		return nil, errors.New("can't upload files of unknown size")
	}
	if size == 0 {
		return nil, fs.ErrorCantUploadEmptyFiles
	}

	leaf, directoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	parentID, err := parseID(directoryID)
	if err != nil {
		return nil, err
	}

	info, err := f.upload(ctx, in, parentID, f.opt.Enc.FromStandardName(leaf), size, options...)
	if err != nil {
		return nil, err
	}

	return f.newObjectWithInfo(ctx, remote, info, parentID)
}

func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	folderID, err := parseID(directoryID)
	if err != nil {
		return err
	}

	var hasContent bool
	err = f.forEachFile(ctx, folderID, func(file api.File) bool {
		hasContent = true
		return true
	})
	if err != nil {
		return err
	}
	if hasContent {
		return fs.ErrorDirectoryNotEmpty
	}

	if err := f.trashSingle(ctx, folderID); err != nil {
		return err
	}

	f.dirCache.FlushDir(dir)
	return nil
}

func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		fs.Debugf(src, "Can't move - not same remote type")
		return nil, fs.ErrorCantMove
	}

	dstLeaf, dstDirectoryID, err := f.dirCache.FindPath(ctx, remote, true)
	if err != nil {
		return nil, err
	}

	dstParentID, err := parseID(dstDirectoryID)
	if err != nil {
		return nil, err
	}

	srcLeaf := path.Base(srcObj.remote)

	if srcObj.parentID != dstParentID {
		if err = f.move(ctx, srcObj.id, dstParentID); err != nil {
			return nil, err
		}
	}

	if srcLeaf != dstLeaf {
		if err = f.rename(ctx, srcObj.id, f.opt.Enc.FromStandardName(dstLeaf)); err != nil {
			return nil, err
		}
	}

	return &Object{
		fs:          f,
		remote:      remote,
		hasMetaData: true,
		size:        srcObj.size,
		modTime:     srcObj.modTime,
		id:          srcObj.id,
		etag:        srcObj.etag,
		parentID:    dstParentID,
	}, nil
}

func (f *Fs) Purge(ctx context.Context, dir string) error {
	return f.purgeCheck(ctx, dir, false)
}

func (f *Fs) purgeCheck(ctx context.Context, dir string, check bool) error {
	directoryID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	dirID, err := parseID(directoryID)
	if err != nil {
		return err
	}

	if check {
		var hasContent bool
		err = f.forEachFile(ctx, dirID, func(file api.File) bool {
			hasContent = true
			return true
		})
		if err != nil {
			return err
		}
		if hasContent {
			return fs.ErrorDirectoryNotEmpty
		}
	}

	if err := f.deleteTree(ctx, dirID); err != nil {
		return fmt.Errorf("Purge: failed to delete contents: %w", err)
	}

	if directoryID != rootID {
		if err := f.trashSingle(ctx, dirID); err != nil {
			return fmt.Errorf("Purge: failed to delete directory: %w", err)
		}
	}

	f.dirCache.FlushDir(dir)
	return nil
}

func (f *Fs) deleteTree(ctx context.Context, rootID int64) error {
	type bfsEntry struct{ id int64 }
	var dirs []bfsEntry
	var batchErr error

	batch := make([]int64, 0, trashBatchSize)
	flushBatch := func() {
		if batchErr != nil || len(batch) == 0 {
			return
		}
		if err := f.trashBatch(ctx, batch); err != nil {
			batchErr = err
		}
		batch = batch[:0]
	}

	queue := []bfsEntry{{id: rootID}}
	for len(queue) > 0 && batchErr == nil {
		current := queue[0]
		queue = queue[1:]

		err := f.forEachFile(ctx, current.id, func(file api.File) bool {
			if file.Type == 1 {
				queue = append(queue, bfsEntry{id: file.FileID})
				dirs = append(dirs, bfsEntry{id: file.FileID})
			} else {
				batch = append(batch, file.FileID)
				if len(batch) >= trashBatchSize {
					flushBatch()
				}
			}
			return batchErr != nil
		})
		if err != nil {
			return err
		}
		flushBatch()
	}

	if batchErr != nil {
		return batchErr
	}

	for i := len(dirs) - 1; i >= 0; i-- {
		if err := f.trashSingle(ctx, dirs[i].id); err != nil {
			return err
		}
	}

	return nil
}

func (f *Fs) CleanUp(ctx context.Context) error {
	fs.Debugf(f, "CleanUp: not supported by Web API")
	return nil
}

func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	info, err := f.userInfo(ctx)
	if err != nil {
		return nil, err
	}

	total := info.Data.SpacePermanent + info.Data.SpaceTemp
	used := info.Data.SpaceUsed
	free := total - used

	return &fs.Usage{
		Total: fs.NewUsageValue(total),
		Used:  fs.NewUsageValue(used),
		Free:  fs.NewUsageValue(free),
	}, nil
}

func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	return nil, fs.ErrorCantCopy
}

func (f *Fs) PublicLink(ctx context.Context, remote string, expire fs.Duration, unlink bool) (string, error) {
	return "", fs.ErrorNotImplemented
}

// ------------------------------------------------------------
// Object interface methods
// ------------------------------------------------------------

func (o *Object) Fs() fs.Info {
	return o.fs
}

func (o *Object) Remote() string {
	return o.remote
}

func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

func (o *Object) Size() int64 {
	return o.size
}

func (o *Object) ModTime(ctx context.Context) time.Time {
	return o.modTime
}

func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	return fs.ErrorCantSetModTime
}

func (o *Object) Storable() bool {
	return true
}

func (o *Object) Hash(ctx context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.etag, nil
}

func (o *Object) ID() string {
	return formatID(o.id)
}

func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	fs.FixRangeOption(options, o.size)

	resp, err := o.fs.downloadInfo(ctx, o.id, o.etag, o.s3KeyFlag, path.Base(o.remote), o.size, 0)
	if err != nil {
		return nil, err
	}

	rawURL := resp.Data.DownloadURL

	// Parse URL and handle base64-encoded params (123pan redirect mechanism)
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	finalURL := rawURL
	params := u.Query().Get("params")
	if params != "" {
		decoded, dErr := base64.StdEncoding.DecodeString(params)
		if dErr == nil {
			decodedURL := strings.TrimSpace(string(decoded))
			if strings.HasPrefix(decodedURL, "http") {
				finalURL = decodedURL
			}
		}
	}

	// Follow redirect to get actual download URL, with Referer header
	var httpResp *http.Response
	err = o.fs.pacer.Call(func() (bool, error) {
		var reqErr error
		req, reqErr := http.NewRequestWithContext(ctx, "GET", finalURL, nil)
		if reqErr != nil {
			return false, reqErr
		}
		req.Header.Set("Referer", "https://www.123pan.com/")
		httpResp, reqErr = o.fs.noRedirectClient.Do(req)
		return shouldRetry(ctx, httpResp, reqErr)
	})
	if err != nil {
		return nil, err
	}

	// Handle 302 redirect
	if httpResp.StatusCode == http.StatusFound || httpResp.StatusCode == http.StatusMovedPermanently {
		location := httpResp.Header.Get("Location")
		httpResp.Body.Close()
		if location == "" {
			return nil, fmt.Errorf("redirect with no Location header")
		}

		httpResp, err = o.fs.httpClient.Get(location)
		if err != nil {
			return nil, err
		}
	} else if httpResp.StatusCode < 300 {
		// Status 200 OK: may contain JSON with redirect_url
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		if len(body) > 0 {
			redirectURL := api.ExtractRedirectURL(body)
			if redirectURL != "" {
				httpResp, err = o.fs.httpClient.Get(redirectURL)
				if err != nil {
					return nil, err
				}
				return httpResp.Body, nil
			}
		}
		// No redirect_url found, return body directly
		return io.NopCloser(bytes.NewReader(body)), nil
	} else {
		body, _ := io.ReadAll(httpResp.Body)
		httpResp.Body.Close()
		return nil, fmt.Errorf("download failed: status %d: %s", httpResp.StatusCode, string(body))
	}

	return httpResp.Body, nil
}

func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	size := src.Size()
	if size < 0 {
		return errors.New("can't upload files of unknown size")
	}
	if size == 0 {
		return fs.ErrorCantUploadEmptyFiles
	}

	info, err := o.fs.upload(ctx, in, o.parentID, o.fs.opt.Enc.FromStandardName(path.Base(o.remote)), size, options...)
	if err != nil {
		return err
	}

	if info.FileID != o.id {
		_ = o.fs.trashSingle(ctx, o.id)
	}

	o.setMetaData(info)
	return nil
}

func (o *Object) Remove(ctx context.Context) error {
	return o.fs.trashSingle(ctx, o.id)
}

func (o *Object) setMetaData(info *api.File) {
	o.hasMetaData = true
	o.size = info.Size
	o.modTime = info.UpdateAt
	o.id = info.FileID
	o.etag = info.Etag
	o.s3KeyFlag = info.S3KeyFlag
}

func (o *Object) readMetaData(ctx context.Context) error {
	leaf, directoryID, err := o.fs.dirCache.FindPath(ctx, o.remote, false)
	if err != nil {
		if err == fs.ErrorDirNotFound {
			return fs.ErrorObjectNotFound
		}
		return err
	}

	parentID, err := parseID(directoryID)
	if err != nil {
		return err
	}

	var found bool
	err = o.fs.forEachFile(ctx, parentID, func(file api.File) bool {
		standardName := o.fs.opt.Enc.ToStandardName(file.FileName)
		if standardName == leaf && file.Type == 0 {
			o.setMetaData(&file)
			o.parentID = parentID
			found = true
			return true
		}
		return false
	})
	if err != nil {
		return err
	}
	if !found {
		return fs.ErrorObjectNotFound
	}
	return nil
}

// ------------------------------------------------------------
// Retry logic
// ------------------------------------------------------------

var retryErrorCodes = []int{
	429,
	500,
	502,
	503,
	504,
}

func shouldRetry(ctx context.Context, resp *http.Response, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	if resp == nil {
		return false, nil
	}
	for _, code := range retryErrorCodes {
		if resp.StatusCode == code {
			return true, nil
		}
	}
	return false, nil
}

// ------------------------------------------------------------
// Interface checks
// ------------------------------------------------------------

var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Mover           = (*Fs)(nil)
	_ fs.Abouter         = (*Fs)(nil)
	_ fs.PutUncheckeder  = (*Fs)(nil)
	_ fs.Purger          = (*Fs)(nil)
	_ dircache.DirCacher = (*Fs)(nil)
	_ fs.Object          = (*Object)(nil)
	_ fs.IDer            = (*Object)(nil)
)
