package api

import (
	"encoding/json"
	"fmt"
	"hash/crc32"
	"math"
	"math/rand"
	"net/url"
	"strings"
	"time"
)

const (
	API              = "https://www.123pan.com/api"
	AApi             = "https://www.123pan.com/a/api"
	BApi             = "https://www.123pan.com/b/api"
	LoginAPI         = "https://login.123pan.com/api"
	MainAPI          = BApi
	SignIn           = LoginAPI + "/user/sign_in"
	Logout           = MainAPI + "/user/logout"
	UserInfo         = MainAPI + "/user/info"
	FileList         = MainAPI + "/file/list/new"
	DownloadInfo     = MainAPI + "/file/download_info"
	Mkdir            = MainAPI + "/file/upload_request"
	Move             = MainAPI + "/file/mod_pid"
	Rename           = MainAPI + "/file/rename"
	Trash            = MainAPI + "/file/trash"
	UploadRequest    = MainAPI + "/file/upload_request"
	UploadComplete   = MainAPI + "/file/upload_complete"
	S3PreSignedURLs  = MainAPI + "/file/s3_repare_upload_parts_batch"
	S3Auth           = MainAPI + "/file/s3_upload_object/auth"
	UploadCompleteV2 = MainAPI + "/file/upload_complete/v2"
	S3Complete       = MainAPI + "/file/s3_complete_multipart_upload"
)

// SignPath generates CRC32-based URL signing parameters.
func SignPath(path, os, version string) (k string, v string) {
	table := []byte{'a', 'd', 'e', 'f', 'g', 'h', 'l', 'm', 'y', 'i', 'j', 'n', 'o', 'p', 'k', 'q', 'r', 's', 't', 'u', 'b', 'c', 'v', 'w', 's', 'z'}
	random := fmt.Sprintf("%.f", math.Round(1e7*rand.Float64()))
	now := time.Now().In(time.FixedZone("CST", 8*3600))
	timestamp := fmt.Sprint(now.Unix())
	nowStr := []byte(now.Format("200601021504"))
	for i := 0; i < len(nowStr); i++ {
		nowStr[i] = table[nowStr[i]-48]
	}
	timeSign := fmt.Sprint(crc32.ChecksumIEEE(nowStr))
	data := strings.Join([]string{timestamp, random, path, os, version, timeSign}, "|")
	dataSign := fmt.Sprint(crc32.ChecksumIEEE([]byte(data)))
	return timeSign, strings.Join([]string{timestamp, random, dataSign}, "-")
}

// GetAPI returns a URL with the signature query parameter appended.
func GetAPI(rawURL string) string {
	u, _ := url.Parse(rawURL)
	query := u.Query()
	k, v := SignPath(u.Path, "web", "3")
	query.Add(k, v)
	u.RawQuery = query.Encode()
	return u.String()
}

// ExtractRedirectURL extracts the redirect_url from a 123pan API JSON response body.
func ExtractRedirectURL(body []byte) string {
	var resp struct {
		Data struct {
			RedirectURL string `json:"redirect_url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &resp); err == nil && resp.Data.RedirectURL != "" {
		return resp.Data.RedirectURL
	}
	return ""
}
