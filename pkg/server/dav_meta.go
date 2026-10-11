// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// davMetaFS 是 WebDAV 挂载面的 **sidecar 联动**装饰器（P1 对抗评审修复）。
//
// webdavcore 直接用裸 LocalFS 写 user 桶，覆盖写既不重建也不失效 sidecar：主文件换新
// 内容而 meta 仍描述旧内容 → filesMetaPolicy.VerifyDownload 首次读即失配 → 该文件经
// /download 永久 fail-closed，且幂等重传不自愈。本装饰器在写/删/改名后联动
// filesMetaPolicy 的 sidecar（正确落在 <owner>/meta/ 桶，与上传/转存同一位置）。
type davMetaFS struct {
	syncpkg.FS
	h     *Handlers
	owner string
	root  *storage.Root
}

// bucketRel 把 WebDAV 相对路径（相对 user 桶）归一为桶相对键 `user/<rel>`。
func (d *davMetaFS) bucketRel(p string) string {
	clean := strings.TrimPrefix(path.Clean("/"+p), "/")
	if clean == "" || clean == "." {
		return ""
	}
	return "user/" + clean
}

func (d *davMetaFS) logger() *slog.Logger {
	if d.h != nil && d.h.logger != nil {
		return d.h.logger
	}
	return slog.Default()
}

// Inner 透明暴露（使 Innermost/ApplyStagingQuota 能下探到最内层原始卷能力）。
func (d *davMetaFS) Inner() syncpkg.FS { return d.FS }

// WithStagingQuota 转发到内层（P1：外层不转发会让 ApplyStagingQuota 静默不包 gate）。
func (d *davMetaFS) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if sc, ok := d.FS.(syncpkg.StagingQuotaCapable); ok {
		sc.WithStagingQuota(q)
	}
}

// writeMeta 为已落盘文件重建 sidecar（best-effort；失败仅告警——读路径退化为直算/流式）。
func (d *davMetaFS) writeMeta(ctx context.Context, p string) {
	rel := d.bucketRel(p)
	if rel == "" {
		return
	}
	if fi, err := d.Stat(ctx, p); err != nil || fi == nil || fi.IsDir {
		return
	}
	if err := (filesMetaPolicy{h: d.h}).WriteMeta(ctx, d.owner, d.root, rel); err != nil {
		d.logger().Warn("dav: 写 meta sidecar 失败（读校验退化为直算/流式）", "path", rel, "error", err)
	}
}

// removeMeta 删除对应 sidecar 并**释放 meta 桶配额**（P2：原实现只 root.Remove 不释放，
// 而 WriteMeta 已 Commit → 每次 DAV DELETE/MOVE 永久泄漏 sidecar 字节，磁盘空闲也回 507；
// 与 s3DeleteMetaAfter / write_ops 删除路径对称）。
func (d *davMetaFS) removeMeta(p string) {
	rel := d.bucketRel(p)
	if rel == "" {
		return
	}
	mrel := meta.MetaPath(rel)
	metaSize := int64(0)
	if d.root != nil {
		if e, serr := d.root.Stat(mrel); serr == nil && e != nil {
			metaSize = e.Size()
		}
		(filesMetaPolicy{h: d.h}).sweepMetaTmp(d.root, mrel)
		_ = d.root.Remove(mrel)
	}
	if metaSize > 0 && d.h != nil {
		if scope := d.h.quotaScopeFor(d.owner, mrel); scope != nil {
			scope.ReleaseUsage(metaSize)
		}
	}
}

func (d *davMetaFS) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	if err := d.FS.WriteFile(ctx, p, r, size, mtime); err != nil {
		return err
	}
	d.writeMeta(ctx, p)
	return nil
}

func (d *davMetaFS) Delete(ctx context.Context, p string) error {
	if err := d.FS.Delete(ctx, p); err != nil {
		return err
	}
	d.removeMeta(p)
	return nil
}

