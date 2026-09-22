// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
	"github.com/cocomhub/sproxy/pkg/syncmgr"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// syncNotConfigured 是 SyncManager 未装配时返回的响应。
// 服务端未配置 sync（sync.max_concurrent 或 sync_remotes）时，创建/查询同步任务应明确提示。
func (h *Handlers) syncNotConfigured(w http.ResponseWriter) {
	sendJSONResponse(w, map[string]string{"error": "sync not configured"}, http.StatusBadRequest)
}

// syncQuotaAdapter 把 StorageManager 适配为 syncmgr.QuotaStore（StorageCategory ↔ int）。
// 仅作 fallback：quotaBucketFor 返回 nil（globalPool 未装配）时回退全局账本（旧行为）。
type syncQuotaAdapter struct {
	sm *capacity.StorageManager
}

func (a syncQuotaAdapter) TryReserve(size int64, cat int) error {
	return a.sm.TryReserve(size, capacity.StorageCategory(cat))
}

func (a syncQuotaAdapter) Release(size int64, cat int) {
	a.sm.Release(size, capacity.StorageCategory(cat))
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

// SyncQuotaScope 返回按任务 owner 解析的 user 桶配额 *quota.Scope（供 syncexec.Executor
// 逐文件写前 guard 使用）。与服务端写路径同一引用（quotaBucketFor 缓存复用），未装配时 nil。
func (h *Handlers) SyncQuotaScope() func(owner string) *quota.Scope {
	return func(owner string) *quota.Scope {
		return h.quotaBucketFor(owner, "user")
	}
}

// SyncScopeFor 返回按 (owner, rel) 解析配额子 Scope 的解析器（供 syncexec.Executor 逐文件
// 写前 guard 按文件实际 rel 路由 bucket_limits 子目录配额；与服务端写路径 quotaScopeFor
// 同一引用，子目录配额对 sync pull 生效）。
func (h *Handlers) SyncScopeFor() func(owner, rel string) *quota.Scope {
	return h.quotaScopeFor
}

// syncTenantRoot 按任务 owner 解析租户 user 根与 meta/sync 持久化目录绝对路径。
// 空 owner → anonymous 租户；租户不可用（非法 owner / 存储根未装配）返回 ok=false
// （写路径 fail-closed，绝不回落全局根）。装配层与 syncmgr.TenantRootResolver 对接，
// 同步 src/dst 相对租户 user 根解析（<root>/<tenant>/user），任务状态落 meta/sync。
//
// 排除面边界（F4 review 成文）：sync pull 目标解析到**默认卷** user 根属默认卷 ACL 排除面的
// 设计内例外（服务端自有 meta/sync 桶 + 同步目标按 owner 逻辑树解析，非 T6b 门禁入口）。
// 见 volumes.go defaultVolumeAllows 边界注释。不改行为，仅记录避免未来误判为漏洞。
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

// Volumes 返回装配后的卷集合（registry.Set，含外部卷 external 句柄）。
// 装配层（cmd/sproxy）经此取 Set 供 baidupcs 工厂查询外部卷（Set.External(volume)）。
func (h *Handlers) Volumes() *registry.Set { return h.volSet }

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
		if errors.Is(err, syncmgr.ErrStorageFull) || errors.Is(err, capacity.ErrStorageFull) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		// 用户卷跨 owner（U4）：404 防枚举（与 ErrNotFound 同语义，不泄露卷是否存在）。
		if errors.Is(err, syncmgr.ErrUserVolumeNotOwned) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
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
// 扇出父任务（Remote 空 + 有 FanoutChildren）返回聚合视图（FanoutSummary：每 remote 子任务状态）。
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
	// 扇出父任务：返回聚合视图（子任务明细由 FanoutChildren 承载）。
	if task.Remote == "" {
		if children := h.syncMgr.FanoutChildren(task.ID); len(children) > 0 {
			sendJSONResponse(w, h.syncMgr.FanoutSummaryOf(task.ID), http.StatusOK)
			return
		}
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

// syncRetryTask 处理 POST /api/sync/tasks/{id}/retry（失败单文件重试）。
// 请求体 JSON：{"files": ["a.txt", "b.txt"]}（空 = 重试全部失败文件）。
// 响应：{retried: [{path, action, error}], skipped: [path...]}。跨 owner 404。
func (h *Handlers) syncRetryTask(w http.ResponseWriter, r *http.Request) {
	if h.syncMgr == nil {
		h.syncNotConfigured(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req struct {
		Files []string `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	id := r.PathValue("id")
	res, err := h.syncMgr.RetryFiles(r.Context(), id, ActorFrom(r.Context()), req.Files)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, syncmgr.ErrNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, res, http.StatusOK)
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

// syncConflictsNotConfigured 冲突索引未装配 → 400。
func (h *Handlers) syncConflictsNotConfigured(w http.ResponseWriter) {
	sendJSONResponse(w, map[string]string{"error": "sync conflicts not configured"}, http.StatusBadRequest)
}

// syncListConflicts 处理 GET /api/sync/conflicts——列出未解决冲突（时间升序）。
func (h *Handlers) syncListConflicts(w http.ResponseWriter, r *http.Request) {
	if h.conflictIndex == nil {
		h.syncConflictsNotConfigured(w)
		return
	}
	items := h.conflictIndex.List(ActorFrom(r.Context()))
	sendJSONResponse(w, map[string]any{"success": true, "conflicts": items}, http.StatusOK)
}

// syncGetConflict 处理 GET /api/sync/conflicts/{id}——单条（含已 resolved 历史）。
func (h *Handlers) syncGetConflict(w http.ResponseWriter, r *http.Request) {
	if h.conflictIndex == nil {
		h.syncConflictsNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	item, ok := h.conflictIndex.Get(id)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "conflict not found"}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, item, http.StatusOK)
}

// syncResolveConflict 处理 POST /api/sync/conflicts/{id}/resolve。
// body: {"choice":"ours|theirs|manual","content":"..."}。
// ours/theirs：把对应侧内容写回冲突文件路径（替换标记文件）；manual：写 content。
// 成功后条目标 resolved（不再列表出现）。
func (h *Handlers) syncResolveConflict(w http.ResponseWriter, r *http.Request) {
	if h.conflictIndex == nil {
		h.syncConflictsNotConfigured(w)
		return
	}
	id := r.PathValue("id")
	var body struct {
		Choice  string `json:"choice"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		sendJSONResponse(w, map[string]string{"error": "无效请求体"}, http.StatusBadRequest)
		return
	}
	item, ok := h.conflictIndex.Get(id)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "conflict not found"}, http.StatusNotFound)
		return
	}
	var content []byte
	switch body.Choice {
	case "ours", "theirs":
		// Resolve 内部取对应侧快照写回（替换标记文件）。
		got, rerr := h.conflictIndex.Resolve(id, body.Choice)
		if rerr != nil {
			sendJSONResponse(w, map[string]string{"error": rerr.Error()}, http.StatusBadRequest)
			return
		}
		content = got
	case "manual":
		if body.Content == "" {
			sendJSONResponse(w, map[string]string{"error": "manual 解决需显式 content"}, http.StatusBadRequest)
			return
		}
		content = []byte(body.Content)
		// 手动内容：直接写回文件 + 标 resolved（走 Resolve 的 ours 语义后覆盖——
		// 用 MarkResolved 语义；此处写文件后调用 index 的 manual 变体）。
		if merr := h.conflictIndex.ResolveManual(id, string(content)); merr != nil {
			sendJSONResponse(w, map[string]string{"error": merr.Error()}, http.StatusBadRequest)
			return
		}
	default:
		sendJSONResponse(w, map[string]string{"error": "choice 仅支持 ours|theirs|manual"}, http.StatusBadRequest)
		return
	}
	// 写回冲突文件路径（替换带标记文件）。Path 是同步相对路径——经 tenant root 解析落盘。
	if werr := h.writeConflictFile(item.Path, content); werr != nil {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("写回冲突文件失败: %v", werr)}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]any{"success": true, "path": item.Path}, http.StatusOK)
}

// writeConflictFile 把解决内容写回冲突文件（Path 相对 user 桶 → tenant user 根解析）。
func (h *Handlers) writeConflictFile(rel string, content []byte) error {
	// Path 形如 "user/dir/f.txt"（相对 user 桶）——tenantRoot 返回 user 根，拼 rel。
	userRoot, _, ok := h.syncTenantRoot("")
	if !ok || userRoot == "" {
		return fmt.Errorf("租户不可用")
	}
	clean, cerr := pathguard.ValidateFilePath(rel)
	if cerr != nil {
		return cerr
	}
	full := filepath.Join(userRoot, filepath.FromSlash(clean))
	if dir := filepath.Dir(full); dir != "" {
		if mkErr := os.MkdirAll(dir, 0o755); mkErr != nil {
			return mkErr
		}
	}
	return os.WriteFile(full, content, 0o644)
}
