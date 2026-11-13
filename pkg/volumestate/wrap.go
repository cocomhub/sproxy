// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"context"
	"fmt"
	"io"
	"strings"

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
//
// 回源后**本地卷**会在 fetch 内补齐条目的 sidecar checksum（R46 S2）——必须在 fetch 内，
// 填充结果才能随快照一起被缓存，命中路径零 I/O。
func (f *CacheFS) ListDir(ctx context.Context, path string) ([]syncpkg.Entry, error) {
	probe := f.genProbe()
	return f.cache.Get(ctx, f.vol, path, probe, func(fctx context.Context) ([]syncpkg.Entry, error) {
		entries, err := f.inner.ListDir(fctx, path)
		if err != nil {
			return nil, err
		}
		f.fillLocalChecksums(fctx, path, entries)
		return entries, nil
	})
}

// fillLocalChecksums 为**本地卷**的列举条目补齐 sidecar 明文 `TotalSHA256`（设计
// docs/designs/2026-11-11-checksum-ledger-read-path.md §S2；读口径见 §4）。
//
// 边界（为什么这样设计）：
//   - **仅本地卷**：远端卷（s3/baidupcs/…）的 sidecar 读是每文件一次网络往返 ⇒
//     N 条目 = N 往返，属性能回归；本地卷读本地小文件，且**随目录快照缓存**
//     （每目录每代际一次）。
//   - 已有 `Checksum` 的条目（secretdata/s3 自报）与目录条目跳过。
//   - sidecar 缺失/损坏/解析失败 ⇒ 留空（不报错、不伪造），由消费方 `stat` 兜底。
func (f *CacheFS) fillLocalChecksums(ctx context.Context, dirPath string, entries []syncpkg.Entry) {
	if !f.IsLocalVolume() {
		return
	}
	pv, ok := f.inner.(meta.Provider)
	if !ok {
		return
	}
	dir := strings.Trim(dirPath, "/")
	for i := range entries {
		e := &entries[i]
		if e.IsDir || e.Checksum != "" {
			continue
		}
		rel := e.Name
		if dir != "" {
			rel = dir + "/" + e.Name
		}
		fm, err := pv.FileMeta(ctx, rel)
		if err != nil || fm == nil || fm.TotalSHA256 == "" {
			continue
		}
		if e.Checksums == nil {
			e.Checksums = make(map[string]string, 1)
		}
		e.Checksum = fm.TotalSHA256
		e.ChecksumType = "sha256"
		e.Checksums["sha256"] = fm.TotalSHA256
	}
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

// ChecksumAndSize 转发可选能力（R21）。
func (f *CacheFS) ChecksumAndSize(ctx context.Context, rel string) (string, int64, error) {
	if cp, ok := f.inner.(meta.ChecksumProvider); ok {
		return cp.ChecksumAndSize(ctx, rel)
	}
	return "", 0, fmt.Errorf("volumestate: 底层未实现 ChecksumAndSize: %w", syncpkg.ErrUnsupported)
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

// 编译期能力断言（门禁 R21）：透明装饰器必须声明全部 `pkg/sync` 可选能力，否则上层
// `fs.(能力)` 断言恒 false ⇒ **静默降级**（批量校验 / 秒传刷新被悄悄跳过）。
var (
	_ syncpkg.BatchVerifier  = (*CacheFS)(nil)
	_ syncpkg.RapidRefresher = (*CacheFS)(nil)
)

// VerifyMany 转发批量校验（透明装饰器必须转发，否则上层 `AssertBatchVerifier` 恒 false）。
//
// 底层未实现该能力 ⇒ 返回 **nil**：调用方已有「结果长度与请求不符 ⇒ 无法判定 ⇒ 跳过批量
// 校验」的防御分支，且 Put 内部本就逐块即时复核 ⇒ 零回归。
// **不得**返回「全部 mismatch」——那会被读成需要重传而触发无谓重传。
func (f *CacheFS) VerifyMany(ctx context.Context, items []syncpkg.VerifyItem) []syncpkg.VerifyResult {
	bv, ok := f.inner.(syncpkg.BatchVerifier)
	if !ok {
		return nil
	}
	return bv.VerifyMany(ctx, items)
}

// RapidRefresh 转发秒传刷新（零传输刷新远端 meta）；底层未实现 ⇒ ErrUnsupported（调用方回落重传）。
func (f *CacheFS) RapidRefresh(ctx context.Context, item syncpkg.VerifyItem) (bool, error) {
	rr, ok := f.inner.(syncpkg.RapidRefresher)
	if !ok {
		return false, fmt.Errorf("CacheFS: 底层未实现 RapidRefresh: %w", syncpkg.ErrUnsupported)
	}
	return rr.RapidRefresh(ctx, item)
}

// ---- 存储审查能力转发（2026-11-11 能力下沉 pkg/sync 后必须转发）----
//
// 为什么必须转发：装饰器不实现这两个接口时，上层 `fs.(StorageInspector)` 断言恒 false ⇒
// **静默降级**（把"底层支持审查"误判为"不支持"）。底层未实现 ⇒ 返回 ErrUnsupported，
// 由调用方按"未实现"处置，而**不是**让断言失败。
func (f *CacheFS) Trace(ctx context.Context, rel string, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	if f, ok := any(f.inner).(syncpkg.StorageInspector); ok {
		return f.Trace(ctx, rel, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

func (f *CacheFS) Explain(ctx context.Context, realRel string, mode syncpkg.InspectMode) (*syncpkg.RealExplanation, error) {
	if f, ok := any(f.inner).(syncpkg.StorageInspector); ok {
		return f.Explain(ctx, realRel, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

func (f *CacheFS) TraceTree(ctx context.Context, rel string, depth int, mode syncpkg.InspectMode) (*syncpkg.PathTrace, error) {
	if f, ok := any(f.inner).(syncpkg.TreeInspector); ok {
		return f.TraceTree(ctx, rel, depth, mode)
	}
	return nil, syncpkg.ErrUnsupported
}

// CleanupStaleDirMeta 转发目录 meta 残留清理（2026-11-12 能力下沉 pkg/sync 后必须转发）。
//
// 与审查能力同口径：不转发 ⇒ 上层 `fs.(syncpkg.DirMetaCleaner)` 断言恒 false ⇒ 静默降级。
func (f *CacheFS) CleanupStaleDirMeta(ctx context.Context, container string) (int, error) {
	if c, ok := any(f.inner).(syncpkg.DirMetaCleaner); ok {
		return c.CleanupStaleDirMeta(ctx, container)
	}
	return 0, syncpkg.ErrUnsupported
}

var (
	_ syncpkg.StorageInspector = (*CacheFS)(nil)
	_ syncpkg.TreeInspector    = (*CacheFS)(nil)
	_ syncpkg.DirMetaCleaner   = (*CacheFS)(nil)
)

// 编译期契约：本装饰器必须支持 `DecoratorCapabilities` 的**全部**共同可选能力
// ⇒ 新增可选能力（加入该聚合接口）会**在此处编译失败**，强制补齐转发。
var (
	_ syncpkg.DecoratorCapabilities = (*CacheFS)(nil)
)
