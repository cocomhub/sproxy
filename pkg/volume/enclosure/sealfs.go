// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"fmt"
	"io"
	"path"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// sealfs.go 实现 **FS 级封印装饰器**：把封装声明产生的封印规则施加到**任意 FS 视图**。
//
// 为什么需要它：本地卷的封印在 `storage.Root` 层——那是所有面（HTTP/DAV/S3/分享/云任务）
// 的**唯一咽喉**；而外部卷/远端后端（s3/baidupcs/pikpak/cluster/secretdata…）**没有 Root**
// ⇒ 只能在 FS 视图上套装饰器；并且**必须套在装配处**（`applyEnclosureSeals` 对
// `acc.external` 就地包装），否则 N 个直连点（`be.FS()` 的十余处调用者）各自绕过封印。
//
// 语义与 Root 层**逐条一致**（复用 `storage.SealAt`/`SealError`/`sealCovers`，单一事实源，
// 不复制判据）：
//   - **写**（WriteFile/MakeDir/Delete/Rename/Move/Copy/Link/WriteIfAbsent/
//     WriteFromStagedPath/ReserveSpace/UpdateMetaExtra）：**任一端**命中 ⇒ `ErrSealed`
//     （HTTP 409），无配置开关（含「移入封印区 / 从封印区移出」）；
//   - **读**（OpenRead/OpenRangeRead/Stat/FileMeta）：`SealHidden`（默认）⇒ 错误同时匹配
//     `fs.ErrNotExist`（404）与 `ErrSealed`（可归因）；`SealReadable` ⇒ 放行；
//   - **列举**（ListDir）：`SealHidden` 下**过滤**被封印条目（不泄漏封装布局）；readable 保留；
//   - **直链**（DirectURL）：命中封印 ⇒ 抑制（直链 = 客户端直连源存储，与封印目标冲突）；
//   - **Inner()**：暴露被包装 FS ⇒ `syncpkg.Innermost` 可下探取得**特权视图**（封装卷自身
//     必须能读写被封区间——与本地源的 `NewLocalFS(卷根)` 特权句柄同型）。
//
// 透明装饰器契约（R21）：无条件转发全部可选能力（否则上层能力断言恒 false = 静默降级）；
// `BlockAccessor` 与其余装饰器同口径豁免（引擎回退语义需 Capability 探测，设计 §8）。
type SealFS struct {
	syncpkg.FS
	rules []storage.SealRule
}

// NewSealFS 构造封印装饰器视图（rules 为空 = 无封印，行为等价于裸视图）。
func NewSealFS(inner syncpkg.FS, rules []storage.SealRule) *SealFS {
	return &SealFS{FS: inner, rules: rules}
}

// Inner 透明暴露被包装 FS（使 `syncpkg.Innermost` 能下探到原始卷能力：能力探测语义）。
func (s *SealFS) Inner() syncpkg.FS { return s.FS }

// UnsealedFS 返回**同后端的未封印视图**（精确剥掉封印层本身）。
//
// 为什么不用 `syncpkg.Innermost` 取特权视图：Innermost 会把**配额（CapacityFS）/可信
// （trusted Wrap/Guard）等装饰器一并剥掉** ⇒ 源卷侧的容量结算与 sidecar 校验全部失效。
// 本方法只剥封印层，其余装饰器原样保留（封装卷自身读写被封区间 = 特权，但仍受源卷配额/
// 完整性约束）。
func (s *SealFS) UnsealedFS() syncpkg.FS { return s.FS }

// Rules 返回本视图施加的封印规则（诊断/测试用；副本，调用方不可改内部状态）。
func (s *SealFS) Rules() []storage.SealRule {
	out := make([]storage.SealRule, len(s.rules))
	copy(out, s.rules)
	return out
}

// readErr 读侧封印判定（命中且 SealHidden → 可归因「不存在」错误；readable → nil）。
func (s *SealFS) readErr(rel string) error {
	return storage.SealError(storage.SealAt(s.rules, rel), rel, false)
}

// writeErr 写侧封印判定（命中 → ErrSealed，任何模式都不放开）。
func (s *SealFS) writeErr(rel string) error {
	return storage.SealError(storage.SealAt(s.rules, rel), rel, true)
}

