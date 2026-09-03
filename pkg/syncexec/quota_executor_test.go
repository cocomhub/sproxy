// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

// newScopeTenantRoot 构造带 user 桶配额 Scope 的租户根解析器：返回 user/meta 根绝对路径
// + alice 的 user 桶 Scope（quota.NewPool，max<0 不限制）。scope 供测试断言对账。
// 同一 owner 复用同一 Scope 实例（缓存），断言与实现写到同一账本。
// aliceMax: aliceMax<0 表示不限制（用 pool.Scope(path, aliceMax) 上限；aliceMax=0 表示无上限）。
func newScopeTenantRoot(base string, pool *quota.Pool, aliceMax int64) (syncmgr.TenantRootResolver, func(owner string) *quota.Scope) {
	cache := make(map[string]*quota.Scope)
	scopeFor := func(owner string) *quota.Scope {
		if owner == "" {
			owner = "anonymous"
		}
		if s, ok := cache[owner]; ok {
			return s
		}
		max := int64(0)
		if owner == "alice" {
			max = aliceMax
		}
		s := pool.Scope("/tenant/"+owner+"/user", max)
		cache[owner] = s
		return s
	}
	res := func(owner string) (string, string, bool) {
		if owner == "" {
			owner = "anonymous"
		}
		return filepath.Join(base, owner, "user"),
			filepath.Join(base, owner, "meta", "sync"), true
	}
	return res, scopeFor
}

func TestExecutor_Pull_PerFileTryReserveChargesUserBucket(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("sub/a.txt", "aaaa")
	remote.SeedFile("sub/b.txt", "bbbbbb")
	remote.SeedDir("sub")
	base := t.TempDir()
	pool := quota.NewPool(0)
	resolver, scopeFor := newScopeTenantRoot(base, pool, -1)
	exec := NewExecutor(resolver, discardLogger())
	exec.SetTenantScopeResolver(scopeFor)

	task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, Owner: "alice"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed, got %q", res.Status)
	}
	// 两个文件 4+6=10 字节全部入账到 alice user 桶 Scope；无预留残留。
	if got := scopeFor("alice").Usage(); got != 10 {
		t.Fatalf("pull 后 alice user 桶 committed=%d want 10（逐文件入账）", got)
	}
	if got := scopeFor("alice").Reserved(); got != 0 {
		t.Fatalf("pull 后 alice user 桶 Reserved()=%d want 0", got)
	}
}

func TestExecutor_Pull_QuotaExceededFilesFailNotWholeSync(t *testing.T) {
	srv, remote := syncmock.NewServer(t)
	remote.SeedFile("sub/big.bin", strings.Repeat("x", 1000))
	remote.SeedFile("sub/small.txt", "ok")
	remote.SeedDir("sub")
	base := t.TempDir()
	pool := quota.NewPool(0)
	resolver, scopeFor := newScopeTenantRoot(base, pool, 1400)
	exec := NewExecutor(resolver, discardLogger())
	exec.SetTenantScopeResolver(scopeFor)
	// 上限 1400（multi，满足 502 ≤ 1400 < 1500）：无论并发序，big(1000)+预留500 必超限
	// （1500>1400）→ big.bin 失败；small(2)+预留500=502 ≤ 1400 恒成功。临界边界避免
	// "big 先占满恰 1500 后 small 失败"的竞态，使失败/成功确定性。
	rr, err := scopeFor("alice").TryReserve(500)
	if err != nil {
		t.Fatal(err)
	}
	rr.Commit(500)

	task := &syncmgr.SyncTask{ID: "t1", Direction: "pull", Remote: "r1", Src: "sub", Dst: "local", Recursive: true, Owner: "alice", ConflictPolicy: "skip"}
	// 枚举 big.bin 与 small.txt 两个文件 → completed；big 单文件错误。
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err == nil {
		// engine 吞单文件错误 → 返回 completed（不 abort）
		if res.Status != "completed" {
			t.Fatalf("单文件配额失败不应中止整体，状态=%q", res.Status)
		}
	} else if res != nil && res.Status == "failed" {
		t.Fatalf("单文件配额失败不应让任务整体 failed: %+v", res)
	}

	// 小文件仍应落盘；大文件因配额不足被拒（ActionError）→ 不落盘。
	// 注意：engine 多文件并发传输（默认 3），Run 返回仅代表 job.Results 已记录——因配额
	// 不足大文件失败快、小文件仍有并发在途；-race 下 engine 的 wg.Wait() 在 Run 返回前
	// 已等待全部文件完成，故此时本地文件已落盘。此处无需额外等待。
	if got := readLocalFile(t, userRootFor(base, "alice"), "local/small.txt"); got != "ok" {
		t.Fatalf("小文件应成功落盘: %q", got)
	}
	for _, r := range res.Results {
		if strings.HasSuffix(r.Path, "big.bin") {
			if r.Action != "error" || r.Error == "" {
				t.Fatalf("big.bin 应记录为单文件错误（含原因）, got %+v", r)
			}
		}
	}
}
