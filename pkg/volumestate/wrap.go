// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"context"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// CacheFS 是卷状态缓存装饰器（P2：装配期唯一入口处包住卷的 FS）。
//
//	读：ListDir → Cache（命中/回源/新鲜度校验）；Stat → 恒回源（A 类权威，单对象成本低）
//	写：WriteFile/Delete/Rename/MakeDir/WriteIfAbsent/Move/Copy/Link 成功后**自动失效**
//	    （P3：调用方无需知道缓存存在——这是 #874 类缺陷的结构解）
//	其余：全量透传（可选能力透传契约，R21/R23 门禁要求）
//
// 为什么 ListDir 缓存而 Stat 不缓存：目录列举是「结构视图」（C 类，可容忍有界过期且成本高），
// 单对象 Stat 是「内容派生值入口」（A 类，安全判定依赖它，必须新鲜）。
type CacheFS struct {
	inner syncpkg.FS
	vol   string
	cache *Cache
}

// Wrap 把卷的 FS 包装为「读缓存 + 写失效」视图。cache 为 nil ⇒ 原样返回（零回归）。
func Wrap(volName string, inner syncpkg.FS, cache *Cache) syncpkg.FS {
	if inner == nil || cache == nil || volName == "" {
		return inner
	}
	return &CacheFS{inner: inner, vol: volName, cache: cache}
}

// Inner 返回底层 FS（装饰器契约：R23 自动发现 + 特权解包）。
func (f *CacheFS) Inner() syncpkg.FS { return f.inner }

// ---- 基础 7 方法 ----

// ListDir 走缓存：命中且新鲜 ⇒ 快照；否则回源（回源时若有代际能力则记录代际）。
func (f *CacheFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	probe := f.genProbe()
	return f.cache.Get(ctx, f.vol, path, probe, func(fctx context.Context) ([]syncpkg.Entry, error) {
		return f.inner.ListDir(fctx, path)
	})
}

// Stat 恒回源（A 类：尺寸/checksum 的安全判定必须新鲜；单对象成本远低于目录列举）。
func (f *CacheFS) Stat(ctx context.Context, path string) (*syncpkg.Entry, error) {
	return f.inner.Stat(ctx, path)
}

func (f *CacheFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return f.inner.OpenRead(ctx, path)
}

func (f *CacheFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	if err := f.inner.WriteFile(ctx, key, path, r, size, mtime); err != nil {
		return err
	}
	f.invalidate(path)
	return nil
}

func (f *CacheFS) Rename(ctx context.Context, from, to string) error {
	if err := f.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	f.invalidate(from)
	f.invalidate(to)
	return nil
}

func (f *CacheFS) Delete(ctx context.Context, path string) error {
	if err := f.inner.Delete(ctx, path); err != nil {
		return err
	}
	f.invalidate(path)
	return nil
}

func (f *CacheFS) MakeDir(ctx context.Context, path string) error {
	if err := f.inner.MakeDir(ctx, path); err != nil {
		return err
	}
	f.invalidate(path)
	return nil
}

// genProbe 返回代际探测函数（卷实现 volume.StateGeneration 时可用；否则 nil）。
//
// 探测**穿透装饰链**（沿 Inner 向内找首个实现者）：装配层可能已套 trusted/capacity 等
// 装饰器，它们都转发 StateGen，但直接用内层实现可少走几跳（两者等价）。
func (f *CacheFS) genProbe() func(context.Context, string) (uint64, bool) {
	sg := stateGenOf(f.inner)
	if sg == nil {
		return nil
	}
	return func(ctx context.Context, path string) (uint64, bool) {
		g, err := sg.StateGen(ctx, path)
		if err != nil {
			return 0, false
		}
		return g, true
	}
}

// stateGenOf 沿装饰链向内找首个 volume.StateGeneration 实现。
func stateGenOf(fs syncpkg.FS) volume.StateGeneration {
	for cur := fs; cur != nil; {
		if sg, ok := cur.(volume.StateGeneration); ok {
			return sg
		}
		inner, ok := cur.(interface{ Inner() syncpkg.FS })
		if !ok {
			return nil
		}
		nxt := inner.Inner()
		if nxt == fs || nxt == nil {
			return nil
		}
		fs = nxt
	}
	return nil
}

// invalidate 发失效事件（卷维度 ref）。
func (f *CacheFS) invalidate(path string) {
	f.cache.Invalidate(ObjectRef{Volume: f.vol, Path: path})
}

// ---- 可选能力透传（R21/R23：包装型 FS 必须全量转发，否则能力在装饰层静默丢失）----

// WriteIfAbsent 委托 inner 并在**写入成功**后失效（已存在 ⇒ written=false 无需失效）。
func (f *CacheFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	pia, ok := f.inner.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("volumestate: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	written, err := pia.WriteIfAbsent(ctx, path, r, size, mtime)
	if err != nil {
		return written, err
	}
	if written {
		f.invalidate(path)
	}
	return written, nil
}

// WriteFromStagedPath 委托 inner（staging 复用路径；成功后失效）。
func (f *CacheFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	sp, ok := f.inner.(syncpkg.StagedPathReuse)
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 WriteFromStagedPath: %w", syncpkg.ErrUnsupported)
	}
	if err := sp.WriteFromStagedPath(ctx, relPath, stagedPath, size, mtime); err != nil {
		return err
	}
	f.invalidate(relPath)
	return nil
}

// Move 委托 inner；成功后失效源与目标。
func (f *CacheFS) Move(ctx context.Context, from, to string) error {
	mv, ok := f.inner.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	if err := mv.Move(ctx, from, to); err != nil {
		return err
	}
	f.invalidate(from)
	f.invalidate(to)
	return nil
}

