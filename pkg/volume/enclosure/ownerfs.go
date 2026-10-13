// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enclosure

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ownerfs.go 实现**按 owner 限定的源视图映射**（设计 §4.1 的“客户端可达封装区间”）。
//
// 背景：源卷的键空间恒为 `<owner>/<bucket>/<rel>`（`volume.Location.FSPath()` 唯一拼接点）。
// 声明 `source: "main:alice/user/vault"` 的语义是「占用**alice 的** user 桶下 vault 子树」；
// 客户端键 `<owner>/<bucket>/<rel>` 必须映射到 `<alice>/<bucket>/<vault>/<rel>`（**非嵌套**，
// 与设计 §4.1 一致），且 **owner 必须与声明一致**（否则会跨 owner 读写 = 越权）。
//
// 为什么不是「前缀前插」（PrefixFS）也不是「owner 通配」：
//   - 前缀前插会把调用方整键（含 owner/bucket）接到声明后 ⇒ 落盘 `<decl>/<owner>/<bucket>/<rel>`
//     （嵌套重复 owner/bucket，与设计 §4.1 承诺不符，且语义读不出来）；
//   - owner 通配（`*/user/vault`）把**模式匹配**压进安全判据（大小写折叠/段数语义/归一交互），
//     难维护且易误封/漏封。
//
// 本视图把 owner **由声明给定（权威）**，键上的 owner 只做**精确相等校验**（不折叠、不模式
// 匹配），随后按 `<declOwner>/<bucket>/<sub>/<rel>` 重写——映射规则只有这一处、可逐字节断言。
//
// 桶维度：**沿用调用方键里的 bucket**（不写死声明里的桶）——user 与 meta（sidecar 镜像）、
// cloud/archive（转存目标）都自然可用；声明里的桶段只用于识别「这是 owner 限定声明」。
type ownerFS struct {
	syncpkg.FS
	owner string // 声明里的 owner（权威；调用方键首段必须与之精确相等）
	sub   string // 声明路径中 `<owner>/<bucket>/` 之后的区域（可为空 = 该 owner 的整桶视图）
}

// newOwnerFS 构造 owner 限定视图（d 必须是 owner 限定声明，见 Decl.OwnerQualified）。
func newOwnerFS(inner syncpkg.FS, d Decl) *ownerFS {
	return &ownerFS{FS: inner, owner: d.Owner, sub: strings.Trim(d.Sub, "/")}
}

// Inner 透明暴露被包装 FS（能力探测/特权解包语义与其余装饰器一致）。
func (o *ownerFS) Inner() syncpkg.FS { return o.FS }

// errOwnerMismatch 是「键首段与声明 owner 不符」的读/写失败错误：对外表现为不存在
// （不泄漏其它 owner 的数据存在性），且可归因（日志可区分「越权访问」与「真的不存在」）。
type errOwnerMismatch struct {
	got  string
	want string
}

func (e *errOwnerMismatch) Error() string {
	return fmt.Sprintf("enclosure: 键首段 owner %q 与声明 owner %q 不符（越权访问被拒）", e.got, e.want)
}

// Is 让错误同时匹配 fs.ErrNotExist（404 语义）——不泄漏其它 owner 的数据存在性。
func (e *errOwnerMismatch) Is(target error) bool {
	return target == fs.ErrNotExist || errors.Is(target, fs.ErrNotExist)
}

