// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloud"
	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/storage/capacity"
)

// saveOrDefault 解析客户端 save 参数（nil = 默认 true 保留，零回归）。
func saveOrDefault(s *bool) bool {
	if s == nil {
		return true
	}
	return *s
}

// isStorageFull 判断错误是否为存储配额超限（全局 storageMgr 账本或租户 quota.Scope）。
func isStorageFull(err error) bool {
	return errors.Is(err, capacity.ErrStorageFull) || errors.Is(err, quota.ErrStorageFull)
}

// checkTransferACL 校验转存目标卷对 owner 的 ACL（H2/R1：防跨租户覆写）。
// transfer 为 nil 或卷放行 → 返回 ""；卷未装配/ACL 拒绝 → 返回错误文案（调用方 403）。
func (h *Handlers) checkTransferACL(owner string, transfer *cloud.TransferSpec) string {
	if transfer == nil {
		return ""
	}
	if !h.volumeAllowedFor(owner, transfer.Volume) {
		return fmt.Sprintf("转存目标卷 %q 对当前用户不可用（ACL 拒绝）", transfer.Volume)
	}
	return ""
}

// cloudCreateDownload 处理 POST /api/cloud/download。
func (h *Handlers) cloudCreateDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req struct {
		URL      string `json:"url"`
		Filename string `json:"filename,omitempty"`
		// Transfer 是转存目标（可选）：下载完成后把产物转存到指定卷。
		Transfer *cloud.TransferSpec `json:"transfer,omitempty"`
		// Save 是否保留 cloud 桶副本（服务端化 keep-files；默认 true 零回归）。
		// false = 任务完成（含转存）后服务端自动删除 cloud 桶文件——客户端异常
		// 也不残留，清理状态记入 CleanupStatus 供审计。
		Save *bool `json:"save,omitempty"`
		// DownloadLocal 客户端是否下载本地（链式拉取 cloud 桶文件）。false = 服务端
		// 可转存后即删。创建期真空洞校验（不下载+不转存+不保留）以此消歧。
		DownloadLocal bool `json:"download_local,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	cleanedURL, cleanedFilename, err := validateCloudDownloadURL(req.URL, req.Filename, h.cloudMgr.AllowPrivate())
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}
	// 真空洞校验（不下载本地 + 无 transfer + save=false → 无任何产出）由 CreateTask
	// fail-closed 兜底；此处无需预检（download_local 消歧后语义完整）。
	// 转存目标卷 ACL 校验（H2/R1：防跨租户覆写）：owner 必须被目标卷 ACL 放行。
	if errMsg := h.checkTransferACL(ActorFrom(r.Context()), req.Transfer); errMsg != "" {
		sendJSONResponse(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	// 创建任务并启动下载。提交时文件大小未知（-1），SubmitAndStart 的同步条件
	// （totalSize > 0 且 < syncThreshold）不满足，因此恒异步执行：客户端断连后
	// 服务端继续异步下载，不阻塞 handler。
	// owner 由请求认证上下文派生（SproxySig→AK，api_keys→key 名，未认证→空串）。
	owner := ActorFrom(r.Context())
	task, err := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, r.Context(), owner, req.Transfer, req.DownloadLocal, saveOrDefault(req.Save))
	if err != nil {
		// 存储不足（storageMgr 全局账本或租户 Scope）映射 507，其余视为 400（URL 等输入问题已提前拦截）
		if isStorageFull(err) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	// 返回任务快照（避免并发修改 data race）
	snapshot, ok := h.cloudMgr.SnapshotTask(task.ID, owner)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "task created but not found"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, snapshot, http.StatusOK)
}

// validateCloudDownloadURL 校验下载 URL 和可选的文件名。
// 执行 scheme 检查、可选 SSRF 防护、文件名提取和路径穿越防护。
// 返回 (cleanedURL, cleanedFilename, error)。
func validateCloudDownloadURL(rawURL, rawFilename string, allowPrivate bool) (string, string, error) {
	entry := cloudfilename.Entry{URL: rawURL, Filename: rawFilename}
	if err := cloudfilename.ValidateEntry(entry); err != nil {
		return "", "", err
	}
	// SSRF 深层防护：检查 host 不解析到内部 IP（除非 allowPrivate）
	if !allowPrivate {
		if hostErr := downloader.ValidateURLHost(rawURL); hostErr != nil {
			return "", "", fmt.Errorf("unsafe URL: %w", hostErr)
		}
	}
	fn, err := cloudfilename.ResolveFilename(entry)
	if err != nil {
		return "", "", err
	}
	parsed, _ := url.Parse(rawURL)
	return parsed.String(), fn, nil
}

// cloudCreateBatchDownload 处理 POST /api/cloud/download/batch。
// 批量创建下载任务，始终异步执行。部分失败不中断，每项返回独立结果。
func (h *Handlers) cloudCreateBatchDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req struct {
		URLs []cloudfilename.Entry `json:"urls"`
		// Transfer 批量转存目标（apply 到每个条目；逐条目可经 Entry 扩展覆盖）。
		Transfer *cloud.TransferSpec `json:"transfer,omitempty"`
		// Save 批量保存 cloud 桶副本（同单条语义；nil = 默认 true）。
		Save *bool `json:"save,omitempty"`
		// DownloadLocal 批量：客户端是否下载本地（同单条语义）。
		DownloadLocal bool `json:"download_local,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := h.cloudMgr.MaxBatchURLs(); len(req.URLs) > maxBatch {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per batch", maxBatch)}, http.StatusBadRequest)
		return
	}

	// 批量转存目标卷 ACL 统一校验（R1：此前 batch 路径漏检，CLI 链式主路径可跨租户转存）。
	if errMsg := h.checkTransferACL(ActorFrom(r.Context()), req.Transfer); errMsg != "" {
		sendJSONResponse(w, map[string]string{"error": errMsg}, http.StatusForbidden)
		return
	}

	results := make([]CloudBatchTaskResult, 0, len(req.URLs))
	owner := ActorFrom(r.Context())
	for _, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.AllowPrivate())
		if err != nil {
			// 校验失败阶段：返回用户原始 URL/Filename（此时尚无规范化值）
			results = append(results, CloudBatchTaskResult{
				URL:      entry.URL,
				Filename: entry.Filename,
				Status:   "failed",
				Error:    err.Error(),
			})
			continue
		}

		// 批量始终异步：nil context。transfer/save/download_local 批量统一 apply；
		// 真空洞（不下载+无 transfer+save=false）由 CreateTask fail-closed 拒绝。
		task, taskErr := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, nil, owner, req.Transfer, req.DownloadLocal, saveOrDefault(req.Save))
		if taskErr != nil {
			results = append(results, CloudBatchTaskResult{
				URL:      cleanedURL,
				Filename: cleanedFilename,
				Status:   "failed",
				Error:    taskErr.Error(),
			})
			continue
		}
		// 使用快照避免并发读写 data race
		snapshot, ok := h.cloudMgr.SnapshotTask(task.ID, owner)
		if !ok {
			results = append(results, CloudBatchTaskResult{
				URL:      cleanedURL,
				Filename: cleanedFilename,
				Status:   "failed",
				Error:    "task created but not found",
			})
			continue
		}
		// 成功项 URL 使用规范化值，与 GET /api/cloud/tasks/{id} 的详情一致
		results = append(results, CloudBatchTaskResult{
			ID:       snapshot.ID,
			Owner:    snapshot.Owner,
			URL:      cleanedURL,
			Filename: cleanedFilename,
			Status:   snapshot.Status,
		})
	}

	sendJSONResponse(w, map[string][]CloudBatchTaskResult{"tasks": results}, http.StatusOK)
}

