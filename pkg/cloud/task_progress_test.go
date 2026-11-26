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

// TestRecordProgressSample_CapAndDecimate 锁定容量语义（2026-11-26 更新）：
// 满窗后**降采样**（不是丢最旧），长度恒 ≤ progressSampleCap，且最早采样仍在。
func TestRecordProgressSample_CapAndDecimate(t *testing.T) {
	t.Parallel()
	task := &CloudTask{}
	base := time.Unix(1_700_000_000, 0)
	total := progressSampleCap * 3
	for i := range total {
		recordProgressSample(task, int64(i), int64(total), base.Add(time.Duration(i)*progressSampleInterval))
	}
	if len(task.Progress) > progressSampleCap {
		t.Fatalf("采样长度 = %d, 应 ≤ %d", len(task.Progress), progressSampleCap)
	}
	// 历史不丢：最早采样仍在（旧实现会丢最旧）
	if got := task.Progress[0].At; !got.Equal(base) {
		t.Fatalf("最早采样 = %v, want %v（降采样不得丢历史）", got, base)
	}
	// 最新采样必须保留
	lastAt := base.Add(time.Duration(total-1) * progressSampleInterval)
	if got := task.Progress[len(task.Progress)-1].At; !got.Equal(lastAt) {
		t.Fatalf("最新采样 = %v, want %v", got, lastAt)
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

// TestProgressDecimation_KeepsHistoryAndStages 锁定长任务降采样（2026-11-26）：
// 满窗后隔点保留（历史不丢、窗口翻倍），且**阶段切换点必留**。
func TestProgressDecimation_KeepsHistoryAndStages(t *testing.T) {
	t.Parallel()
	task := &CloudTask{TotalSize: 1 << 30}
	base := time.Unix(1_700_000_000, 0)
	// 写满 cap 点（每 10s 一点，全部 downloading）
	for i := range progressSampleCap {
		recordProgressSample(task, int64(i)*(1<<20), task.TotalSize, base.Add(time.Duration(i)*progressSampleInterval))
	}
	if len(task.Progress) != progressSampleCap {
		t.Fatalf("满窗长度 = %d, want %d", len(task.Progress), progressSampleCap)
	}
	firstBefore := task.Progress[0].At
	// 触发一次降采样（再记一点，且是阶段切换 ⇒ 必留）
	task.Diagnostics = &TaskDiagnostics{Stage: "transferring"}
	at := base.Add(time.Duration(progressSampleCap) * progressSampleInterval)
	if !recordProgressSample(task, task.TotalSize, task.TotalSize, at) {
		t.Fatal("阶段切换应立即采样")
	}
	if len(task.Progress) > progressSampleCap {
		t.Fatalf("降采样后长度 = %d, 应 ≤ %d", len(task.Progress), progressSampleCap)
	}
	// 历史起点不得被丢弃（此前环形裁剪会丢最旧）
	if !task.Progress[0].At.Equal(firstBefore) {
		t.Fatalf("降采样不得丢最早采样：got %v want %v", task.Progress[0].At, firstBefore)
	}
	// 阶段切换点必须保留
	found := false
	for _, s := range task.Progress {
		if s.Stage == "transferring" {
			found = true
		}
	}
	if !found {
		t.Fatal("阶段切换采样必须保留（时间轴依赖它）")
	}
}

// TestDecimateProgressSamples_KeepsStageBoundaries 单元级：奇偶点 + 阶段边界。
func TestDecimateProgressSamples_KeepsStageBoundaries(t *testing.T) {
	t.Parallel()
	in := []ProgressSample{
		{At: time.Unix(0, 0), Stage: "downloading"},
		{At: time.Unix(1, 0), Stage: "downloading"},
		{At: time.Unix(2, 0), Stage: "downloading"},
		{At: time.Unix(3, 0), Stage: "verifying"}, // 奇数下标 + 阶段切换 ⇒ 必留
		{At: time.Unix(4, 0), Stage: "verifying"},
	}
	out := decimateProgressSamples(in)
	if len(out) != 4 {
		t.Fatalf("降采样结果 = %d, want 4（偶数下标 0/2/4 + 阶段切换点 3）", len(out))
	}
	seen := map[time.Time]bool{}
	for _, s := range out {
		seen[s.At] = true
	}
	for _, want := range []time.Time{time.Unix(0, 0), time.Unix(2, 0), time.Unix(3, 0), time.Unix(4, 0)} {
		if !seen[want] {
			t.Fatalf("缺少采样点 %v（got %v）", want, out)
		}
	}
}
