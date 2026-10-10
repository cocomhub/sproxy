// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
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
	// 先清目标旧 sidecar（释放其配额 + 防陈旧），再写新 meta——否则目标键已有 sidecar 时
	// writeMeta 失败会残留描述旧内容的 sidecar（读校验固化失败，P2）。
	d.removeMeta(to)
	d.removeMeta(from)
	d.writeMeta(ctx, to)
	return nil
}
