// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package trusted

// guard_fs_test.go 钉住 meta 桶隔离守卫（分层隔离第 2 层）：
// 业务层 FS 触达 `meta/` 桶 → fail-closed 拒绝（凭据/sidecar 不可旁路）；
// 其余功能命名空间（user/cloud/backup/secrets 等）透传。

import (
	"context"
	"strings"
	"testing"
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
