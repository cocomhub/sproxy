// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/capacity"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// ownerfs.go 实现 **owner 限定声明的「调用方面」键映射视图**（设计 §4.1「客户端可达区间」）。
//
// **两个视图（必须分离，2026-10-13 用户裁定 A + CI 实测）**：
//   - **wrapper 面**：`NewFS(src, d)` = 区域相对视图（`PrefixFS(特权源, 声明前缀)`）——封装处理器
//     （secretdata 等）把视图当通用 FS，在区域内自建**随机容器名 + blob 名**（区域相对键）；
//   - **调用方面**：本文件的 `ownerFS`，装在 `backend.FS()` 上——调用方（HTTP/DAV/S3/分享/云任务/
//     接缝）恒用 `<owner>/<bucket>/<rel>`。
//
// 二者**共用一个 FS 对象**是不可能的：CI 实证 `secretdata: 写目录 meta <随机容器> 失败: 键首段
// owner "lUFXQHBU8KCp9OFI" 与声明 owner "alice" 不符`（单一映射无法同时满足两种键空间）。
//
// 映射（调用方键 → 源卷键）：
//
//	<owner>/<bucket>/<rel>  →  <declOwner>/<bucket>/<sub>/<rel>
//
// **不变式（逐条都有判据/测试）**：
//  1. **owner 由声明给定（权威）**，键上的 owner 只做**精确相等**校验（不折叠、不通配；
//     不符 ⇒ 不存在语义，不泄漏他人数据存在性）；
//  2. **bucket 白名单 = 保留功能桶（全部 8 个）**，与 `Decl.Footprint()` 的封印足迹**同源**
//     ⇒「能碰到的键集合」与「封住的键集合」结构性相等（否则会出现「映射可写、但没被封」的洞）；
//  3. **严格段校验**：拒绝空段、`.`、`..`、反斜杠、控制字符 ⇒ 绝不使用会归一化的 `path.Join`
//     （否则 `alice/user/../meta/x` 会被静默折叠、越出区域甚至越出 owner）；
//  4. **根/无 owner 键拒绝**（区域是**每桶**的，卷根无对应物 ⇒ 明确规定为不存在，不做猜测）；
//  5. 映射是**定值插入**：单射、无别名（不同调用方键从不落到同一源卷键）；
//  6. **真值报告键翻译**：链上 `TrueUsageReporter` 在区域键空间工作，本层实现该接口并翻译键，
//     使 `capacity.TrueUsageOf(be.FS()).TrueSize(callerKey)` 仍成立（装饰器不冒充真值）。
type ownerFS struct {
	syncpkg.FS
	owner string
	sub   string
	vol   string
}

// errOwnerMismatch 表示键首段与声明 owner 不符（越权访问被拒；对外表现为「不存在」）。
type errOwnerMismatch struct{ got, want string }

func (e *errOwnerMismatch) Error() string {
	return fmt.Sprintf("enclosure: 键首段 owner %q 与声明 owner %q 不符（越权访问被拒）", e.got, e.want)
}

func (e *errOwnerMismatch) Is(target error) bool { return target == fs.ErrNotExist }

// errBadKey 表示键不满足「调用方面」契约（段非法 / 根键 / 非保留桶）：一律按不存在拒绝。
type errBadKey struct {
	vol    string
	key    string
	reason string
}

func (e *errBadKey) Error() string {
	return fmt.Sprintf("enclosure: 卷 %q 的键 %q 非法（%s）", e.vol, e.key, e.reason)
}

func (e *errBadKey) Is(target error) bool { return target == fs.ErrNotExist }

