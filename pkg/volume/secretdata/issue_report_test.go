// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	shardseal "github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// captureReporter 记录上报的存储问题（测试替身；构造注入，不触碰包级状态）。
type captureReporter struct {
	mu     sync.Mutex
	issues []capturedIssue
}

type capturedIssue struct {
	Volume, Kind, Path, Detail string
	Retryable                  bool
}

func (r *captureReporter) ReportStorageIssue(volume, kind, path, detail string, retryable bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.issues = append(r.issues, capturedIssue{Volume: volume, Kind: kind, Path: path, Detail: detail, Retryable: retryable})
}

func (r *captureReporter) snapshot() []capturedIssue {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedIssue, len(r.issues))
	copy(out, r.issues)
	return out
}

// newIssueFS 构造带 reporter 的 secretdata FS（底层为本地目录，可选故障包装）。
func newIssueFS(t *testing.T, inner syncpkg.FS, rep IssueReporter) *SecretdataFS {
	t.Helper()
	fs, err := NewFS(inner, Options{
		Secret:        []byte("test-secret-key-000"),
		Algorithm:     testAlgo,
		Block:         shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:       t.TempDir(),
		IssueReporter: rep,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// TestReportStorageIssue_NilSafe 锁定：未装配 reporter 时不得 panic（零回归）。
func TestReportStorageIssue_NilSafe(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs := newIssueFS(t, inner, nil)
	// 不应 panic
	fs.reportStorageIssue(issueKindDirMetaCleanupFailed, "p", "d", true)
	fs.reportStorageIssue(issueKindDirMetaCleanupFailed, "p", "d", true)
}

// TestReportStorageIssue_Forwards 锁定上报内容被原样转发（类型码 / 路径 / 详情 / 可重试标记）。
func TestReportStorageIssue_Forwards(t *testing.T) {
	t.Parallel()
	rep := &captureReporter{}
	fs := newIssueFS(t, syncpkg.NewLocalFS(t.TempDir(), nil), rep)
	fs.reportStorageIssue(issueKindDirMetaCleanupFailed, "c/old-blob", "详细原因", true)

	got := rep.snapshot()
	if len(got) != 1 {
		t.Fatalf("应上报 1 条，got %d", len(got))
	}
	if got[0].Kind != issueKindDirMetaCleanupFailed || got[0].Path != "c/old-blob" ||
		got[0].Detail != "详细原因" || !got[0].Retryable {
		t.Fatalf("上报内容不符：%+v", got[0])
	}
}

// TestRenameDir_OldMetaDeleteFails_ReportsIssue 端到端锁定用户要求（2026-11-10）：
// **改名/移动后删除旧目录 meta 失败**时，必须**登记为可自动重试的事项**
// （此前只有一条 WARN 日志 ⇒ 运维无从感知、无从重试）。
func TestRenameDir_OldMetaDeleteFails_ReportsIssue(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	inner := syncpkg.NewLocalFS(root, nil)

	// 先正常建目录（生成 dir-meta），再用故障包装重建 FS（模拟"改名时底层删失败"）。
	seed := newIssueFS(t, inner, nil)
	ctx := context.Background()
	if err := seed.MakeDir(ctx, "olddir"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}

	rep := &captureReporter{}
	faulty := &faultDeleteFS{wrap: inner, fail: 1} // 第一次 Delete 失败
	fs := newIssueFS(t, faulty, rep)

	if err := fs.Rename(ctx, "olddir", "newdir"); err != nil {
		// 改名本身**不应失败**（旧 meta 删不掉是「有界降级」，靠 Version 仲裁继续工作）。
		t.Fatalf("改名不应因旧 meta 删除失败而报错: %v", err)
	}
	got := rep.snapshot()
	if len(got) == 0 {
		t.Fatal("旧目录 meta 删除失败必须登记事项（此前只有 WARN 日志）")
	}
	if got[0].Kind != issueKindDirMetaCleanupFailed {
		t.Fatalf("类型码 = %q，期望 %q", got[0].Kind, issueKindDirMetaCleanupFailed)
	}
	if !got[0].Retryable {
		t.Fatal("该事项应标记为**可自动重试**（重试删除旧 blob 直到成功）")
	}
	// 详情必须能让人明白"发生了什么 + 为什么要重试 + 人工注意什么"。
	for _, want := range []string{"olddir", "newdir", "两个目录 meta", "重试删除旧 blob"} {
		if !containsStr(got[0].Detail, want) {
			t.Errorf("Detail 应包含 %q（便于人工判断），got %q", want, got[0].Detail)
		}
	}
}

func containsStr(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
