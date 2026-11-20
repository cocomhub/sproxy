// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"net/http"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

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
	job := h.orphanAuditJobFor(name)
	if job == nil {
		sendJSONResponse(w, map[string]any{"success": false, "message": "审计任务未装配"}, http.StatusServiceUnavailable)
		return
	}
	refresh := r.URL.Query().Get("refresh") == "1"
	st := job.snapshot()
	if refresh || (!st.running && st.startedAt.IsZero()) {
		job.start("孤儿审计", func(ctx context.Context) (syncpkg.OrphanReport, error) {
			return a.OrphanChunks(ctx)
		}, h.logger)
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
		"report":     st.result}, http.StatusOK)
}

// jobFor 取（或惰性创建）某 key 的后台审计任务。同一进程内按 key **单飞**。
func (h *Handlers) jobFor(key string, mk func() any) any {
	h.auditJobsMu.Lock()
	defer h.auditJobsMu.Unlock()
	if h.auditJobs == nil {
		h.auditJobs = map[string]any{}
	}
	j, ok := h.auditJobs[key]
	if !ok {
		j = mk()
		h.auditJobs[key] = j
	}
	return j
}

// orphanAuditJobFor 取某卷的孤儿审计任务（同卷单飞、结果缓存）。
func (h *Handlers) orphanAuditJobFor(name string) *orphanAuditJob {
	j, _ := h.jobFor(name+"|orphans", func() any { return &orphanAuditJob{} }).(*orphanAuditJob)
	return j
}

// metaAuditJobFor 取某卷的元数据审计任务（同卷单飞、结果缓存）。
func (h *Handlers) metaAuditJobFor(name string) *metaAuditJob {
	j, _ := h.jobFor(name+"|meta", func() any { return &metaAuditJob{} }).(*metaAuditJob)
	return j
}
