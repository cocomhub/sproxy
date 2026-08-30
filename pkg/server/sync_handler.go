// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
)

// syncNotConfigured 是 SyncManager 未装配时返回的响应。
// 服务端未配置 sync（sync.max_concurrent 或 sync_remotes）时，创建/查询同步任务应明确提示。
func (h *Handlers) syncNotConfigured(w http.ResponseWriter) {
	sendJSONResponse(w, map[string]string{"error": "sync not configured"}, http.StatusBadRequest)
}

// syncQuotaAdapter 把 StorageManager 适配为 syncmgr.QuotaStore（StorageCategory ↔ int）。
type syncQuotaAdapter struct {
	sm *StorageManager
}

func (a syncQuotaAdapter) TryReserve(size int64, cat int) error {
	return a.sm.TryReserve(size, StorageCategory(cat))
}

func (a syncQuotaAdapter) Release(size int64, cat int) {
	a.sm.Release(size, StorageCategory(cat))
}

func (a syncQuotaAdapter) Usage() int64 { return a.sm.Usage() }

func (a syncQuotaAdapter) MaxBytes() int64 { return a.sm.MaxBytes() }

// SyncQuotaStore 返回适配为 syncmgr.QuotaStore 的存储配额接口（供 cmd/sproxy 装配 SyncManager）。
func (h *Handlers) SyncQuotaStore() syncmgr.QuotaStore {
	return syncQuotaAdapter{sm: h.storageMgr}
}

// syncCreateTask 处理 POST /api/sync/tasks（创建并启动同步任务）。
func (h *Handlers) syncCreateTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req syncmgr.CreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	task, isNew, err := h.syncMgr.SubmitAndStart(req)
	if err != nil {
		// 存储不足（pull 方向预留失败）映射 507；其余（输入校验/remote 缺失等）400
		if errors.Is(err, syncmgr.ErrStorageFull) || errors.Is(err, ErrStorageFull) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	snapshot := h.syncMgr.Get(task.ID)
	if snapshot == nil {
		sendJSONResponse(w, map[string]string{"error": "task created but not found"}, http.StatusInternalServerError)
		return
	}
	// 新建 201，去重复用既有活跃任务 200（审查 M-8）
	status := http.StatusCreated
	if !isNew {
		status = http.StatusOK
	}
	sendJSONResponse(w, snapshot, status)
}

// syncListTasks 处理 GET /api/sync/tasks。
func (h *Handlers) syncListTasks(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	tasks := h.syncMgr.List()
	sendJSONResponse(w, map[string]any{"success": true, "tasks": tasks}, http.StatusOK)
}

// syncGetTask 处理 GET /api/sync/tasks/{id}。
func (h *Handlers) syncGetTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	task := h.syncMgr.Get(id)
	if task == nil {
		sendJSONResponse(w, map[string]string{"error": "task not found"}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, task, http.StatusOK)
}

// syncCancelTask 处理 POST /api/sync/tasks/{id}/cancel。
func (h *Handlers) syncCancelTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	if err := h.syncMgr.CancelTask(id); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, syncmgr.ErrNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// syncDeleteTask 处理 DELETE /api/sync/tasks/{id}。
func (h *Handlers) syncDeleteTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	if err := h.syncMgr.DeleteTask(id); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "deleted"}, http.StatusOK)
}
