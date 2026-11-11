// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// TestIssueCenter_ReportStorageIssue 锁定适配器契约：领域包只需最小接口就能上报，
// 且上报内容（类型码/路径/详情/可重试）原样进入 Manager。
func TestIssueCenter_ReportStorageIssue(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	c.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "jav/makers/natural-high/ACME",
		"改名后删除旧目录 meta 失败：应重试删旧 blob", true)

	got := c.Manager().List(issues.ListFilter{})
	if len(got) != 1 {
		t.Fatalf("应登记 1 条，got %d", len(got))
	}
	if got[0].Kind != issues.KindDirMetaCleanupFailed || got[0].Path != "jav/makers/natural-high/ACME" {
		t.Fatalf("登记内容不符：%+v", got[0])
	}
	if !got[0].Retryable || got[0].Status != issues.StatusOpen {
		t.Fatalf("可重试事项应为 open：%+v", got[0])
	}
}

// TestIssueCenter_NilSafe 锁定：nil 接收者/nil manager 不得 panic（未装配即 no-op，零回归）。
func TestIssueCenter_NilSafe(t *testing.T) {
	t.Parallel()
	var c *IssueCenter
	c.ReportStorageIssue("v", "k", "p", "d", true) // 不应 panic
	if c.Manager() != nil {
		t.Fatal("nil center 的 Manager 应为 nil")
	}
	if res := c.RunIssuesOnce(context.Background(), time.Now(), nil); res.Ran != 0 {
		t.Fatalf("nil center 不应执行重试：%+v", res)
	}
}

// TestIssueCenter_SatisfiesSecretdataReporter 锁定：IssueCenter **可被直接注入**给
// secretdata 卷（结构上满足其最小接口）——这是"装配层把 Manager 适配进领域包"的契约。
func TestIssueCenter_SatisfiesSecretdataReporter(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	var rep interface {
		ReportStorageIssue(volume, kind, path, detail string, retryable bool)
	} = c
	rep.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "p", "d", true)
	if got := len(c.Manager().List(issues.ListFilter{})); got != 1 {
		t.Fatalf("经最小接口上报后应有 1 条，got %d", got)
	}
}

// TestIssueCenter_PersistsAndResumes 锁定：账本持久化 + 重启可续（重启后仍可见、仍可重试）。
func TestIssueCenter_PersistsAndResumes(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "issues.json")
	c1 := NewIssueCenter(p)
	c1.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "container-A", "旧 meta 删除失败", true)

	c2 := NewIssueCenter(p)
	got := c2.Manager().List(issues.ListFilter{})
	if len(got) != 1 || got[0].Path != "container-A" {
		t.Fatalf("重启后应保留事项：%+v", got)
	}
	if got[0].NextRetryAt.IsZero() {
		t.Fatal("重启后仍应保有下次重试时间（可继续自动重试）")
	}
}

// TestDefaultIssuesStorePath 锁定默认路径（运维只需看一个目录：<root>/meta/state/）。
func TestDefaultIssuesStorePath(t *testing.T) {
	t.Parallel()
	if got := DefaultIssuesStorePath("/data/sproxy"); got != filepath.Join("/data/sproxy", "meta", "state", "issues.json") {
		t.Fatalf("默认路径 = %q", got)
	}
	if DefaultIssuesStorePath("") != "" {
		t.Fatal("空 root 应返回空路径（退化为内存存储）")
	}
}

// TestIssueCenter_RunIssuesOnce 锁定：调度入口把到期事项交给执行器；成功后转 resolved。
func TestIssueCenter_RunIssuesOnce(t *testing.T) {
	t.Parallel()
	c := NewIssueCenter("")
	c.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "p1", "d1", true)

	// 首次（未到期）不执行
	if res := c.RunIssuesOnce(context.Background(), time.Now(), map[issues.Kind]issues.RetryFunc{
		issues.KindDirMetaCleanupFailed: func(context.Context, issues.Item) error { return nil },
	}); res.Ran != 0 {
		t.Fatalf("未到期不应执行：%+v", res)
	}
	// 到期后执行并成功
	res := c.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), map[issues.Kind]issues.RetryFunc{
		issues.KindDirMetaCleanupFailed: func(context.Context, issues.Item) error { return nil },
	})
	if res.Ran != 1 || res.Succeeded != 1 {
		t.Fatalf("到期应执行且成功：%+v", res)
	}
	if got := c.Manager().List(issues.ListFilter{UnresolvedOnly: true}); len(got) != 0 {
		t.Fatalf("成功后不应再有未解决事项，got %d", len(got))
	}
}
