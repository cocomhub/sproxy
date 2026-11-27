// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

// manager_concurrent_readers_test.go 是**行为级**并发守卫：真实下载进行中，读面
// （ListTasks / SnapshotTask / saveTask / flushDirty）与写面（进度/分块上报、失败产物处置）
// 并发访问同一条 task，`-race` 下必须无告警。
//
// 为什么用行为测试而不是源码文本门禁：`pkg/cloud` 的锁纪律靠**约定**（`xxxLocked` 后缀 +
// 「调用方须持 m.mu」注释）维持，任何"扫描 task.X = 赋值是否在锁内"的文本判据都不可靠
// （实测 61 处写入里 22 处被误报 —— 它们都在持锁调用方里）。只有真并发 + `-race` 才能可靠发现。
//
// 历史缺陷（2026-11-26 CI -race，已由 #1304 修）：`settleArtifactsOnFail` 锁外写
// `task.Diagnostics`；#1308 为它加了针对性守卫（失败路径）。本用例把面扩大到**整条下载
// 生命周期**，覆盖进度写面（`task.Downloaded` / `recordProgressSample` / `recordChunkState`）。
//
// **让「被竞争的那次写入」反复发生**（本会话第二次踩到这条）：4MiB 内容按 32KiB × 2ms 慢速
// 分块返回 ⇒ 进度回调触发 130+ 次、下载持续 ~250ms，读面才可能与之重叠。
// 服务端 `SyncThreshold <= 0` 会被配置归一成默认 20MB（⇒ 走**同步**下载，读面完全无法重叠），
// 故这里必须显式给正值 1。

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestDownloadLifecycle_ConcurrentReadersNoRace 在真实下载过程中并发打读面。
func TestDownloadLifecycle_ConcurrentReadersNoRace(t *testing.T) {
	t.Parallel()
	// 服务端**慢速分多块**返回：必须让下载跨足够长的时间窗，才能覆盖到进度写面
	// （`task.Downloaded` 在每次 copy 迭代被写）。窗口太短时 -race 观察不到竞争 ——
	// 实测 512KiB×1ms 只产生 1 个采样点，变异不报；改为 4MiB×32KiB×2ms ≈ 256ms。
	const total = 4 << 20
	body := bytes.Repeat([]byte("R"), total)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		const chunk = 32 << 10
		for off := 0; off < len(body); off += chunk {
			end := min(off+chunk, len(body))
			_, _ = w.Write(body[off:end])
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			<-time.After(2 * time.Millisecond) // 让下载跨多个 tick，进度回调多次触发
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	sm := capacity.NewStorageManager(dir, 16<<20, nil, testLogger())
	mgr, _ := newCloudTestManager(t, dir, sm, &CloudDownloadConfig{
		MaxConcurrent: 2,
		// **必须是正值**：配置归一里 `SyncThreshold <= 0` 会回落默认 20MB，那样本次下载会走
		// **同步**路径（在 SubmitAndStart 内完成），读面完全无法与写面重叠（实测踩到）。
		SyncThreshold:                    1,
		AllowPrivate:                     true,
		TaskTTL:                          time.Hour,
		FailedTaskTTL:                    time.Hour,
		DiscardPartialOnTransientFailure: false,
	})

	task, err := mgr.SubmitAndStart("url", srv.URL, "race-lifecycle.bin", total, t.Context(), "",
		TaskParams{Save: true})
	if err != nil {
		t.Fatalf("SubmitAndStart: %v", err)
	}

	// 读面：与下载 goroutine 的写面完全并发。
	var wg sync.WaitGroup
	wg.Add(1)
	stop := make(chan struct{})
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = mgr.ListTasks("", 0, 0, "")
			// 读回进度/分块字段（与下载 goroutine 的写面直接冲突的点）。
			if snap, ok := mgr.SnapshotTask(task.ID, ""); ok {
				_ = snap.Downloaded
				_ = snap.Progress
				_ = snap.Chunks
			}
			_, _ = mgr.GetTask(task.ID, "")
			_ = mgr.saveTask(task) // 与 flushDirty 同路径（RLock + json.Marshal）
			mgr.markDirty(task.ID) // 触碰 dirty 集（与 flushDirty 的 dirtyMu 交互）
		}
	}()

	// 等任务到达终态（完成或失败都算生命周期走完），再停读面。
	testutil.WaitFor(t, testutil.ScaleTimeout(30*time.Second), func() bool {
		snap, ok := mgr.SnapshotTask(task.ID, "")
		if !ok {
			return false
		}
		return snap.Status == "completed" || snap.Status == "failed" || snap.Status == "cancelled"
	}, "下载未在 30s 内到达终态")

	close(stop)
	wg.Wait()

	snap, ok := mgr.SnapshotTask(task.ID, "")
	if !ok {
		t.Fatal("任务应仍存在")
	}
	if snap.Status != "completed" {
		t.Fatalf("任务状态 = %q（error=%q），期望 completed", snap.Status, snap.Error)
	}
}
