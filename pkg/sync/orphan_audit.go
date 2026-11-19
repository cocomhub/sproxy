// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "context"

// OrphanBlob 是一条「未被引用」的 blob 记录（只读审计结果，不含删除动作）。
type OrphanBlob struct {
	// Container 是 blob 所在容器（卷内相对路径）。
	Container string `json:"container"`
	// Name 是 blob 文件名。
	Name string `json:"name"`
}

// OrphanReport 是只读孤儿审计结果。
type OrphanReport struct {
	// Containers 是本次扫描的容器数（便于判断覆盖面）。
	Containers int `json:"containers"`
	// Chunks 是「未被任何 meta 引用、且非去重池引用」的分块。
	Chunks []OrphanBlob `json:"chunks"`
}

// OrphanAuditor 是**可选能力接口**：只读列出卷内未被引用的分块（**不删除**）。
//
// 为什么需要（用户 2026-11-18）：清理生产卷前要先出清单、人工逐条判断；GC 会直接删除，不可用于评审。
//
// 转发要求（R21/R24）：**所有透明包装器必须转发**该能力（已纳入 `DecoratorCapabilities` 聚合接口
// ⇒ 漏转发**编译期**即失败），否则 `VolumeView` 装饰链上断言恒失败 ⇒ 功能静默不可用 ✗。
// 未实现该能力的**原始卷**（非包装器）⇒ `AssertOrphanAuditor` 返回 nil，调用方按「不支持」处理。
type OrphanAuditor interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），设计保留
	OrphanChunks(ctx context.Context) (OrphanReport, error)
}

// AssertOrphanAuditor 查询 fs 是否具备「孤儿审计」能力（未实现 ⇒ nil）。
//
// 注意：只做类型断言，**不穿透**装饰链——包装器应通过转发使其在装饰后依然可断言。
func AssertOrphanAuditor(fs FS) OrphanAuditor {
	if fs == nil {
		return nil
	}
	if a, ok := fs.(OrphanAuditor); ok {
		return a
	}
	return nil
}
