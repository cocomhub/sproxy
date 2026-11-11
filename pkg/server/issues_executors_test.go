// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// fakeCleaner 是 DirMetaCleaner 测试替身（记录被调用的容器、可注入错误/删除数）。
type fakeCleaner struct {
	got     []string
	removed int
	err     error
}

func (f *fakeCleaner) CleanupStaleDirMeta(_ context.Context, container string) (int, error) {
	f.got = append(f.got, container)
	return f.removed, f.err
}

// TestIssuesExecutors_DirMetaCleanup_Success 锁定核心闭环：
// 执行器把事项（Path = **容器路径**）交给卷的清理能力；**成功（含 removed=0 的幂等情形）⇒
// 返回 nil ⇒ Manager 会把事项置为 resolved**。
func TestIssuesExecutors_DirMetaCleanup_Success(t *testing.T) {
	t.Parallel()
	cleaner := &fakeCleaner{removed: 0} // 幂等：已清干净也算成功
	center := NewIssueCenter(filepath.Join(t.TempDir(), "issues.json"))
	execs := NewIssuesExecutors(ExecutorDeps{
		Resolve: func(vol string) any {
			if vol == "baidu" {
				return cleaner
			}
			return nil
		},
	})

	center.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed),
		"OypE/abc/ContainerX", "改名后旧 meta 删除失败（逻辑路径 d1 → d2）", true)

	// 未到期 ⇒ 不执行
	if res := center.RunIssuesOnce(context.Background(), time.Now(), execs); res.Ran != 0 {
		t.Fatalf("未到期不应执行：%+v", res)
	}
	// 到期 ⇒ 执行成功 ⇒ 事项 resolved
	res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs)
	if res.Ran != 1 || res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("重试结果不符：%+v", res)
	}
	if len(cleaner.got) != 1 || cleaner.got[0] != "OypE/abc/ContainerX" {
		t.Fatalf("执行器应把**容器路径**传给清理能力，got %v", cleaner.got)
	}
	if n := len(center.Manager().List(issues.ListFilter{UnresolvedOnly: true})); n != 0 {
		t.Fatalf("成功后不应再有未解决事项，got %d", n)
	}
}

// TestIssuesExecutors_DirMetaCleanup_FailuresKeepRetrying 锁定：可重试的失败必须**保留事项并继续退避重试**
// （不静默丢弃），直到有界上限后转人工。
func TestIssuesExecutors_DirMetaCleanup_FailuresKeepRetrying(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		resolve func(string) any
		vol     string
		path    string
	}{
		{"卷不可用", func(string) any { return nil }, "baidu", "c1"},
		{"卷不支持该能力", func(string) any { return &struct{}{} }, "baidu", "c1"},
		{"缺卷名", func(string) any { return &fakeCleaner{} }, "", "c1"},
		{"缺容器路径", func(string) any { return &fakeCleaner{} }, "baidu", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			center := NewIssueCenter("")
			execs := NewIssuesExecutors(ExecutorDeps{Resolve: c.resolve})
			center.ReportStorageIssue(c.vol, string(issues.KindDirMetaCleanupFailed), c.path, "d", true)
			res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs)
			if res.Failed != 1 {
				t.Fatalf("%s：应记为失败（等待退避重试），got %+v", c.name, res)
			}
			it := center.Manager().List(issues.ListFilter{})[0]
			if it.Status != issues.StatusOpen || it.Attempts != 1 {
				t.Fatalf("%s：应保持 open 且累计尝试，got status=%q attempts=%d", c.name, it.Status, it.Attempts)
			}
			if it.LastError == "" {
				t.Fatalf("%s：应记录失败原因", c.name)
			}
		})
	}
}

// TestIssuesExecutors_CleanerErrorPropagates 锁定：领域清理真实报错 ⇒ 记为失败（退避重试）。
func TestIssuesExecutors_CleanerErrorPropagates(t *testing.T) {
	t.Parallel()
	cleaner := &fakeCleaner{err: errors.New("远端 5xx")}
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{Resolve: func(string) any { return cleaner }})
	center.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "c1", "d", true)

	res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs)
	if res.Failed != 1 {
		t.Fatalf("应记为失败：%+v", res)
	}
	it := center.Manager().List(issues.ListFilter{})[0]
	if it.LastError == "" || it.Attempts != 1 {
		t.Fatalf("应保留失败原因并累计次数：%+v", it)
	}
}

// TestIssuesExecutors_UnregisteredKindBecomesNeedsHuman 锁定：**未注册执行器的类型不得静默跳过**，
// 而是转人工（避免"以为在自动重试、其实永远没跑"）。
func TestIssuesExecutors_UnregisteredKindBecomesNeedsHuman(t *testing.T) {
	t.Parallel()
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{Resolve: func(string) any { return &fakeCleaner{} }})
	// container_not_indexed 当前**未注册**执行器
	center.ReportStorageIssue("baidu", string(issues.KindContainerNotIndexed), "c1", "dir_id 不在索引", true)

	res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs)
	if res.Deferred != 1 || res.Ran != 0 {
		t.Fatalf("未注册类型应记为 Deferred：%+v", res)
	}
	it := center.Manager().List(issues.ListFilter{})[0]
	if it.Status != issues.StatusNeedsHuman {
		t.Fatalf("应转 needs_human，got %q", it.Status)
	}
	if it.LastError == "" {
		t.Fatal("应写明原因（无自动处理能力）")
	}
}

// TestIssuesExecutors_NeedsHumanNotRetried 锁定：不可自主决策的事项（Retryable=false）**不进入重试**。
func TestIssuesExecutors_NeedsHumanNotRetried(t *testing.T) {
	t.Parallel()
	cleaner := &fakeCleaner{}
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{Resolve: func(string) any { return cleaner }})
	center.ReportStorageIssue("baidu", string(issues.KindDirMetaCleanupFailed), "c1", "d", false)

	if res := center.RunIssuesOnce(context.Background(), time.Now().Add(100*time.Hour), execs); res.Ran != 0 {
		t.Fatalf("needs_human 不应执行：%+v", res)
	}
	if len(cleaner.got) != 0 {
		t.Fatal("needs_human 不得调用清理能力")
	}
}
