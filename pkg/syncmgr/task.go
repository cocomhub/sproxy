// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package syncmgr 提供服务端文件同步任务的生命周期管理（SyncManager）：
// 任务模型 / 状态机 / 持久化 / 并发 / 配额，执行复用 pkg/sync 同步引擎。
//
// 包名用 syncmgr 而非 sync，避免与标准库 sync 及 pkg/sync 冲突。
package syncmgr

import (
	"maps"
	"time"
)

// Direction 表示同步方向。
type Direction string

const (
	DirectionPush Direction = "push" // 本地推送到远程
	DirectionPull Direction = "pull" // 从远程拉取到本地
	DirectionBoth Direction = "both" // 双向：push+pull 一次任务
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
// Owner 是任务级多租户隔离字段（阶段 6 工作项 C）：创建时由请求 AK 派生
// （SproxySig → AK；api_keys → key 名；未认证 → 空串）。过滤规则见 ownerVisible：
// 空 owner（全局/旧任务/未认证创建）对所有人可见；非空 owner 只对匹配用户
// （或空 owner 的管理员/未认证）可见。访问边界：List/Get/CancelTask/DeleteTask
// 均按 owner 过滤，跨 owner 视为不存在（404 防枚举）。
type SyncTask struct {
	ID             string   `json:"id"`
	Owner          string   `json:"owner,omitempty"` // 任务归属（创建者 AK / API key 名；空 = 全局兼容）
	Direction      string   `json:"direction"`
	Remote         string   `json:"remote"`              // sync_remotes.<name> 配置名
	Kind           string   `json:"kind,omitempty"`      // 载体类型（direct|mesh；创建时按远端配置归一）
	Transport      string   `json:"transport,omitempty"` // mesh 载体的选路（relay|auto|webrtc；direct 留空）
	Src            string   `json:"src"`                 // FS 根相对路径（"" = 整个根）
	Dst            string   `json:"dst"`
	Recursive      bool     `json:"recursive"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	ConflictPolicy string   `json:"conflict_policy"`
	DeletePolicy   string   `json:"delete_policy,omitempty"` // 源删除传播：skip（默认）| propagate
	SyncEmptyDirs  bool     `json:"sync_empty_dirs"`
	FollowSymlinks bool     `json:"follow_symlinks"`
	Status         string   `json:"status"` // pending | syncing | retrying | completed | failed | cancelled
	// Retries 已重试次数（阶段 6：瞬时网络错误自动重试）。持久化，重启恢复后继续从该计数累计。
	Retries      int              `json:"retries"`
	FilesTotal   int64            `json:"files_total"`
	FilesDone    int64            `json:"files_done"`
	BytesTotal   int64            `json:"bytes_total"`
	BytesDone    int64            `json:"bytes_done"`
	FilesDeleted int64            `json:"files_deleted,omitempty"` // 删除传播删除数
	Results      []SyncFileResult `json:"results,omitempty"`
	// Carriers 是本次执行实际使用过的载体计数（webrtc/relay；执行结束回填，见 syncmgr.RunResult）。
	Carriers     map[string]int `json:"carriers,omitempty"`
	Error        string         `json:"error,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	ExpiresAt    time.Time      `json:"expires_at"`
	ReservedSize int64          `json:"-"` // 预留配额，不持久化
	// Restored 标记任务是从磁盘恢复的（不持久化）。恢复后 StorageManager 已按磁盘扫描
	// 校准配额，pull 方向完成对账时不应再次 TryReserve（否则磁盘已记账字节被二次预留，
	// 配额虚高、瞬时 507，审查 I-2）。
	Restored bool `json:"-"`
}

// SyncTaskMeta 是列表返回的精简任务元信息（含 owner，供多租户隔离展示）。
type SyncTaskMeta struct {
	ID           string    `json:"id"`
	Owner        string    `json:"owner,omitempty"` // 任务归属（创建者 AK / API key 名；空 = 全局兼容）
	Direction    string    `json:"direction"`
	Remote       string    `json:"remote"`
	Src          string    `json:"src"`
	Dst          string    `json:"dst"`
	Status       string    `json:"status"`
	Retries      int       `json:"retries"` // 已重试次数（阶段 6 自动重试；审查 M-5 列表暴露）
	FilesTotal   int64     `json:"files_total"`
	FilesDone    int64     `json:"files_done"`
	BytesTotal   int64     `json:"bytes_total"`
	BytesDone    int64     `json:"bytes_done"`
	FilesDeleted int64     `json:"files_deleted,omitempty"` // 删除传播删除数
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ExpiresAt    time.Time `json:"expires_at"`

	// ---- 载体可见性（W1）----
	//
	// **必须与 SyncTask 的同名字段同步**：List 返回的是投影，漏一个字段就断了 Web UI 的载体展示
	// （实测踩到：这三个字段曾漏在投影外 ⇒ `GET /api/sync/tasks` 不含它们 ⇒ 徽标永远不显示）。
	// 漂移门禁：`pkg/syncmgr/task_meta_drift_test.go`（反射断言 SyncTask 的对外字段全在 Meta 里）。
	Kind      string         `json:"kind,omitempty"`      // 载体类型：direct | mesh（创建时归一）
	Transport string         `json:"transport,omitempty"` // 仅 mesh：relay | auto | webrtc
	Carriers  map[string]int `json:"carriers,omitempty"`  // 终态回填的实际载体计数
}

// copyCarriers 深拷贝载体计数（nil 保持 nil，便于 json omitempty）。
//
// 为什么必须拷：Carriers 是 map，`c := *t` 只复制引用 ⇒ 调用方（序列化/UI/测试）与后台回填
// 并发读写同一张 map 会触发 `concurrent map read and map write`。与既有的 Include/Exclude/Results
// 切片拷贝同一原则。
func copyCarriers(in map[string]int) map[string]int {
	// maps.Clone 保留 nil（nil in → nil out），正合 json omitempty 语义。
	return maps.Clone(in)
}

// CreateRequest 是创建同步任务的请求。
// Owner 由服务端从请求认证上下文派生（阶段 6 工作项 C：SproxySig→AK，api_keys→key 名），
// json:"-" 阻止客户端在 body 中伪造 owner——多租户归属只能由认证决定，绝不信任客户端输入。
type CreateRequest struct {
	Direction      string   `json:"direction"`
	Remote         string   `json:"remote"`
	Src            string   `json:"src"`
	Dst            string   `json:"dst"`
	Recursive      bool     `json:"recursive"`
	Include        []string `json:"include,omitempty"`
	Exclude        []string `json:"exclude,omitempty"`
	ConflictPolicy string   `json:"conflict_policy"`
	DeletePolicy   string   `json:"delete_policy,omitempty"` // 源删除传播：skip（默认）| propagate
	SyncEmptyDirs  bool     `json:"sync_empty_dirs"`
	FollowSymlinks bool     `json:"follow_symlinks"`
	Owner          string   `json:"-"` // 服务端派生，客户端不可设置
}