// parseOffsetLimit 解析 ?offset=&limit= 查询参数。
// 解析失败（缺失或非整数）返回默认值：offset=-1（不偏移）、limit=0（返回全部）。
func parseOffsetLimit(r *http.Request) (offset, limit int) {
	offset = -1
	limit = 0
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			offset = n
		}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	return offset, limit
}

// cloudListTasks 处理 GET /api/cloud/tasks。
// 返回 {tasks, total} 容器；total 为按 status 过滤后的任务总数（不受分页影响）。
func (h *Handlers) cloudListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	offset, limit := parseOffsetLimit(r)
	tasks, total := h.cloudMgr.ListTasks(status, offset, limit, ActorFrom(r.Context()))
	sendJSONResponse(w, map[string]any{"tasks": tasks, "total": total}, http.StatusOK)
}

// cloudGetTask 处理 GET /api/cloud/tasks/{id}。
func (h *Handlers) cloudGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := h.cloudMgr.SnapshotTask(id, ActorFrom(r.Context()))
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "task not found"}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, task, http.StatusOK)
}

// cloudCancelTask 处理 POST /api/cloud/tasks/{id}/cancel。
func (h *Handlers) cloudCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.CancelTask(id, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "cloud_cancel", ObjectType: "task", Object: id,
			Result: AuditResultError, Detail: err.Error(),
		})
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "cloud_cancel", ObjectType: "task", Object: id,
		Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// cloudDeleteTask 处理 DELETE /api/cloud/tasks/{id}。
