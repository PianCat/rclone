package api

import (
	"time"
)

type Response interface {
	IsError() bool
	GetCode() int
	GetMessage() string
}

type BaseResponse struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (r *BaseResponse) IsError() bool          { return r.Code != 0 }
func (r *BaseResponse) GetCode() int           { return r.Code }
func (r *BaseResponse) GetMessage() string     { return r.Message }

// File represents a file/directory item
type File struct {
	FileID      int64     `json:"FileId"`
	FileName    string    `json:"FileName"`
	Type        int       `json:"Type"` // 0=file, 1=dir
	Size        int64     `json:"Size"`
	Etag        string    `json:"Etag"`
	S3KeyFlag   string    `json:"S3KeyFlag"`
	DownloadURL string    `json:"DownloadUrl"`
	UpdateAt    time.Time `json:"UpdateAt"`
}

// FileListResponse from file list API
type FileListResponse struct {
	BaseResponse
	Data struct {
		Next     string `json:"Next"` // "-1" means last page
		Total    int    `json:"Total"`
		InfoList []File `json:"InfoList"`
	} `json:"data"`
}

// DownloadInfoResponse from download info API
type DownloadInfoResponse struct {
	BaseResponse
	Data struct {
		DownloadURL string `json:"DownloadUrl"`
	} `json:"data"`
}

// UploadRequestResponse from upload_request API
type UploadRequestResponse struct {
	BaseResponse
	Data struct {
		AccessKeyID     string `json:"AccessKeyId"`
		SecretAccessKey string `json:"SecretAccessKey"`
		SessionToken    string `json:"SessionToken"`
		Bucket          string `json:"Bucket"`
		Key             string `json:"Key"`
		FileID          int64  `json:"FileId"`
		Reuse           bool   `json:"Reuse"`
		EndPoint        string `json:"EndPoint"`
		StorageNode     string `json:"StorageNode"`
		UploadID        string `json:"UploadId"`
	} `json:"data"`
}

// S3PreSignedURLsResponse from S3 pre-signed URLs API
type S3PreSignedURLsResponse struct {
	BaseResponse
	Data struct {
		PreSignedURLs map[string]string `json:"presignedUrls"`
	} `json:"data"`
}

// UserInfoResponse from user info API
type UserInfoResponse struct {
	BaseResponse
	Data struct {
		UID            int64  `json:"UID"`
		Nickname       string `json:"Nickname"`
		SpaceUsed      int64  `json:"SpaceUsed"`
		SpacePermanent int64  `json:"SpacePermanent"`
		SpaceTemp      int64  `json:"SpaceTemp"`
		FileCount      int    `json:"FileCount"`
	} `json:"data"`
}

// SignInResponse from sign in API
type SignInResponse struct {
	BaseResponse
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}
