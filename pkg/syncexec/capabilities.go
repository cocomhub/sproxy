// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"fmt"
	"io"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// capabilities.go 为 quotaLocalFS 补齐**可选能力透明转发**（评审 P2：此前它只实现基础 7 方法，
// 把内层刚补好的能力又全剥掉 ⇒ sync pull 本地目标上 `meta.Provider`/`WriteIfAbsent`/`Mover`/
// `Copier`/`RangeReader`/`StagedPathReuse`/`MetaExtraUpdater` 等断言恒 false，静默降级）。
//
// 契约与 pkg/sync.PrefixFS 同口径：无条件转发；底层未实现 → ErrUnsupported（能力探测语义：
// 「未实现」必须与「空操作成功」可区分）。BlockAccessor 与其余装饰器一致豁免（引擎回退语义
// 需 Capability 探测）。
func (q *quotaLocalFS) Inner() syncpkg.FS { return q.inner }

// WriteIfAbsent 转发（配额预留仅在真实写入分支上做，见 WriteFile）。
func (q *quotaLocalFS) WriteIfAbsent(ctx context.Context, path string, r io.Reader, size, mtime int64) (bool, error) {
	wa, ok := q.inner.(syncpkg.WriteIfAbsent)
	if !ok {
		return false, fmt.Errorf("syncexec: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
	}
	return wa.WriteIfAbsent(ctx, path, r, size, mtime)
}

func (q *quotaLocalFS) WriteFromStagedPath(ctx context.Context, relPath, stagedPath string, size, mtime int64) error {
	sp, ok := q.inner.(syncpkg.StagedPathReuse)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 WriteFromStagedPath: %w", syncpkg.ErrUnsupported)
	}
	return sp.WriteFromStagedPath(ctx, relPath, stagedPath, size, mtime)
}

func (q *quotaLocalFS) Move(ctx context.Context, from, to string) error {
	m, ok := q.inner.(syncpkg.Mover)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
	}
	return m.Move(ctx, from, to)
}

func (q *quotaLocalFS) Copy(ctx context.Context, from, to string) error {
	c, ok := q.inner.(syncpkg.Copier)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
	}
	return c.Copy(ctx, from, to)
}

func (q *quotaLocalFS) Link(ctx context.Context, from, to string) error {
	l, ok := q.inner.(syncpkg.Linker)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
	}
	return l.Link(ctx, from, to)
}

func (q *quotaLocalFS) ReserveSpace(ctx context.Context, relPath string, size int64) error {
	rs, ok := q.inner.(syncpkg.ReserveSpace)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
	}
	return rs.ReserveSpace(ctx, relPath, size)
}

func (q *quotaLocalFS) OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error) {
	rr, ok := q.inner.(syncpkg.RangeReader)
	if !ok {
		return nil, fmt.Errorf("syncexec: 底层未实现 OpenRangeRead: %w", syncpkg.ErrUnsupported)
	}
	return rr.OpenRangeRead(ctx, path, offset, size)
}

func (q *quotaLocalFS) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	dp, ok := q.inner.(syncpkg.DirectURLProvider)
	if !ok {
		return "", false, fmt.Errorf("syncexec: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
	}
	return dp.DirectURL(ctx, relPath)
}

func (q *quotaLocalFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	pv, ok := q.inner.(meta.Provider)
	if !ok {
		return nil, fmt.Errorf("syncexec: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
	}
	return pv.FileMeta(ctx, rel)
}

func (q *quotaLocalFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	ue, ok := q.inner.(syncpkg.MetaExtraUpdater)
	if !ok {
		return fmt.Errorf("syncexec: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
	}
	return ue.UpdateMetaExtra(ctx, rel, extra)
}

func (q *quotaLocalFS) IsLocalVolume() bool {
	lv, ok := q.inner.(syncpkg.LocalVolume)
	return ok && lv.IsLocalVolume()
}

func (q *quotaLocalFS) ExemptStagingQuota() bool {
	ex, ok := q.inner.(syncpkg.StagingQuotaExempt)
	return ok && ex.ExemptStagingQuota()
}

func (q *quotaLocalFS) WithStagingQuota(t syncpkg.StagingQuotaTracker) {
	if sc, ok := q.inner.(syncpkg.StagingQuotaCapable); ok {
		sc.WithStagingQuota(t)
	}
}

// 编译期能力断言（R21：登记为透明装饰器后必须声明或显式豁免；BlockAccessor 同口径豁免）。
var (
	_ syncpkg.FS                  = (*quotaLocalFS)(nil)
	_ syncpkg.WriteIfAbsent       = (*quotaLocalFS)(nil)
	_ syncpkg.StagedPathReuse     = (*quotaLocalFS)(nil)
	_ syncpkg.Mover               = (*quotaLocalFS)(nil)
	_ syncpkg.Copier              = (*quotaLocalFS)(nil)
	_ syncpkg.Linker              = (*quotaLocalFS)(nil)
	_ syncpkg.ReserveSpace        = (*quotaLocalFS)(nil)
	_ syncpkg.RangeReader         = (*quotaLocalFS)(nil)
	_ syncpkg.DirectURLProvider   = (*quotaLocalFS)(nil)
	_ meta.Provider               = (*quotaLocalFS)(nil)
	_ syncpkg.MetaExtraUpdater    = (*quotaLocalFS)(nil)
	_ syncpkg.LocalVolume         = (*quotaLocalFS)(nil)
	_ syncpkg.StagingQuotaExempt  = (*quotaLocalFS)(nil)
	_ syncpkg.StagingQuotaCapable = (*quotaLocalFS)(nil)
)