func (h *Handlers) cloudDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.DeleteTask(id, ActorFrom(r.Context())); err != nil {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "cloud_delete", ObjectType: "task", Object: id,
			Result: AuditResultError, Detail: err.Error(),
		})
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
		return
	}
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "cloud_delete", ObjectType: "task", Object: id,
		Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// cloudResumeTask 处理 POST /api/cloud/tasks/{id}/resume。
func (h *Handlers) cloudResumeTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // 解析失败使用默认 false
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	if err := h.cloudMgr.ResumeTask(id, req.Force, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "resumed"}, http.StatusOK)
}

// cloudCreateGroup 处理 POST /api/cloud/groups。
func (h *Handlers) cloudCreateGroup(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB

	var req struct {
		Name string                `json:"name"`
		URLs []cloudfilename.Entry `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := h.cloudMgr.MaxBatchURLs(); len(req.URLs) > maxBatch {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per group", maxBatch)}, http.StatusBadRequest)
		return
	}

	// 校验并规范化 URL：必须把规范化后的 URL/Filename 传给 CreateGroup，与单条/
	// 批量路径保持一致——否则同一内容的不同拼写（如 http://host/a 与 http://host/a/）
	// 在单条路径会被去重、在组路径会生成两个下载，组内文件名冲突判定也基于未
	// 规范化的值，导致与 UI/CLI 本地预检偶发不一致。
	normalized := make([]cloudfilename.Entry, len(req.URLs))
	for i, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.AllowPrivate())
		if err != nil {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
		normalized[i] = cloudfilename.Entry{URL: cleanedURL, Filename: cleanedFilename}
	}

	group, err := h.cloudMgr.SubmitAndStartGroup(req.Name, normalized, ActorFrom(r.Context()))
	if err != nil {
		// 文件名冲突与重复 URL 均属客户端输入错误，映射 409 而非 500
		if strings.Contains(err.Error(), "filename conflict") ||
			strings.Contains(err.Error(), "duplicate URL") {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
			return
		}
		// 存储不足映射 507（与单条/批量路径一致）
		if isStorageFull(err) {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
			return
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	// 返回快照副本：SubmitAndStartGroup 返回的指针与 m.groups 共享，下载 goroutine
	// 可能在 json.Marshal 期间并发写组状态字段（UpdateGroupStatus），需副本隔离防 data race。
	snapshot, ok := h.cloudMgr.GetGroup(group.ID, ActorFrom(r.Context()))
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "group created but not found"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, snapshot, http.StatusOK)
}

// cloudGetGroup 处理 GET /api/cloud/groups/{id}。
func (h *Handlers) cloudGetGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := ActorFrom(r.Context())

	// 审查 Minor 2：先 GetGroup 校验 owner 可见性（跨 owner 立即 404，不对不可见资源
	// 执行 UpdateGroupStatus 写操作，消除计时侧信道），通过后再刷新状态并二次 GetGroup
	// 取最新快照。
	if _, ok := h.cloudMgr.GetGroup(id, owner); !ok {
		sendJSONResponse(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}
	h.cloudMgr.UpdateGroupStatus(id)
	group, ok := h.cloudMgr.GetGroup(id, owner)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 获取组详情时一并返回子任务（仅对请求者可见的子任务，IDOR 防护）。
	// 批量快照走领域 API（domain 侧一次持锁 + 同一 ownerVisible 规则），装配层不触碰
	// 领域内部状态。
	tasks := h.cloudMgr.SnapshotTasks(group.TaskIDs, owner)

	resp := map[string]any{
		"group": group,
		"tasks": tasks,
	}
	sendJSONResponse(w, resp, http.StatusOK)
}

// cloudListGroups 处理 GET /api/cloud/groups。
// 返回 {groups, total} 容器；total 为按 status 过滤后的组总数（不受分页影响）。
func (h *Handlers) cloudListGroups(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	offset, limit := parseOffsetLimit(r)
	owner := ActorFrom(r.Context())
	// 先刷新所有可见组的最新状态，再按 status 过滤返回。否则只刷新"当前已处于该状态"
	// 的组，刚转换到目标状态的组会被过滤查询漏掉，客户端看到的状态滞后。
	allGroups, _ := h.cloudMgr.ListGroups("", -1, 0, owner)
	for _, g := range allGroups {
		h.cloudMgr.UpdateGroupStatus(g.ID)
	}
	// total 需按同 status 过滤后的总数计算（ListGroups 内部过滤后统计）
	groups, total := h.cloudMgr.ListGroups(status, offset, limit, owner)
	sendJSONResponse(w, map[string]any{"groups": groups, "total": total}, http.StatusOK)
}

// cloudCancelGroup 处理 POST /api/cloud/groups/{id}/cancel。
func (h *Handlers) cloudCancelGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.CancelGroup(id, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// cloudDeleteGroup 处理 DELETE /api/cloud/groups/{id}。
// P5：组删除时联动清理组归档文件并释放 archive 桶 Scope（防孤儿归档虚高占用）。
func (h *Handlers) cloudDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	owner := ActorFrom(r.Context())
	// 先取组快照（含 ArchiveFile），DeleteGroup 后组对象已从 map 移除无法再查。
	var archiveFile string
	if g, ok := h.cloudMgr.GetGroup(id, owner); ok {
		archiveFile = g.ArchiveFile
	}
	if err := h.cloudMgr.DeleteGroup(id, owner); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	// 组归档文件（存的是归档名，如 "g1.tar.gz"）联动删除并释放 Scope。失败仅记日志
	// （组删除已完成，归档残留由周期扫描校准兜底；不把归档清理失败当作组删除失败）。
	if archiveFile != "" {
		if aErr := h.deleteCloudArchive(owner, archiveFile); aErr != nil {
			h.logger.Warn("删除组归档失败（残留由周期扫描校准）", "group_id", id, "archive", archiveFile, "error", aErr)
		}
	}
	sendJSONResponse(w, map[string]string{"status": "deleted"}, http.StatusOK)
}

// cloudResumeGroup 处理 POST /api/cloud/groups/{id}/resume。
func (h *Handlers) cloudResumeGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	if err := h.cloudMgr.ResumeGroup(id, req.Force, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), msgNotFound) {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "resumed"}, http.StatusOK)
}

// cloudArchiveGroup 处理 POST /api/cloud/groups/{id}/archive。
// 收集组内所有已完成子任务的文件打包为单个 tar.gz（未完成任务跳过并记录）。
func (h *Handlers) cloudArchiveGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	owner := ActorFrom(r.Context())
	if _, ok := h.cloudMgr.GetGroup(groupID, owner); !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return
	}

	// 解析请求体
	req, ok := h.cloudArchiveParseRequest(w, r)
	if !ok {
		return
	}

	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName, ok := h.cloudArchiveResolveName(w, req, groupID)
	if !ok {
		return
	}

	// 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户 cloud/ 桶）
	groupFiles, skippedTasks, totalSourceSize, ok := h.cloudArchiveCollectFiles(w, groupID, owner)
	if !ok {
		return
	}

	if len(groupFiles) == 0 {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "no completed files to archive in group",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return
	}

	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	root, rel, ok := h.cloudArchivePrepTarget(w, owner, req, archiveName)
	if !ok {
		return
	}

	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	res, pre, ok := h.cloudArchiveReserveQuota(w, owner, totalSourceSize)
	if !ok {
		return
	}

	// 多文件打包
	checksum, ok := h.cloudArchiveCreateTar(w, groupFiles, root, rel, res, pre, groupID)
	if !ok {
		return
	}

	// 按磁盘实际大小对账预留配额：scope 优先单步 Commit(actual)（多预留部分自动归还）；
	// storageMgr 回退保留 3 步（Release(pre)+TryReserve(actual)，全局账本 /stats 兼容）。
	if !h.cloudArchiveReconcileReservation(w, root, rel, res, pre, groupID) {
		return
	}

	info, _ := root.Stat(rel)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	// P5 归档占用登记（删除时按登记释放 Scope，不再依赖周期扫描自愈）。
	h.recordArchiveUsage(owner, archiveName, size)

	// 更新组归档路径（落库到真实组对象；仅存归档名，客户端不接触 .__ 内部路径）
	archiveFile := archiveName
	h.cloudMgr.SetGroupArchiveFile(groupID, archiveFile)

	sendJSONResponse(w, CloudArchiveResult{
		Success:      true,
		File:         archiveFile,
		Size:         size,
		Checksum:     checksum,
		TaskCount:    len(groupFiles),
		SkippedCount: len(skippedTasks),
		SkippedTasks: skippedTasks,
	}, http.StatusOK)
}

// cloudArchiveParseRequest 解析归档请求体（限 1 MiB + bodyValidator EOF 哈希校验）。
// 返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveParseRequest(w http.ResponseWriter, r *http.Request) (*CloudArchiveRequest, bool) {
	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req CloudArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgInvalidRequestBody}, http.StatusBadRequest)
		return nil, false
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return nil, false
	}
	return &req, true
}

// cloudArchiveResolveName 确定归档文件名（默认使用 groupID 保证唯一性；路径穿越防护 +
// 长度校验 + 补 .tar.gz 后缀）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveResolveName(w http.ResponseWriter, req *CloudArchiveRequest, groupID string) (string, bool) {
	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("%s-%d.tar.gz", groupID, time.Now().Unix())
	}
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive name"}, http.StatusBadRequest)
		return "", false
	}
	if !strings.HasSuffix(archiveName, tarGZExt) {
		archiveName += tarGZExt
	}
	if len(archiveName) > 255 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive name too long"}, http.StatusBadRequest)
		return "", false
	}
	return archiveName, true
}

// cloudArchiveCollectFiles 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户
// cloud/ 桶；未完成任务跳过并记录；返回 false 表示已回错误响应包）。
func (h *Handlers) cloudArchiveCollectFiles(w http.ResponseWriter, groupID, owner string) ([]fileWithRelPath, []string, int64, bool) {
	// 按子任务目录收集已完成文件（子任务文件按任务 owner 落租户 cloud/ 桶）
	var groupFiles []fileWithRelPath
	var skippedTasks []string
	var totalSourceSize int64

	group, ok := h.cloudMgr.GetGroup(groupID, owner)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgGroupNotFound}, http.StatusNotFound)
		return nil, nil, 0, false
	}
	for _, taskID := range group.TaskIDs {
		task, found := h.cloudMgr.SnapshotTask(taskID, owner)
		if !found {
			h.logger.Warn("cloud group archive: skipping task not found", "group_id", groupID, "task_id", taskID)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		if task.Status != "completed" {
			h.logger.Warn("cloud group archive: skipping task with non-completed status",
				"group_id", groupID, "task_id", taskID, "status", task.Status)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}

		// 源文件按任务 owner 租户 cloud/ 桶解析（根内 rel = cloud/<taskID>/<file>）。
		// FeatureRel 逐段校验（fail-closed，防 task.Filename 穿越任务目录）。
		srcTnt := h.tenantFor(task.Owner)
		if srcTnt == nil {
			h.logger.Warn("cloud group archive: skipping task with unavailable tenant",
				"group_id", groupID, "task_id", taskID, "owner", task.Owner)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		srcRel, relOK := srcTnt.FeatureRel("cloud", task.ID+"/"+task.Filename)
		if !relOK {
			h.logger.Warn("cloud group archive: skipping task with invalid source path",
				"group_id", groupID, "task_id", taskID, "file", task.Filename)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		// 收集后、打包前文件可能被删除/替换，先确认存在并统计大小（用于总量限制与配额预估）
		if info, statErr := srcTnt.Root().Stat(srcRel); statErr != nil {
			h.logger.Warn("cloud group archive: skipping missing file",
				"group_id", groupID, "task_id", taskID, "rel", srcRel, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
		} else {
			totalSourceSize += info.Size()
		}
		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		groupFiles = append(groupFiles, fileWithRelPath{root: srcTnt.Root(), rel: srcRel, tarRel: relPath})
	}
	return groupFiles, skippedTasks, totalSourceSize, true
}

// cloudArchivePrepTarget 确保输出目录存在并解析归档目标相对路径（租户 archive 桶），
// 含用户指定名时的同名冲突检查。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchivePrepTarget(w http.ResponseWriter, owner string, req *CloudArchiveRequest, archiveName string) (*storage.Root, string, bool) {
	// 确保输出目录存在（租户 archive 桶：<root>/<tenant>/archive/）
	tnt := h.tenantFor(owner)
	if tnt == nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	root := tnt.Root()
	if mkErr := root.MkdirAll("archive", 0755); mkErr != nil {
		h.logger.Error(msgArchiveDirFail, "error", mkErr)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: msgArchiveDirFail}, http.StatusInternalServerError)
		return nil, "", false
	}
	rel, ok := tnt.FeatureRel("archive", archiveName)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return nil, "", false
	}
	// 用户指定归档名时，校验同名文件是否已存在，存在则拒绝（落在租户 archive 桶下，
	// 审查 F3 语义保留：不查全局根，避免误 409）
	if req.ArchiveName != "" {
		if _, err := root.Stat(rel); err == nil {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists: " + archiveName}, http.StatusConflict)
			return nil, "", false
		}
	}
	return root, rel, true
}

// cloudArchiveReserveQuota 打包前做总量限制（cloud_archive_max_bytes）+ 配额预留
// （scope 优先单步；storageMgr 仅作回退）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveReserveQuota(w http.ResponseWriter, owner string, totalSourceSize int64) (*quota.Reservation, int64, bool) {
	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	if maxBytes := h.cloudArchiveMaxBytes(); maxBytes > 0 && totalSourceSize > maxBytes {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSourceSize, maxBytes),
		}, http.StatusBadRequest)
		return nil, 0, false
	}
	pre := totalSourceSize + cloudArchiveReservePlaceholder
	// P5 收敛：双轨 TryReserve（storageMgr + Scope 同时预留同量字节）改为二选一——
	// 生产环境 scope 恒非 nil（全局兜底由 Scope 父链生效），storageMgr 仅作回退。
	var res *quota.Reservation
	if scope := h.quotaBucketFor(owner, "archive"); scope != nil {
		rr, reserveErr := scope.TryReserve(pre)
		if reserveErr != nil {
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
			}, http.StatusInsufficientStorage)
			return nil, 0, false
		}
		res = rr
	} else if h.storageMgr != nil {
		if reserveErr := h.storageMgr.TryReserve(pre, capacity.CategoryCloud); reserveErr != nil {
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
			}, http.StatusInsufficientStorage)
			return nil, 0, false
		}
	}
	return res, pre, true
}

// cloudArchiveCreateTar 执行多文件打包（流式 checksum；O_EXCL 同名冲突与失败路径释放
// 预留并清理半成品）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveCreateTar(w http.ResponseWriter, groupFiles []fileWithRelPath, root *storage.Root, rel string, res *quota.Reservation, pre int64, groupID string) (string, bool) {
	// 多文件打包
	created := false
	logger := h.logger.With("archive", "group", "group_id", groupID)
	checksum, err := createMultiFileTarGz(groupFiles, root, rel, logger, &created)
	if err != nil {
		if !created {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏（与 TryReserve 二选一对称）
			releaseArchiveReservation(res, h.storageMgr, pre)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return "", false
		}
		h.logger.Error("failed to create group archive", "group_id", groupID, "error", err)
		_ = root.Remove(rel)
		releaseArchiveReservation(res, h.storageMgr, pre)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return "", false
	}
	return checksum, true
}

// cloudArchiveReconcileReservation 按磁盘实际大小对账预留配额：scope 优先单步 Commit(actual)
// （多预留部分自动归还）；storageMgr 回退保留 3 步（Release(pre)+TryReserve(actual)，全局账本
// /stats 兼容）。返回 false 表示已回错误响应包。
func (h *Handlers) cloudArchiveReconcileReservation(w http.ResponseWriter, root *storage.Root, rel string, res *quota.Reservation, pre int64, groupID string) bool {
	// 按磁盘实际大小对账预留配额：scope 优先单步 Commit(actual)（多预留部分自动归还）；
	// storageMgr 回退保留 3 步（Release(pre)+TryReserve(actual)，全局账本 /stats 兼容）。
	actual := int64(0)
	if res != nil {
		if info, statErr := root.Stat(rel); statErr == nil {
			actual = info.Size()
			res.Commit(actual)
			res = nil
		} else {
			// stat 失败（罕见）：释放预留避免永久挂账（归档文件已落盘，下次扫描校准全局账本）。
			res.Release()
			res = nil
		}
	} else if h.storageMgr != nil {
		h.storageMgr.Release(pre, capacity.CategoryCloud)
		if info, statErr := root.Stat(rel); statErr == nil {
			actual = info.Size()
			if rErr := h.storageMgr.TryReserve(actual, capacity.CategoryCloud); rErr != nil {
				h.logger.Error("storage full, removing archive to keep ledger consistent", "group_id", groupID, "error", rErr)
				_ = root.Remove(rel)
				sendJSONResponse(w, CloudArchiveResult{
					Success: false, Message: fmt.Sprintf("insufficient storage for archive: %v", rErr),
				}, http.StatusInsufficientStorage)
				return false
			}
		}
	}
	return true
}
