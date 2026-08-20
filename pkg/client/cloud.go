// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
)

// CloudTask 表示一个云端下载任务。
type CloudTask struct {
	ID         string    `json:"id"`
	URL        string    `json:"url"`
	Method     string    `json:"method,omitempty"` // 下载方法，如 "http"、"scraper" 等，空值表示自动选择
	Filename   string    `json:"filename"`
	Status     string    `json:"status"`
	TotalSize  int64     `json:"total_size"`
	Downloaded int64     `json:"downloaded"`
	Checksum   string    `json:"checksum"`
	Error      string    `json:"error"`
	GroupID    string    `json:"group_id,omitempty"` // 所属下载组 ID（可选）
	FileMTime  int64     `json:"file_mtime,omitempty"`
	CreatedAt  time.Time `json:"created_at"` // 创建时间（服务端始终设置，零值仅出现于持久化恢复前）
	UpdatedAt  time.Time `json:"updated_at"` // 更新时间（同上）
	ExpiresAt  time.Time `json:"expires_at"` // 过期时间（同上，与 TaskTTL 关联）
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

// WithCloudDownloadMaxBatchURLs 设置批量下载的客户端侧 URL 数量上限（可选）。
// 上限由服务端配置 cloud_max_batch_urls 强制（默认 100）；客户端默认不预检数量，
// 发送超过服务端上限的请求会收到 400 错误并使创建失败。
// 传入 n>0 时作为客户端本地护栏，在发送前拦截；传入 n<=0 表示不限制（交给服务端），
// 且可覆盖此前设置的任何值（复位为"不预检"）。
func WithCloudDownloadMaxBatchURLs(n int) CloudDownloadOption {
	return func(o *cloudDownloadOptions) {
		o.maxBatchURLs = n
	}
}

// CloudArchiveResult 表示云端归档操作的结果。
type CloudArchiveResult struct {
	Success   bool   `json:"success"`
	Message   string `json:"message,omitempty"`
	File      string `json:"file,omitempty"`
	Size      int64  `json:"size,omitempty"`
	Checksum  string `json:"checksum,omitempty"`
	TaskCount int    `json:"task_count,omitempty"`
}

// CloudArchiveResult 实现 successChecker 接口，支持 doJSON 自动检查。
func (r *CloudArchiveResult) isSuccess() bool { return r.Success }

func (r *CloudArchiveResult) message() string { return r.Message }

// CloudDownload 创建云端下载任务。
// 小文件（<20MB）同步完成，大文件异步执行。
func (c *FileClient) CloudDownload(ctx context.Context, urlStr string, opts ...CloudDownloadOption) (*CloudTask, error) {
	// 基本 URL 格式校验：避免无效 URL 浪费服务端资源
	// （与批量/组路径共用 cloudfilename.ValidateEntry，规则单一来源）
	if err := cloudfilename.ValidateEntry(cloudfilename.Entry{URL: urlStr}); err != nil {
		return nil, fmt.Errorf("云端下载: %w", err)
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
// 如需为每个 URL 指定保存文件名，使用 CloudDownloadBatchEntries。
func (c *FileClient) CloudDownloadBatch(ctx context.Context, urls []string, opts ...CloudDownloadOption) ([]CloudTask, error) {
	entries := make([]cloudfilename.Entry, len(urls))
	for i, u := range urls {
		entries[i] = cloudfilename.Entry{URL: u}
	}
	return c.CloudDownloadBatchEntries(ctx, entries, opts...)
}

// CloudDownloadBatchEntries 批量创建云端下载任务，每个条目可单独指定保存文件名。
// Filename 为空时由服务端按 URL 自动生成。
func (c *FileClient) CloudDownloadBatchEntries(ctx context.Context, entries []cloudfilename.Entry, opts ...CloudDownloadOption) ([]CloudTask, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("批量云端下载: URL 列表不能为空")
	}
	if err := cloudfilename.ValidateEntries(entries); err != nil {
		return nil, err
	}
	cfg := &cloudDownloadOptions{}
	for _, opt := range opts {
		opt(cfg)
	}
	// 数量上限由服务端 cloud_max_batch_urls 强制；客户端仅在显式设置
	// WithCloudDownloadMaxBatchURLs 时做本地预检，默认发送后由服务端 400 报错。
	if cfg.maxBatchURLs > 0 && len(entries) > cfg.maxBatchURLs {
		return nil, fmt.Errorf("批量云端下载: 最多 %d 个 URL，收到 %d 个", cfg.maxBatchURLs, len(entries))
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
		return nil, fmt.Errorf("列举云端任务: %w", err)
	}
	return tasks, nil
}

// GetCloudTask 查询单个任务详情。
func (c *FileClient) GetCloudTask(ctx context.Context, taskID string) (*CloudTask, error) {
	if taskID == "" {
		return nil, fmt.Errorf("云端下载: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID)
	var task CloudTask
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &task); err != nil {
		return nil, fmt.Errorf("获取云端任务: %w", err)
	}
	return &task, nil
}

// CancelCloudTask 取消云端下载任务。
func (c *FileClient) CancelCloudTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("云端下载: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, apiPath, nil, nil)
}

// DeleteCloudTask 删除云端下载任务及关联文件。
func (c *FileClient) DeleteCloudTask(ctx context.Context, taskID string) error {
	if taskID == "" {
		return fmt.Errorf("云端下载: taskID 不能为空")
	}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID)
	return c.doJSON(ctx, http.MethodDelete, apiPath, nil, nil)
}

// ArchiveCloudTask 将单任务文件打包为 tar.gz 并存放到 uploads 目录。
func (c *FileClient) ArchiveCloudTask(ctx context.Context, taskID, archiveName string) (*CloudArchiveResult, error) {
	if taskID == "" {
		return nil, fmt.Errorf("云端打包: taskID 不能为空")
	}
	if archiveName == "" {
		return nil, fmt.Errorf("云端打包: 归档名称不能为空")
	}
	body := map[string]string{"archive_name": archiveName}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/archive"
	var result CloudArchiveResult
	if err := c.doJSON(ctx, http.MethodPost, apiPath, body, &result); err != nil {
		return nil, fmt.Errorf("云端打包: %w", err)
	}
	return &result, nil
}

// ArchiveCloudTasks 将多个任务的文件打包为一个 tar.gz。
func (c *FileClient) ArchiveCloudTasks(ctx context.Context, taskIDs []string, archiveName string) (*CloudArchiveResult, error) {
	if len(taskIDs) == 0 {
		return nil, fmt.Errorf("云端打包: taskIDs 不能为空")
	}
	if archiveName == "" {
		return nil, fmt.Errorf("云端打包: 归档名称不能为空")
	}
	body := map[string]any{
		"task_ids":     taskIDs,
		"archive_name": archiveName,
	}
	var result CloudArchiveResult
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/archive", body, &result); err != nil {
		return nil, fmt.Errorf("云端打包: %w", err)
	}
	return &result, nil
}

// CloudGroup 表示云端下载任务组。
type CloudGroup struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Status      string   `json:"status"`
	TaskIDs     []string `json:"task_ids"`
	TotalTasks  int      `json:"total_tasks"`
	Completed   int      `json:"completed"`
	Failed      int      `json:"failed"`
	Error       string   `json:"error,omitempty"`
	ArchiveFile string   `json:"archive_file,omitempty"`
	CreatedAt   string   `json:"created_at"`
	UpdatedAt   string   `json:"updated_at"`
}

