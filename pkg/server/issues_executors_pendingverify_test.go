// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/server/issues"
)

// 2026-11-17 实测缺口：`KindBlobPendingVerify`（写入时远端可见性未收敛 ⇒ 计入待复核）
// **只登记、不重试** ✗ ⇒ 若块实际未落盘（远端 31066），写入仍报成功，直到**读取时**才 500 ✗
// （实测：卷索引有 st.bin、meta 完整，但 blob `AAAXbvyX…` 在百度侧不存在）。
//
// 修复：注册执行器 ⇒ ① 可见性延迟可**自愈**（重试后 resolve）；
// ② 真丢块 ⇒ 每次重试失败，**达上限转人工**（绝不静默）✗。

// TestIssuesExecutors_PendingVerifyRegistered 锁定：该类型**必须有执行器**
// （未注册会被 RunDue 转 needs_human，等于"从不重试"）。
func TestIssuesExecutors_PendingVerifyRegistered(t *testing.T) {
	t.Parallel()
	center := NewIssueCenter("")
	called := 0
	execs := NewIssuesExecutors(ExecutorDeps{
		ReverifyPendingBlob: func(_ context.Context, volume, rel string) error {
			called++
			if volume != "baidu" || rel != "user/st.bin" {
				t.Fatalf("执行器参数应为 (volume, rel)，got (%q,%q)", volume, rel)
			}
			return nil // 已确认可见 ⇒ resolve
		},
	})
	center.ReportStorageIssue("baidu", string(issues.KindBlobPendingVerify), "user/st.bin", "可见性未收敛", true)

	res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs)
	if called != 1 {
		t.Fatalf("待复核事项必须被重试执行 1 次，实际 %d（未注册 ⇒ 永不重试 ✗）", called)
	}
	if res.Succeeded != 1 || res.Failed != 0 {
		t.Fatalf("执行成功应记为 succeeded（事项 resolve）：%+v", res)
	}
}

// TestIssuesExecutors_PendingVerifyStillInvisibleRetries 锁定：仍不可见 ⇒ 记失败并**累计次数**
// ⇒ 退避重试（不是一次就放弃，也不是静默成功）。
func TestIssuesExecutors_PendingVerifyStillInvisibleRetries(t *testing.T) {
	t.Parallel()
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{
		ReverifyPendingBlob: func(context.Context, string, string) error {
			return errors.New("重校验 codeblob 仍有 1/3 项未通过（invisible）")
		},
	})
	center.ReportStorageIssue("baidu", string(issues.KindBlobPendingVerify), "user/st.bin", "可见性未收敛", true)

	if res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs); res.Failed != 1 {
		t.Fatalf("仍不可见应记为失败（退避重试）：%+v", res)
	}
	it := center.Manager().List(issues.ListFilter{})[0]
	if it.Attempts != 1 || it.LastError == "" {
		t.Fatalf("应累计次数并保留失败原因：%+v", it)
	}
}

// TestIssuesExecutors_PendingVerifyExhaustedNeedsHuman 锁定「多次失败后人工介入」：
// 重试达上限 ⇒ **转 needs_human**（绝不无限重试、绝不静默放弃）。
func TestIssuesExecutors_PendingVerifyExhaustedNeedsHuman(t *testing.T) {
	t.Parallel()
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{
		ReverifyPendingBlob: func(context.Context, string, string) error {
			return errors.New("blob 仍不可见（远端 31066）")
		},
	})
	center.ReportStorageIssue("baidu", string(issues.KindBlobPendingVerify), "user/st.bin", "可见性未收敛", true)

	now := time.Now()
	for i := range 40 {
		center.RunIssuesOnce(context.Background(), now.Add(time.Duration(i+1)*time.Hour), execs)
		if it := center.Manager().List(issues.ListFilter{})[0]; it.Status == issues.StatusNeedsHuman {
			if !strings.Contains(it.LastError, "31066") {
				t.Fatalf("转人工时应保留最后失败原因，got %q", it.LastError)
			}
			return
		}
	}
	t.Fatal("重试达上限后必须转 needs_human（人工介入）✗ 实测仍在自动重试")
}

// TestIssuesExecutors_PendingVerifyMissingFieldsNeedsHuman 锁定：事项缺 volume/rel
// ⇒ 执行器**明确报错**（转人工），不静默成功。
func TestIssuesExecutors_PendingVerifyMissingFieldsNeedsHuman(t *testing.T) {
	t.Parallel()
	center := NewIssueCenter("")
	execs := NewIssuesExecutors(ExecutorDeps{
		ReverifyPendingBlob: func(context.Context, string, string) error { return nil },
	})
	center.ReportStorageIssue("", string(issues.KindBlobPendingVerify), "", "缺字段", true)
	if res := center.RunIssuesOnce(context.Background(), time.Now().Add(time.Hour), execs); res.Failed != 1 {
		t.Fatalf("缺字段应失败并转人工：%+v", res)
	}
	if it := center.Manager().List(issues.ListFilter{})[0]; !strings.Contains(it.LastError, "缺少") {
		t.Fatalf("失败原因应说明缺什么，got %q", it.LastError)
	}
}

// TestRunCloudDownloadRetry 锁定「云端下载失败」执行器契约（2026-11-21）：
// 缺卷/路径 ⇒ 报错转人工；未注入能力 ⇒ 报错；注入 ⇒ 按 (volume, rel) 调用一次。
func TestRunCloudDownloadRetry(t *testing.T) {
	t.Parallel()
	it := issues.Item{Kind: issues.KindCloudDownloadFailed, Volume: "v", Path: "a/b.mp4"}
	if err := runCloudDownloadRetry(t.Context(), ExecutorDeps{}, it); err == nil {
		t.Fatal("未注入执行器必须报错（不静默成功）")
	}
	if err := runCloudDownloadRetry(t.Context(), ExecutorDeps{RetryCloudDownload: func(context.Context, string, string) error { return nil }},
		issues.Item{Kind: issues.KindCloudDownloadFailed, Path: "x"}); err == nil {
		t.Fatal("缺少卷名必须报错")
	}
	var gotV, gotR string
	d := ExecutorDeps{RetryCloudDownload: func(_ context.Context, v, r string) error { gotV, gotR = v, r; return nil }}
	if err := runCloudDownloadRetry(t.Context(), d, it); err != nil {
		t.Fatalf("注入后应成功: %v", err)
	}
	if gotV != "v" || gotR != "a/b.mp4" {
		t.Fatalf("参数透传错误: %s/%s", gotV, gotR)
	}
}
