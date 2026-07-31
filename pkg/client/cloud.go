// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// CloudTask 表示一个云端下载任务。
type CloudTask struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Filename   string    `json:"filename"`
	Status     string    `json:"status"`
	TotalSize  int64     `json:"total_size"`
	Downloaded int64     `json:"downloaded"`
	Checksum   string    `json:"checksum"`
	Error      string    `json:"error"`
	FileMTime  int64     `json:"file_mtime,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CloudTask 状态常量。
const (
	TaskStatusPending     = "pending"
	TaskStatusDownloading = "downloading"
	TaskStatusCompleted   = "completed"
	TaskStatusFailed      = "failed"
	TaskStatusCancelled   = "cancelled"
)

// CloudDownloadOption 配置云端下载行为。
type CloudDownloadOption func(*cloudDownloadOptions)

type cloudDownloadOptions struct {
	filename     string
	maxBatchURLs int
}

// WithCloudDownloadFilename 设置云端下载的文件名（覆盖 URL 自动提取的文件名）。
func WithCloudDownloadFilename(name string) CloudDownloadOption {
	return func(o *cloudDownloadOptions) {
		o.filename = name
	}
}

// WithCloudDownloadMaxBatchURLs 设置批量下载的最大 URL 数量上限。
// 默认 100，服务端也限制 100 URL。设置为 0 使用默认值。
func WithCloudDownloadMaxBatchURLs(n int) CloudDownloadOption {
	return func(o *cloudDownloadOptions) {
		if n > 0 {
			o.maxBatchURLs = n
		}
	}
}

// ArchiveResult 表示归档操作的结果。
type ArchiveResult struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	TaskCount int    `json:"task_count,omitempty"`
}

// CloudDownload 创建云端下载任务。
// 小文件（<20MB）同步完成，大文件异步执行。
func (c *FileClient) CloudDownload(ctx context.Context, urlStr string, opts ...CloudDownloadOption) (*CloudTask, error) {
	if urlStr == "" {
		return nil, fmt.Errorf("cloud download: url is required")
	}
	// 基本 URL 格式校验：避免无效 URL 浪费服务端资源
	u, err := url.Parse(urlStr)
	if err != nil {
		return nil, fmt.Errorf("cloud download: invalid URL %q: %w", urlStr, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("cloud download: 不支持的 URL scheme %q (仅支持 http/https)", u.Scheme)
	}
	cfg := &cloudDownloadOptions{}
	for _, opt := range opts {
		opt(cfg)
	}
	body := map[string]string{"url": urlStr}
	if cfg.filename != "" {
		body["filename"] = cfg.filename
	}

	var task CloudTask
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download", body, &task); err != nil {
		return nil, fmt.Errorf("云端下载: %w", err)
	}
	return &task, nil
}

// CloudDownloadBatch 批量创建云端下载任务（最多 100 URL）。
// 可以用 WithCloudDownloadMaxBatchURLs 调整上限，但不能超过服务端限制。
func (c *FileClient) CloudDownloadBatch(ctx context.Context, urls []string, opts ...CloudDownloadOption) ([]CloudTask, error) {
	if len(urls) == 0 {
		return nil, fmt.Errorf("cloud download batch: urls is required")
	}
	cfg := &cloudDownloadOptions{}
	for _, opt := range opts {
		opt(cfg)
	}
	maxBatch := 100
	if cfg.maxBatchURLs > 0 {
		maxBatch = cfg.maxBatchURLs
	}
	if len(urls) > maxBatch {
		return nil, fmt.Errorf("cloud download batch: 最多 %d 个 URL，收到 %d 个", maxBatch, len(urls))
	}

	// 校验每个 URL 的格式
	for _, urlStr := range urls {
		u, err := url.Parse(urlStr)
		if err != nil {
			return nil, fmt.Errorf("cloud download batch: invalid URL %q: %w", urlStr, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("cloud download batch: 不支持的 URL scheme %q (仅支持 http/https)", u.Scheme)
		}
	}

	entries := make([]map[string]string, len(urls))
	for i, u := range urls {
		entries[i] = map[string]string{"url": u}
	}
	body := map[string]any{"urls": entries}

	var result struct {
		Tasks []CloudTask `json:"tasks"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download/batch", body, &result); err != nil {
		return nil, fmt.Errorf("批量云端下载: %w", err)
	}
	return result.Tasks, nil
}

// ListCloudTasks 列举云端下载任务。
// status 可选过滤：pending/downloading/completed/failed/cancelled，为空时返回全部。
func (c *FileClient) ListCloudTasks(ctx context.Context, status string) ([]CloudTask, error) {
	urlPath := "/api/cloud/tasks"
	params := url.Values{}
	if status != "" {
		params.Set("status", status)
	}
	if len(params) > 0 {
		urlPath += "?" + params.Encode()
	}
	tasks := make([]CloudTask, 0)
	if err := c.doJSON(ctx, http.MethodGet, urlPath, nil, &tasks); err != nil {
		return nil, fmt.Errorf("list cloud tasks: %w", err)
	}
	return tasks, nil
}

// GetCloudTask 查询单个任务详情。
func (c *FileClient) GetCloudTask(ctx context.Context, taskID string) (*CloudTask, error) {
	if taskID == "" {
		return nil, fmt.Errorf("cloud download: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID)
	var task CloudTask
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &task); err != nil {
		return nil, fmt.Errorf("get cloud task: %w", err)
	}
	return &task, nil
}

// CancelCloudTask 取消云端下载任务。
func (c *FileClient) CancelCloudTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("cloud download: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, apiPath, nil, nil)
}

// DeleteCloudTask 删除云端下载任务及关联文件。
func (c *FileClient) DeleteCloudTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("cloud download: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID)
	return c.doJSON(ctx, http.MethodDelete, apiPath, nil, nil)
}

// ArchiveCloudTask 将单任务文件打包为 tar.gz 并存放到 uploads 目录。
func (c *FileClient) ArchiveCloudTask(ctx context.Context, taskID, archiveName string) (*ArchiveResult, error) {
	body := map[string]string{"archive_name": archiveName}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/archive"
	var result ArchiveResult
	if err := c.doJSON(ctx, http.MethodPost, apiPath, body, &result); err != nil {
		return nil, fmt.Errorf("archive cloud task: %w", err)
	}
	return &result, nil
}

// ArchiveCloudTasks 将多个任务的文件打包为一个 tar.gz。
func (c *FileClient) ArchiveCloudTasks(ctx context.Context, taskIDs []string, archiveName string) (*ArchiveResult, error) {
	body := map[string]any{
		"task_ids":     taskIDs,
		"archive_name": archiveName,
	}
	var result ArchiveResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/archive", body, &result); err != nil {
		return nil, fmt.Errorf("archive cloud tasks: %w", err)
	}
	return &result, nil
}
