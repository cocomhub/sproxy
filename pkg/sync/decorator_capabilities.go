// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import "github.com/cocomhub/sproxy/pkg/files/meta"

// DecoratorCapabilities 是**透明装饰器必须共同支持的可选能力集合**（聚合接口）。
//
// # 为什么要有它（机制，2026-11-13）
//
// 历史教训（两次）：新增**可选能力接口**（如 `StorageInspector`、`meta.ChecksumProvider`）后，
// 若某个透明装饰器漏转发，**编译仍通过**，但上层 `fs.(能力)` 断言恒为 false ⇒
// **静默回落重路径** ⇒ 优化归零（#1127 的收益就曾在真实装饰链上失效）。
//
// 本接口把「装饰器必须支持哪些能力」变成**编译期契约**：
//   - 每个透明装饰器写一句 `var _ DecoratorCapabilities = (*XxxFS)(nil)`；
//   - **新增可选能力**时，只要把它加进本接口 ⇒ **所有装饰器立即编译失败** ⇒ 强制补齐转发；
//   - 若某能力**只部分装饰器支持**（有豁免），则**不要**放进来 —— 它由 R21 门禁
//     （`internal/archcheck/capability_parity_gate_test.go`）按「登记 + 豁免」逐项把关。
//
// ⇒ 两层合起来：**聚合接口管「共同能力」（编译期）** + **R21 管「差异能力」（运行期门禁 + 文档）**。
//
// # 注意（不要放进来的东西）
//
//   - `FS` 本身（基础接口，另有断言）；
//   - `StagingQuotaTracker`（配额钩子的**参数**接口，非 FS 能力）；
//   - **处理层**（如 `EncryptedFS`/`SecretdataFS`）**不适用**：它们改字节语义
//     （RangeReader 的 offset 在密文空间），可选能力**不得**透明转发。
type DecoratorCapabilities interface {
	WriteIfAbsent
	ReserveSpace
	LocalVolume
	RangeReader
	MetaExtraUpdater
	NameLister
	DirectURLProvider
	BatchVerifier
	RapidRefresher
	DirMetaCleaner
	StorageInspector
	TreeInspector
	StagedPathReuse
	Mover
	Copier
	Linker
	StagingQuotaCapable
	StagingQuotaExempt

	// —— 跨包能力（`pkg/files/meta`）：**正是历史漏网点** ——
	meta.Provider
	meta.ChecksumProvider
}