// CloudGroupDetail 包含组详情和子任务列表。
type CloudGroupDetail struct {
	Group *CloudGroup `json:"group"`
	Tasks []CloudTask `json:"tasks"`
}

// CloudCreateGroup 创建云端下载任务组。
// 如需为每个 URL 指定保存文件名，使用 CloudCreateGroupEntries。
func (c *FileClient) CloudCreateGroup(ctx context.Context, name string, urls []string) (*CloudGroup, error) {
	entries := make([]cloudfilename.Entry, len(urls))
	for i, u := range urls {
		entries[i] = cloudfilename.Entry{URL: u}
	}
	return c.CloudCreateGroupEntries(ctx, name, entries)
}

// CloudCreateGroupEntries 创建云端下载任务组，每个条目可单独指定保存文件名。
// Filename 为空时由服务端按 URL 自动生成；服务端在创建前校验文件名冲突（重复返回 409）。
func (c *FileClient) CloudCreateGroupEntries(ctx context.Context, name string, entries []cloudfilename.Entry) (*CloudGroup, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("创建下载组: URL 列表不能为空")
	}
	// 校验每个条目的 URL 格式（scheme + host）与同 URL 不同 Filename 冲突，
	// 与服务端规则对齐（cloudfilename.ValidateEntries），避免服务端 400/409 往返
	if err := cloudfilename.ValidateEntries(entries); err != nil {
		return nil, err
	}
	body := map[string]any{
		"name": name,
		"urls": entries,
	}
	var group CloudGroup
	if err := c.doJSON(ctx, http.MethodPost, "/api/cloud/groups", body, &group); err != nil {
		return nil, fmt.Errorf("创建下载组: %w", err)
	}
	return &group, nil
}

