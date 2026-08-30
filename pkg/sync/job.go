// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sync 提供节点间文件同步/复制的纯逻辑核心：
// 目录枚举、差异计算、冲突决策与同步编排，不包含任何网络传输实现。
package sync

// Direction 表示同步方向。
type Direction string

const (
	DirectionPush Direction = "push" // 本地推送到远程
	DirectionPull Direction = "pull" // 从远程拉取到本地
)

// Status 表示同步任务状态。
type Status string

const (
	StatusPending   Status = "pending"
	StatusSyncing   Status = "syncing"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
	StatusCancelled Status = "cancelled"
)

// ConflictPolicy 表示冲突处理策略。
type ConflictPolicy string

const (
	ConflictSkip      ConflictPolicy = "skip"
	ConflictOverwrite ConflictPolicy = "overwrite"
	ConflictLWW       ConflictPolicy = "lww"
	ConflictRename    ConflictPolicy = "conflict_rename"
)

// Filter 表示一条 include/exclude glob 过滤器。
type Filter struct {
	Pattern string // glob 模式，path.Match 语义
	Exclude bool   // true=排除；false=包含
}

// Progress 表示同步进度。
type Progress struct {
	FilesDone  int64
	FilesTotal int64
	BytesDone  int64
	BytesTotal int64
}

// Action 表示单个条目的同步结果动作。
type Action string

const (
	ActionCreated         Action = "created"          // 新文件
	ActionUpdated         Action = "updated"          // 覆盖/更新
	ActionSkipped         Action = "skipped"          // 相同跳过
	ActionSkippedConflict Action = "skipped_conflict" // 冲突且策略跳过
	ActionSkippedSymlink  Action = "skipped_symlink"  // 符号链接跳过
	ActionConflictRenamed Action = "conflict_renamed" // 目标被改名保留
	ActionError           Action = "error"
)

// FileResult 表示单个文件的同步结果。
type FileResult struct {
	Path     string // 目标侧路径
	Action   Action
	Error    string
	Size     int64
	MTime    int64
	Checksum string
}

// RemoteRef 描述远程节点与寻址信息（供任务展示/持久化；Engine 不感知网络）。
type RemoteRef struct {
	Node string
	Addr string
}

// Job 描述一次同步任务。
type Job struct {
	ID             string
	Direction      Direction
	Src, Dst       string
	Recursive      bool
	Filters        []Filter
	ConflictPolicy ConflictPolicy
	SyncEmptyDirs  bool // 空目录是否在目标创建（默认 false=跳过）
	FollowSymlinks bool // 是否跟随符号链接（默认 false=跳过）
	Status         Status
	Stats          Progress
	Results        []FileResult
	Remote         RemoteRef
}
