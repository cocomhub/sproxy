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
	// CRC32 是内容的 CRC32 IEEE（无符号十进制字符串；秒传类后端的刷新参数，可空）。
	CRC32 string
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
	// Refreshed 表示后端已用「秒传刷新」自愈（零传输刷新远端 md5）并复验通过。
	// 使用方无需理解刷新细节：看到 State==OK 即可。
	Refreshed bool
}

// BatchVerifier 是**可选**能力：批量延迟校验。
//
// 语义契约：
//   - 对每个 item 返回恰好一条结果（顺序与输入一致）；
//   - 允许后端内部并发（实现方自行限流）；
//   - **不得**因为单项不可见而返回整体错误（逐项表达）；
//   - **允许后端自愈**：发现摘要不一致时，后端**应当**先尝试「秒传刷新」（零传输刷新远端
//     md5，见 docs/external-dependencies.md §7.6）并复验；自愈成功则返回 State=OK +
//     Refreshed=true。这样**使用方无需理解/编排刷新逻辑**（避免每个消费方各写一遍）。
//   - 不得因单项不可见而返回整体错误（逐项表达）。
type BatchVerifier interface {
	VerifyMany(ctx context.Context, items []VerifyItem) []VerifyResult
}

// AssertBatchVerifier 查询后端是否支持批量校验（未实现 ⇒ ok=false，调用方回落逐次校验）。
func AssertBatchVerifier(fs FS) (BatchVerifier, bool) {
	v, ok := fs.(BatchVerifier)
	return v, ok
}

// RapidRefresher 是**可选**能力：用「秒传（rapidupload）」**零传输刷新**远端 meta。
//
// 场景（实测 2026-11-04，详 docs/external-dependencies.md）：百度网盘分片上传后记录的
// md5 **可能不可靠**，而内容其实已在盘 ⇒ 用内容 md5/size/crc32 调 rapidupload 命中后，
// 远端 md5 会被刷新为权威整文件 md5。**这是修复「ETag 不一致」的首选手段**——
// 比整块/整文件重传快几个数量级（零传输）。
//
// 契约：命中返回 (true, nil)；未命中返回 (false, nil)（调用方回落重传）；错误仅表示调用失败。
type RapidRefresher interface {
	RapidRefresh(ctx context.Context, item VerifyItem) (bool, error)
}

// AssertRapidRefresher 查询后端是否支持秒传刷新（未实现 ⇒ ok=false，调用方回落重传）。
func AssertRapidRefresher(fs FS) (RapidRefresher, bool) {
	v, ok := fs.(RapidRefresher)
	return v, ok
}
