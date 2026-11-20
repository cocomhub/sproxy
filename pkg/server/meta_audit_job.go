// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// metaAuditHandler 是 GET /api/volumes/{name}/meta-audit 的实体。
//
// 只读审计元数据层（file/dir meta 可解密性、墓碑数、存活 meta 引用的分块是否齐全），
// 用于**跑 GC 清理墓碑前**确认元数据健康。异步语义与孤儿审计一致：
// 首次/`?refresh=1` ⇒ 202 running；完成 ⇒ 200 + report；失败 ⇒ 503；不支持 ⇒ 501。
func (h *Handlers) metaAuditHandler(w http.ResponseWriter, r *http.Request) {
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
	a := syncpkg.AssertMetaAuditor(fsys)
	if a == nil {
		sendJSONResponse(w, map[string]any{"success": false,
			"message": "该卷不支持元数据审计（不支持，非无异常）"}, http.StatusNotImplemented)
		return
	}
	job := h.metaAuditJobFor(name)
	refresh := r.URL.Query().Get("refresh") == "1"
	st := job.snapshot()
	if refresh || (!st.running && st.startedAt.IsZero()) {
		job.start("元数据审计", a.AuditMeta, h.logger)
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
