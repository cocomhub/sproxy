// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import "context"

// RunResult 是 Executor.Run 返回的同步结果（回填到 SyncTask 的进度/状态/结果）。
type RunResult struct {
	Status     string // completed | failed | cancelled
	FilesTotal int64
	FilesDone  int64
	BytesTotal int64
	BytesDone  int64
	Results    []SyncFileResult // 扁平化文件级结果
	Error      string           // Status==failed 时的错误文本
}

// Executor 执行一次同步任务（src/dst 传输编排）。
//
// 设计约束（模块边界）：syncmgr 不依赖 pkg/sync（其 HTTPTransport 依赖 pkg/client，
// 若 pkg/server 传递依赖 pkg/client 会与 pkg/client 的 e2e 测试形成 import cycle）。
// 因此实际同步执行由 cmd/sproxy 装配的实现注入（基于 pkg/sync.Engine 的
// pkg/syncexec.Executor），syncmgr 只编排任务生命周期并应用 RunResult。
type Executor interface {
	// Run 执行一次同步。task 为只读快照（含方向/路径/过滤/冲突策略）；remote 为
	// 已校验的远程配置。返回的 RunResult 非 nil（即使出错，Engine 也会产出状态）；
	// 仅在执行器自身构造失败（如无法创建远程传输）时返回 (nil, err)。
	Run(ctx context.Context, task *SyncTask, remote RemoteConfig) (*RunResult, error)
}
