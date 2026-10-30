// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/files/meta"
)

// prefixfs.go 是**桶前缀视图**装饰器：把「调用方键空间」（用户相对路径 rel）映射到
// 目标 FS 的**桶键空间**（`<prefix>/rel`），并在 ListDir/Stat 结果里剥回前缀，使调用方
// 看到的键空间与源一致。
//
// 用途（统一前缀入口——HTTP 上传/转存/备份/同步引擎同形）：
//   - 外部卷（共享卷）：底层键空间恒 `<owner>/<bucket>/...`（`volume.Location.FSPath`），
//     前缀 = `<owner>/user/`；
//   - 本地卷：底层 root 是租户根（`<storage_root>/<owner>`），前缀 = `user/`；
//   - 备份 manifest 功能桶：前缀 = `<owner>/backup/` / `backup/`。
//
// 若把用户相对路径（无 owner/bucket 段）直接喂给 Guard/Wrap：
//  1. `Guard` 的 `BucketOf` 会把 `docs/meta/x` 的第二段 meta 误判为 meta 桶 → 合法文件失败；
//  2. `meta.MetaPath` 无桶段 → sidecar 落用户可见目录（不被 `IsMetaPath` 过滤、与用户真实
//     `*.meta` 同路径互毁）；
//  3. 无 owner 前缀 → 不同 owner 同名相对路径互相覆盖（跨租户破坏）。
//
// 前缀化后三者同时消除（`Wrap` 在 `<owner>/user/rel` 上把 sidecar 写到 `<owner>/meta/rel.meta`）。
//
// **透明装饰器契约（R21）**：无条件转发全部可选能力接口（能力探测/`Innermost` 下探不得
// 因本层而恒 false——否则 RangeReader/meta.Provider/WriteIfAbsent/移动复制等静默降级）。
// 唯一有意豁免：`BlockAccessor`（直接转发会让引擎对无该能力的底层走块级路径，
// `ErrUnsupported` 无法表达「未实现」，需 Capability 探测语义——与其余装饰器同口径）。
type PrefixFS struct {
	FS
	// Prefix 是桶键空间前缀（恒以 "/" 结尾，除根映射外）。
	Prefix string
}

// NewPrefixFS 构造桶前缀视图（prefix 建议以 "/" 结尾，如 `alice/user/`）。
func NewPrefixFS(fs FS, prefix string) *PrefixFS {
	return &PrefixFS{FS: fs, Prefix: prefix}
}

// Inner 透明暴露被包装 FS（使 `Innermost` / `ApplyStagingQuota` 能下探到原始卷能力）。
func (p *PrefixFS) Inner() FS { return p.FS }

// Key 把调用方键映射到桶键空间。空键（根）必须映射为前缀**去尾斜杠**：否则
// `prefix+""` = `alice/user/` → Guard 拆到空段而被拒。
func (p *PrefixFS) Key(k string) string {
	t := trimSlashes(k)
	if t == "" {
		return trimSuffixSlash(p.Prefix)
	}
	return p.Prefix + t
}

// strip 把桶键空间路径剥回调用方键空间。
// strip 把底层 FS 键还原为视图相对键。
//
// **视图根必须互逆**（独立审核 P1，2026-10-18）：`Key("")` 返回 `trimSuffixSlash(Prefix)`
// （如 `"user"`，**去掉尾斜杠**），若只按 `Prefix`（`"user/"`）去前缀则 `strip("user") == "user"`
// ✗（应为 `""`）⇒ 根条目的 `Path` 泄漏内层桶键 ⇒ 下游把它当**用户路径**用：备份空子树时
// `WalkEntries` 原样返回根条目 ⇒ 目标 `PrefixFS("user/")` 上 `MakeDir("user")` ⇒ **幽灵目录
// `user/user`**；同步/删除传播也会按错路径 `Stat`/`Delete`。⇒ 显式映射回视图根 `""`。
func (p *PrefixFS) strip(k string) string {
	if k == trimSuffixSlash(p.Prefix) {
		return ""
	}
	return trimPrefix(k, p.Prefix)
}

// entry 剥回条目的 Path（nil 安全）。
func (p *PrefixFS) entry(e *Entry) *Entry {
	if e != nil {
		e.Path = p.strip(e.Path)
	}
	return e
}

// ListDir 列目录（剥回前缀）。
func (p *PrefixFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	es, err := p.FS.ListDir(ctx, p.Key(path))
	for i := range es {
		es[i].Path = p.strip(es[i].Path)
	}
	return es, err
}