// sealFilter 在 SealHidden 下过滤被封印的直接子条目（readable 保留 = 只读透视语义）。
func (s *SealFS) sealFilter(dir string, entries []syncpkg.Entry) []syncpkg.Entry {
	if len(s.rules) == 0 || len(entries) == 0 {
		return entries
	}
	out := entries[:0:0]
	for _, e := range entries {
		if rule := storage.SealAt(s.rules, path.Join(dir, e.Name)); rule != nil && rule.Mode == storage.SealHidden {
			continue
		}
		out = append(out, e)
	}
	return out
}

// ListDir 列举：目标本身被封印（hidden）→ 不存在；否则过滤被封印的直接子条目。
func (s *SealFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := s.readErr(p); err != nil {
		return nil, err
	}
	entries, lerr := s.FS.ListDir(ctx, p)
	if lerr != nil {
		return nil, lerr
	}
	return s.sealFilter(p, entries), nil
}

// Stat 元信息：被封印（hidden）→ 不存在（父目录不受影响）。
func (s *SealFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if err := s.readErr(p); err != nil {
		return nil, err
	}
	return s.FS.Stat(ctx, p)
}

// OpenRead 读：被封印（hidden）→ 不存在。
func (s *SealFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := s.readErr(p); err != nil {
		return nil, err
	}
	return s.FS.OpenRead(ctx, p)
}

