// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// storage_volume.go —— **统一存储卷模型**的接口定义（M0，加法式，零行为变更）。
//
// 完整设计：`docs/designs/2026-10-16-quota-volume-layer.md` §11。现状取证：
//
//   - **数据面已统一** ✓：本地与外部卷都经 `sync.FS`（本包 `ExternalBackend` 只有 `FS()+Close()`）；
//   - **存储身份未统一** ✗：`*storage.Root` 在非测试源码出现 254 处 —— 它是**局部特例**；
//   - **装配分派未统一** ✗：`local` **不是**注册后端（`RegisterBackend` 显式拒绝），装配层以
//     `Type == "" || TypeLocal` 走特例分支；
//   - **类型分类部分存在** ⚠️：本包已有 wrapper 事实源（静态基线 + `MarkWrapperType` 动态登记）
//     ⇒ `KindOf` 只是把它**显式化**为类型化枚举。
//
// **为什么定义在本包而非 `pkg/volume`**：`pkg/volume` 不能 import `pkg/sync`
// （`sync → pkg/files/meta → pkg/volume` 成环 ✗）；本包已同时持有两侧 ✓，且
// `ExternalBackend`（同形状：FS + Close）本就在此 ✓ ⇒ 二者**并列**便于对照迁移。

// StorageVolume 是**存储卷的统一身份**：数据面 + 结构性类型 + 生命周期。
//
// 迁移目标（设计 §11.4）：`local` 与外部后端都实现本接口 ⇒ 装配层删除
// `Type == "" || TypeLocal` 特例分支；`*storage.Root` 成为 local 后端的**内部实现细节**。
//
// 与 `ExternalBackend` 的关系：本接口是其**统一化超集**（多出 `Name`/`Kind`）——
// M1 让两者由同一类型满足（见 storage_volume_kind_test.go 的接缝断言 ✓）。
//
// **配额语义**（设计 §1）：配额层只包 `KindStorage` 卷 ⇒ 它与真实字节之间**无变换层**
// ⇒ 它看到的尺寸**天生是物理**（`storage.Root.Stat` 在加密卷上即密文长度）⇒
// **无需知道卷是否加密** ✓。`KindWrapper` 卷**不注册配额层**（其读写都落在某个存储卷上）。
type StorageVolume interface {
	// Name 返回卷名（装配期唯一）。
	Name() string
	// Kind 返回结构性类型（`volume.KindStorage` / `volume.KindWrapper`）。
	Kind() volume.Kind
	// FS 返回数据面视图（装配后只读：多次调用返回同一实例）。
	FS() syncpkg.FS
	// Close 释放后端资源（幂等：重复调用安全）。
	Close() error
}

// 可选**能力**接口（`Footprinter` / `PhysBound` / `CapacityOwner` / `StatsProvider`）
// 定义在 `pkg/volume`（它们不依赖 `pkg/sync`，可留在域模型包内 ✓）。
