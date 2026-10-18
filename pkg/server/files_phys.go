// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// files_phys.go 把**卷自报的物理足迹能力**暴露给写内核（`files.VolumePhysFootprinter`），
// 使配额账本按**真实物理占用**记账（配额模型 v3，用户裁定 2026-10-14）。
//
// 为什么需要这一层：物理量含**逻辑不可推**的部分——secretdata 的 meta blob pad 到
// [base, 2·base)（默认 1–2MB/文件、1–2MB/目录，尺寸混淆设计，与文件逻辑大小无关）、XOR parity
// （可达逻辑 2×）。按逻辑记账在小文件/多目录场景下物理可达逻辑的**千倍**。
//
// 断言必须**穿透装饰器**（Guard/Wrap/CapacityFS/StagingQuotaGateFS）：`externalFSFor` 返回的是
// 可信链（`Guard(Wrap(be.FS()))`），能力方法在链上某个真实实现上 ⇒ **沿链向内找首个实现者**
// （`physFS`；#786 教训：装饰器层断言会把「支持」判成「不支持」而静默回落）。能力断言用
// `syncpkg.Innermost`（见下方 physFS；**不在业务代码里手写 Inner 链遍历**——R22 门禁）。
// 2026-10-18 勘误：旧头注释称「不能用 Innermost，因为 SecretdataFS 自身实现 Inner()」，
// 与代码事实相反（secretdata 包内**无** `func (*SecretdataFS) Inner()`）⇒ 已删该错误理由。

// filesPhysFootprinter 实现 files.VolumePhysFootprinter。
type filesPhysFootprinter struct{ h *Handlers }

// physFS 返回卷装饰链的**最内层真实 FS**（能力断言用），并要求其满足 probe。
//
// 用受认可的 `syncpkg.Innermost`（**不能**在业务代码里手写「Inner 链遍历」——R22 门禁：
// 那会绕过 Guard/Wrap/Capacity 语义）。当前 `SecretdataFS` **不实现 Inner** ⇒ Innermost
// 恰好停在它上面 ⇒ 能力命中（若将来它包了可剥离内层，应改用 `pkg/volume/capacity` 层暴露的
// 「链上首个实现者」助手，而不是在 pkg/server 手写遍历）。
func (p filesPhysFootprinter) physFS(volName string, probe func(syncpkg.FS) bool) (syncpkg.FS, bool) {
	fsys, ok := p.h.externalFSFor(volName)
	if !ok || fsys == nil {
		return nil, false
	}
	inner := syncpkg.Innermost(fsys)
	if inner == nil || !probe(inner) {
		return nil, false
	}
	return inner, true
}

// filePhysFS 返回实现**文件足迹**能力的 FS（`PhysFootprint`）。
func (p filesPhysFootprinter) filePhysFS(volName string) (syncpkg.FS, bool) {
	return p.physFS(volName, func(f syncpkg.FS) bool {
		_, ok := f.(interface {
			PhysFootprint(rel string) (int64, bool)
		})
		return ok
	})
}

// dirPhysFS 返回实现**目录足迹**能力的 FS（`DirChainFootprint`/`DirPhysBound`）。
func (p filesPhysFootprinter) dirPhysFS(volName string) (syncpkg.FS, bool) {
	return p.physFS(volName, func(f syncpkg.FS) bool {
		_, ok := f.(interface {
			DirChainFootprint(ctx context.Context, rel string) (int64, bool)
		})
		return ok
	})
}

// PhysFootprint 实现 files.VolumePhysFootprinter：用户可见路径 → 卷内键（`Location.FSPath()`，
// 唯一拼接点）→ 卷自报实测物理字节。
func (p filesPhysFootprinter) PhysFootprint(volName, owner, remotePath string) (int64, bool) {
	if p.h.volSet == nil {
		return 0, false
	}
	v, ok := p.h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return 0, false
	}
	loc, kerr := v.ResolveUserLocation(owner, remotePath)
	if kerr != nil {
		return 0, false
	}
	raw, ok := p.filePhysFS(volName)
	if !ok {
		return 0, false
	}
	f, ok := raw.(interface {
		PhysFootprint(rel string) (int64, bool)
	})
	if !ok {
		return 0, false
	}
	return f.PhysFootprint(loc.FSPath())
}

// PhysBound 实现 files.VolumePhysFootprinter：逻辑字节 → 物理**上界**（预占用；结算按实测收敛）。
// 无该能力/未知卷 ⇒ 0（调用方回落既有折算，零回归）。
func (p filesPhysFootprinter) PhysBound(volName string, logical int64) int64 {
	raw, ok := p.filePhysFS(volName)
	if !ok {
		return 0
	}
	f, ok := raw.(interface{ PhysBound(logical int64) int64 })
	if !ok {
		return 0
	}
	return f.PhysBound(logical)
}

// DirChainFootprint 实现 files.VolumePhysFootprinter：目录（含祖先链）的物理足迹实测。
func (p filesPhysFootprinter) DirChainFootprint(volName, owner, remotePath string) (int64, bool) {
	raw, key, ok := p.resolveDir(volName, owner, remotePath)
	if !ok {
		return 0, false
	}
	f, ok := raw.(interface {
		DirChainFootprint(ctx context.Context, rel string) (int64, bool)
	})
	if !ok {
		return 0, false
	}
	return f.DirChainFootprint(context.Background(), key)
}

// DirPhysBound 实现 files.VolumePhysFootprinter：单个目录 meta blob 的物理上界（预占用）。
func (p filesPhysFootprinter) DirPhysBound(volName string) int64 {
	raw, ok := p.dirPhysFS(volName)
	if !ok {
		return 0
	}
	f, ok := raw.(interface{ DirPhysBound() int64 })
	if !ok {
		return 0
	}
	return f.DirPhysBound()
}

// resolveDir 把（卷, owner, 用户可见路径）解析为最内层真实 FS + 卷内键。
func (p filesPhysFootprinter) resolveDir(volName, owner, remotePath string) (syncpkg.FS, string, bool) {
	if p.h.volSet == nil || remotePath == "" {
		return nil, "", false
	}
	v, ok := p.h.volSet.ByName(volName)
	if !ok || !v.Authorize(owner) {
		return nil, "", false
	}
	loc, kerr := v.ResolveUserLocation(owner, remotePath)
	if kerr != nil {
		return nil, "", false
	}
	raw, ok := p.dirPhysFS(volName)
	if !ok {
		return nil, "", false
	}
	return raw, loc.FSPath(), true
}

// physBoundFor 是装配层统一入口（预占用上界；无能力 ⇒ 原样返回 logical）。
func (h *Handlers) physBoundFor(volName string, logical int64) int64 {
	if logical <= 0 {
		return logical
	}
	if b := (filesPhysFootprinter{h: h}).PhysBound(volName, logical); b > logical {
		return b
	}
	return logical
}
