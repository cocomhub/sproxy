// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package trusted

// guard_fs.go 实现 **meta 桶隔离守卫**（分层隔离第 2 层，用户裁定：完全隔离——
// 禁止业务层操作内部元数据桶）。
//
// 背景：可信卷的 sidecar 与凭据（credentials.json）、云任务状态、sync 持久化等
// **关键内部数据**都落在 `meta/` 功能桶。业务层（下载/备份/同步/直传/secrets 等）
// 若拿到能触达 meta 桶的 FS，即可绕过可信层读写 sidecar/凭据——信任保证被旁路。
//
// 本包装器把 meta 桶从业务层 keyspace 中**结构性移除**（fail-closed）：所有 FS 方法
// 的路径参数经 volume.BucketOf 判定，命中 `meta` 桶 → 拒绝（ErrInvalidPath 类）；
// 其余功能命名空间（user/cloud/archive/chunk/version/trash/secrets/backup 等——
// 它们各有正当业务用途，且不承载敏感内部数据）放行透传。
//
// 与 trusted.Wrap 组合为**门面**：业务层只能拿到 `guard(wrapped)`——meta 桶既被
// Wrap 隐藏过滤（读侧）又被 guard 拒绝（写侧/任意方法），类型层面不可达。

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// MetaExtraUpdater 是旁路 Extra 更新能力接口（damaged 源完整性 / transfer_verified 标记
// 写入 meta.Extra）。trusted.Wrap 装饰器实现；Guard 透传（P0-1 修复——生产链
// Guard(Wrap(fs)) 此前遮蔽，cloud transfer 的 metaExtraUpdater 断言恒落空，旁路标记
// 静默丢失）。cloud 侧私有 metaExtraUpdater 接口按方法集匹配，无需引用本类型。
type MetaExtraUpdater interface {
	UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error
}

// MetaBucketGuard 是 meta 桶隔离守卫（实现 syncpkg.FS，透明委托 inner）。
// inner 可为可信卷装饰器（Wrap 后）或裸 FS——守卫只做桶判定，与可信增强正交。
type MetaBucketGuard struct {
	inner syncpkg.FS
}

// Guard 包装 inner 为 meta 桶隔离视图（业务层 FS 门面的第二层）。
func Guard(inner syncpkg.FS) *MetaBucketGuard {
	return &MetaBucketGuard{inner: inner}
}

// Inner 返回被守卫的底层 FS（受控装配/能力透传取用）。
func (g *MetaBucketGuard) Inner() syncpkg.FS { return g.inner }

// guardPath 判定路径是否触达 meta 桶（fail-closed 拒绝）。owner 前缀共享卷经
// volume.BucketOf 结构识别（首段/次段），用户真实目录 `user/dir/meta/x` 的桶段是
// user（meta 是用户子目录，放行——用户目录可叫保留名，用户裁定）。
//
// P2-2 修复：**先归一/拒绝逃逸**——守卫不能依赖调用方归一。否则 `user/../meta/x`
// 经 BucketOf 被判为 user 桶（首段命中保留桶）而放行，inner（LocalFS 等）归一后实际
// 落到 meta 桶（凭据/ sidecar 可达）。同理拒绝空段/`.`/`..`/绝对路径。
func (g *MetaBucketGuard) guardPath(p string) error {
	if p == "" {
		return nil // 桶根/空路径不触达 meta
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("trusted: 路径不得为绝对路径: %q", p)
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("trusted: 路径含非法段（空/./..）: %q", p)
		}
	}
	if bucket, _, ok := volume.BucketOf(p); ok && bucket == "meta" {
		return fmt.Errorf("trusted: meta 桶不可达（业务层隔离）: %q", p)
	}
	return nil
}

var _ syncpkg.FS = (*MetaBucketGuard)(nil)

// ListDir 委托 inner（meta 桶隔离：目录枚举路径同样守卫）。
func (g *MetaBucketGuard) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if err := g.guardPath(p); err != nil {
		return nil, err
	}
	return g.inner.ListDir(ctx, p)
}

// Stat 委托 inner（meta 桶路径 → fail-closed 拒绝，防凭据/sidecar 探测）。
func (g *MetaBucketGuard) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if err := g.guardPath(p); err != nil {
		return nil, err
	}
	return g.inner.Stat(ctx, p)
}

