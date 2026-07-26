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
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CloudDownloadOption 配置云端下载行为。
type CloudDownloadOption func(*cloudDownloadOptions)

type cloudDownloadOptions struct {
	filename string
}

// WithCloudDownloadFilename 设置云端下载的文件名（覆盖 URL 自动提取的文件名）。
func WithCloudDownloadFilename(name string) CloudDownloadOption {
	return func(o *cloudDownloadOptions) {
		o.filename = name
	}
}

// ArchiveResult 表示归档操作的结果。
type ArchiveResult struct {
	Success   bool   `json:"success"`
	File      string `json:"file"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum"`
	TaskCount int    `json:"task_count,omitempty"`
}

// CloudDownload 创建云端下载任务。
// 小文件（<20MB）同步完成，大文件异步执行。
func (c *FileClient) CloudDownload(ctx context.Context, urlStr string, opts ...CloudDownloadOption) (*CloudTask, error) {
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
		return nil, fmt.Errorf("cloud download: %w", err)
	}
	return &task, nil
}

// CloudDownloadBatch 批量创建云端下载任务（最多 100 URL）。
func (c *FileClient) CloudDownloadBatch(ctx context.Context, urls []string) ([]CloudTask, error) {
	entries := make([]map[string]string, len(urls))
	for i, u := range urls {
		entries[i] = map[string]string{"url": u}
	}
	body := map[string]any{"urls": entries}

	var result struct {
		Tasks []CloudTask `json:"tasks"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/download/batch", body, &result); err != nil {
		return nil, fmt.Errorf("cloud download batch: %w", err)
	}
	return result.Tasks, nil
}

// ListCloudTasks 列举云端下载任务。
// status 可选过滤：pending/downloading/completed/failed/cancelled，为空时返回全部。
func (c *FileClient) ListCloudTasks(ctx context.Context, status string) ([]CloudTask, error) {
	urlPath := "/api/cloud/tasks"
	if status != "" {
		urlPath += "?status=" + url.QueryEscape(status)
	}
	var tasks []CloudTask
	if err := c.doJSON(ctx, http.MethodGet, urlPath, nil, &tasks); err != nil {
		return nil, fmt.Errorf("list cloud tasks: %w", err)
	}
	return tasks, nil
}

// GetCloudTask 查询单个任务详情。
func (c *FileClient) GetCloudTask(ctx context.Context, taskID string) (*CloudTask, error) {
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID)
	var task CloudTask
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &task); err != nil {
		return nil, fmt.Errorf("get cloud task: %w", err)
	}
	return &task, nil
}

// CancelCloudTask 取消云端下载任务。
func (c *FileClient) CancelCloudTask(ctx context.Context, taskID string) error {
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, apiPath, nil, nil)
}

// DeleteCloudTask 删除云端下载任务及关联文件。
func (c *FileClient) DeleteCloudTask(ctx context.Context, taskID string) error {
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