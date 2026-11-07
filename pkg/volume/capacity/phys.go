// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

// phys.go 是卷级容量记账的**物理占入口径**（用户裁定 2026-10-29：按实际占用算容量）。
//
// 背景：此前 `CapacityFS` 一律按**逻辑字节**（明文 size / 对象 size）记账。对加密卷
// （secretdata）而言物理量含**逻辑不可推**的部分——内容分块的 chunk 头/尾部、meta blob
// 的 1–2MiB pad、XOR parity（可达逻辑 2×）⇒ 小文件/多目录场景物理可达逻辑的千倍，
// 卷容量上限可被绕过。用户裁定按**实际占用**记账。
//
// 实现方式：卷可实现可选能力（与 `pkg/server/files_phys.go` 消费的同一组方法，不新增契约）：
//
//	PhysFootprint(rel string) (int64, bool) // 卷内键 → 实测物理字节（O(1)）
//	PhysBound(logical int64) int64          // 逻辑字节 → 物理上界（写前预留用）
//
// 能力沿装饰链向内找首个实现者（`syncpkg.Innermost`，与 files 侧同判据）。无该能力 ⇒
// 回落逻辑口径（普通本地/外部卷零回归）。

import (
	"context"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// physSizer 是卷自报物理足迹的可选能力（secretdata 等「掌握物理布局」的卷实现）。
type physSizer interface { // NOSONAR: S8196 — 能力接口，设计保留（与 files.VolumePhysFootprinter 同源）
	// PhysFootprint 返回卷内键 rel 的**实测物理字节**（O(1)：meta 直取，不遍历内容）。
	PhysFootprint(rel string) (int64, bool)
	// PhysBound 返回逻辑字节对应的物理**上界**（写前预留；写后按实测收敛）。
	PhysBound(logical int64) int64
}

// physCapability 返回该容量视图可用的物理足迹能力（无 ⇒ ok=false，回落逻辑口径）。
func (f *CapacityFS) physCapability() (physSizer, bool) {
	inner := syncpkg.Innermost(f.inner)
	if inner == nil {
		return nil, false
	}
	p, ok := inner.(physSizer)
	return p, ok
}

// physSize 返回 rel 的记账字节数：有物理能力 ⇒ 实测物理；否则 ⇒ 逻辑大小。
func (f *CapacityFS) physSize(ctx context.Context, rel string) int64 {
	if p, ok := f.physCapability(); ok {
		if n, found := p.PhysFootprint(rel); found {
			return n
		}
		return 0
	}
	return f.innerSize(ctx, rel)
}

// physSizeChecked 同 physSize，但区分「Stat 失败」与「不存在」（写失败复核用）。
func (f *CapacityFS) physSizeChecked(ctx context.Context, rel string) (int64, bool) {
	if p, ok := f.physCapability(); ok {
		if n, found := p.PhysFootprint(rel); found {
			return n, true
		}
		// 物理能力存在但该 rel 查不到：可能是「不存在」（0, true），也可能是 meta 暂不可读。
		// 用 inner Stat 区分（存在但无物理记录 ⇒ 退回逻辑值，避免少记）。
		n, ok := f.innerSizeChecked(ctx, rel)
		return n, ok
	}
	return f.innerSizeChecked(ctx, rel)
}

// physBound 返回逻辑字节的物理上界（写前预留）：无能力 ⇒ 逻辑值本身。
func (f *CapacityFS) physBound(logical int64) int64 {
	if logical <= 0 {
		return logical
	}
	if p, ok := f.physCapability(); ok {
		if b := p.PhysBound(logical); b > 0 {
			return b
		}
	}
	return logical
}
