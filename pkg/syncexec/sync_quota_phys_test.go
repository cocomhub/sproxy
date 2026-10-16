// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// sync_quota_phys_test.go —— sync pull 的逐文件配额记账必须按**物理**口径（与本地 FS 同源）。
//
// 缺陷（2026-10-16 高风险区审核发现，第四个「单位不一致」）：
// sync pull 的本地目标经 `TenantRootFS` = `server.NewRootFS(tnt.Root())` **透明加密落盘**
// （物理 > 逻辑），而 `quotaLocalFS.WriteFile` 用源条目 `size`（**逻辑**）做 TryReserve/Commit
// ⇒ owner Scope **少记** `(物理 − 逻辑)`（且 owner 配额侧**永不收敛**，只有卷池靠磁盘扫描收敛）
// ⇒ **可超真实配额**（违反 G1）。
//
// 修：`Executor.TenantPhysSize`（装配注入 `h.SyncTenantPhysSize` = `files.RootPhysSizer` 同源）
// ⇒ `quotaLocalFS.phys` 在**账本边界**折算（不改动写入语义）。
//
// 变异：去掉折算（phys 改 nil 或改回 `size`）⇒ 断言红。

import (
	"context"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func TestQuotaLocalFS_ChargedPhysicalBytes(t *testing.T) {
	t.Parallel()
	const n = 8192 // 源条目逻辑长度
	pool := quota.NewPool(0)
	scope := pool.Scope("/tenant/alice/user", 1<<20)
	q := &quotaLocalFS{
		inner:    syncpkg.NewLocalFS(t.TempDir(), discardLogger()),
		owner:    "alice",
		scopeFor: func(string, string) *quota.Scope { return scope },
		phys:     storage.EncryptedPhysicalSize,
	}
	if err := q.WriteFile(context.Background(), "f.bin", strings.NewReader(strings.Repeat("x", n)), n, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if got, want := scope.Usage(), storage.EncryptedPhysicalSize(n); got != want {
		t.Fatalf("sync pull 本地写必须按**物理**口径入账（与 TenantRootFS 的加密落盘同源）: usage=%d want=%d（逻辑=%d）",
			got, want, n)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("预留应已结算清空: reserved=%d", got)
	}

	// 未注入 phys（明文卷 / 路径式 LocalFS）⇒ 恒等（零回归）。
	pool2 := quota.NewPool(0)
	scope2 := pool2.Scope("/tenant/alice/user", 1<<20)
	q2 := &quotaLocalFS{
		inner:    syncpkg.NewLocalFS(t.TempDir(), discardLogger()),
		owner:    "alice",
		scopeFor: func(string, string) *quota.Scope { return scope2 },
	}
	if err := q2.WriteFile(context.Background(), "p.bin", strings.NewReader(strings.Repeat("y", n)), n, 0); err != nil {
		t.Fatalf("WriteFile(明文卷): %v", err)
	}
	if got := scope2.Usage(); got != n {
		t.Fatalf("未注入 phys 必须恒等（明文卷/未装配零回归）: usage=%d want=%d", got, n)
	}
}

// TestExecutor_PhysFor 钉住注入解析：未注入 ⇒ nil（恒等）；注入 ⇒ 直接透传。
func TestExecutor_PhysFor(t *testing.T) {
	t.Parallel()
	if got := (&Executor{}).physFor("alice"); got != nil {
		t.Fatal("未注入 TenantPhysSize 时必须返回 nil（恒等，零回归）")
	}
	want := storage.EncryptedPhysicalSize
	e := &Executor{TenantPhysSize: func(string) func(int64) int64 { return want }}
	got := e.physFor("alice")
	if got == nil || got(8192) != want(8192) {
		t.Fatal("注入后必须透传该 owner 的折算函数")
	}
}
