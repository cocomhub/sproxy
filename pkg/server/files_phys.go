// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
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
// 可信链（`Guard(Wrap(be.FS()))`），能力方法在最内层真实实现上 ⇒ 用 `syncpkg.Innermost` 取
// （#786 教训：装饰器层断言会把「支持」判成「不支持」而静默回落）。

// filesPhysFootprinter 实现 files.VolumePhysFootprinter。
type filesPhysFootprinter struct{ h *Handlers }

// rawFS 返回卷的最内层真实 FS（能力断言用）。
func (p filesPhysFootprinter) rawFS(volName string) (syncpkg.FS, bool) {
	fsys, ok := p.h.externalFSFor(volName)
	if !ok || fsys == nil {
		return nil, false
	}
	return syncpkg.Innermost(fsys), true
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
	raw, ok := p.rawFS(volName)
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
	raw, ok := p.rawFS(volName)
	if !ok {
		return 0
	}
	f, ok := raw.(interface{ PhysBound(logical int64) int64 })
	if !ok {
		return 0
	}
	return f.PhysBound(logical)
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
