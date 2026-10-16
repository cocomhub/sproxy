// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// backup_quota_phys_test.go —— 备份目标记账必须与**上传路径同源**（物理口径）。
//
// 缺陷（2026-10-16 高风险区审核发现，跨写路径单位不一致）：
// 本地备份目标链 = `PrefixFS(Guard(Wrap(NewRootFS(tnt.Root()))))` ⇒ `RootFS.Stat` 返回
// **`LogicalSize`（明文长度）**、`RootFS.WriteFile` 经 `writeUserVisible` **透明加密**（落盘为
// 物理 > 逻辑）。而 `backupQuotaFS` 此前把 `inner.Stat` 的尺寸与调用方声明的 `size` **原样**
// 入账 ⇒ **备份按逻辑记账、上传按物理记账** ⇒ 加密卷上 owner Scope **少记**（磁盘实际占用是
// 物理），且 owner 配额侧**永不收敛**（只有卷池靠磁盘扫描收敛）⇒ **可超真实配额**（违反 G1）。
//
// 修：`backupQuotaFS.phys` 注入本卷物理折算（`files.RootPhysSizer`），`statSize` 与 `WriteFile`
// 入口**各折算一次**（单一折算点，避免双重折算）。
//
// 变异：去掉 `phys` 折算（或改回 `e.Size`/原样 `size`）⇒ 断言红。

import (
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

func TestBackupQuotaFS_ChargedPhysicalBytes(t *testing.T) {
	t.Parallel()
	// 模拟加密卷目标：phys 与生产装配（`files.RootPhysSizer`）同源实现。
	phys := storage.EncryptedPhysicalSize
	pool := quota.NewPool(1 << 20)
	scope := pool.Scope("/tenant/alice/user", 1<<20)
	inner := &backupSizedMemFS{backupMemFS{files: map[string]string{}}}
	q := &backupQuotaFS{inner: inner, scope: scope, phys: phys}
	ctx := context.Background()
	const n = 8192

	if err := q.WriteFile(ctx, "a.bin", strings.NewReader(strings.Repeat("x", n)), n, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, want := scope.Usage(), phys(n); got != want {
		t.Fatalf("备份结算必须按**物理**口径（与上传路径同源）: usage=%d want=%d（逻辑=%d）", got, want, n)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("预留应已结算清空: reserved=%d", got)
	}

	// 覆盖写：`prev`（来自 inner.Stat = 逻辑）同样必须折算，否则泄漏 (物理−逻辑)。
	if err := q.WriteFile(ctx, "a.bin", strings.NewReader(strings.Repeat("y", 2*n)), 2*n, 0); err != nil {
		t.Fatalf("WriteFile(覆盖): %v", err)
	}
	if got, want := scope.Usage(), phys(2*n); got != want {
		t.Fatalf("覆盖写后 usage 必须 == 新物理字节（无泄漏）: usage=%d want=%d", got, want)
	}

	// 删除：按物理释放 ⇒ 归零（释放侧与结算侧同源）。
	if err := q.Delete(ctx, "a.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if got := scope.Usage(); got != 0 {
		t.Fatalf("删除后应归零（按物理释放）: usage=%d", got)
	}
}

// TestBackupQuotaFS_NilPhysIsIdentity 钉住零回归：未注入 phys（明文卷 / 外部卷）⇒ 恒等口径。
func TestBackupQuotaFS_NilPhysIsIdentity(t *testing.T) {
	t.Parallel()
	pool := quota.NewPool(1 << 20)
	scope := pool.Scope("/tenant/alice/user", 1<<20)
	inner := &backupSizedMemFS{backupMemFS{files: map[string]string{}}}
	q := &backupQuotaFS{inner: inner, scope: scope}
	const n = 4096
	if err := q.WriteFile(context.Background(), "p.bin", strings.NewReader(strings.Repeat("z", n)), n, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got := scope.Usage(); got != n {
		t.Fatalf("未注入 phys 时必须恒等（明文卷/外部卷零回归）: usage=%d want=%d", got, n)
	}
}
