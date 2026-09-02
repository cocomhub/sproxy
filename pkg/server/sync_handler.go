// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/server/syncmgr"
)

// syncNotConfigured 是 SyncManager 未装配时返回的响应。
// 服务端未配置 sync（sync.max_concurrent 或 sync_remotes）时，创建/查询同步任务应明确提示。
func (h *Handlers) syncNotConfigured(w http.ResponseWriter) {
	sendJSONResponse(w, map[string]string{"error": "sync not configured"}, http.StatusBadRequest)
}

// syncQuotaAdapter 把 StorageManager 适配为 syncmgr.QuotaStore（StorageCategory ↔ int）。
// 仅作 fallback：quotaBucketFor 返回 nil（globalPool 未装配）时回退全局账本（旧行为）。
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

// scopeQuotaAdapter 把 *quota.Scope 适配为 syncmgr.QuotaStore（per-owner user 桶配额）。
// syncmgr 的 TryReserve/Release 是"单计数器"语义（预留即落地、释放即扣减）：
// TryReserve → scope.TryReserve + 立即 Commit（net committed += size、reserved 归零）；
// Release → scope.ReleaseUsage(size)（committed -= size）。沿父链聚合到租户上限与 globalPool，
// 使 owner_quotas 对 sync pull 生效（I3 修复：原 syncQuotaAdapter 只受全局 max_storage_bytes 约束）。
type scopeQuotaAdapter struct {
	scope *quota.Scope
}

func (a scopeQuotaAdapter) TryReserve(size int64, _ int) error {
	res, err := a.scope.TryReserve(size)
	if err != nil {
		return err
	}
	res.Commit(size)
	return nil
}

func (a scopeQuotaAdapter) Release(size int64, _ int) {
	a.scope.ReleaseUsage(size)
}

func (a scopeQuotaAdapter) Usage() int64 { return a.scope.Usage() }

func (a scopeQuotaAdapter) MaxBytes() int64 { return a.scope.MaxBytes() }

// SyncQuotaStore 返回按任务 owner 解析的配额存储解析器（P4/P5：sync pull 按 owner 在 user
// 桶 Scope 上预留/对账，使 owner_quotas 对同步生效）。未装配 quota（scope 不可用）时回退
// 全局 storageMgr 适配器（旧行为）。
func (h *Handlers) SyncQuotaStore() func(owner string) syncmgr.QuotaStore {
	return func(owner string) syncmgr.QuotaStore {
		if scope := h.quotaBucketFor(owner, "user"); scope != nil {
			return scopeQuotaAdapter{scope: scope}
		}
		return syncQuotaAdapter{sm: h.storageMgr}
	}
}

// syncTenantRoot 按任务 owner 解析租户 user 根与 meta/sync 持久化目录绝对路径。
// 空 owner → anonymous 租户；租户不可用（非法 owner / 存储根未装配）返回 ok=false
// （写路径 fail-closed，绝不回落全局根）。装配层与 syncmgr.TenantRootResolver 对接，
// 同步 src/dst 相对租户 user 根解析（<root>/<tenant>/user），任务状态落 meta/sync。
func (h *Handlers) syncTenantRoot(owner string) (userRootAbs, persistDirAbs string, ok bool) {
	tnt := h.tenantFor(owner)
	if tnt == nil {
		return "", "", false
	}
	userAbs, ok1 := tnt.Root().Abs(tnt.UserRoot())
	if !ok1 {
		return "", "", false
	}
	metaAbs, ok2 := tnt.Root().Abs("meta/sync")
	if !ok2 {
		return "", "", false
	}
	return userAbs, metaAbs, true
}

// SyncTenantResolver 返回 syncmgr.TenantRootResolver（按任务 owner 解析租户 user/meta 根，
// 供 SyncManager 与 syncexec.Executor 装配）。
func (h *Handlers) SyncTenantResolver() syncmgr.TenantRootResolver { return h.syncTenantRoot }

// SyncTenantList 返回租户名列表函数（磁盘扫描，供 SyncManager 恢复遍历全部租户的 meta/sync）。
func (h *Handlers) SyncTenantList() func() []string { return h.listTenantIDs }

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
	// 多租户：owner 由请求认证上下文派生（SproxySig→AK，api_keys→key 名，未认证→空）。
	// CreateRequest.Owner 为 json:"-"，客户端 body 无法伪造。
	req.Owner = ActorFrom(r.Context())

	task, isNew, err := h.syncMgr.SubmitAndStart(req)
	if err != nil {
		// 存储不足映射 507（pull 占位预留已降级为按需，创建不再 507；此处兜底其余配额错误路径）；
		// 其余（输入校验/remote 缺失等）400
		if errors.Is(err, syncmgr.ErrStorageFull) || errors.Is(err, ErrStorageFull) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	snapshot := h.syncMgr.Get(task.ID, ActorFrom(r.Context()))
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
	tasks := h.syncMgr.List(ActorFrom(r.Context()))
	sendJSONResponse(w, map[string]any{"success": true, "tasks": tasks}, http.StatusOK)
}

// syncGetTask 处理 GET /api/sync/tasks/{id}。
func (h *Handlers) syncGetTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	task := h.syncMgr.Get(id, ActorFrom(r.Context()))
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
	if err := h.syncMgr.CancelTask(id, ActorFrom(r.Context())); err != nil {
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
	if err := h.syncMgr.DeleteTask(id, ActorFrom(r.Context())); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "deleted"}, http.StatusOK)
}
