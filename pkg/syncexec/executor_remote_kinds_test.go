// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

// executor_remote_kinds_test.go 是 **P4** 的兼容性回归钉住：
//
//  1. **旧配置（`sync_remotes` 无 kind）继续可跑**：`Kind` 缺省即 direct（零迁移），
//     且该形态必须能通过任务期校验并真的跑完整条 HTTP 直连链路；
//  2. **两载体共存互不干扰**：同一个 Executor 上 direct 与 mesh 远端各跑一次，direct 不得触碰
//     mesh 工厂、mesh 不得走 HTTP。
//
// 说明：本文件的用例是**兼容性保证**（P4），不是新行为 —— 因此设计上先绿；其「会咬人」由
// 变异验证证明（见 PR/提交说明：把 `KindOrDirect` 的空值改为 mesh、或让 direct 分支误调工厂，
// 对应用例即红）。

import (
	"context"
	"sync/atomic"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

// TestExecutor_LegacyRemoteWithoutKindRunsDirect 钉住「旧配置零迁移」：**不带 kind** 的远端
// 就是 direct，且能跑完整条 HTTP 直连链路（上传落盘到 mock 远端）。
func TestExecutor_LegacyRemoteWithoutKindRunsDirect(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	srv, remote := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "legacy.txt", "legacy direct payload")

	// 旧形态：只有 name/url/凭据，**没有 Kind 字段**。
	rc := syncmgr.RemoteConfig{
		Name: "legacy", URL: srv.URL,
		AccessKey: "legacy-ak", AccessKeySecret: "legacy-sk-0123456789", AccessKeyID: "skey-legacy",
	}
	if got := rc.KindOrDirect(); got != syncmgr.RemoteKindDirect {
		t.Fatalf("Kind 缺省应为 direct（零迁移）, got %q", got)
	}
	if err := rc.ValidateForTask(); err != nil {
		t.Fatalf("旧形态应通过任务期校验: %v", err)
	}

	// 不得因「装了 mesh 能力」而改变 direct 行为：即使注入了工厂，direct 远端也不得调用它。
	var factoryCalls int64
	exec.SetMeshFSFactory(func(context.Context, syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		atomic.AddInt64(&factoryCalls, 1)
		return nil, nil, nil
	})

	task := &syncmgr.SyncTask{ID: "t-legacy", Direction: "push", Remote: "legacy", Src: "", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, rc)
	if err != nil {
		t.Fatalf("旧配置推送应成功: %v", err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed, got %q", res.Status)
	}
	if got := atomic.LoadInt64(&factoryCalls); got != 0 {
		t.Fatalf("direct 载体不得触碰 mesh 工厂（调用 %d 次）", got)
	}
	// 内容真的到了 mock 远端。
	if f, ok := remote.SnapshotFiles()["legacy.txt"]; !ok || string(f.Data) != "legacy direct payload" {
		t.Fatalf("远端应存在 legacy.txt 且内容正确: %+v", remote.SnapshotFiles())
	}
}

// TestExecutor_TwoKindsCoexist 钉住两载体共存：同一 Executor 上 direct 与 mesh 各跑一次，
// 各自成功且互不串道（direct 走 HTTP、mesh 走注入的 FS）。
func TestExecutor_TwoKindsCoexist(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	srv, remote := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "direct payload")
	writeLocalFile(t, userRootFor(base, ""), "b.txt", "mesh payload")

	fake := newFakeMeshFS()
	var meshCalls int64
	exec.SetMeshFSFactory(func(_ context.Context, rc2 syncmgr.RemoteConfig) (syncpkg.FS, func(), error) {
		atomic.AddInt64(&meshCalls, 1)
		if rc2.Name != "r-mesh" {
			t.Errorf("工厂只应服务 mesh 远端, got %q", rc2.Name)
		}
		return fake, func() {}, nil
	})

	direct := syncmgr.RemoteConfig{
		Name: "r-direct", URL: srv.URL,
		AccessKey: "ak", AccessKeySecret: "0123456789abcdef", AccessKeyID: "skey-d",
	}
	mesh := syncmgr.RemoteConfig{
		Name: "r-mesh", Kind: syncmgr.RemoteKindMesh,
		Node: "nodeB", Volume: "main", PeerPins: []string{meshTestPin},
	}

	// direct：走 HTTP。
	if _, err := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-direct", Direction: "push", Remote: "r-direct", Src: "a.txt", Dst: "a.txt", ConflictPolicy: "skip",
	}, direct); err != nil {
		t.Fatalf("direct 推送: %v", err)
	}
	// mesh：走注入的 FS。
	if _, err := exec.Run(context.Background(), &syncmgr.SyncTask{
		ID: "t-mesh", Direction: "push", Remote: "r-mesh", Src: "b.txt", Dst: "b.txt", ConflictPolicy: "skip",
	}, mesh); err != nil {
		t.Fatalf("mesh 推送: %v", err)
	}

	if got := atomic.LoadInt64(&meshCalls); got != 1 {
		t.Fatalf("工厂应只被调用 1 次（仅 mesh 远端）, got %d", got)
	}
	if _, ok := remote.SnapshotFiles()["a.txt"]; !ok {
		t.Fatalf("direct 远端应收到 a.txt: %+v", remote.SnapshotFiles())
	}
	if _, ok := remote.SnapshotFiles()["b.txt"]; ok {
		t.Fatal("mesh 远端的内容不得落到 HTTP 直连远端（串道）")
	}
	if got, ok := fake.content("b.txt"); !ok || got != "mesh payload" {
		t.Fatalf("mesh FS 应收到 b.txt 且内容正确: %q ok=%v", got, ok)
	}
}
