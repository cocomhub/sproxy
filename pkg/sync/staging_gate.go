// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// staging_gate.go 实现**默认强制**的本地 staging 配额门卫（用户裁定 2026-10-10：
// 独立 staging Scope + 全部外部卷强制；不需要 staging 或只占部分空间的卷**显式实现**
// StagingQuotaExempt 才豁免，获取配额句柄后自行接管的卷实现 StagingQuotaCapable）。
//
// 机制化保证（防新卷遗漏）：装配层对所有外部卷 `be.FS()` **统一包 StagingQuotaGateFS**
// ——WriteFile 前预留 size 到独立 staging Scope、写后释放。未实现任何豁免/自管接口的
// 卷默认被强制预留（fail-safe：宁多勿漏，本地磁盘打满风险由装配层统一兜底，不依赖
// 卷实现者自觉）。

import (
	"context"
	"io"
)

// StagingQuotaGateFS 包装任意 FS 的本地 staging 配额门卫：WriteFile 前经
// StagingQuotaTracker.ReserveUsage(size) 预留、写后 ReleaseUsage——本地磁盘防打满。
// 所有 FS 方法透明委派 inner（gate 只拦截写路径）。
type StagingQuotaGateFS struct {
	inner FS
	quota StagingQuotaTracker
}

// WrapStagingQuota 包装 fs 为带 staging 配额门卫的 FS（quota nil = 不记账直通——
// 无独立 staging Scope 装配时零回归）。
func WrapStagingQuota(fs FS, q StagingQuotaTracker) *StagingQuotaGateFS {
	return &StagingQuotaGateFS{inner: fs, quota: q}
}

var _ FS = (*StagingQuotaGateFS)(nil)

func (g *StagingQuotaGateFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return g.inner.ListDir(ctx, path)
}
func (g *StagingQuotaGateFS) Stat(ctx context.Context, path string) (*Entry, error) {
	return g.inner.Stat(ctx, path)
}
func (g *StagingQuotaGateFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	return g.inner.OpenRead(ctx, path)
}
func (g *StagingQuotaGateFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	if g.quota != nil {
		if err := g.quota.ReserveUsage(ctx, size); err != nil {
			return err
		}
		defer g.quota.ReleaseUsage(size)
	}
	return g.inner.WriteFile(ctx, path, r, size, mtime)
}
func (g *StagingQuotaGateFS) Rename(ctx context.Context, from, to string) error {
	return g.inner.Rename(ctx, from, to)
}
func (g *StagingQuotaGateFS) Delete(ctx context.Context, path string) error {
	return g.inner.Delete(ctx, path)
}
func (g *StagingQuotaGateFS) MakeDir(ctx context.Context, path string) error {
	return g.inner.MakeDir(ctx, path)
}
