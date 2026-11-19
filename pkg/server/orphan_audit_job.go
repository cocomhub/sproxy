// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// orphanAuditJob 是一次**后台**孤儿审计任务的状态。
//
// 为什么必须异步（2026-11-20 生产实测）：审计要对每个容器列举、并读取/解密每个 file meta，
// 生产卷约 1800 次**外部 CLI 往返**（BaiduPCS-Go 每次 1–3 s）⇒ 同步 HTTP 请求必然超过客户端超时
// （实测 9.5 min 仍未完成、客户端 `timeout: 1800` 报 context deadline exceeded）。
//
// 语义：同一卷**单飞**（同一时刻只跑一个任务）；结果缓存到下次手动刷新（`refresh=1`）。
type orphanAuditJob struct {
	mu        sync.Mutex
	running   bool
	startedAt time.Time
	endedAt   time.Time
	report    syncpkg.OrphanReport
	err       string
	// done 在本次任务结束时关闭（供调用方/测试等待，无需轮询睡眠）。
	done chan struct{}
}

// snapshot 是任务状态的只读快照（供 HTTP 渲染）。
type orphanAuditState struct {
	running   bool
	startedAt time.Time
	endedAt   time.Time
	report    syncpkg.OrphanReport
	err       string
}

func (j *orphanAuditJob) snapshot() orphanAuditState {
	j.mu.Lock()
	defer j.mu.Unlock()
	return orphanAuditState{running: j.running, startedAt: j.startedAt, endedAt: j.endedAt, report: j.report, err: j.err}
}

// start 在后台启动审计（已在跑 ⇒ 直接返回当前状态，不重复启动）。
func (j *orphanAuditJob) start(a syncpkg.OrphanAuditor, logger *slog.Logger) {
	j.mu.Lock()
	if j.running {
		j.mu.Unlock()
		return
	}
	j.running = true
	j.done = make(chan struct{})
	j.startedAt = time.Now()
	j.endedAt = time.Time{}
	j.err = ""
	j.mu.Unlock()

	go func() {
		// 后台任务用独立 ctx：客户端断开不得中断审计（否则永远拿不到结果）。
		ctx, cancel := context.WithTimeout(context.Background(), orphanAuditJobTimeout)
		defer cancel()
		started := time.Now()
		rep, err := a.OrphanChunks(ctx)
		elapsed := time.Since(started)
		j.mu.Lock()
		j.running = false
		j.endedAt = time.Now()
		if err != nil {
			j.err = err.Error()
		} else {
			j.report = rep
		}
		close(j.done)
		j.mu.Unlock()
		if logger != nil {
			if err != nil {
				logger.Warn("孤儿审计失败", "elapsed", elapsed.String(), "err", err)
				return
			}
			logger.Info("孤儿审计完成", "elapsed", elapsed.String(),
				"containers", rep.Containers, "orphans", len(rep.Chunks))
		}
	}()
}

// waitDone 等待本次任务结束（超时返回 false）。不轮询、不睡眠。
func (j *orphanAuditJob) waitDone(timeout time.Duration) bool {
	j.mu.Lock()
	done := j.done
	j.mu.Unlock()
	if done == nil {
		return true
	}
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}

// orphanAuditJobTimeout 是后台审计的总时限（覆盖 1800+ 次外部往返；超时即失败而非无限挂起）。
const orphanAuditJobTimeout = 2 * time.Hour

// orphansHandler 是 GET /api/volumes/{name}/orphans 的实体。
//
// 行为（2026-11-20 起异步）：
//   - 首次调用 / `?refresh=1`：启动后台任务，返回 **202** `{state:"running"}`；
//   - 任务进行中：202 `{state:"running", started_at}`；
//   - 任务完成：**200** `{success:true, report:{...}}`；
//   - 任务失败：**503** `{success:false, message}`（可用 `refresh=1` 重试）；
//   - 卷不支持该能力：**501**（不得当作"无孤儿"✗）。
func (h *Handlers) orphansHandler(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name == "" || h.volSet == nil {
		sendJSONResponse(w, map[string]any{"success": false, "message": "缺少卷名"}, http.StatusBadRequest)
		return
	}
	fsys, ok := h.volSet.VolumeView(name, registry.ViewUser)
	if !ok || fsys == nil {
		sendJSONResponse(w, map[string]any{"success": false, "message": "卷不存在或视图不可用"}, http.StatusNotFound)
		return
	}
	a := syncpkg.AssertOrphanAuditor(fsys)
	if a == nil {
		sendJSONResponse(w, map[string]any{"success": false,
			"message": "该卷不支持孤儿审计（不支持，非无孤儿）"}, http.StatusNotImplemented)
		return
	}
	job := h.orphanJob(name)
	if job == nil {
		sendJSONResponse(w, map[string]any{"success": false, "message": "审计任务未装配"}, http.StatusServiceUnavailable)
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	st := job.snapshot()
	if refresh || (!st.running && st.startedAt.IsZero()) {
		job.start(a, h.logger)
		st = job.snapshot()
	}
	if st.running {
		sendJSONResponse(w, map[string]any{"success": true, "state": "running",
			"started_at": st.startedAt.UTC().Format(time.RFC3339)}, http.StatusAccepted)
		return
	}
	if st.err != "" {
		sendJSONResponse(w, map[string]any{"success": false, "state": "failed", "message": st.err},
			http.StatusServiceUnavailable)
		return
	}
	sendJSONResponse(w, map[string]any{"success": true, "state": "done",
		"started_at": st.startedAt.UTC().Format(time.RFC3339),
		"ended_at":   st.endedAt.UTC().Format(time.RFC3339),
		"report":     st.report}, http.StatusOK)
}

// orphanJob 取（或惰性创建）某卷的审计任务状态。同一进程内按卷**单飞**。
func (h *Handlers) orphanJob(name string) *orphanAuditJob {
	h.orphanJobsMu.Lock()
	defer h.orphanJobsMu.Unlock()
	if h.orphanJobs == nil {
		h.orphanJobs = map[string]*orphanAuditJob{}
	}
	j, ok := h.orphanJobs[name]
	if !ok {
		j = &orphanAuditJob{}
		h.orphanJobs[name] = j
	}
	return j
}
