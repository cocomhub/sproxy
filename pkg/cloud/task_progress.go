// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// task_progress.go 是**下载进度打点**（用户 2026-11-26）：
// 每任务保存一段环形采样（时间/已下字节/总字节/窗口速率/阶段），供
// `GET /api/cloud/tasks/{id}/progress?since=` 增量拉取并在 v2 页面画速率曲线。
//
// 设计取舍：
//   - **随任务持久化**（saveTask 的 JSON 里），重启后历史仍在；
//   - **环形上限** progressSampleCap（默认 240 点 ≈ 40 分钟 @10s），单任务 ~15KiB；
//   - 采样节流 progressSampleInterval（10s）：进度回调很密（每 1MiB 一次），
//     不节流会写爆磁盘/JSON。
package cloud

import (
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

const (
	// progressSampleInterval 是两次采样之间的最小间隔（节流）。
	progressSampleInterval = 10 * time.Second
	// progressSampleCap 是每任务保留的采样点数（环形：超出丢最旧）。
	progressSampleCap = 240
	// chunkStateCap 是每任务保留的分块状态上限（≤20 行自适应几何下通常 ≤20 块）。
	chunkStateCap = 128
)

// ProgressSample 是一次进度采样点。
type ProgressSample struct {
	At         time.Time `json:"at"`
	Downloaded int64     `json:"downloaded"`
	Total      int64     `json:"total"`
	// RateBps 是**相邻两次采样之间的实测速率**（字节/秒；首点 0，不编造）。
	RateBps int64 `json:"rate_bps"`
	// Stage 是采样时所处阶段：resolving | downloading | verifying | transferring。
	Stage string `json:"stage,omitempty"`
}

// recordProgressSample 记录一次采样（调用方需持有 m.mu；节流 + 环形裁剪）。
// 返回是否真的追加了采样（供测试与调用方判断）。
func recordProgressSample(task *CloudTask, downloaded, total int64, now time.Time) bool {
	if task == nil {
		return false
	}
	stage := ""
	if task.Diagnostics != nil {
		stage = task.Diagnostics.Stage
	}
	var rate int64
	if n := len(task.Progress); n > 0 {
		last := task.Progress[n-1]
		// 阶段变化立即采样（阶段时间轴需要精确起点），否则按间隔节流。
		if stage == last.Stage && now.Sub(last.At) < progressSampleInterval {
			return false
		}
		if secs := now.Sub(last.At).Seconds(); secs > 0 && downloaded > last.Downloaded {
			rate = int64(float64(downloaded-last.Downloaded) / secs)
		}
	}
	task.Progress = append(task.Progress, ProgressSample{
		At: now, Downloaded: downloaded, Total: total, RateBps: rate, Stage: stage,
	})
	if len(task.Progress) > progressSampleCap {
		// 满窗**降采样**而不是丢最旧（2026-11-26）：真实任务可达数小时（64GiB @3MB/s ≈ 6h），
		// 只保留最近 240 点（40 分钟）会让整条速率曲线只剩尾巴。改为隔点保留 + **阶段切换点
		// 必留**（时间轴需要精确起点），窗口随之翻倍（40min→80min→160min…），分辨率逐步变粗
		// 但历史不丢。
		task.Progress = decimateProgressSamples(task.Progress)
	}
	return true
}

// decimateProgressSamples 隔点降采样：保留偶数下标 + **阶段切换点**（其前一采样阶段不同）。
// 返回新切片（不共享底层数组，避免旧数组滞留）。
func decimateProgressSamples(in []ProgressSample) []ProgressSample {
	out := make([]ProgressSample, 0, len(in)/2+1)
	prevStage := ""
	for i := range in {
		// 阶段切换点必留（i==0 时 prevStage 为空串，同样保留）。
		if i%2 == 0 || in[i].Stage != prevStage {
			out = append(out, in[i])
		}
		prevStage = in[i].Stage
	}
	return out
}

// ProgressSince 返回 at 之后的采样点（at 为零值 ⇒ 全部）。供 HTTP 增量接口使用。
func (t *CloudTask) ProgressSince(since time.Time) []ProgressSample {
	if t == nil {
		return nil
	}
	if since.IsZero() {
		return append([]ProgressSample(nil), t.Progress...)
	}
	out := make([]ProgressSample, 0, len(t.Progress))
	for _, s := range t.Progress {
		if s.At.After(since) {
			out = append(out, s)
		}
	}
	return out
}

// recordChunkState 记录一次分块状态（调用方持有 m.mu）：同 Index 覆盖，超上限丢弃新块。
// 覆盖而非追加：同一块会经历 pending→downloading→done 多次上报，保留最新状态。
func recordChunkState(task *CloudTask, st downloader.ChunkState) {
	if task == nil || st.Index < 0 {
		return
	}
	for i := range task.Chunks {
		if task.Chunks[i].Index == st.Index {
			task.Chunks[i] = st
			return
		}
	}
	if len(task.Chunks) >= chunkStateCap {
		return
	}
	task.Chunks = append(task.Chunks, st)
}
