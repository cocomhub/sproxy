// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/cloudfilename"
	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// cloudCreateDownload 处理 POST /api/cloud/download。
func (h *Handlers) cloudCreateDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req struct {
		URL      string `json:"url"`
		Filename string `json:"filename,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	cleanedURL, cleanedFilename, err := validateCloudDownloadURL(req.URL, req.Filename, h.cloudMgr.config.AllowPrivate)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	// 创建任务并启动下载。提交时文件大小未知（-1），SubmitAndStart 的同步条件
	// （totalSize > 0 且 < syncThreshold）不满足，因此恒异步执行：客户端断连后
	// 服务端继续异步下载，不阻塞 handler。
	// owner 由请求认证上下文派生（SproxySig→AK，api_keys→key 名，未认证→空串）。
	owner := ActorFrom(r.Context())
	task, err := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, r.Context(), owner)
	if err != nil {
		// 存储不足（ErrStorageFull）映射 507，其余视为 400（URL 等输入问题已提前拦截）
		if errors.Is(err, ErrStorageFull) {
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
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := h.cloudMgr.config.MaxBatchURLs; len(req.URLs) > maxBatch {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per batch", maxBatch)}, http.StatusBadRequest)
		return
	}

	results := make([]CloudBatchTaskResult, 0, len(req.URLs))
	owner := ActorFrom(r.Context())
	for _, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.config.AllowPrivate)
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

		// 批量始终异步：nil context
		task, taskErr := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, nil, owner)
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
		if strings.Contains(err.Error(), "not found") {
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	if err := h.cloudMgr.ResumeTask(id, req.Force, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
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
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if maxBatch := h.cloudMgr.config.MaxBatchURLs; len(req.URLs) > maxBatch {
		sendJSONResponse(w, map[string]string{"error": fmt.Sprintf("maximum %d URLs per group", maxBatch)}, http.StatusBadRequest)
		return
	}

	// 校验并规范化 URL：必须把规范化后的 URL/Filename 传给 CreateGroup，与单条/
	// 批量路径保持一致——否则同一内容的不同拼写（如 http://host/a 与 http://host/a/）
	// 在单条路径会被去重、在组路径会生成两个下载，组内文件名冲突判定也基于未
	// 规范化的值，导致与 UI/CLI 本地预检偶发不一致。
	normalized := make([]cloudfilename.Entry, len(req.URLs))
	for i, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.config.AllowPrivate)
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
		if errors.Is(err, ErrStorageFull) {
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
		sendJSONResponse(w, map[string]string{"error": "group not found"}, http.StatusNotFound)
		return
	}
	h.cloudMgr.UpdateGroupStatus(id)
	group, ok := h.cloudMgr.GetGroup(id, owner)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "group not found"}, http.StatusNotFound)
		return
	}

	// 获取组详情时一并返回子任务（仅对请求者可见的子任务，IDOR 防护）
	h.cloudMgr.mu.RLock()
	var tasks []*CloudTask
	for _, tid := range group.TaskIDs {
		if t, exists := h.cloudMgr.tasks[tid]; exists && ownerVisible(t.Owner, owner) {
			c := *t
			tasks = append(tasks, &c)
		}
	}
	h.cloudMgr.mu.RUnlock()

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
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// cloudDeleteGroup 处理 DELETE /api/cloud/groups/{id}。
func (h *Handlers) cloudDeleteGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.DeleteGroup(id, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	if err := h.cloudMgr.ResumeGroup(id, req.Force, ActorFrom(r.Context())); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
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
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "group not found"}, http.StatusNotFound)
		return
	}

	// 解析请求体
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MiB
	var req CloudArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	// 确定归档文件名。默认使用 groupID 保证唯一性。
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("%s-%d.tar.gz", groupID, time.Now().Unix())
	}
	archiveName = filepath.Base(archiveName)
	if archiveName == "" || archiveName == "." || archiveName == ".." {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive name"}, http.StatusBadRequest)
		return
	}
	if !strings.HasSuffix(archiveName, ".tar.gz") {
		archiveName += ".tar.gz"
	}
	if len(archiveName) > 255 {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive name too long"}, http.StatusBadRequest)
		return
	}

	// 用户指定归档名时，校验同名文件是否已存在，存在则拒绝
	if req.ArchiveName != "" {
		if _, err := os.Stat(filepath.Join(h.cloudMgr.uploadsDir, archiveName)); err == nil {
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists: " + archiveName}, http.StatusConflict)
			return
		}
	}

	// 按子任务目录收集已完成文件（子任务文件实际保存在 .__cloud__/<taskID>/ 下）
	cloudDir := filepath.Join(h.cloudMgr.uploadsDir, cloudDirName)
	var groupFiles []fileWithRelPath
	var skippedTasks []string
	var totalSourceSize int64

	group, ok := h.cloudMgr.GetGroup(groupID, owner)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "group not found"}, http.StatusNotFound)
		return
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

		sourceDir := filepath.Join(cloudDir, task.ID)
		sourceFile := filepath.Join(sourceDir, task.Filename)
		// 校验目标文件位于任务子目录内（防御 task.Filename 含路径穿越）。
		// 与单任务/批量归档的校验基准（IsPathWithin(sourceDir)）对齐，更严格——
		// 仅校验落在 cloud 根目录内允许 task.Filename 带 ../ 逃出任务目录。
		if !IsPathWithin(sourceFile, sourceDir) {
			h.logger.Error("path traversal detected in group archive",
				"group_id", groupID, "task_id", taskID, "source_file", sourceFile)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		// 收集后、打包前文件可能被删除/替换，先确认存在并统计大小（用于总量限制与配额预估）
		if info, statErr := os.Stat(sourceFile); statErr != nil {
			h.logger.Warn("cloud group archive: skipping missing file",
				"group_id", groupID, "task_id", taskID, "source_file", sourceFile, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
		} else {
			totalSourceSize += info.Size()
		}
		relPath := filepath.ToSlash(filepath.Join(task.ID, task.Filename))
		groupFiles = append(groupFiles, fileWithRelPath{fullPath: sourceFile, relPath: relPath})
	}

	if len(groupFiles) == 0 {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: "no completed files to archive in group",
			SkippedCount: len(skippedTasks), SkippedTasks: skippedTasks,
		}, http.StatusBadRequest)
		return
	}

	// 确保输出目录存在
	archiveDir := filepath.Join(h.cloudMgr.uploadsDir, cloudArchiveDirName)
	if mkErr := os.MkdirAll(archiveDir, 0755); mkErr != nil {
		h.logger.Error("failed to create archive directory", "error", mkErr)
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "failed to create archive directory"}, http.StatusInternalServerError)
		return
	}
	outputPath := filepath.Join(archiveDir, archiveName)
	if !strings.HasPrefix(filepath.Clean(outputPath), filepath.Clean(archiveDir)+string(filepath.Separator)) {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "invalid archive path"}, http.StatusInternalServerError)
		return
	}

	// 打包前：总量限制 + 配额预留（与单任务/批量归档一致）
	if maxBytes := h.cloudArchiveMaxBytes(); maxBytes > 0 && totalSourceSize > maxBytes {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("archive exceeds cloud_archive_max_bytes: %d > %d", totalSourceSize, maxBytes),
		}, http.StatusBadRequest)
		return
	}
	pre := totalSourceSize + cloudArchiveReservePlaceholder
	if reserveErr := h.storageMgr.TryReserve(pre, CategoryCloud); reserveErr != nil {
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("insufficient storage: %v", reserveErr),
		}, http.StatusInsufficientStorage)
		return
	}

	// 多文件打包
	created := false
	logger := h.logger.With("archive", "group", "group_id", groupID)
	checksum, err := createMultiFileTarGz(groupFiles, outputPath, logger, &created)
	if err != nil {
		if !created {
			// O_EXCL：同名归档已存在，释放已预留的配额避免泄漏
			h.storageMgr.Release(pre, CategoryCloud)
			sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "archive file already exists"}, http.StatusConflict)
			return
		}
		h.logger.Error("failed to create group archive", "group_id", groupID, "error", err)
		_ = os.Remove(outputPath)
		h.storageMgr.Release(pre, CategoryCloud)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
	}

	// 按磁盘实际大小对账预留配额：释放预占后按实际 size 重新预留，账本收敛到 actual。
	// 注意不能只释放 pre 而不补留——压缩后 actual 通常远小于 pre，若账本净为 0 会少计
	// actual 字节（配额窗口被放大，直到 30min 扫描校准）。
	h.storageMgr.Release(pre, CategoryCloud)
	if actual, statErr := os.Stat(outputPath); statErr == nil {
		if rErr := h.storageMgr.TryReserve(actual.Size(), CategoryCloud); rErr != nil {
			h.logger.Error("storage full, removing archive to keep ledger consistent", "group_id", groupID, "error", rErr)
			_ = os.Remove(outputPath)
			sendJSONResponse(w, CloudArchiveResult{
				Success: false, Message: fmt.Sprintf("insufficient storage for archive: %v", rErr),
			}, http.StatusInsufficientStorage)
			return
		}
	}

	info, _ := os.Stat(outputPath)
	size := int64(0)
	if info != nil {
		size = info.Size()
	}

	// 更新组归档路径（落库到真实组对象）
	archiveFile := filepath.ToSlash(filepath.Join(cloudArchiveDirName, archiveName))
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
