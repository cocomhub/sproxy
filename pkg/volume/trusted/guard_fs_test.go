// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package trusted

// guard_fs_test.go 钉住 meta 桶隔离守卫（分层隔离第 2 层）：
// 业务层 FS 触达 `meta/` 桶 → fail-closed 拒绝（凭据/sidecar 不可旁路）；
// 其余功能命名空间（user/cloud/backup/secrets 等）透传。

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestGuard_MetaBucketDenied meta 桶路径（含 owner 前缀共享卷）→ 拒绝。
func TestGuard_MetaBucketDenied(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	g := Guard(inner)
	ctx := context.Background()

	// 写 meta 桶（共享卷 owner 前缀 + 独享）→ 拒绝。
	for _, p := range []string{"meta/creds.json", "alice/meta/sidecar.meta"} {
		if err := g.WriteFile(ctx, p, strings.NewReader("x"), 1, 0); err == nil {
			t.Fatalf("写 meta 桶 %q 应拒绝（业务层隔离）", p)
		}
		if _, err := g.Stat(ctx, p); err == nil {
			t.Fatalf("Stat meta 桶 %q 应拒绝", p)
		}
		if err := g.Delete(ctx, p); err == nil {
			t.Fatalf("Delete meta 桶 %q 应拒绝", p)
		}
		if _, err := g.OpenRead(ctx, p); err == nil {
			t.Fatalf("OpenRead meta 桶 %q 应拒绝", p)
		}
	}
}

// TestGuard_NonMetaBucketAllowed 用户数据/功能命名空间（user/cloud/backup/secrets）放行。
func TestGuard_NonMetaBucketAllowed(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	g := Guard(inner)
	ctx := context.Background()

	// user 桶 + 用户子目录叫 meta（桶段=user，放行——用户裁定目录可叫保留名）。
	for _, p := range []string{"user/a.txt", "alice/user/a.txt", "user/dir/meta/x.txt"} {
		if err := g.WriteFile(ctx, p, strings.NewReader("ok"), 2, 0); err != nil {
			t.Fatalf("非 meta 桶 %q 应放行: %v", p, err)
		}
	}
	// cloud/backup 功能命名空间放行（各有着落点）。
	for _, p := range []string{"cloud/t1/f.bin", "backup/manifest.json"} {
		if err := g.WriteFile(ctx, p, strings.NewReader("ok"), 2, 0); err != nil {
			t.Fatalf("功能命名空间 %q 应放行: %v", p, err)
		}
	}
}

// TestGuard_InnerCapabilities 守卫透传能力接口（WriteIfAbsent/IsLocalVolume/ErrUnsupported）。
func TestGuard_InnerCapabilities(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	g := Guard(inner)
	ctx := context.Background()

	// LocalVolume 透传（LocalFS 自述内部卷 true——守卫不影响本地性判定）。
	if !g.IsLocalVolume() {
		t.Fatal("Guard(LocalFS) IsLocalVolume 应透传 true（内部卷自述）")
	}
	// WriteIfAbsent 透传：LocalFS 实现 → 成功；meta 桶 → 拒绝。
	ok, err := g.WriteIfAbsent(ctx, "user/wa.bin", strings.NewReader("x"), 1, 0)
	if err != nil || !ok {
		t.Fatalf("WriteIfAbsent user 桶应成功: ok=%v err=%v", ok, err)
	}
	if _, err := g.WriteIfAbsent(ctx, "meta/wa.bin", strings.NewReader("x"), 1, 0); err == nil {
		t.Fatal("WriteIfAbsent meta 桶应拒绝")
	}
	// Move/Copy/Link：meta 桶源/目标 → 拒绝（不触碰底层）。
	if err := g.Move(ctx, "meta/a", "user/b"); err == nil {
		t.Fatal("Move 源 meta 桶应拒绝")
	}
	if err := g.Move(ctx, "user/a", "meta/b"); err == nil {
		t.Fatal("Move 目标 meta 桶应拒绝")
	}
	if err := g.Copy(ctx, "meta/a", "user/b"); err == nil {
		t.Fatal("Copy 源 meta 桶应拒绝")
	}
	if err := g.Link(ctx, "user/a", "meta/b"); err == nil {
		t.Fatal("Link 目标 meta 桶应拒绝")
	}
}

// exemptProviderFS 是 FS + meta.Provider + StagingQuotaExempt（断言 Guard 能力透传）。
type exemptProviderFS struct{ *providerFS }

func (e *exemptProviderFS) ExemptStagingQuota() bool { return true }

// TestGuard_PathNormalizationDenied P2-2：含 .. / 绝对路径 / 空段 → fail-closed
// （守卫自身归一，不得依赖调用方——否则 user/../meta/x 经 BucketOf 被误判为 user 桶放行）。
func TestGuard_PathNormalizationDenied(t *testing.T) {
	t.Parallel()
	g := Guard(newInner(t))
	ctx := context.Background()
	for _, p := range []string{"user/../meta/creds.json", "alice/user/../meta/sidecar.meta", "/meta/x", "user//x"} {
		if _, err := g.Stat(ctx, p); err == nil {
			t.Fatalf("路径 %q 应拒绝（归一/逃逸）", p)
		}
		if err := g.WriteFile(ctx, p, strings.NewReader("x"), 1, 0); err == nil {
			t.Fatalf("写路径 %q 应拒绝", p)
		}
	}
}

// TestGuard_CapabilityForwarding P0-1：Guard 透传 meta.Provider / ExemptStagingQuota；
// 生产装配形状 Guard(Wrap(fs)) 仍是 meta.Provider（转存/下载校验可达）。
func TestGuard_CapabilityForwarding(t *testing.T) {
	t.Parallel()
	inner := newInner(t)
	g := Guard(&providerFS{inner: inner})
	pv, ok := any(g).(meta.Provider)
	if !ok {
		t.Fatal("Guard(providerFS) 应实现 meta.Provider")
	}
	if _, err := pv.FileMeta(context.Background(), "user/f.bin"); err != nil {
		t.Fatalf("FileMeta 应转发: %v", err)
	}
	if !Guard(&exemptProviderFS{providerFS: &providerFS{inner: inner}}).ExemptStagingQuota() {
		t.Fatal("Guard 应转发 ExemptStagingQuota")
	}
	if _, ok := any(Guard(Wrap(newInner(t), Options{}))).(meta.Provider); !ok {
		t.Fatal("Guard(Wrap(fs)) 应实现 meta.Provider（生产装配形状）")
	}
}

// TestWrap_CapabilityForwarding A7：Wrap 透传 RangeReader/DirectURL（inner 未实现 → ErrUnsupported）。
func TestWrap_CapabilityForwarding(t *testing.T) {
	t.Parallel()
	w := Wrap(&noCapsFS{}, Options{})
	rr, ok := any(w).(syncpkg.RangeReader)
	if !ok {
		t.Fatal("Wrap 应实现 RangeReader")
	}
	if _, err := rr.OpenRangeRead(context.Background(), "user/x", 0, 1); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("noCapsFS 无 RangeReader → ErrUnsupported, got %v", err)
	}
	du, ok := any(w).(syncpkg.DirectURLProvider)
	if !ok {
		t.Fatal("Wrap 应实现 DirectURLProvider")
	}
	if _, _, err := du.DirectURL(context.Background(), "user/x"); !errors.Is(err, syncpkg.ErrUnsupported) {
		t.Fatalf("noCapsFS 无 DirectURL → ErrUnsupported, got %v", err)
	}
}
