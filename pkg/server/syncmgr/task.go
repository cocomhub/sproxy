// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package syncmgr 提供服务端文件同步任务的生命周期管理（SyncManager）：
// 任务模型 / 状态机 / 持久化 / 并发 / 配额，执行复用 pkg/sync 同步引擎。
//
// 包名用 syncmgr 而非 sync，避免与标准库 sync 及 pkg/sync 冲突。
package syncmgr

import "time"

// Direction 表示同步方向。
type Direction string

const (
	DirectionPush Direction = "push" // 本地推送到远程
	DirectionPull Direction = "pull" // 从远程拉取到本地
)

// 任务状态常量（对齐 pkg/sync.Status，syncmgr 不依赖 pkg/sync 故本地定义）。
const (
	StatusPending   = "pending"
	StatusSyncing   = "syncing"
	StatusRetrying  = "retrying" // 执行中遇可重试瞬时错误，指数退避后自动重试（阶段 6）
	StatusCompleted = "completed"
	StatusFailed    = "failed"
	StatusCancelled = "cancelled"
)

// ConflictPolicy 冲突处理策略（对齐 pkg/sync.ConflictPolicy）。
const (
	ConflictSkip      = "skip"
	ConflictOverwrite = "overwrite"
	ConflictLWW       = "lww"
	ConflictRename    = "conflict_rename"
)

// SyncFileResult 表示单个文件的同步结果（扁平化，便于持久化与 API 序列化）。
type SyncFileResult struct {
	Path     string `json:"path"`
	Action   string `json:"action"`
	Error    string `json:"error,omitempty"`
	Size     int64  `json:"size"`
	MTime    int64  `json:"mtime"`
	Checksum string `json:"checksum,omitempty"`
}

// SyncTask 表示一个服务端同步任务。
// ReservedSize 不持久化（重启后由 StorageManager 磁盘扫描校准，任务不再持有预留）。
//
// 访问边界（安全审查 MEDIUM，书面确认）：同步任务是 **server-global** 状态，与
// CloudTask（云下载任务）一致——所有经认证的操作者（access_keys 持有者互信，或
// api_keys 多用户 Bearer）视为受信任管理员，可列出/操作全部任务。第一版**不区分
// 创建者 owner**（对齐 CloudTask 无 owner）；若未来启用多租户隔离，需加 owner 字段
// （由请求 AK 派生）并按 owner 过滤列表/详情/取消/删除。
type SyncTask struct {
	ID             string   `json:"id"`
	Direction      string   `json:"direction"`
	Remote         string   `json:"remote"` // sync_remotes.<name> 配置名
	Src            string   `json:"src"`    // FS 根相对路径（"" = 整个根）
	Dst            string   `json:"dst"`
	Recursive      bool     `json:"recursive"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	ConflictPolicy string   `json:"conflict_policy"`
	SyncEmptyDirs  bool     `json:"sync_empty_dirs"`
	FollowSymlinks bool     `json:"follow_symlinks"`
	Status         string   `json:"status"` // pending | syncing | retrying | completed | failed | cancelled
	// Retries 已重试次数（阶段 6：瞬时网络错误自动重试）。持久化，重启恢复后继续从该计数累计。
	Retries      int              `json:"retries"`
	FilesTotal   int64            `json:"files_total"`
	FilesDone    int64            `json:"files_done"`
	BytesTotal   int64            `json:"bytes_total"`
	BytesDone    int64            `json:"bytes_done"`
	Results      []SyncFileResult `json:"results,omitempty"`
	Error        string           `json:"error,omitempty"`
	CreatedAt    time.Time        `json:"created_at"`
	UpdatedAt    time.Time        `json:"updated_at"`
	ExpiresAt    time.Time        `json:"expires_at"`
	ReservedSize int64            `json:"-"` // 预留配额，不持久化
	// Restored 标记任务是从磁盘恢复的（不持久化）。恢复后 StorageManager 已按磁盘扫描
	// 校准配额，pull 方向完成对账时不应再次 TryReserve（否则磁盘已记账字节被二次预留，
	// 配额虚高、瞬时 507，审查 I-2）。
	Restored bool `json:"-"`
}

// SyncTaskMeta 是列表返回的精简任务元信息。
type SyncTaskMeta struct {
	ID         string    `json:"id"`
	Direction  string    `json:"direction"`
	Remote     string    `json:"remote"`
	Src        string    `json:"src"`
	Dst        string    `json:"dst"`
	Status     string    `json:"status"`
	Retries    int       `json:"retries"` // 已重试次数（阶段 6 自动重试；审查 M-5 列表暴露）
	FilesTotal int64     `json:"files_total"`
	FilesDone  int64     `json:"files_done"`
	BytesTotal int64     `json:"bytes_total"`
	BytesDone  int64     `json:"bytes_done"`
	Error      string    `json:"error,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// CreateRequest 是创建同步任务的请求。
type CreateRequest struct {
	Direction      string   `json:"direction"`
	Remote         string   `json:"remote"`
	Src            string   `json:"src"`
	Dst            string   `json:"dst"`
	Recursive      bool     `json:"recursive"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	ConflictPolicy string   `json:"conflict_policy"`
	SyncEmptyDirs  bool     `json:"sync_empty_dirs"`
	FollowSymlinks bool     `json:"follow_symlinks"`
}