// CloudGetGroup 获取组详情（含子任务列表）。
func (c *FileClient) CloudGetGroup(ctx context.Context, groupID string) (*CloudGroupDetail, error) {
	if groupID == "" {
		return nil, fmt.Errorf("获取下载组: groupID 不能为空")
	}
	apiPath := "/api/cloud/groups/" + url.PathEscape(groupID)
	var detail CloudGroupDetail
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &detail); err != nil {
		return nil, fmt.Errorf("获取下载组: %w", err)
	}
	return &detail, nil
}

// CloudListGroups 列举所有下载组。
func (c *FileClient) CloudListGroups(ctx context.Context, status string) ([]CloudGroup, error) {
	apiPath := "/api/cloud/groups"
	if status != "" {
		apiPath += "?status=" + url.QueryEscape(status)
	}
	var groups []CloudGroup
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &groups); err != nil {
		return nil, fmt.Errorf("列举下载组: %w", err)
	}
	return groups, nil
}

// CloudCancelGroup 取消组内所有下载任务。
func (c *FileClient) CloudCancelGroup(ctx context.Context, groupID string) error {
	if groupID == "" {
		return fmt.Errorf("取消下载组: groupID 不能为空")
	}
	apiPath := "/api/cloud/groups/" + url.PathEscape(groupID) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, apiPath, nil, nil)
}

// CloudDeleteGroup 删除组及所有关联文件。
func (c *FileClient) CloudDeleteGroup(ctx context.Context, groupID string) error {
	if groupID == "" {
		return fmt.Errorf("删除下载组: groupID 不能为空")
	}
	apiPath := "/api/cloud/groups/" + url.PathEscape(groupID)
	return c.doJSON(ctx, http.MethodDelete, apiPath, nil, nil)
}

// CloudArchiveGroup 将组内所有文件打包为 tar.gz。
func (c *FileClient) CloudArchiveGroup(ctx context.Context, groupID, archiveName string) (*CloudArchiveResult, error) {
	if groupID == "" {
		return nil, fmt.Errorf("打包下载组: groupID 不能为空")
	}
	body := map[string]string{"archive_name": archiveName}
	apiPath := "/api/cloud/groups/" + url.PathEscape(groupID) + "/archive"
	var result CloudArchiveResult
	if err := c.doJSON(ctx, http.MethodPost, apiPath, body, &result); err != nil {
		return nil, fmt.Errorf("打包下载组: %w", err)
	}
	return &result, nil
}

// CloudResumeTask 恢复云端下载任务。
func (c *FileClient) CloudResumeTask(ctx context.Context, taskID string, force bool) error {
	if taskID == "" {
		return fmt.Errorf("恢复下载任务: taskID 不能为空")
	}
	body := map[string]bool{"force": force}
	apiPath := "/api/cloud/tasks/" + url.PathEscape(taskID) + "/resume"
	return c.doJSON(ctx, http.MethodPost, apiPath, body, nil)
}

// CloudResumeGroup 恢复组内所有失败任务。
func (c *FileClient) CloudResumeGroup(ctx context.Context, groupID string, force bool) error {
	if groupID == "" {
		return fmt.Errorf("恢复下载组: groupID 不能为空")
	}
	body := map[string]bool{"force": force}
	apiPath := "/api/cloud/groups/" + url.PathEscape(groupID) + "/resume"
	return c.doJSON(ctx, http.MethodPost, apiPath, body, nil)
}