// Copy 委托 inner；成功后失效目标（源不变）。
func (f *CacheFS) Copy(ctx context.Context, from, to string) error {
	cp, ok := f.inner.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	if err := cp.Copy(ctx, from, to); err != nil {
		return err
	}
	f.invalidate(to)
	return nil
}

// CopyFile 委托 inner 的**同卷对象级复制**快路径（secretdata 的 blob 直拷）；成功后失效目标。
//
// 为什么必须转发：该能力由卷（secretdata）实现、由装配层经 `Innermost` 探测；若装饰器不实现，
// 上层只能直接调用**最内层**实例 ⇒ 绕过本层失效 ⇒ 列表要等 TTL 才更新（默认 5 分钟，不可接受）。
// 转发后调用方可以走装饰后的视图，失效与复制在同一个动作里完成（不留"记得失效"的纪律）。
func (f *CacheFS) CopyFile(ctx context.Context, from, to string) error {
	cf, ok := f.inner.(interface {
		CopyFile(ctx context.Context, from, to string) error
	})
	if !ok {
		// 能力可能由**最内层**卷实现（中间装饰器如 trusted/capacity 不转发该匿名能力）⇒
		// 穿透取实现，保证「经装饰视图复制」也能成立（否则调用方只能直连最内层而绕过失效）。
		cf, ok = syncpkg.Innermost(f.inner).(interface {
			CopyFile(ctx context.Context, from, to string) error
		})
	}
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 CopyFile: %w", syncpkg.ErrUnsupported)
	}
	if err := cf.CopyFile(ctx, from, to); err != nil {
		return err
	}
	f.invalidate(to)
	return nil
}

// Link 委托 inner；成功后失效目标（硬链接新建对象）。
func (f *CacheFS) Link(ctx context.Context, from, to string) error {
	lk, ok := f.inner.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	if err := lk.Link(ctx, from, to); err != nil {
		return err
	}
	f.invalidate(to)
	return nil
}

// ReserveSpace 委托 inner（容量预检，不涉及状态缓存）。
func (f *CacheFS) ReserveSpace(ctx context.Context, path string, size int64) error {
	if rs, ok := f.inner.(syncpkg.ReserveSpace); ok {
		return rs.ReserveSpace(ctx, path, size)
	}
	return fmt.Errorf("volumestate: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
}

// IsLocalVolume 委托 inner（本地性自述）。
func (f *CacheFS) IsLocalVolume() bool {
	if lv, ok := f.inner.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// OpenRangeRead 委托 inner。
func (f *CacheFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	if rr, ok := f.inner.(syncpkg.RangeReader); ok {
		return rr.OpenRangeRead(ctx, path, offset, size)
	}
	return nil, fmt.Errorf("volumestate: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
}

// DirectURL 委托 inner。
func (f *CacheFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if d, ok := f.inner.(syncpkg.DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("volumestate: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
}

// FileMeta 委托 inner（meta.Provider：读侧分块校验 / 转存读回）。
func (f *CacheFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if pv, ok := f.inner.(meta.Provider); ok {
		return pv.FileMeta(ctx, rel)
	}
	return nil, fmt.Errorf("volumestate: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
}

// UpdateMetaExtra 委托 inner（旁路标记；**不失效内容视图**——extra 不改变结构/内容）。
func (f *CacheFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := f.inner.(syncpkg.MetaExtraUpdater)
	if !ok {
		return fmt.Errorf("volumestate: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return ue.UpdateMetaExtra(ctx, rel, extra)
}

// WithStagingQuota 委托 inner（自管 staging 配额）。
func (f *CacheFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if qc, ok := f.inner.(syncpkg.StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner。
func (f *CacheFS) ExemptStagingQuota() bool {
	if ex, ok := f.inner.(syncpkg.StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// 编译期断言：能力透传契约（新增能力接口后此处缺失会被 R21 门禁发现）。
var (
	_ syncpkg.FS                  = (*CacheFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*CacheFS)(nil)
	_ syncpkg.StagedPathReuse     = (*CacheFS)(nil)
	_ syncpkg.Mover               = (*CacheFS)(nil)
	_ syncpkg.Copier              = (*CacheFS)(nil)
	_ syncpkg.Linker              = (*CacheFS)(nil)
	_ syncpkg.ReserveSpace        = (*CacheFS)(nil)
	_ syncpkg.LocalVolume         = (*CacheFS)(nil)
	_ syncpkg.RangeReader         = (*CacheFS)(nil)
	_ syncpkg.DirectURLProvider   = (*CacheFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*CacheFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*CacheFS)(nil)
	_ meta.Provider               = (*CacheFS)(nil)
	_ syncpkg.MetaExtraUpdater    = (*CacheFS)(nil)
	// 同卷对象级复制（secretdata blob 直拷）：转发 + 失效目标，见 CopyFile 注释。
	_ interface {
		CopyFile(ctx context.Context, from, to string) error
	} = (*CacheFS)(nil)
)

// ListDirNames 转发轻量列举（未实现时回落 ListDir）：装配期只按名字分类时避免外部卷
// 逐条补精确 size（见 sync.NameLister）。这里**不写缓存**——轻量列举的 Size 是近似值，
// 不能污染目录快照（读路径仍走 ListDir + 缓存）。
func (c *CacheFS) ListDirNames(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if nl, ok := syncpkg.AssertNameLister(c.inner); ok {
		return nl.ListDirNames(ctx, p)
	}
	return c.inner.ListDir(ctx, p)
}

// _ 能力断言：漏转发会让上层静默降级（装配期退回高成本列举）。
var (
	_ syncpkg.NameLister = (*CacheFS)(nil)
)