// key 把调用方键 `<owner>/<bucket>/<rel>` 映射为源卷键 `<declOwner>/<bucket>/<sub>/<rel>`。
// 键段数不足（<owner>/<bucket>）或 owner 不符 ⇒ 错误（不存在语义）。
func (o *ownerFS) key(k string) (string, error) {
	t := strings.Trim(strings.ReplaceAll(k, `\`, "/"), "/")
	if t == "" || t == "." {
		// 根：仅当声明是「该 owner 的整桶视图」时才有意义（sub 空且要求调用方给出 owner/bucket）。
		return "", &errOwnerMismatch{got: "", want: o.owner}
	}
	segs := strings.Split(t, "/")
	if len(segs) < 2 {
		return "", &errOwnerMismatch{got: segs[0], want: o.owner}
	}
	if segs[0] != o.owner {
		return "", &errOwnerMismatch{got: segs[0], want: o.owner}
	}
	parts := make([]string, 0, len(segs)+2)
	parts = append(parts, o.owner, segs[1])
	if o.sub != "" {
		parts = append(parts, o.sub)
	}
	parts = append(parts, segs[2:]...)
	return path.Join(parts...), nil
}

// strip 把源卷键 `<declOwner>/<bucket>/<sub>/<rel>` 反向剥离回调用方键空间
// （列举/Stat 条目回填；不在本视图前缀内的路径原样返回）。
func (o *ownerFS) strip(k string) string {
	t := strings.Trim(strings.ReplaceAll(k, `\`, "/"), "/")
	prefix := o.owner
	if o.sub != "" {
		// 形如 <owner>/<bucket>/<sub>：需要保留 bucket 段 ⇒ 按段剥离。
		segs := strings.Split(t, "/")
		if len(segs) < 3 || segs[0] != o.owner {
			return k
		}
		subSegs := strings.Split(o.sub, "/")
		if len(segs) < 2+len(subSegs) || strings.Join(segs[2:2+len(subSegs)], "/") != o.sub {
			return k
		}
		out := append([]string{segs[0], segs[1]}, segs[2+len(subSegs):]...)
		return strings.Join(out, "/")
	}
	if !strings.HasPrefix(t, prefix+"/") {
		return k
	}
	return t
}

// entry 反向映射条目路径（nil 安全）。
func (o *ownerFS) entry(e *syncpkg.Entry) *syncpkg.Entry {
	if e == nil {
		return nil
	}
	e.Path = o.strip(e.Path)
	return e
}

// ListDir 列出目录（键重映射 + 条目反向映射）。
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

// Stat 元信息（键重映射 + 条目反向映射）。
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

// OpenRead 读（键重映射）。
func (o *ownerFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	src, err := o.key(p)
	if err != nil {
		return nil, err
	}
	return o.FS.OpenRead(ctx, src)
}

// OpenRangeRead 定点读（RangeReader；键重映射）。
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

// FileMeta 读 sidecar（meta.Provider；键重映射）。
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

// WriteFile 写（键重映射）。
func (o *ownerFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.WriteFile(ctx, src, r, size, mtime)
}

// WriteIfAbsent 原子唯一写（键重映射）。
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

// MakeDir 建目录（键重映射）。
func (o *ownerFS) MakeDir(ctx context.Context, p string) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.MakeDir(ctx, src)
}

// Delete 删除（键重映射）。
func (o *ownerFS) Delete(ctx context.Context, p string) error {
	src, err := o.key(p)
	if err != nil {
		return err
	}
	return o.FS.Delete(ctx, src)
}

// Rename 改名（两端键重映射）。
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

// Move 同卷移动（Mover；两端键重映射）。
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

// Copy 同卷复制（Copier；两端键重映射）。
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

// Link 硬链接（Linker；两端键重映射）。
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

// WriteFromStagedPath staging 复用（键重映射；stagedPath 是本地临时路径，原样透传）。
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

// ReserveSpace 容量预检（键重映射）。
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

// UpdateMetaExtra 旁路 sidecar 更新（键重映射）。
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

// DirectURL 直链（键重映射后询问底层）。
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

// IsLocalVolume 转发本地性自述（不涉及路径）。
func (o *ownerFS) IsLocalVolume() bool {
	lv, ok := o.FS.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

// WithStagingQuota 转发 staging 配额钩子（不涉及路径）。
func (o *ownerFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if c, ok := o.FS.(syncpkg.StagingQuotaCapable); ok {
		c.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 转发 staging 豁免自述。
func (o *ownerFS) ExemptStagingQuota() bool {
	ex, ok := o.FS.(syncpkg.StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
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
)