// OpenRead 委托 inner（meta 桶路径 → 拒绝）。
func (g *MetaBucketGuard) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	if err := g.guardPath(p); err != nil {
		return nil, err
	}
	return g.inner.OpenRead(ctx, p)
}

// WriteFile 委托 inner（meta 桶路径 → 拒绝——业务层不能写 sidecar/凭据）。
func (g *MetaBucketGuard) WriteFile(ctx context.Context, p string, r io.Reader, size, mtime int64) error {
	if err := g.guardPath(p); err != nil {
		return err
	}
	return g.inner.WriteFile(ctx, p, r, size, mtime)
}

// Rename 委托 inner（源/目标任一路径触达 meta 桶 → 拒绝）。
func (g *MetaBucketGuard) Rename(ctx context.Context, from, to string) error {
	if err := g.guardPath(from); err != nil {
		return err
	}
	if err := g.guardPath(to); err != nil {
		return err
	}
	return g.inner.Rename(ctx, from, to)
}

// Delete 委托 inner（meta 桶路径 → 拒绝——业务层不能删 sidecar/凭据）。
func (g *MetaBucketGuard) Delete(ctx context.Context, p string) error {
	if err := g.guardPath(p); err != nil {
		return err
	}
	return g.inner.Delete(ctx, p)
}

// MakeDir 委托 inner（meta 桶路径 → 拒绝）。
func (g *MetaBucketGuard) MakeDir(ctx context.Context, p string) error {
	if err := g.guardPath(p); err != nil {
		return err
	}
	return g.inner.MakeDir(ctx, p)
}

// ---- 可选能力透传（守卫透明委托，业务层能力不降级）----

// WriteIfAbsent 委托 inner（meta 桶路径拒绝）。
func (g *MetaBucketGuard) WriteIfAbsent(ctx context.Context, p string, r io.Reader, size, mtime int64) (bool, error) {
	if err := g.guardPath(p); err != nil {
		return false, err
	}
	if pia, ok := g.inner.(syncpkg.WriteIfAbsent); ok {
		return pia.WriteIfAbsent(ctx, p, r, size, mtime)
	}
	return false, fmt.Errorf("trusted: 底层未实现 WriteIfAbsent: %w", syncpkg.ErrUnsupported)
}

// ReserveSpace 委托 inner。
func (g *MetaBucketGuard) ReserveSpace(ctx context.Context, p string, size int64) error {
	if err := g.guardPath(p); err != nil {
		return err
	}
	if rs, ok := g.inner.(syncpkg.ReserveSpace); ok {
		return rs.ReserveSpace(ctx, p, size)
	}
	return fmt.Errorf("trusted: 底层未实现 ReserveSpace: %w", syncpkg.ErrUnsupported)
}

// IsLocalVolume 委托 inner（本地性判定不涉及路径，直接透传）。
func (g *MetaBucketGuard) IsLocalVolume() bool {
	if lv, ok := g.inner.(syncpkg.LocalVolume); ok {
		return lv.IsLocalVolume()
	}
	return false
}

// Move 委托 inner（源/目标触达 meta 桶拒绝）。
func (g *MetaBucketGuard) Move(ctx context.Context, from, to string) error {
	if err := g.guardPath(from); err != nil {
		return err
	}
	if err := g.guardPath(to); err != nil {
		return err
	}
	if mv, ok := g.inner.(syncpkg.Mover); ok {
		return mv.Move(ctx, from, to)
	}
	return fmt.Errorf("trusted: 底层未实现 Move: %w", syncpkg.ErrUnsupported)
}

// Copy 委托 inner（源/目标触达 meta 桶拒绝）。
func (g *MetaBucketGuard) Copy(ctx context.Context, from, to string) error {
	if err := g.guardPath(from); err != nil {
		return err
	}
	if err := g.guardPath(to); err != nil {
		return err
	}
	if cp, ok := g.inner.(syncpkg.Copier); ok {
		return cp.Copy(ctx, from, to)
	}
	return fmt.Errorf("trusted: 底层未实现 Copy: %w", syncpkg.ErrUnsupported)
}

// Link 委托 inner（源/目标触达 meta 桶拒绝）。
func (g *MetaBucketGuard) Link(ctx context.Context, from, to string) error {
	if err := g.guardPath(from); err != nil {
		return err
	}
	if err := g.guardPath(to); err != nil {
		return err
	}
	if lk, ok := g.inner.(syncpkg.Linker); ok {
		return lk.Link(ctx, from, to)
	}
	return fmt.Errorf("trusted: 底层未实现 Link: %w", syncpkg.ErrUnsupported)
}

