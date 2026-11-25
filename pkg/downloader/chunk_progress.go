// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// chunk_progress.go 是**跨下载器的分块进度通道**（用户 2026-11-26）。
//
// 为什么需要：分块级进度此前只有 pikpak hybrid 通过 `HybridConfig.ChunkProgress` 暴露给
// pikget（CLI 逐行显示）；sproxy 云端是**注册表单例**、多任务并发共享，无法用配置级回调
// 区分任务。改用 **context 通道**：调用方（cloud manager）在每次下载的 ctx 上挂回调，
// 下载器从 ctx 取用 ⇒ 天然按任务隔离、无需改接口签名。
package downloader

import "context"

// ChunkState 是一次分块状态（与具体下载器无关的通用形态）。
//
// 语义：`Phase` ∈ pending/downloading/done/failed/cancelled；`Done` 是该块**已落盘字节**
// （子块续传下按 1MiB 粒度推进），`Length` 是块总字节 ⇒ UI 可直接画「一行多小块」矩阵。
type ChunkState struct {
	Index  int    `json:"index"`
	Offset int64  `json:"offset"`
	Length int64  `json:"length"`
	Phase  string `json:"phase"`
	Done   int64  `json:"done"`
	Source string `json:"source,omitempty"`
	Err    string `json:"err,omitempty"`
}

// ChunkProgressFunc 接收分块状态更新（可能被多个下载 goroutine 并发调用）。
type ChunkProgressFunc func(ChunkState)

type chunkProgressKey struct{}

// WithChunkProgress 在 ctx 上挂分块进度回调（nil 表示不关心）。
func WithChunkProgress(ctx context.Context, fn ChunkProgressFunc) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, chunkProgressKey{}, fn)
}

// ChunkProgressFrom 取出 ctx 上的分块进度回调（未设置返回 nil）。
func ChunkProgressFrom(ctx context.Context) ChunkProgressFunc {
	if ctx == nil {
		return nil
	}
	fn, _ := ctx.Value(chunkProgressKey{}).(ChunkProgressFunc)
	return fn
}