// OpenRangeRead 定点读（RangeReader）：与 OpenRead 同判据。
func (s *SealFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if err := s.readErr(p); err != nil {
		return nil, err
	}
	rr, ok := s.FS.(syncpkg.RangeReader)
	if !ok {
		return nil, fmt.Errorf("SealFS: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
	}
	return rr.OpenRangeRead(ctx, p, offset, size)
}

// FileMeta 读 sidecar（meta.Provider）：与 OpenRead 同判据。
func (s *SealFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if err := s.readErr(rel); err != nil {
		return nil, err
	}
	pv, ok := s.FS.(meta.Provider)
	if !ok {
		return nil, fmt.Errorf("SealFS: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
	}
	return pv.FileMeta(ctx, rel)
}

// DirectURL 直链：命中封印 ⇒ **抑制**（ok=false，与封装视图默认策略同口径）；未命中转发。
func (s *SealFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if storage.SealAt(s.rules, relPath) != nil {
		return "", false, nil
	}
	d, ok := s.FS.(syncpkg.DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("SealFS: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
	}
	return d.DirectURL(ctx, relPath)
}

// WriteFile 写：被封印 ⇒ ErrSealed（409）。
func (s *SealFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if err := s.writeErr(p); err != nil {
		return err
	}
	return s.FS.WriteFile(ctx, key, p, r, size, mtime)
}

// WriteIfAbsent 原子唯一写：被封印 ⇒ ErrSealed（不得返回 (false,nil)——会被读成「已存在」）。
func (s *SealFS) WriteIfAbsent(ctx context.Context, p string, r io.Reader, size, mtime int64) (bool, error) {
	if err := s.writeErr(p); err != nil {
		return false, err
	}
	w, ok := s.FS.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("SealFS: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	return w.WriteIfAbsent(ctx, p, r, size, mtime)
}

// MakeDir 建目录：被封印 ⇒ ErrSealed。
func (s *SealFS) MakeDir(ctx context.Context, p string) error {
	if err := s.writeErr(p); err != nil {
		return err
	}
	return s.FS.MakeDir(ctx, p)
}

// Delete 删除：被封印 ⇒ ErrSealed。
func (s *SealFS) Delete(ctx context.Context, p string) error {
	if err := s.writeErr(p); err != nil {
		return err
	}
	return s.FS.Delete(ctx, p)
}

// Rename 改名：**两端**都判（移入封印区 / 从封印区移出都必须拒绝）。
func (s *SealFS) Rename(ctx context.Context, from, to string) error {
	if err := s.writeErr(from); err != nil {
		return err
	}
	if err := s.writeErr(to); err != nil {
		return err
	}
	return s.FS.Rename(ctx, from, to)
}

// Move 同卷移动（Mover）：两端都判。
func (s *SealFS) Move(ctx context.Context, from, to string) error {
	if err := s.writeErr(from); err != nil {
		return err
	}
	if err := s.writeErr(to); err != nil {
		return err
	}
	m, ok := s.FS.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	return m.Move(ctx, from, to)
}

// Copy 同卷复制（Copier）：两端都判（复制进封印区同样破坏封装语义）。
func (s *SealFS) Copy(ctx context.Context, from, to string) error {
	if err := s.writeErr(from); err != nil {
		return err
	}
	if err := s.writeErr(to); err != nil {
		return err
	}
	c, ok := s.FS.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	return c.Copy(ctx, from, to)
}

// Link 硬链接（Linker）：两端都判（链接到封印区 = 绕过封印的旁路读）。
func (s *SealFS) Link(ctx context.Context, from, to string) error {
	if err := s.writeErr(from); err != nil {
		return err
	}
	if err := s.writeErr(to); err != nil {
		return err
	}
	l, ok := s.FS.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	return l.Link(ctx, from, to)
}

// WriteFromStagedPath staging 复用（StagedPathReuse）：relPath 判封印（stagedPath 是本地临时路径）。
func (s *SealFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	if err := s.writeErr(relPath); err != nil {
		return err
	}
	sp, ok := s.FS.(syncpkg.StagedPathReuse)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 WriteFromStagedPath: %w", syncpkg.ErrUnsupported)
	}
	return sp.WriteFromStagedPath(ctx, relPath, stagedPath, size, mtime)
}

// ReserveSpace 容量预检（ReserveSpace）：命中封印 ⇒ ErrSealed（预检不应为封印区预留）。
func (s *SealFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	if err := s.writeErr(relPath); err != nil {
		return err
	}
	rs, ok := s.FS.(syncpkg.ReserveSpace)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
	}
	return rs.ReserveSpace(ctx, relPath, size)
}

// UpdateMetaExtra 旁路 sidecar 更新（MetaExtraUpdater）：属写面 ⇒ 命中封印拒绝。
func (s *SealFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	if err := s.writeErr(rel); err != nil {
		return err
	}
	u, ok := s.FS.(syncpkg.MetaExtraUpdater)
	if !ok {
		return fmt.Errorf("SealFS: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return u.UpdateMetaExtra(ctx, rel, extra)
}

// IsLocalVolume 转发本地性自述（不涉及路径）；底层未实现 = 外部卷（全仓「未实现默认外部」）。
func (s *SealFS) IsLocalVolume() bool {
	lv, ok := s.FS.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

// WithStagingQuota 转发 staging 配额钩子（不涉及路径）。
func (s *SealFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if c, ok := s.FS.(syncpkg.StagingQuotaCapable); ok {
		c.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 转发 staging 豁免自述；底层未实现 = 不豁免（默认强制 gate，fail-safe）。
func (s *SealFS) ExemptStagingQuota() bool {
	ex, ok := s.FS.(syncpkg.StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
}

// 编译期能力断言（R21：装饰器必须声明全部可选能力；BlockAccessor 有意豁免——引擎回退
// 语义需 Capability 探测，与其余装饰器同口径）。
var (
	_ syncpkg.FS                  = (*SealFS)(nil)
	_ syncpkg.RangeReader         = (*SealFS)(nil)
	_ syncpkg.DirectURLProvider   = (*SealFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*SealFS)(nil)
	_ syncpkg.ReserveSpace        = (*SealFS)(nil)
	_ syncpkg.LocalVolume         = (*SealFS)(nil)
	_ syncpkg.Mover               = (*SealFS)(nil)
	_ syncpkg.Copier              = (*SealFS)(nil)
	_ syncpkg.Linker              = (*SealFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*SealFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*SealFS)(nil)
	_ syncpkg.MetaExtraUpdater    = (*SealFS)(nil)
	_ syncpkg.StagedPathReuse     = (*SealFS)(nil)
	_ meta.Provider               = (*SealFS)(nil)
)

// sealBackend 是把封印装饰器挂到后端句柄上的包装：`FS()` 返回封印视图，其余能力（如
// Presigner）**不转发**——预签名 URL 可让客户端直连源存储，与封印目标冲突（fail-closed）。
type sealBackend struct {
	registry.ExternalBackend
	fs syncpkg.FS
}

// FS 返回封印视图（装配后只读：多次调用返回同一实例）。
func (b *sealBackend) FS() syncpkg.FS { return b.fs }

// UnsealedFS 逐层剥净封印（同一外部卷被多个封装声明分别封印时为多层包装）：包装内层仍是
// 封印后端时继续下探，直到得到未封印视图（其余装饰器保留）。
func (b *sealBackend) UnsealedFS() syncpkg.FS {
	if u, ok := b.ExternalBackend.(unsealedSource); ok {
		return u.UnsealedFS()
	}
	return b.ExternalBackend.FS()
}

// unsealedSource 是「同后端未封印视图」的可选能力（装配层取特权源视图用）。
type unsealedSource interface{ UnsealedFS() syncpkg.FS }

// UnsealedSource 取**未封印**的源视图：逐层剥掉封印装饰器（同类多层封印也能剥净），
// 其余装饰器（配额/可信）原样保留；未封印时返回 `be.FS()` 原样。
func UnsealedSource(be registry.ExternalBackend) syncpkg.FS {
	if be == nil {
		return nil
	}
	if u, ok := be.(unsealedSource); ok {
		return u.UnsealedFS()
	}
	return be.FS()
}

// SealBackend 用封印规则包装外部后端：`be.FS()` 变为封印视图，而**特权视图**经
// `syncpkg.Innermost(be.FS())` 取得（封装卷自身读写被封区间不受影响）。
func SealBackend(be registry.ExternalBackend, rules []storage.SealRule) registry.ExternalBackend {
	if be == nil || len(rules) == 0 {
		return be
	}
	return &sealBackend{ExternalBackend: be, fs: NewSealFS(be.FS(), rules)}
}

// SealedAt 在后端（可含「调用方面 owner 映射」与封印装饰层）上查覆盖 rootRel 的封印规则
// （无封印/未覆盖 → nil）。
//
// 供**旁路写面**复用同一份匹配语义：此类通道不经后端 FS 视图（如 S3 预签名直传由客户端直连
// 对象存储），封印装饰器无从拦截，只能在签发期按规则判定。逐层剥开 owner 映射与封印装饰，
// 任一层的规则命中即返回（多层串联 = 规则并集，与读写路径的封锁效果一致）。
func SealedAt(be registry.ExternalBackend, rootRel string) *storage.SealRule {
	for be != nil {
		switch t := be.(type) {
		case *sealBackend:
			if sfs, ok := t.fs.(*SealFS); ok {
				if r := storage.SealAt(sfs.rules, rootRel); r != nil {
					return r
				}
			}
			be = t.ExternalBackend
		case *ownerFacingBackend:
			be = t.ExternalBackend
		default:
			return nil
		}
	}
	return nil
}

// ApplyBackendSeals 返回「恰好施加 rules 封印」的后端：已封印的后端先**剥净**封印层，再按
// rules 重新施加（rules 空 = 纯剥离 ⇒ 返回裸后端）。
//
// 与 SealBackend 的分工：SealBackend 用于**装配期一次性**注入（层只加不撤）；本函数用于
// **运行期声明增删**——必须让封印规则集合与「当前声明集合」恒等，否则删除封装卷后旧规则
// 永久残留（源区间被无故封印：数据不可见 + 无法复用）。剥净再施加也保证任一时刻只有一个
// 封印层（`Rules()` 可读、诊断不歧义；等价性上多层叠加虽同样保守，但不可回收）。
func ApplyBackendSeals(be registry.ExternalBackend, rules []storage.SealRule) registry.ExternalBackend {
	for {
		sb, ok := be.(*sealBackend)
		if !ok || sb.ExternalBackend == nil {
			break
		}
		be = sb.ExternalBackend
	}
	if be == nil || len(rules) == 0 {
		return be
	}
	return SealBackend(be, rules)
}

// ListDirNames 转发轻量列举（未实现时回落 ListDir）：装配期只按名字分类时避免
// 外部卷逐条补精确 size（1.2 万 blob ⇒ 数百次 meta CLI + 网盘限流）。
func (s *SealFS) ListDirNames(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := s.readErr(p); err != nil {
		return nil, err
	}
	nl, ok := syncpkg.AssertNameLister(s.FS)
	if !ok {
		return s.ListDir(ctx, p)
	}
	entries, lerr := nl.ListDirNames(ctx, p)
	if lerr != nil {
		return nil, lerr
	}
	return s.sealFilter(p, entries), nil
}