// OpenRangeRead 透传 RangeReader（服务端随机访问读回——secretdata 视频关键帧
// 定点读；meta 桶路径拒绝）。
func (g *MetaBucketGuard) OpenRangeRead(ctx context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if err := g.guardPath(p); err != nil {
		return nil, err
	}
	if rr, ok := g.inner.(syncpkg.RangeReader); ok {
		return rr.OpenRangeRead(ctx, p, offset, size)
	}
	return nil, fmt.Errorf("trusted: 底层未实现 RangeReader: %w", syncpkg.ErrUnsupported)
}

// DirectURL 透传 DirectURLProvider（集群出口 302 直链；meta 桶路径拒绝）。
func (g *MetaBucketGuard) DirectURL(ctx context.Context, relPath string) (string, bool, error) {
	if err := g.guardPath(relPath); err != nil {
		return "", false, err
	}
	if d, ok := g.inner.(syncpkg.DirectURLProvider); ok {
		return d.DirectURL(ctx, relPath)
	}
	return "", false, fmt.Errorf("trusted: 底层未实现 DirectURL: %w", syncpkg.ErrUnsupported)
}

// ---- P0-1 修复：能力透传补齐（业务门面能力不降级）----
// Guard(Wrap(fs)) 生产链此前遮蔽内层 Wrap 的 meta.Provider / staging 配额能力——
// `fs.(meta.Provider)` 断言落在 Guard 上恒 false，转存分块校验/下载校验/staging
// 豁免/自管全链路静默失效。以下逐一透明委托 inner（meta 桶路径仍 fail-closed 拒绝）。

// FileMeta 委托 inner（meta.Provider：转存读端分块校验 / 外部卷下载校验消费）。
func (g *MetaBucketGuard) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if err := g.guardPath(rel); err != nil {
		return nil, err
	}
	if pv, ok := g.inner.(meta.Provider); ok {
		return pv.FileMeta(ctx, rel)
	}
	return nil, fmt.Errorf("trusted: 底层未实现 FileMeta: %w", syncpkg.ErrUnsupported)
}

// UpdateMetaExtra 委托 inner（damaged 源完整性标记写入 meta.Extra）。
func (g *MetaBucketGuard) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	if err := g.guardPath(rel); err != nil {
		return err
	}
	if ue, ok := g.inner.(MetaExtraUpdater); ok {
		return ue.UpdateMetaExtra(ctx, rel, extra)
	}
	return fmt.Errorf("trusted: 底层未实现 UpdateMetaExtra: %w", syncpkg.ErrUnsupported)
}

// WithStagingQuota 委托 inner（StagingQuotaCapable 自管：baidupcs 获取配额句柄内部接管）。
func (g *MetaBucketGuard) WithStagingQuota(q syncpkg.StagingQuotaTracker) {
	if qc, ok := g.inner.(syncpkg.StagingQuotaCapable); ok {
		qc.WithStagingQuota(q)
	}
}

// ExemptStagingQuota 委托 inner（StagingQuotaExempt 显式豁免：s3 流式直传不落本地盘）。
func (g *MetaBucketGuard) ExemptStagingQuota() bool {
	if ex, ok := g.inner.(syncpkg.StagingQuotaExempt); ok {
		return ex.ExemptStagingQuota()
	}
	return false
}

// 编译期断言：Guard 实现全部能力接口（能力透传契约，防未来新增能力接口后遗漏）。
var (
	_ meta.Provider               = (*MetaBucketGuard)(nil)
	_ MetaExtraUpdater            = (*MetaBucketGuard)(nil)
	_ syncpkg.StagingQuotaCapable = (*MetaBucketGuard)(nil)
	_ syncpkg.StagingQuotaExempt  = (*MetaBucketGuard)(nil)
	_ syncpkg.WriteIfAbsent       = (*MetaBucketGuard)(nil)
	_ syncpkg.ReserveSpace        = (*MetaBucketGuard)(nil)
	_ syncpkg.Mover               = (*MetaBucketGuard)(nil)
	_ syncpkg.Copier              = (*MetaBucketGuard)(nil)
	_ syncpkg.Linker              = (*MetaBucketGuard)(nil)
	_ syncpkg.RangeReader         = (*MetaBucketGuard)(nil)
)
