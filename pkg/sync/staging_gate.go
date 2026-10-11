// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// staging_gate.go 实现**默认强制**的本地 staging 配额门卫（用户裁定 2026-10-10：
// 独立 staging Scope + 全部外部卷强制；不需要 staging 或只占部分空间的卷**显式实现**
// StagingQuotaExempt 才豁免，获取配额句柄后自行接管的卷实现 StagingQuotaCapable）。
//
// 机制化保证（防新卷遗漏）：装配层对所有外部卷 `be.FS()` **统一包 StagingQuotaGateFS**
// ——WriteFile 前预留 size 到独立 staging Scope、写后释放。未实现任何豁免/自管接口的
// 卷默认被强制预留（fail-safe：宁多勿漏，本地磁盘打满风险由装配层统一兜底，不依赖
// 卷实现者自觉）。

import (
	"context"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// innerFS 是**透明装饰器**（暴露被包装内层 FS 的包装层，如 trusted 的 Guard/Wrap、
// capacity.CapacityFS、本包的 StagingQuotaGateFS）的统一标记。Innermost 据此逐层下探，
// 使「卷自身能力」（豁免/自管）的判定不被转发层污染。
type innerFS interface{ Inner() FS }

// Innermost 逐层剥去透明装饰器，返回**最内层原始 FS**（无限/自环防御）。
// 能力探测必须用它：装饰器会无条件回显可选能力接口（透传契约），直接对装饰层做类型
// 断言会恒真，从而击穿「默认强制 gate」等分支（P0：staging 配额门卫恒被跳过）。
func Innermost(fs FS) FS {
	for {
		u, ok := fs.(innerFS)
		if !ok {
			return fs
		}
		nxt := u.Inner()
		if nxt == nil || nxt == fs {
			return fs
		}
		fs = nxt
	}
}

// StagingQuotaExempted 报告**最内层原始卷**是否显式豁免本地 staging 记账。
func StagingQuotaExempted(fs FS) bool {
	if ex, ok := Innermost(fs).(StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// ApplyStagingQuota 按用户裁定（2026-10-10）对任意 FS 施加本地 staging 配额：
//   - 最内层原始卷显式豁免（StagingQuotaExempt）→ 原样返回；
//   - 最内层原始卷自管（StagingQuotaCapable）→ 经外层透明转发注入句柄后原样返回；
//   - 否则包 StagingQuotaGateFS 强制预留/释放（fail-safe：新卷未实现也不漏）。
//
// q == nil（未装配独立 staging 配额）→ 原样返回（零回归）。
// **判据一律基于 Innermost(fs)**：直接对 `fs` 做断言会被装饰器透传接口恒真击穿。
func ApplyStagingQuota(fs FS, q StagingQuotaTracker) FS {
	if StagingQuotaExempted(fs) {
		return fs
	}
	if q == nil {
		return fs
	}
	if _, ok := Innermost(fs).(StagingQuotaCapable); ok {
		if sc, ok := fs.(StagingQuotaCapable); ok {
			sc.WithStagingQuota(q) // 外层透传至最内层自管卷
		}
		return fs
	}
	return WrapStagingQuota(fs, q)
}

// StagingQuotaGateFS 包装任意 FS 的本地 staging 配额门卫：WriteFile 前经
// StagingQuotaTracker.ReserveUsage(size) 预留、写后 ReleaseUsage——本地磁盘防打满。
// 所有 FS 方法透明委派 inner（gate 只拦截写路径）。
type StagingQuotaGateFS struct {
	inner FS
	quota StagingQuotaTracker
}

// WrapStagingQuota 包装 fs 为带 staging 配额门卫的 FS（quota nil = 不记账直通——
// 无独立 staging Scope 装配时零回归）。
func WrapStagingQuota(fs FS, q StagingQuotaTracker) *StagingQuotaGateFS {
	return &StagingQuotaGateFS{inner: fs, quota: q}
}

var _ FS = (*StagingQuotaGateFS)(nil)

// Inner 返回底层 FS（透明装饰器标记：使 Innermost/Wrap 能下探，避免把本转发层误判为
// 「卷自带 meta/能力」——P0 同源缺陷的纵深防御）。
func (g *StagingQuotaGateFS) Inner() FS { return g.inner }

func (g *StagingQuotaGateFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return g.inner.ListDir(ctx, path)
}
func (g *StagingQuotaGateFS) Stat(ctx context.Context, path string) (*Entry, error) {
	return g.inner.Stat(ctx, path)
}
func (g *StagingQuotaGateFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return g.inner.OpenRead(ctx, path)
}
func (g *StagingQuotaGateFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	if g.quota != nil {
		if err := g.quota.ReserveUsage(ctx, size); err != nil {
			return err
		}
		defer g.quota.ReleaseUsage(size)
	}
	return g.inner.WriteFile(ctx, path, r, size, mtime)
}
func (g *StagingQuotaGateFS) Rename(ctx context.Context, from, to string) error {
	return g.inner.Rename(ctx, from, to)
}
func (g *StagingQuotaGateFS) Delete(ctx context.Context, path string) error {
	return g.inner.Delete(ctx, path)
}
func (g *StagingQuotaGateFS) MakeDir(ctx context.Context, path string) error {
	return g.inner.MakeDir(ctx, path)
}

// ---- 能力透传（P1-5 修复）：gate 是最外层装饰器时必须透明转发可选能力，
// 否则转存的 WriteIfAbsent/ReserveSpace/meta.Provider/damaged 标记/staging 豁免/自管
// 会在此层被吞（断言落在 gate 上恒 false），与「门卫只拦截写路径」的定位矛盾。
// 写类能力（WriteIfAbsent）与 WriteFile 同语义：预留 → 写 → 释放。----

// WriteIfAbsent 委托 inner（预留在前、释放在后；inner 未实现 → ErrUnsupported 供调用方回落）。
func (g *StagingQuotaGateFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	pia, ok := g.inner.(WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("sync: 底层未实现 WriteIfAbsent: %w", ErrUnsupported)
	}
	if g.quota != nil {
		if err := g.quota.ReserveUsage(ctx, size); err != nil {
			return false, err
		}
		defer g.quota.ReleaseUsage(size)
	}
	return pia.WriteIfAbsent(ctx, path, r, size, mtime)
}

// ReserveSpace 委托 inner（容量预检，不涉及本地 staging 记账）。
func (g *StagingQuotaGateFS) ReserveSpace(ctx context.Context, path string, size int64) error {
	if rs, ok := g.inner.(ReserveSpace); ok {
		return rs.ReserveSpace(ctx, path, size)
	}
	return fmt.Errorf("sync: 底层未实现 ReserveSpace: %w", ErrUnsupported)
}

// IsLocalVolume 委托 inner（本地性自述不涉及路径）。
func (g *StagingQuotaGateFS) IsLocalVolume() bool {
	if lv, ok := g.inner.(LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// Move 委托 inner（inner 未实现 → ErrUnsupported）。
func (g *StagingQuotaGateFS) Move(ctx context.Context, from, to string) error {
	if mv, ok := g.inner.(Mover); ok {
		return mv.Move(ctx, from, to)
	}
	return fmt.Errorf("sync: 底层未实现 Move: %w", ErrUnsupported)
}

// Copy 委托 inner（inner 未实现 → ErrUnsupported）。
func (g *StagingQuotaGateFS) Copy(ctx context.Context, from, to string) error {
	if cp, ok := g.inner.(Copier); ok {
		return cp.Copy(ctx, from, to)
	}
	return fmt.Errorf("sync: 底层未实现 Copy: %w", ErrUnsupported)
}

// Link 委托 inner（inner 未实现 → ErrUnsupported）。
func (g *StagingQuotaGateFS) Link(ctx context.Context, from, to string) error {
	if lk, ok := g.inner.(Linker); ok {
		return lk.Link(ctx, from, to)
	}
	return fmt.Errorf("sync: 底层未实现 Link: %w", ErrUnsupported)
}

// OpenRangeRead 委托 inner（inner 未实现 → ErrUnsupported）。
func (g *StagingQuotaGateFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	if rr, ok := g.inner.(RangeReader); ok {
		return rr.OpenRangeRead(ctx, path, offset, size)
	}
	return nil, fmt.Errorf("sync: 底层未实现 OpenRangeRead: %w", ErrUnsupported)
}

// DirectURL 委托 inner（inner 未实现 → ErrUnsupported）。
func (g *StagingQuotaGateFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if d, ok := g.inner.(DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("sync: 底层未实现 DirectURL: %w", ErrUnsupported)
}

// FileMeta 委托 inner（meta.Provider：转存读端分块校验 / 下载校验消费）。
func (g *StagingQuotaGateFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if pv, ok := g.inner.(meta.Provider); ok {
		return pv.FileMeta(ctx, rel)
	}
	return nil, fmt.Errorf("sync: 底层未实现 FileMeta: %w", ErrUnsupported)
}

// UpdateMetaExtra 委托 inner（damaged 源完整性 / transfer_verified 旁路标记）。
func (g *StagingQuotaGateFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := g.inner.(interface {
		UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
	})
	if !ok {
		return fmt.Errorf("sync: 底层未实现 UpdateMetaExtra: %w", ErrUnsupported)
	}
	return ue.UpdateMetaExtra(ctx, rel, extra)
}

// WithStagingQuota 委托 inner（StagingQuotaCapable 自管：注入配额句柄）。
func (g *StagingQuotaGateFS) WithStagingQuota(q StagingQuotaTracker) {
	if qc, ok := g.inner.(StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner（StagingQuotaExempt 显式豁免）。
func (g *StagingQuotaGateFS) ExemptStagingQuota() bool {
	if ex, ok := g.inner.(StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// 编译期断言：gate 实现全部能力接口（能力透传契约，防未来新增能力接口后遗漏）。
var (
	_ WriteIfAbsent       = (*StagingQuotaGateFS)(nil)
	_ ReserveSpace        = (*StagingQuotaGateFS)(nil)
	_ LocalVolume         = (*StagingQuotaGateFS)(nil)
	_ Mover               = (*StagingQuotaGateFS)(nil)
	_ Copier              = (*StagingQuotaGateFS)(nil)
	_ Linker              = (*StagingQuotaGateFS)(nil)
	_ RangeReader         = (*StagingQuotaGateFS)(nil)
	_ DirectURLProvider   = (*StagingQuotaGateFS)(nil)
	_ meta.Provider       = (*StagingQuotaGateFS)(nil)
	_ StagingQuotaCapable = (*StagingQuotaGateFS)(nil)
	_ StagingQuotaExempt  = (*StagingQuotaGateFS)(nil)
)