func (d *davMetaFS) Rename(ctx context.Context, from, to string) error {
	if err := d.FS.Rename(ctx, from, to); err != nil {
		return err
	}
	d.relinkAfterMove(ctx, from, to)
	return nil
}

// relinkAfterMove 改名/移动/复制后重链 sidecar：先清目标旧 sidecar（释放其配额 + 防陈旧），
// 再按新路径重建；目录级操作再整棵迁移子 sidecar 子树（M8：Move/Copy/Link 此前**未重链**
// meta——被嵌入 FS 的方法直通，目标键无证据/源 sidecar 成孤儿）。
func (d *davMetaFS) relinkAfterMove(ctx context.Context, from, to string) {
	d.removeMeta(to)
	d.removeMeta(from)
	d.writeMeta(ctx, to)
	// 目录级 MOVE：子文件 sidecar 在 `meta/<from>/**` 子树，必须整棵迁移——否则新位置
	// 逐分块校验静默失效（退化为直算）且旧子树成孤儿白占 meta 桶配额。
	d.migrateMetaSubtree(from, to)
}

// migrateMetaSubtree 目录级 MOVE 时整棵迁移 sidecar 子树：`meta/<from>/**` → `meta/<to>/**`。
// best-effort（单条失败 Warn 并继续——失败只退化为缺/孤 sidecar，读侧 missing 直算兜底）；
// 返回迁移条数（0 = 源无子树，正常）。字节总量不变，无需配额调整。
func (d *davMetaFS) migrateMetaSubtree(from, to string) int {
	if d.root == nil {
		return 0
	}
	srcRel, dstRel := d.bucketRel(from), d.bucketRel(to)
	if srcRel == "" || dstRel == "" {
		return 0
	}
	srcRoot := meta.MetaDirPath(srcRel)
	dstRoot := meta.MetaDirPath(dstRel)
	if srcRoot == dstRoot {
		return 0
	}
	done := 0
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := d.root.ReadDir(dir)
		if err != nil {
			return // 源子树不存在（该目录下无 sidecar）→ 正常跳过
		}
		for _, e := range entries {
			child := dir + "/" + e.Name()
			if e.IsDir() {
				walk(child)
				continue
			}
			if d.migrateMetaEntry(child, srcRoot, dstRoot) {
				done++
			}
		}
	}
	walk(srcRoot)
	return done
}

// migrateMetaEntry 迁移单条 sidecar（目标父目录先建；失败 Warn 并返回 false）。
func (d *davMetaFS) migrateMetaEntry(child, srcRoot, dstRoot string) bool {
	dst := dstRoot + strings.TrimPrefix(child, srcRoot)
	if merr := d.root.MkdirAll(path.Dir(dst), 0o755); merr != nil {
		d.logger().Warn("dav: sidecar 目标目录建立失败（新位置退化为直算）", "dir", path.Dir(dst), "error", merr)
		return false
	}
	if rerr := d.root.Rename(child, dst); rerr != nil {
		d.logger().Warn("dav: sidecar 子树迁移失败（新位置退化为直算）", "from", child, "to", dst, "error", rerr)
		return false
	}
	return true
}

// ---- 可选能力转发（M8，第 7 轮 MAJOR）：透明装饰器契约 ----
//
// 此前 davMetaFS 只转发 7 个基础方法 + Inner/WithStagingQuota：消费方对
// `meta.Provider`/`Mover`/`Copier`/`Linker`/`WriteIfAbsent`/`RangeReader`/`DirectURL`
// 的断言虽为真（类型断言命中本层）却语义落空（方法被嵌入 FS 直通 → meta 不重链；
// 或根本未实现 → 断言恒 false 静默降级）。逐一显式实现：写面能力**带 meta 重链**，
// 读/元信息能力透传内层（未实现返回 ErrUnsupported，与其余装饰器同口径）。

