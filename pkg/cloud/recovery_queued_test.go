// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// recovery_queued_test.go 锁定评审 R14 的两条恢复缺陷：
//
//	C1 排队中的任务（磁盘 pending、但已被 SubmitAndStart 接管）重启后必须**继续**，
//	   只有「从未提交」的 pending 才是孤儿（转 failed）；
//	C2 下载产物已完整时（崩溃发生在转存/终态之前），重启必须**复用产物**而不是整文件重下；
//	NEW-2 恢复时读不出的任务必须计数并上报指标（此前只逐条 Warn ⇒ 汇总「全部成功」）。
//
// 反向约束（防过度复用）：显式 `ResumeTask` 一律重下（清标记）——completed+damaged 的
// 自愈修复不能被「复用坏产物」短路。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// urlSeq 生成唯一后缀（让每条种子任务有不同 URL，避免 CreateTask 去重）。
var urlSeq atomic.Uint64

// seedPersistedTask 直接落盘一条任务（不启动下载），返回其 ID。
func seedPersistedTask(t *testing.T, mgr *CloudDownloadManager, status string, submitted bool) string {
	t.Helper()
	// URL 必须唯一：CreateTask 对同 URL 去重（会返回既有任务，导致只落一条）。
	url := fmt.Sprintf("http://127.0.0.1:1/%s-%d.bin", status, urlSeq.Add(1))
	task, err := mgr.CreateTask("url", url, "x.bin", 1024, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	mgr.mu.Lock()
	task.Status = status
	task.Submitted = submitted
	task.UpdatedAt = time.Now()
	mgr.mu.Unlock()
	if serr := mgr.saveTask(task); serr != nil {
		t.Fatalf("saveTask: %v", serr)
	}
	return task.ID
}

// TestRecovery_QueuedSubmittedTaskRestarts C1：pending+Submitted 必须重启；pending 未提交仍转 failed。
func TestRecovery_QueuedSubmittedTaskRestarts(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	queued := seedPersistedTask(t, mgr, "pending", true)
	orphan := seedPersistedTask(t, mgr, "pending", false)

	mgr.mu.Lock()
	delete(mgr.tasks, queued)
	delete(mgr.tasks, orphan)
	mgr.mu.Unlock()

	root := mgr.cloudStateRootFor("")
	if root == nil {
		t.Fatal("cloudStateRootFor 返回 nil")
	}
	recovered, restarted, _, _ := mgr.recoverTasksFromDir(root)
	if recovered != 2 {
		t.Fatalf("recovered=%d, want 2", recovered)
	}
	if restarted != 1 {
		t.Fatalf("restarted=%d, want 1（只有已提交的排队任务应重启）", restarted)
	}

	mgr.mu.RLock()
	q, okQ := mgr.tasks[queued]
	o, okO := mgr.tasks[orphan]
	var qErr, qStatus, oStatus string
	if okQ {
		qErr, qStatus = q.Error, q.Status
	}
	if okO {
		oStatus = o.Status
	}
	mgr.mu.RUnlock()
	// 断言的是**孤儿判定**（C1 的缺陷），而不是终态：重启后的下载会真的跑起来（URL 不可达），
	// 因此排队任务可能以**网络错误**失败 —— 这是正确行为；被当作孤儿才是缺陷（错误文案固定）。
	if !okQ || strings.Contains(qErr, "interrupted before start") {
		t.Fatalf("已提交的排队任务被误判孤儿（status=%q err=%q）——重启应继续而不是转到「从未启动」终态", qStatus, qErr)
	}
	if !okO || oStatus != "failed" {
		t.Fatalf("从未提交的 pending 应转 failed（代表 CreateTask 与启动之间崩溃），实际 status=%q", oStatus)
	}
}

// TestRecovery_UnreadableTaskCounted NEW-2：坏 JSON 计数 + 指标可见，且不影响好任务恢复。
func TestRecovery_UnreadableTaskCounted(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	good := seedPersistedTask(t, mgr, "downloading", true)
	root := mgr.cloudStateRootFor("")
	if root == nil {
		t.Fatal("cloudStateRootFor 返回 nil")
	}
	badRel := fmt.Sprintf("%s/broken.json", cloudStateRel)
	if err := writeRootFile(root, badRel, []byte("{not-json")); err != nil {
		t.Fatalf("write broken task: %v", err)
	}

	mgr.mu.Lock()
	delete(mgr.tasks, good)
	mgr.mu.Unlock()

	recovered, _, _, skipped := mgr.recoverTasksFromDir(root)
	if recovered != 1 {
		t.Fatalf("recovered=%d, want 1（坏文件不应影响好任务）", recovered)
	}
	if skipped != 1 {
		t.Fatalf("skipped=%d, want 1", skipped)
	}
	if got := mgr.metrics.RecoverySkipped.Load(); got != 1 {
		t.Fatalf("RecoverySkipped 指标=%d, want 1（不可读必须可观测）", got)
	}
}

// TestReuseDownloadedArtifact C2：产物完整才复用；尺寸不符/无标记则复用失效。
func TestReuseDownloadedArtifact(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	task, err := mgr.CreateTask("url", "http://127.0.0.1:1/a.bin", "a.bin", 8, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	dest := filepath.Join(t.TempDir(), "a.bin")
	if werr := os.WriteFile(dest, []byte("12345678"), 0o600); werr != nil {
		t.Fatalf("write artifact: %v", werr)
	}

	// ① 未标记 ⇒ 不复用（正常重下）
	if r := mgr.reuseDownloadedArtifact(task, dest); r != nil {
		t.Fatal("未标记下载完成时不得复用")
	}

	// ② 标记 + 尺寸一致 ⇒ 复用，且 checksum 透传
	mgr.mu.Lock()
	task.DownloadDone = true
	task.DownloadedSize = 8
	task.DownloadedChecksum = "abc123"
	mgr.mu.Unlock()
	r := mgr.reuseDownloadedArtifact(task, dest)
	if r == nil {
		t.Fatal("产物完整时应急复用")
	}
	if r.Size != 8 || r.Checksum != "abc123" || r.Integrity != downloader.ModeLocalOnly {
		t.Fatalf("复用结果字段不符: %+v", r)
	}

	// ③ 尺寸不符（磁盘被截断/替换）⇒ 不复用
	if werr := os.WriteFile(dest, []byte("123"), 0o600); werr != nil {
		t.Fatalf("truncate artifact: %v", werr)
	}
	if r := mgr.reuseDownloadedArtifact(task, dest); r != nil {
		t.Fatal("尺寸不符时不得复用（否则会把残缺文件转存出去）")
	}
}

// TestSubmitAndStart_PersistsSubmitted C1 的**写入侧**回归：
// 排队任务重启可继续的前提是磁盘上有 `submitted=true`。此前单测直接手写该字段 ⇒ 漏掉了
// 「SubmitAndStart 到底有没有落盘」这一环（真实 E2E 抓到：单条提交路径根本没写）。
func TestSubmitAndStart_PersistsSubmitted(t *testing.T) {
	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 1<<30, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, defaultCloudDownloadConfig())
	t.Cleanup(mgr.Close)

	// 不可达地址：任务会立刻失败，但**提交时**必须已把 submitted 落盘（与成败无关）。
	task, err := mgr.SubmitAndStart("url", "http://127.0.0.1:1/never.bin", "never.bin", 1024, nil, "", TaskParams{Save: true})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}

	root := mgr.cloudStateRootFor("")
	if root == nil {
		t.Fatal("cloudStateRootFor 返回 nil")
	}
	data, rerr := readRootFile(root, cloudStateRel+"/"+task.ID+".json")
	if rerr != nil {
		t.Fatalf("读持久化任务: %v", rerr)
	}
	var got CloudTask
	if uerr := json.Unmarshal(data, &got); uerr != nil {
		t.Fatalf("解析持久化任务: %v", uerr)
	}
	if !got.Submitted {
		t.Fatalf("SubmitAndStart 必须把 submitted 落盘（否则重启时排队任务被当孤儿）: %s", string(data))
	}
}
