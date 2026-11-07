// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// stagedpath.go 定义**本地 staging 路径复用**机制：卷若声明「可直接消费调用方已落盘的
// 本地 staging 文件路径」，则同一份数据只落盘一次（本地峰值 1×size），与
// `StagingQuotaGateFS` 按 1×size 预留的口径一致。
//
// 背景：此前这类卷（如 baidupcs）在内部又落一份自己的 staging 文件——调用方暂存 +
// 卷内部暂存 = 两份并存，峰值 2×size，而配额只按 1× 预留（保护减半）。
//
// 两条协作路径（任选其一，语义等价）：
//  1. **能力接口**（推荐，可静态探测）：卷实现 `StagedPathReuse.WriteFromStagedPath`，
//     调用方把已暂存路径直接交给它；
//  2. **ctx 传递**：调用方经 `WithStagedLocalPath` 把路径放入 ctx，卷侧写面
//     （如 baidupcs `StorageFS.WriteFile`）自取复用——适合调用方已经是通用上传器、
//     不希望为「可复用」引入额外接口分支的场景。
//
// 生命周期契约：**stagedPath 只读**——实现方不得移动/删除/改写它，其清理由调用方负责
// （实现方可能在返回前并发读它，调用方须在调用返回后再删）。
type StagedPathReuse interface {
	// WriteFromStagedPath 把本地已暂存文件 stagedPath 上传为 relPath；size/mtime 由调用方
	// 声明（size<=0 = 未知，实现方按实测结算）。
	WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error
}

// stagedPathKey 是 ctx 中「已暂存本地文件路径」的私有键类型。
type stagedPathKey struct{}

// WithStagedLocalPath 把**已落盘的本地 staging 文件路径**放入 ctx（见文件头「ctx 传递」）。
// path 为空 = 不传递（卷侧照常自行暂存）。
func WithStagedLocalPath(ctx context.Context, path string) context.Context {
	if path == "" {
		return ctx
	}
	return context.WithValue(ctx, stagedPathKey{}, path)
}

// StagedLocalPathFrom 取 ctx 中的已暂存本地文件路径（无则 ok=false）。
func StagedLocalPathFrom(ctx context.Context) (string, bool) {
	p, ok := ctx.Value(stagedPathKey{}).(string)
	return p, ok && p != ""
}
