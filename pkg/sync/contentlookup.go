// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// ContentLocator 是**可选能力接口**：按**内容校验和**（整文件 sha256 hex）反查卷内路径。
//
// 为什么需要（2026-11-17 实测）：上传结果/manifest 记录的 `rel` 是**上传当时的快照**，文件事后
// 可能被既有能力改动——**命名纠正**（`<CODE>.ext` → `CODE <标题>.ext`）或**目录搬移**
// （`<桶>/<目录>/…` → `<卷内基准>/<桶>/…`）⇒ 用旧 `rel` 做 Stat 会 `not found`，
// 调用方（如"重入校验后清理本地原件"）只能保守拒绝 ✗。按**内容**反查即解耦路径变化。
//
// 契约：返回该卷内与该 checksum 匹配的**全部** rel（卷内相对路径，未含 owner 前缀）；
// 无命中 ⇒ 空切片。实现应只读索引/元数据，**不得**为此下载内容。
// 未实现该接口的卷/装饰器 ⇒ 由 `AssertContentLocator` 返回 nil（调用方按"不支持"处理，
// **不得**当作"无命中"✗）。
//
// 转发要求（R21）：**透明装饰器必须转发**该能力（`trusted.Guard`/`trusted.Wrap`/卷状态缓存等）；
// 只有最内层真正实现该能力的卷（如 secretdata）返回结果，装饰层只负责委托。
type ContentLocator interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），设计保留
	LookupByChecksum(checksum string) []string
}

// AssertContentLocator 查询 fs 是否具备「按内容反查」能力（未实现 ⇒ nil）。
//
// 注意：它**只做类型断言**（不穿透装饰链）——透明装饰器应通过转发使其在装饰后依然可断言；
// 若断言为 nil 而底层其实支持，说明某层装饰器漏转发（由 R21 门禁/单测把关）。
func AssertContentLocator(fs FS) ContentLocator {
	if fs == nil {
		return nil
	}
	if cl, ok := fs.(ContentLocator); ok {
		return cl
	}
	return nil
}