// validSegments 把键切成段并做**严格校验**（不变式 3）：空段/`.`/`..`/反斜杠/控制字符一律拒绝。
// 刻意不使用 `path.Join`（它会静默折叠 `..` ⇒ 越界）。
func validSegments(key string) ([]string, error) {
	if strings.Contains(key, `\`) {
		return nil, errors.New("含反斜杠")
	}
	t := strings.Trim(key, "/")
	if t == "" {
		return nil, errors.New("空键（卷根无对应区域）")
	}
	segs := strings.Split(t, "/")
	for _, s := range segs {
		if s == "" {
			return nil, errors.New("含空路径段")
		}
		if s == "." || s == ".." {
			return nil, errors.New("含 . / .. 路径段")
		}
		for i := 0; i < len(s); i++ {
			if s[i] < 0x20 || s[i] == 0x7f {
				return nil, errors.New("含控制字符")
			}
		}
	}
	return segs, nil
}

// key 把调用方键映射为源卷键（不变式 1/2/3/4）。
func (o *ownerFS) key(k string) (string, error) {
	segs, err := validSegments(k)
	if err != nil {
		return "", &errBadKey{vol: o.vol, key: k, reason: err.Error()}
	}
	if len(segs) < 2 {
		// 只有 <owner> 一段：无法确定桶（区域是每桶的）⇒ 明确规定为不存在。
		return "", &errBadKey{vol: o.vol, key: k, reason: "缺少 <bucket> 段（区域按桶定义）"}
	}
	if segs[0] != o.owner {
		return "", &errOwnerMismatch{got: segs[0], want: o.owner}
	}
	if !storage.IsReservedBucketName(segs[1]) {
		// 白名单与封印足迹同源（不变式 2）：非保留桶不在足迹内 ⇒ 必须拒绝，否则出现「可写未封」。
		return "", &errBadKey{vol: o.vol, key: k, reason: "非保留功能桶（不在封印足迹内）"}
	}
	parts := make([]string, 0, len(segs)+2)
	parts = append(parts, o.owner, segs[1])
	if o.sub != "" {
		parts = append(parts, strings.Split(o.sub, "/")...)
	}
	parts = append(parts, segs[2:]...)
	return strings.Join(parts, "/"), nil
}

// strip 把源卷键反向剥离回调用方键空间（列举/Stat 回填）；不在足迹内的路径原样返回。
func (o *ownerFS) strip(k string) string {
	t := strings.Trim(strings.Trim(k, "/"), "/")
	segs := strings.Split(t, "/")
	if len(segs) < 2 || segs[0] != o.owner || !storage.IsReservedBucketName(segs[1]) {
		return k
	}
	rest := segs[2:]
	if o.sub != "" {
		subSegs := strings.Split(o.sub, "/")
		if len(rest) < len(subSegs) {
			return k
		}
		if strings.Join(rest[:len(subSegs)], "/") != o.sub {
			return k
		}
		rest = rest[len(subSegs):]
	}
	out := append([]string{segs[0], segs[1]}, rest...)
	return strings.Join(out, "/")
}

func (o *ownerFS) entry(e *syncpkg.Entry) *syncpkg.Entry {
	if e == nil {
		return nil
	}
	e.Path = o.strip(e.Path)
	return e
}

// ---- FS 基础方法（全部经 key 映射，失败即 fail-closed） ----

func (o *ownerFS) Inner() syncpkg.FS { return o.FS }

func (o *ownerFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	entries, lerr := o.FS.ListDir(ctx, src)
	if lerr != nil {
		return nil, lerr
	}
	for i := range entries {
		o.entry(&entries[i])
	}
	return entries, nil
}

// ListDirNames 转发轻量列举（未实现时回落 ListDir）：装配期只按名字分类时，
// 避免外部卷对每个条目批量 `meta` 补精确 size（1.2 万 blob ⇒ 数百次 CLI + 限流）。
func (o *ownerFS) ListDirNames(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	nl, ok := syncpkg.AssertNameLister(o.FS)
	if !ok {
		return o.ListDir(ctx, p)
	}
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	entries, lerr := nl.ListDirNames(ctx, src)
	if lerr != nil {
		return nil, lerr
	}
	for i := range entries {
		o.entry(&entries[i])
	}
	return entries, nil
}

func (o *ownerFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	e, serr := o.FS.Stat(ctx, src)
	if serr != nil {
		return nil, serr
	}
	return o.entry(e), nil
}

func (o *ownerFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	return o.FS.OpenRead(ctx, src)
}

func (o *ownerFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.WriteFile(ctx, key, src, r, size, mtime)
}

func (o *ownerFS) MakeDir(ctx context.Context, p string) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.MakeDir(ctx, src)
}

func (o *ownerFS) Delete(ctx context.Context, p string) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.Delete(ctx, src)
}

func (o *ownerFS) Rename(ctx context.Context, from, to string) error {
	sf, err := o.key(from)
	if err != nil {
		return err
	}
	st, err := o.key(to)
	if err != nil {
		return err
	}
	return o.FS.Rename(ctx, sf, st)
}

// ---- 可选能力（R21：全量转发；键映射后委托；底层未实现 → ErrUnsupported） ----

func (o *ownerFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	rr, ok := o.FS.(syncpkg.RangeReader)
	if !ok {
		return nil, fmt.Errorf("ownerFS: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
	}
	return rr.OpenRangeRead(ctx, src, offset, size)
}

func (o *ownerFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	src, err := o.key(rel)
	if err != nil {
		return nil, err
	}
	pv, ok := o.FS.(meta.Provider)
	if !ok {
		return nil, fmt.Errorf("ownerFS: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
	}
	return pv.FileMeta(ctx, src)
}

func (o *ownerFS) WriteIfAbsent(ctx context.Context, p string, r io.Reader, size, mtime int64) (bool, error) {
	src, err := o.key(p)
	if err != nil {
		return false, err
	}
	w, ok := o.FS.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("ownerFS: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	return w.WriteIfAbsent(ctx, src, r, size, mtime)
}

func (o *ownerFS) Move(ctx context.Context, from, to string) error {
	sf, err := o.key(from)
	if err != nil {
		return err
	}
	st, err := o.key(to)
	if err != nil {
		return err
	}
	m, ok := o.FS.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	return m.Move(ctx, sf, st)
}

func (o *ownerFS) Copy(ctx context.Context, from, to string) error {
	sf, err := o.key(from)
	if err != nil {
		return err
	}
	st, err := o.key(to)
	if err != nil {
		return err
	}
	c, ok := o.FS.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	return c.Copy(ctx, sf, st)
}

func (o *ownerFS) Link(ctx context.Context, from, to string) error {
	sf, err := o.key(from)
	if err != nil {
		return err
	}
	st, err := o.key(to)
	if err != nil {
		return err
	}
	l, ok := o.FS.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	return l.Link(ctx, sf, st)
}

func (o *ownerFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	src, err := o.key(relPath)
	if err != nil {
		return err
	}
	sp, ok := o.FS.(syncpkg.StagedPathReuse)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 WriteFromStagedPath: %w", syncpkg.ErrUnsupported)
	}
	return sp.WriteFromStagedPath(ctx, src, stagedPath, size, mtime)
}

func (o *ownerFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	src, err := o.key(relPath)
	if err != nil {
		return err
	}
	rs, ok := o.FS.(syncpkg.ReserveSpace)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
	}
	return rs.ReserveSpace(ctx, src, size)
}

func (o *ownerFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	src, err := o.key(rel)
	if err != nil {
		return err
	}
	u, ok := o.FS.(syncpkg.MetaExtraUpdater)
	if !ok {
		return fmt.Errorf("ownerFS: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return u.UpdateMetaExtra(ctx, src, extra)
}

func (o *ownerFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	src, err := o.key(relPath)
	if err != nil {
		return "", false, err
	}
	d, ok := o.FS.(syncpkg.DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("ownerFS: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
	}
	return d.DirectURL(ctx, src)
}

// TrueUsage 真值报告：键翻译后委托（不变式 6）。
// 只翻译 **TrueSize 的键**（TrueUsage 是卷级合计，无键）。
func (o *ownerFS) TrueUsage() (int64, bool) {
	r, ok := capacity.TrueUsageOf(o.FS)
	if !ok {
		return 0, false
	}
	return r.TrueUsage()
}

// TrueSize 键翻译后取真值（调用方键 → 区域键 ⇒ 处理器层按自己的键空间回答）。
func (o *ownerFS) TrueSize(rel string) (int64, bool) {
	r, ok := capacity.TrueUsageOf(o.FS)
	if !ok {
		return 0, false
	}
	src, err := o.key(rel)
	if err != nil {
		return 0, false
	}
	return r.TrueSize(src)
}

func (o *ownerFS) IsLocalVolume() bool {
	lv, ok := o.FS.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

func (o *ownerFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if c, ok := o.FS.(syncpkg.StagingQuotaCapable); ok {
		c.WithStagingQuota(q)
	}
}

func (o *ownerFS) ExemptStagingQuota() bool {
	ex, ok := o.FS.(syncpkg.StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
}

// ownerFacingBackend 把「调用方面」视图装在 `FS()` 上；区域/特权视图经 `UnsealedSource` 可取回。
type ownerFacingBackend struct {
	registry.ExternalBackend
	fs syncpkg.FS
}

// FS 返回调用方面视图（owner 映射；装配后只读）。
func (b *ownerFacingBackend) FS() syncpkg.FS { return b.fs }

// UnsealedSource 返回**区域相对视图**（封装处理器键空间）——装配层需要「同后端原始视图」时用它
// （与封印解包同名语义：不返回**调用方**映射视图）。
func (b *ownerFacingBackend) UnsealedSource() syncpkg.FS { return b.ExternalBackend.FS() }

// WrapOwnerFacing 为 owner 限定声明把后端包成「调用方面 owner 映射视图」。
// 非 owner 限定（卷根专有区间）原样返回（零回归）。
func WrapOwnerFacing(be registry.ExternalBackend, d Decl) registry.ExternalBackend {
	if be == nil || !d.OwnerQualified() {
		return be
	}
	return &ownerFacingBackend{ExternalBackend: be, fs: &ownerFS{
		FS: be.FS(), owner: d.Owner, sub: strings.Trim(d.Sub, "/"), vol: d.Vol,
	}}
}

// 编译期能力断言（R21：装饰器必须声明全部可选能力；BlockAccessor 与其余装饰器同口径豁免）。
var (
	_ syncpkg.FS                  = (*ownerFS)(nil)
	_ syncpkg.RangeReader         = (*ownerFS)(nil)
	_ syncpkg.DirectURLProvider   = (*ownerFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*ownerFS)(nil)
	_ syncpkg.ReserveSpace        = (*ownerFS)(nil)
	_ syncpkg.LocalVolume         = (*ownerFS)(nil)
	_ syncpkg.Mover               = (*ownerFS)(nil)
	_ syncpkg.Copier              = (*ownerFS)(nil)
	_ syncpkg.Linker              = (*ownerFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*ownerFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*ownerFS)(nil)
	_ syncpkg.MetaExtraUpdater    = (*ownerFS)(nil)
	_ syncpkg.StagedPathReuse     = (*ownerFS)(nil)
	_ meta.Provider               = (*ownerFS)(nil)
	_ capacity.TrueUsageReporter  = (*ownerFS)(nil)
)
