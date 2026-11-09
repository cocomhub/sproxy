// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// upload_progress.go —— **上传阶段进度回调的跨层传递**（进度可视化二期，2026-11-10 用户要求）。
//
// 背景与痛点：一条文件上传横跨**两层**——
//   客户端 → sproxy：分块上传（sdserver 侧可见，字节进度已接，见 sdserver 一期）；
//   sproxy 内部：**加密**（secretdata 分块 AES-GCM）→ **远端卷上传**（baidupcs）—— 这两个阶段
//     发生在**卷层**，而上传会话（`upload_id`）活在 `pkg/files` ⇒ 两者无法直接互相引用。
//
// 解决方式（**不用包级全局变量**，遵守本仓「测试不得依赖全局可变状态」纪律）：
// `pkg/files` 在写入前把回调**注入 ctx**，卷层实现按需从 ctx 取出并上报；
// 未注入时一切上报都是 no-op（零回归，历史路径不受影响）。
//
// 约定（调用方负责一致性）：
//   - `stage` 是**受控枚举**：`encrypt`（加密）/ `remote`（远端卷上传）；
//   - 同一 stage 内 `total` 恒定、`done` **只增不减**（并发上报可能乱序，接收方需取最大值）；
//   - 回调**可能被并发调用**（如 secretdata 块级并发上传）⇒ 实现必须自身并发安全。

import "context"

// UploadStage 是上传阶段名（受控枚举，供指标标签/UI 分区展示）。
type UploadStage string

// 上传阶段（sproxy 服务端内部可见的两阶段；客户端阶段见 sdserver 一期）。
const (
	// StageEncrypt 是加密阶段（secretdata 分块 AES-GCM 写入）。
	StageEncrypt UploadStage = "encrypt"
	// StageRemote 是远端卷上传阶段（baidupcs 等外部后端写入）。
	StageRemote UploadStage = "remote"
)

// UploadProgressFn 上报某阶段进度：done/total 的单位由该 stage 的**上报方固定约定**
// （加密阶段用**分块数**、远端阶段用**分片数**），接收方不做单位换算。
//
// 实现必须**并发安全**且**不得阻塞**（会被卷层高频调用）。
type UploadProgressFn func(stage UploadStage, done, total int64)

type uploadProgressCtxKey struct{}

// WithUploadProgress 把进度回调注入 ctx 返回新 ctx。
// fn 为 nil ⇒ 视为清除（返回不含回调的 ctx），避免"注入 nil 后调用方仍以为可上报"。
func WithUploadProgress(ctx context.Context, fn UploadProgressFn) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if fn == nil {
		return context.WithValue(ctx, uploadProgressCtxKey{}, UploadProgressFn(nil))
	}
	return context.WithValue(ctx, uploadProgressCtxKey{}, fn)
}

// UploadProgressFrom 取出进度回调；未注入/被清除 ⇒ (nil, false)。
func UploadProgressFrom(ctx context.Context) (UploadProgressFn, bool) {
	if ctx == nil {
		return nil, false
	}
	fn, ok := ctx.Value(uploadProgressCtxKey{}).(UploadProgressFn)
	if !ok || fn == nil {
		return nil, false
	}
	return fn, true
}

// ReportUploadProgress 便捷上报：未注入回调时**静默 no-op**（调用方无需到处判空）。
// 语义上等价于 `if fn, ok := UploadProgressFrom(ctx); ok { fn(stage, done, total) }`。
func ReportUploadProgress(ctx context.Context, stage UploadStage, done, total int64) {
	if fn, ok := UploadProgressFrom(ctx); ok {
		fn(stage, done, total)
	}
}
