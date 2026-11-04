// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// batchverify.go —— **批量延迟校验**（2026-11-04，用户设计）。
//
// 背景：某些远端（百度网盘）对**新建目录树**存在强一致性延迟——`ls`（ListDir）立即可见，
// 但 `meta`（Stat）延迟 5–10 分钟。若每次写入后立即 Stat 复核，新目录场景必然反复超时重传
// （实测 90s×3 后失败，整体 9m38s 后 500）。
//
// 用户设计（2026-11-04）：
//   - **目录**：`ls` 存在即可（不等 Stat）；
//   - **文件**：Stat 与 md5 **必须一致**才算成功（不放宽）；
//   - **时机**：批量上传完成后**统一校验**（此时索引已建立，几乎全命中）；
//   - **不一致**：自动重传（本地副本仍在）而非直接失败；重传耗尽后失败，但**源文件保留 ⇒
//     重跑即可恢复**。
//
// 本文件只定义**跨后端中性**的能力接口与 ctx 标记（后端按自身协议实现；未实现 ⇒ 调用方回落
// 逐次校验，行为零变化）。

// VerifyItem 是一项待校验的对象（键 = 后端存储层 key）。
type VerifyItem struct {
	// Key 是对象键（相对卷根，正斜杠）。
	Key string
	// MD5 是期望的整内容 md5（hex；空 = 只能校验存在性/大小）。
	MD5 string
	// Size 是期望的字节数（<=0 = 不校验大小）。
	Size int64
}

// VerifyState 是单项校验结果。
type VerifyState string

const (
	// VerifyOK 表示对象存在且（在可判定时）内容摘要一致。
	VerifyOK VerifyState = "ok"
	// VerifyMismatch 表示对象可见但摘要/大小不一致（内容可能损坏/被覆盖）⇒ 应重传。
	VerifyMismatch VerifyState = "mismatch"
	// VerifyInvisible 表示对象在等待窗口内不可见 ⇒ 应重试或入待恢复清单。
	VerifyInvisible VerifyState = "invisible"
)

// VerifyResult 是单项校验结果明细（便于日志/重传定位）。
type VerifyResult struct {
	Item  VerifyItem
	State VerifyState
	// RemoteETag 是远端摘要（空 = 远端未提供，如新目录一致性延迟窗口）。
	RemoteETag string
	// RemoteSize 是远端大小（<=0 = 未知）。
	RemoteSize int64
	// Reason 是人类可读原因（日志用）。
	Reason string
}

// BatchVerifier 是**可选**能力：批量延迟校验。
//
// 语义契约：
//   - 对每个 item 返回恰好一条结果（顺序与输入一致）；
//   - 允许后端内部并发（实现方自行限流）；
//   - **不得**因为单项不可见而返回整体错误（逐项表达）；
//   - **不修改**远端内容（纯读）。
type BatchVerifier interface {
	VerifyMany(ctx context.Context, items []VerifyItem) []VerifyResult
}

// AssertBatchVerifier 查询后端是否支持批量校验（未实现 ⇒ ok=false，调用方回落逐次校验）。
func AssertBatchVerifier(fs FS) (BatchVerifier, bool) {
	v, ok := fs.(BatchVerifier)
	return v, ok
}