// ListDirNames 转发轻量列举能力（未实现时回落 ListDir）：装配期只分类名字的场景
// 可跳过外部卷逐条补齐精确 size 的高成本路径（见 NameLister）。
func (p *PrefixFS) ListDirNames(ctx context.Context, path string) ([]Entry, error) {
	nl, ok := AssertNameLister(p.FS)
	if !ok {
		return p.ListDir(ctx, path)
	}
	es, err := nl.ListDirNames(ctx, p.Key(path))
	for i := range es {
		es[i].Path = p.strip(es[i].Path)
	}
	return es, err
}

// Stat 返回条目（剥回前缀）。
//
// **视图根**（path==""）恒视为存在的目录：底层 `<prefix>` 可能尚未创建（首次 pull、
// 空用户桶），而调用方（sync 引擎枚举源根）需要「空目录」语义——与 `StorageFS.Stat("")`
// 返回合成根目录的既有约定一致；否则首次 pull 会报「源路径不存在」。真实错误
// （网络/权限等）不吞。
func (p *PrefixFS) Stat(ctx context.Context, path string) (*Entry, error) {
	e, err := p.FS.Stat(ctx, p.Key(path))
	if path == "" && e == nil && (err == nil || errors.Is(err, os.ErrNotExist)) {
		return &Entry{Path: "", IsDir: true}, nil
	}
	return p.entry(e), err
}

// OpenRead 读文件（加前缀）。
func (p *PrefixFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return p.FS.OpenRead(ctx, p.Key(path))
}

// WriteFile 写文件（加前缀）。
func (p *PrefixFS) WriteFile(ctx context.Context, key LogicalKey, path string, r io.Reader, size, mtime int64) error {
	return p.FS.WriteFile(ctx, key, p.Key(path), r, size, mtime)
}

// WriteIfAbsent 转发原子唯一写（加前缀）；底层未实现 → ErrUnsupported（调用方回落）。
func (p *PrefixFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	w, ok := p.FS.(WriteIfAbsent)
	if !ok {
		// 不得返回 (false,nil)：「未实现」会被调用方读成「目标已存在」（转存幂等分支）→ 静默丢写。
		return false, fmt.Errorf("PrefixFS: 底层未实现 WriteIfAbsent: %w", ErrUnsupported)
	}
	return w.WriteIfAbsent(ctx, p.Key(path), r, size, mtime)
}

// Rename 改名（两端都加前缀）。
func (p *PrefixFS) Rename(ctx context.Context, from, to string) error {
	return p.FS.Rename(ctx, p.Key(from), p.Key(to))
}

// Delete 删除（加前缀）。
func (p *PrefixFS) Delete(ctx context.Context, path string) error {
	return p.FS.Delete(ctx, p.Key(path))
}

// MakeDir 建目录（加前缀）。
func (p *PrefixFS) MakeDir(ctx context.Context, path string) error {
	return p.FS.MakeDir(ctx, p.Key(path))
}

// WriteFromStagedPath 转发 staging 路径复用（relPath 走本层键映射；stagedPath 原样透传）。
func (p *PrefixFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	sp, ok := p.FS.(StagedPathReuse)
	if !ok {
		return fmt.Errorf("PrefixFS: 底层未实现 WriteFromStagedPath: %w", ErrUnsupported)
	}
	return sp.WriteFromStagedPath(ctx, p.Key(relPath), stagedPath, size, mtime)
}

// Move 转发同卷移动（两端加前缀）；底层未实现 → ErrUnsupported。
func (p *PrefixFS) Move(ctx context.Context, from, to string) error {
	m, ok := p.FS.(Mover)
	if !ok {
		return fmt.Errorf("PrefixFS: 底层未实现 Move: %w", ErrUnsupported)
	}
	return m.Move(ctx, p.Key(from), p.Key(to))
}

// Copy 转发同卷复制（两端加前缀）；底层未实现 → ErrUnsupported。
func (p *PrefixFS) Copy(ctx context.Context, from, to string) error {
	c, ok := p.FS.(Copier)
	if !ok {
		return fmt.Errorf("PrefixFS: 底层未实现 Copy: %w", ErrUnsupported)
	}
	return c.Copy(ctx, p.Key(from), p.Key(to))
}

