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

// SyncTaskRequest 创建同步任务的请求（对齐服务端 syncmgr.CreateRequest JSON）。
type SyncTaskRequest struct {
	Direction      string   `json:"direction"` // push|pull
	Remote         string   `json:"remote"`    // 服务端 sync_remotes.<name> 配置名
	Src            string   `json:"src"`       // FS 根相对路径（"" = 整个根）
	Dst            string   `json:"dst"`
	Recursive      bool     `json:"recursive"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	ConflictPolicy string   `json:"conflict_policy"` // skip|overwrite|lww|conflict_rename
	SyncEmptyDirs  bool     `json:"sync_empty_dirs"`
	FollowSymlinks bool     `json:"follow_symlinks"`
}

// SyncFileResult 表示单个文件的同步结果（对齐服务端 syncmgr.SyncFileResult JSON）。
type SyncFileResult struct {
	Path     string `json:"path"`
	Action   string `json:"action"`
	Error    string `json:"error,omitempty"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
	Checksum string `json:"checksum,omitempty"`
}

// SyncTask 表示一个服务端同步任务（对齐服务端 syncmgr.SyncTask JSON）。
type SyncTask struct {
	ID             string           `json:"id"`
	Direction      string           `json:"direction"`
	Remote         string           `json:"remote"`
	Src            string           `json:"src"`
	Dst            string           `json:"dst"`
	Recursive      bool             `json:"recursive"`
	Include        []string         `json:"include,omitempty"`
	Exclude        []string         `json:"exclude,omitempty"`
	ConflictPolicy string           `json:"conflict_policy"`
	SyncEmptyDirs  bool             `json:"sync_empty_dirs"`
	FollowSymlinks bool             `json:"follow_symlinks"`
	Status         string           `json:"status"` // pending | syncing | completed | failed | cancelled
	FilesTotal     int64            `json:"files_total"`
	FilesDone      int64            `json:"files_done"`
	BytesTotal     int64            `json:"bytes_total"`
	BytesDone      int64            `json:"bytes_done"`
	Results        []SyncFileResult `json:"results,omitempty"`
	Error          string           `json:"error,omitempty"`
	CreatedAt      time.Time        `json:"created_at"`
	UpdatedAt      time.Time        `json:"updated_at"`
	ExpiresAt      time.Time        `json:"expires_at"`
}

// SyncTaskMeta 是列表返回的精简任务元信息（对齐服务端 syncmgr.SyncTaskMeta JSON）。
type SyncTaskMeta struct {
	ID         string    `json:"id"`
	Direction  string    `json:"direction"`
	Remote     string    `json:"remote"`
	Src        string    `json:"src"`
	Dst        string    `json:"dst"`
	Status     string    `json:"status"`
	FilesTotal int64     `json:"files_total"`
	FilesDone  int64     `json:"files_done"`
	BytesTotal int64     `json:"bytes_total"`
	BytesDone  int64     `json:"bytes_done"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// SyncTaskList 是 GET /api/sync/tasks 的响应容器（服务端返回 {success, tasks}）。
type SyncTaskList struct {
	Success bool           `json:"success"`
	Tasks   []SyncTaskMeta `json:"tasks"`
}

// 同步任务状态常量（对齐服务端 syncmgr）。
const (
	SyncStatusPending   = "pending"
	SyncStatusSyncing   = "syncing"
	SyncStatusCompleted = "completed"
	SyncStatusFailed    = "failed"
	SyncStatusCancelled = "cancelled"
)

// CreateSyncTask 创建并启动同步任务（POST /api/sync/tasks）。
// 服务端返回 201（新建）或 200（去重复用既有活跃任务），响应体均为 SyncTask。
func (c *FileClient) CreateSyncTask(ctx context.Context, req SyncTaskRequest) (*SyncTask, error) {
	var task SyncTask
	if err := c.doJSON(ctx, http.MethodPost, "/api/sync/tasks", req, &task); err != nil {
		return nil, fmt.Errorf("创建同步任务: %w", err)
	}
	return &task, nil
}

// GetSyncTask 查询单个同步任务详情（GET /api/sync/tasks/{id}）。
func (c *FileClient) GetSyncTask(ctx context.Context, id string) (*SyncTask, error) {
	if id == "" {
		return nil, fmt.Errorf("同步任务: id 不能为空")
	}
	apiPath := "/api/sync/tasks/" + url.PathEscape(id)
	var task SyncTask
	if err := c.doJSON(ctx, http.MethodGet, apiPath, nil, &task); err != nil {
		return nil, fmt.Errorf("获取同步任务: %w", err)
	}
	return &task, nil
}

// ListSyncTasks 列举所有同步任务（GET /api/sync/tasks）。
func (c *FileClient) ListSyncTasks(ctx context.Context) ([]SyncTaskMeta, error) {
	var list SyncTaskList
	if err := c.doJSON(ctx, http.MethodGet, "/api/sync/tasks", nil, &list); err != nil {
		return nil, fmt.Errorf("列举同步任务: %w", err)
	}
	// 审查 M-3：显式校验 Success（服务端恒 success:true；未来语义变化时客户端不静默返回空）。
	if !list.Success {
		return nil, fmt.Errorf("列举同步任务: 服务端返回 success=false")
	}
	return list.Tasks, nil
}

// CancelSyncTask 取消同步任务（POST /api/sync/tasks/{id}/cancel）。
func (c *FileClient) CancelSyncTask(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("同步任务: id 不能为空")
	}
	apiPath := "/api/sync/tasks/" + url.PathEscape(id) + "/cancel"
	return c.doJSON(ctx, http.MethodPost, apiPath, nil, nil)
}

// DeleteSyncTask 删除同步任务（DELETE /api/sync/tasks/{id}）。
func (c *FileClient) DeleteSyncTask(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("同步任务: id 不能为空")
	}
	apiPath := "/api/sync/tasks/" + url.PathEscape(id)
	return c.doJSON(ctx, http.MethodDelete, apiPath, nil, nil)
}
