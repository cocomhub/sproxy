// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volume

import "context"

// storage_volume.go —— **统一存储卷模型**（M0：类型定义，加法式，**零行为变更**）。
//
// 背景与完整设计见 `docs/designs/2026-10-16-quota-volume-layer.md` §11。现状取证：
//
//   - **数据面已统一** ✓：本地与外部卷都经 `sync.FS`（`registry.ExternalBackend` 只有
//     `FS() + Close()`）；
//   - **存储身份未统一** ✗：`*storage.Root` 在非测试源码出现 254 处 —— 它是**局部特例**；
//   - **装配分派未统一** ✗：`local` **不是**注册后端（`registry.RegisterBackend` 显式禁止），
//     装配层以 `Type == "" || TypeLocal` 走特例分支；
//   - **类型分类部分存在** ⚠️：registry 已有 `backendCategory` 静态基线 + `wrapperTypes` 动态登记。
//
// 本文件只定义**形状**（不改动任何现有路径）：
//
//  1. `Kind`：卷的**结构性类型**（Storage = 承载字节；Wrapper = 视图层）；
//  2. 可选**能力**接口：足迹 / 物理上界 / 整卷容量 / 容量统计。
//
// **为何 `StorageVolume` 接口不在此包**：`pkg/volume` **不能** import `pkg/sync`
// （`sync → pkg/files/meta → pkg/volume` 成环 ✗）⇒ 持有 `syncpkg.FS` 的接口定义在
// `pkg/volume/registry`（它已同时持有两侧 ✓，且 `ExternalBackend` 本就在那里 ✓）。
//
// **为什么是「必需核心 + 可选能力」而不是一个大接口**：外部后端今天只需 `FS()+Close()`
// ⇒ 保持**零回归**；能力按需类型断言探测 —— 与仓内既有模式一致（`Presigner` / `URLResolver` /
// `VolumeStatsProvider` / `ReserveSpace` / `WriteIfAbsent` / `StagingQuotaCapable`）。
//
// **配额语义**（设计 §1）：配额层只包**存储卷** ⇒ 它与真实字节之间**没有变换层** ⇒ 它看到的
// 尺寸**天生是物理**（`storage.Root.Stat` 在加密卷上即密文长度）⇒ **无需知道卷是否加密** ✓。
// 封装卷（`KindWrapper`）**不注册配额层**：它的一切读写都发生在某个存储卷上，只调用配额卷能力。

// Kind 是卷的**结构性类型**（装配期判定一次，运行期只读）。
//
// **零值（0）表示「未判定」**：显式判定入口是 `registry.KindOf`（它持有 wrapper 事实源）
// ⇒ 零值不得静默当作 Storage ✗（消费方应按需要先判定，或显式处置未知值）。
type Kind uint8

const (
	// KindStorage 是**存储卷**：承载真实字节、可读写、**需要配额层**。
	KindStorage Kind = iota + 1
	// KindWrapper 是**封装卷**：视图层（secretdata / 镜像 / 压缩…），**不承载独立字节** ——
	// 其一切读写都发生在某个存储卷上 ⇒ **不注册配额层**，只使用配额卷提供的能力。
	KindWrapper
)

// String 返回 Kind 的可读名（日志/错误用；未知值显式标注）。
func (k Kind) String() string {
	switch k {
	case KindStorage:
		return "storage"
	case KindWrapper:
		return "wrapper"
	default:
		return "unknown"
	}
}

// StorageVolume 与可选能力的**数据面形态**定义在 `pkg/volume/registry`（见包内
// storage_volume.go：该包同时持有 `pkg/volume` 与 `pkg/sync`，避开 import 环 ✗）。

// ---- 可选能力（类型断言探测；未实现 = 不支持，调用方必须显式处置）----

// Footprinter 是**物理足迹**能力：返回对象在**该卷上**的实际占用字节。
//
// 为什么必须由卷实现：加密卷 = 密文长度（公式可算）；secretdata = 分片 + parity + meta pad
// （**只能实测**）；外部卷 = 远端 stat ⇒ **没有一种卷的物理尺寸能由调用方推出** ✗。
// 配额层（设计 §1.3）是它的**唯一消费者**（写后结算 / 删前取释放值 —— 两侧同源 ✓）。
type Footprinter interface {
	// Footprint 返回 (owner, rel) 对象的记账字节（物理）；不支持/不可得 ⇒ ok=false。
	Footprint(owner, rel string) (int64, bool)
}

// PhysBound 是**写前物理上界**能力：把逻辑字节折算为**记账口径的上界**（严格 ≥ 实际占用）。
//
// 用途：配额**预占**（G1「绝不超真实配额」的机制）。写前无法知道精确物理尺寸（secretdata 的
// parity/meta pad 只能实测）⇒ 上界预占 → 写后按 `Footprinter` **实测结算**（设计 §2）。
type PhysBound interface {
	// MaxPhysical 返回 logical 字节在该卷上的记账上界（非正 logical ⇒ 原样返回）。
	MaxPhysical(owner, rel string, logical int64) int64
}

// CapacityOwner 是**整卷容量**能力：卷自身容量的读取与运行期调整。
//
// 与「每用户配额」并列（设计 §1.3）：配额卷同时提供二者；整卷容量是卷池上限的事实源。
type CapacityOwner interface {
	// Capacity 返回整卷上限字节（0 = 不限制）。
	Capacity() int64
	// SetCapacity 运行期调整整卷上限（0 = 取消上限）。
	SetCapacity(n int64)
}

// StatsProvider 是**容量统计**能力：返回该卷当前占用/总量（后端自报）。
//
// 与 `registry.VolumeStats` 同语义（C1 外部卷容量纳管）；本接口是其在统一模型下的形态，
// 便于配额层按需探测（`ok=false` = 后端不支持查询，调用方按"未知"处置，禁静默当 0 ✗）。
type StatsProvider interface {
	VolumeStats() (used, total int64, ok bool)
}

// StateGeneration 是**卷状态的廉价新鲜度探针**（可选能力，设计
// docs/designs/2026-10-28-volume-state-maintenance.md §3.1）。
//
// 语义：返回 `(卷根相对路径 path 的子树)` 的代际号——同代际 ⇒ 该子树的结构视图与内容
// 派生值自上次取用后未变。缓存层据此在**命中前**比对，变化即回源；这是「跨进程/外部
// 改动」的收敛路径（本进程写穿已由装饰器覆盖）。
//
// 实现约束（**必须廉价，不得全量遍历**）：
//   - 本地卷：目录 mtime + 直接子项数（一次 Stat，O(1)）；
//   - secretdata：容器 meta 版本/条目数；
//   - S3：子树 LastModified 最大值；baidupcs：ctime + 条目数。
//
// 未实现 ⇒ 缓存退化为 TTL-only（外部卷）或写穿-only（本地卷），零回归。
//
// **本接口只报告事实，不得实现 list 缓存**（门禁 R40：加速统一由 pkg/volumestate 提供）。
type StateGeneration interface { // NOSONAR: S8196 — 能力接口（非 -er 角色命名），表达能力语义，设计保留
	StateGen(ctx context.Context, path string) (uint64, error)
}