// Link 转发硬链接（两端加前缀）；底层未实现 → ErrUnsupported。
func (p *PrefixFS) Link(ctx context.Context, from, to string) error {
	l, ok := p.FS.(Linker)
	if !ok {
		return fmt.Errorf("PrefixFS: 底层未实现 Link: %w", ErrUnsupported)
	}
	return l.Link(ctx, p.Key(from), p.Key(to))
}

// ReserveSpace 转发卷容量预检（加前缀）；底层未实现 → ErrUnsupported（调用方跳过卷配额）。
func (p *PrefixFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	rs, ok := p.FS.(ReserveSpace)
	if !ok {
		return fmt.Errorf("PrefixFS: 底层未实现 ReserveSpace: %w", ErrUnsupported)
	}
	return rs.ReserveSpace(ctx, p.Key(relPath), size)
}

// OpenRangeRead 转发定点读（加前缀）；底层未实现 → ErrUnsupported。
func (p *PrefixFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	rr, ok := p.FS.(RangeReader)
	if !ok {
		return nil, fmt.Errorf("PrefixFS: 底层未实现 OpenRangeRead: %w", ErrUnsupported)
	}
	return rr.OpenRangeRead(ctx, p.Key(path), offset, size)
}

// DirectURL 转发直链（加前缀）；底层未实现 → ErrUnsupported（调用方回落服务端转发）。
func (p *PrefixFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	d, ok := p.FS.(DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("PrefixFS: 底层未实现 DirectURL: %w", ErrUnsupported)
	}
	return d.DirectURL(ctx, p.Key(relPath))
}

// FileMeta 转发 sidecar 读取（加前缀）；底层未实现 → ErrUnsupported。
func (p *PrefixFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	pv, ok := p.FS.(meta.Provider)
	if !ok {
		return nil, fmt.Errorf("PrefixFS: 底层未实现 FileMeta: %w", ErrUnsupported)
	}
	return pv.FileMeta(ctx, p.Key(rel))
}

// IsLocalVolume 转发本地性自述（不涉及路径）；底层未实现 = 外部卷（全仓「未实现默认外部」）。
func (p *PrefixFS) IsLocalVolume() bool {
	lv, ok := p.FS.(LocalVolume)
	return ok && lv.IsLocalVolume()
}

// ExemptStagingQuota 转发 staging 豁免自述；底层未实现 = 不豁免（默认强制 gate，fail-safe）。
func (p *PrefixFS) ExemptStagingQuota() bool {
	ex, ok := p.FS.(StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
}

// WithStagingQuota 转发到内层（`ApplyStagingQuota` 对「最内层自管但外层不转发」会静默
// 不注入且不包 gate → staging 配额失效；装饰器必须透明转发）。
func (p *PrefixFS) WithStagingQuota(q StagingQuotaTracker) {
	if sc, ok := p.FS.(StagingQuotaCapable); ok {
		sc.WithStagingQuota(q)
	}
}

// trimSlashes 去掉首尾斜杠（空串安全）。
func trimSlashes(s string) string { return trimSuffixSlash(trimPrefixSlash(s)) }

// trimPrefixSlash 去掉**全部**前导斜杠。
func trimPrefixSlash(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	return s
}

// trimSuffixSlash 去掉全部尾随斜杠。
func trimSuffixSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

// trimPrefix 去前缀（对齐 strings.TrimPrefix，但仅在命中时拷贝）。
func trimPrefix(s, prefix string) string {
	if len(prefix) > 0 && len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

// ---- 能力平权断言（R21：新增可选能力接口必须在此补断言或显式豁免）----

var (
	_ FS                  = (*PrefixFS)(nil)
	_ innerFS             = (*PrefixFS)(nil)
	_ WriteIfAbsent       = (*PrefixFS)(nil)
	_ ReserveSpace        = (*PrefixFS)(nil)
	_ LocalVolume         = (*PrefixFS)(nil)
	_ Mover               = (*PrefixFS)(nil)
	_ Copier              = (*PrefixFS)(nil)
	_ Linker              = (*PrefixFS)(nil)
	_ RangeReader         = (*PrefixFS)(nil)
	_ DirectURLProvider   = (*PrefixFS)(nil)
	_ StagingQuotaCapable = (*PrefixFS)(nil)
	_ StagingQuotaExempt  = (*PrefixFS)(nil)
	_ meta.Provider       = (*PrefixFS)(nil)
	_ StagedPathReuse     = (*PrefixFS)(nil)
	// BlockAccessor 有意豁免（见文件头注释）。
)
