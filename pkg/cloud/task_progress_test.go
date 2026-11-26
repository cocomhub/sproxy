// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"testing"
	"time"
)

// TestRecordProgressSample_ThrottleAndRate 锁定打点语义（用户 2026-11-26）：
// 10s 内重复采样被节流；超过间隔后按**实测增量**算速率；阶段变化立即采样。
func TestRecordProgressSample_ThrottleAndRate(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	base := time.Unix(1_700_000_000, 0)
	if !recordProgressSample(task, 0, 1000, base) {
		t.Fatal("首点应记录")
	}
	// 5s 后（< 10s）⇒ 节流
	if recordProgressSample(task, 500, 1000, base.Add(5*time.Second)) {
		t.Fatal("10s 内不应重复采样")
	}
	// 10s 后 ⇒ 记录，速率 = 500B/10s = 50B/s
	if !recordProgressSample(task, 500, 1000, base.Add(10*time.Second)) {
		t.Fatal("超过间隔应记录")
	}
	if got := task.Progress[1].RateBps; got != 50 {
		t.Fatalf("RateBps = %d, want 50", got)
	}
	// 阶段变化立即采样（即使未到间隔），且速率按实际耗时计算
	task.Diagnostics = &TaskDiagnostics{Stage: "verifying"}
	if !recordProgressSample(task, 1000, 1000, base.Add(12*time.Second)) {
		t.Fatal("阶段变化应立即采样")
	}
	if got := task.Progress[2].Stage; got != "verifying" {
		t.Fatalf("Stage = %q, want verifying", got)
	}
}

// TestRecordProgressSample_RingCap 锁定环形上限：超出 progressSampleCap 丢最旧。
func TestRecordProgressSample_RingCap(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	base := time.Unix(1_700_000_000, 0)
	for i := range progressSampleCap + 30 {
		recordProgressSample(task, int64(i), int64(progressSampleCap+30), base.Add(time.Duration(i)*progressSampleInterval))
	}
	if len(task.Progress) != progressSampleCap {
		t.Fatalf("环形缓冲长度 = %d, want %d", len(task.Progress), progressSampleCap)
	}
	// 最旧的已丢弃（保留最近 progressSampleCap 点）
	if got := task.Progress[0].Downloaded; got != 30 {
		t.Fatalf("首点 Downloaded = %d, want 30（最旧 30 点应被丢弃）", got)
	}
}

// TestProgressSince 锁定增量语义：since 之后（严格大于）的采样。
func TestProgressSince(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	base := time.Unix(1_700_000_000, 0)
	for i := range 3 {
		recordProgressSample(task, int64(i*100), 300, base.Add(time.Duration(i)*progressSampleInterval))
	}
	if got := len(task.ProgressSince(time.Time{})); got != 3 {
		t.Fatalf("零值 since 应返回全部，got %d", got)
	}
	if got := task.ProgressSince(base); len(got) != 2 {
		t.Fatalf("since=首点 应返回 2 点，got %d", len(got))
	}
	if got := task.ProgressSince(base.Add(time.Hour)); len(got) != 0 {
		t.Fatalf("未来 since 应返回 0 点，got %d", len(got))
	}
}

// TestSetStage_RecordsSampleImmediately 锁定「阶段变化立即打点」：
// 短阶段（如转存很快）若只等下一次进度回调，会在时间轴上整段丢失。
func TestSetStage_RecordsSampleImmediately(t *testing.T) {
	t.Parallel()
	mgr := &CloudDownloadManager{}
	task := &CloudTask{ID: "t-stage", TotalSize: 1000, Downloaded: 1000}
	mgr.setStage(task, "downloading")
	mgr.setStage(task, "transferring") // 同一时刻也必须有采样（阶段变化优先于节流）
	if len(task.Progress) != 2 {
		t.Fatalf("阶段变化应各记一条采样，got %d 条", len(task.Progress))
	}
	if task.Progress[0].Stage != "downloading" || task.Progress[1].Stage != "transferring" {
		t.Fatalf("阶段序列异常: %+v", task.Progress)
	}
	if task.Diagnostics == nil || task.Diagnostics.Stage != "transferring" {
		t.Fatalf("Diagnostics.Stage 未更新: %+v", task.Diagnostics)
	}
}