// WriteIfAbsent 唯一写（内层未实现 → ErrUnsupported，调用方回落）。
func (d *davMetaFS) WriteIfAbsent(ctx context.Context, p string, r io.Reader, size, mtime int64) (bool, error) {
	w, ok := d.FS.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("dav: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	written, err := w.WriteIfAbsent(ctx, p, r, size, mtime)
	if err != nil || !written {
		return written, err
	}
	d.writeMeta(ctx, p) // 写面同步重链 sidecar（与 http 上传/DAV PUT 一致）
	return true, nil
}

// Move 同卷移动 + meta 重链（内层未实现 → ErrUnsupported）。
func (d *davMetaFS) Move(ctx context.Context, from, to string) error {
	m, ok := d.FS.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("dav: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	if err := m.Move(ctx, from, to); err != nil {
		return err
	}
	d.relinkAfterMove(ctx, from, to)
	return nil
}

// Copy 同卷复制 + 目标 meta 重建（内层未实现 → ErrUnsupported）。
func (d *davMetaFS) Copy(ctx context.Context, from, to string) error {
	c, ok := d.FS.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("dav: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	if err := c.Copy(ctx, from, to); err != nil {
		return err
	}
	d.relinkAfterMove(ctx, from, to)
	return nil
}

// Link 硬链接 + 目标 meta 重建（内层未实现 → ErrUnsupported）。
func (d *davMetaFS) Link(ctx context.Context, from, to string) error {
	l, ok := d.FS.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("dav: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	if err := l.Link(ctx, from, to); err != nil {
		return err
	}
	d.relinkAfterMove(ctx, from, to)
	return nil
}

// ReserveSpace 透传卷容量预检（内层未实现 → ErrUnsupported，调用方跳过卷配额）。
func (d *davMetaFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	rs, ok := d.FS.(syncpkg.ReserveSpace)
	if !ok {
		return fmt.Errorf("dav: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
	}
	return rs.ReserveSpace(ctx, relPath, size)
}

// IsLocalVolume 转发本地性自述（未实现 = 外部卷，与全仓「未实现默认外部」一致）。
func (d *davMetaFS) IsLocalVolume() bool {
	lv, ok := d.FS.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

// ExemptStagingQuota 转发 staging 豁免自述（未实现 = 不豁免，默认强制 gate）。
func (d *davMetaFS) ExemptStagingQuota() bool {
	ex, ok := d.FS.(syncpkg.StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
}

// OpenRangeRead 透传定点读（内层未实现 → ErrUnsupported，调用方整块回退）。
func (d *davMetaFS) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	rr, ok := d.FS.(syncpkg.RangeReader)
	if !ok {
		return nil, fmt.Errorf("dav: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
	}
	return rr.OpenRangeRead(ctx, p, offset, size)
}

// DirectURL 透传直链（内层未实现 → ErrUnsupported，调用方回落服务端转发）。
func (d *davMetaFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	dp, ok := d.FS.(syncpkg.DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("dav: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
	}
	return dp.DirectURL(ctx, relPath)
}

// FileMeta 透传 sidecar 读取（内层未实现 → ErrUnsupported；本装饰器只做**写面重链**，
// 读面证据由底层可信卷提供）。
func (d *davMetaFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	pv, ok := d.FS.(meta.Provider)
	if !ok {
		return nil, fmt.Errorf("dav: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
	}
	return pv.FileMeta(ctx, rel)
}

// ---- 能力平权断言（R21）----
var (
	_ syncpkg.FS                  = (*davMetaFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*davMetaFS)(nil)
	_ syncpkg.ReserveSpace        = (*davMetaFS)(nil)
	_ syncpkg.LocalVolume         = (*davMetaFS)(nil)
	_ syncpkg.Mover               = (*davMetaFS)(nil)
	_ syncpkg.Copier              = (*davMetaFS)(nil)
	_ syncpkg.Linker              = (*davMetaFS)(nil)
	_ syncpkg.RangeReader         = (*davMetaFS)(nil)
	_ syncpkg.DirectURLProvider   = (*davMetaFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*davMetaFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*davMetaFS)(nil)
	_ meta.Provider               = (*davMetaFS)(nil)
	// BlockAccessor 有意豁免（与其余装饰器同口径：需 Capability 探测语义）。
)
