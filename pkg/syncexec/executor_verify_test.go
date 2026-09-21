// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncexec

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/testutil/syncmock"
)

// NewTestLocalFSForVerify 构造 base 下子目录的 LocalFS（测试隔离用）。
func NewTestLocalFSForVerify(base, name string) *syncpkg.LocalFS {
	dir := filepath.Join(base, name)
	_ = os.MkdirAll(dir, 0o755)
	return syncpkg.NewLocalFS(dir, nil)
}

// sha256HexOf 返回字符串的 SHA-256 hex（与 syncmock.SHA256Hex 等价）。
func sha256HexOf(s string) string {
	return syncmock.SHA256Hex([]byte(s))
}

// TestVerifyAfterSync_On_DetectsTamper 验证 verifyAfterSync 开启时：
// 目标内容被篡改（与 job.Results 记录的源 checksum 不一致）→ 返回校验失败数 >0，
// 并把 verify_failed 结果追加进 job.Results。
func TestVerifyAfterSync_On_DetectsTamper(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	src := NewTestLocalFSForVerify(base, "src")
	dst := NewTestLocalFSForVerify(base, "dst")
	writeLocalFile(t, filepath.Join(base, "src"), "a.txt", "hello verify")

	job := &syncpkg.Job{
		Results: []syncpkg.FileResult{
			{Path: "a.txt", Action: syncpkg.ActionCreated, Size: 12, Checksum: sha256HexOf("hello verify")},
		},
		VerifyAfter: true,
	}
	// 篡改目标（模拟落盘损坏；checksum 与源不一致）。
	if err := os.WriteFile(filepath.Join(base, "dst", "a.txt"), []byte("tampered!"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	n := exec.verifyAfterSync(context.Background(), src, dst, job)
	if n == 0 {
		t.Fatalf("目标被篡改应检出校验失败，got 0, results=%+v", job.Results)
	}
	found := false
	for _, r := range job.Results {
		if r.Action == syncpkg.ActionVerifyFailed && r.Path == "a.txt" {
			found = true
		}
	}
	if !found {
		t.Fatalf("job.Results 应追加 a.txt verify_failed，got %+v", job.Results)
	}
}

// TestVerifyAfterSync_Off_ZeroOverhead 验证默认关闭零回归：
// VerifyAfter=false 时不调用 Verify——即使目标损坏也不产生校验失败。
func TestVerifyAfterSync_Off_ZeroOverhead(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	src := NewTestLocalFSForVerify(base, "src")
	dst := NewTestLocalFSForVerify(base, "dst")
	writeLocalFile(t, filepath.Join(base, "src"), "a.txt", "hello verify")

	job := &syncpkg.Job{
		Results: []syncpkg.FileResult{
			{Path: "a.txt", Action: syncpkg.ActionCreated, Size: 12, Checksum: sha256HexOf("hello verify")},
		},
		VerifyAfter: false,
	}
	if err := os.WriteFile(filepath.Join(base, "dst", "a.txt"), []byte("tampered!"), 0o644); err != nil {
		t.Fatal(err)
	}
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	n := exec.verifyAfterSync(context.Background(), src, dst, job)
	if n != 0 {
		t.Fatalf("默认关闭不应校验，got %d", n)
	}
	for _, r := range job.Results {
		if r.Action == syncpkg.ActionVerifyFailed {
			t.Fatalf("默认关闭不应追加 verify_failed: %+v", r)
		}
	}
}

// TestExecutor_VerifyAfter_Consistent 验证装配层：verify_after=true 且目标一致时
// 校验通过（RunResult.VerifyFailed=0，results 无 verify_failed）。
func TestExecutor_VerifyAfter_Consistent(t *testing.T) {
	t.Parallel()
	srv, _ := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "hello verify")

	task := &syncmgr.SyncTask{
		ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "",
		ConflictPolicy: "skip", VerifyAfter: true,
	}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != "completed" {
		t.Fatalf("状态应为 completed，got %q", res.Status)
	}
	if res.VerifyFailed != 0 {
		t.Fatalf("校验一致应 0 失败，got %d", res.VerifyFailed)
	}
	for _, r := range res.Results {
		if r.Action == "verify_failed" {
			t.Fatalf("不应出现 verify_failed: %+v", r)
		}
	}
}

// TestExecutor_VerifyAfter_DefaultOff 验证装配层默认（VerifyAfter=false）零回归：
// RunResult.VerifyFailed=0。
func TestExecutor_VerifyAfter_DefaultOff(t *testing.T) {
	t.Parallel()
	srv, _ := syncmock.NewServer(t)
	base := t.TempDir()
	exec := NewExecutor(newTestTenantRoot(base), discardLogger())
	writeLocalFile(t, userRootFor(base, ""), "a.txt", "hello default")

	task := &syncmgr.SyncTask{ID: "t1", Direction: "push", Remote: "r1", Src: "", Dst: "", ConflictPolicy: "skip"}
	res, err := exec.Run(context.Background(), task, remoteConfig(srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if res.VerifyFailed != 0 {
		t.Fatalf("默认关闭不应有校验失败，got %d", res.VerifyFailed)
	}
}
