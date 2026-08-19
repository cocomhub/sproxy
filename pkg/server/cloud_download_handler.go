// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
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

	cleanedURL, cleanedFilename, err := validateCloudDownloadURL(req.URL, req.Filename, h.cloudMgr.config.AllowPrivate)
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
		return
	}

	// 创建任务并启动下载（同步模式使用 r.Context()）
	task, err := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, r.Context())
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
		return
	}

	// 返回任务快照（避免并发修改 data race）
	snapshot, ok := h.cloudMgr.SnapshotTask(task.ID)
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
	if rawURL == "" {
		return "", "", fmt.Errorf("url is required")
	}

	// SSRF 防护：校验 URL scheme 和 host
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", "", fmt.Errorf("only http/https URLs are allowed")
	}
	if parsed.Host == "" {
		return "", "", fmt.Errorf("invalid URL: missing host")
	}
	// SSRF 深层防护：检查 host 不解析到内部 IP（除非 allowPrivate）
	if !allowPrivate {
		if hostErr := downloader.ValidateURLHost(rawURL); hostErr != nil {
			return "", "", fmt.Errorf("unsafe URL: %w", hostErr)
		}
	}

	filename := rawFilename
	if filename == "" {
		filename = extractFilename(rawURL)
	}
	// 路径穿越防护：清理文件名中的路径分隔符
	filename = filepathSafe(filename)

	return parsed.String(), filename, nil
}

// cloudCreateBatchDownload 处理 POST /api/cloud/download/batch。
// 批量创建下载任务，始终异步执行。部分失败不中断，每项返回独立结果。
func (h *Handlers) cloudCreateBatchDownload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 限 1 MiB

	var req CloudBatchRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) > 100 {
		sendJSONResponse(w, map[string]string{"error": "maximum 100 URLs per batch"}, http.StatusBadRequest)
		return
	}

	results := make([]CloudBatchTaskResult, 0, len(req.URLs))
	for _, entry := range req.URLs {
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.config.AllowPrivate)
		if err != nil {
			results = append(results, CloudBatchTaskResult{
				URL:      entry.URL,
				Filename: entry.Filename,
				Status:   "failed",
				Error:    err.Error(),
			})
			continue
		}

		// 批量始终异步：nil context
		task, taskErr := h.cloudMgr.SubmitAndStart("url", cleanedURL, cleanedFilename, -1, nil)
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
		snapshot, ok := h.cloudMgr.SnapshotTask(task.ID)
		if !ok {
			results = append(results, CloudBatchTaskResult{
				URL:      cleanedURL,
				Filename: cleanedFilename,
				Status:   "failed",
				Error:    "task created but not found",
			})
			continue
		}
		results = append(results, CloudBatchTaskResult{
			ID:       snapshot.ID,
			URL:      entry.URL,
			Filename: cleanedFilename,
			Status:   snapshot.Status,
		})
	}

	sendJSONResponse(w, map[string][]CloudBatchTaskResult{"tasks": results}, http.StatusOK)
}

// cloudListTasks 处理 GET /api/cloud/tasks。
func (h *Handlers) cloudListTasks(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	tasks := h.cloudMgr.ListTasks(status)
	sendJSONResponse(w, tasks, http.StatusOK)
}

// cloudGetTask 处理 GET /api/cloud/tasks/{id}。
func (h *Handlers) cloudGetTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	task, ok := h.cloudMgr.SnapshotTask(id)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "task not found"}, http.StatusNotFound)
		return
	}
	sendJSONResponse(w, task, http.StatusOK)
}

// cloudCancelTask 处理 POST /api/cloud/tasks/{id}/cancel。
func (h *Handlers) cloudCancelTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.CancelTask(id); err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "not found") {
			status = http.StatusNotFound
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, status)
		return
	}
	sendJSONResponse(w, map[string]string{"status": "cancelled"}, http.StatusOK)
}

// cloudDeleteTask 处理 DELETE /api/cloud/tasks/{id}。
func (h *Handlers) cloudDeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.DeleteTask(id); err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusNotFound)
		return
	}
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

	if err := h.cloudMgr.ResumeTask(id, req.Force); err != nil {
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
		Name string          `json:"name"`
		URLs []CloudBatchURL `json:"urls"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) == 0 {
		sendJSONResponse(w, map[string]string{"error": "urls is required"}, http.StatusBadRequest)
		return
	}
	if len(req.URLs) > 100 {
		sendJSONResponse(w, map[string]string{"error": "maximum 100 URLs per group"}, http.StatusBadRequest)
		return
	}

	// 校验 URL 合法性
	for _, entry := range req.URLs {
		if _, _, err := validateCloudDownloadURL(entry.URL, entry.Filename, h.cloudMgr.config.AllowPrivate); err != nil {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
			return
		}
	}

	group, err := h.cloudMgr.SubmitAndStartGroup(req.Name, req.URLs)
	if err != nil {
		// 文件名冲突与重复 URL 均属客户端输入错误，映射 409 而非 500
		if strings.Contains(err.Error(), "filename conflict") ||
			strings.Contains(err.Error(), "duplicate URL") {
			sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusConflict)
			return
		}
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, group, http.StatusOK)
}

// cloudGetGroup 处理 GET /api/cloud/groups/{id}。
func (h *Handlers) cloudGetGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	// 先刷新组状态，再读取最新快照（避免返回更新前的副本）
	h.cloudMgr.UpdateGroupStatus(id)
	group, ok := h.cloudMgr.GetGroup(id)
	if !ok {
		sendJSONResponse(w, map[string]string{"error": "group not found"}, http.StatusNotFound)
		return
	}

	// 获取组详情时一并返回子任务
	h.cloudMgr.mu.RLock()
	var tasks []*CloudTask
	for _, tid := range group.TaskIDs {
		if t, exists := h.cloudMgr.tasks[tid]; exists {
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
func (h *Handlers) cloudListGroups(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("status")
	// 先更新每个组的状态，再取最新快照列表
	if status == "" {
		for _, g := range h.cloudMgr.ListGroups("") {
			h.cloudMgr.UpdateGroupStatus(g.ID)
		}
	} else {
		for _, g := range h.cloudMgr.ListGroups(status) {
			h.cloudMgr.UpdateGroupStatus(g.ID)
		}
	}
	groups := h.cloudMgr.ListGroups(status)
	sendJSONResponse(w, groups, http.StatusOK)
}

// cloudCancelGroup 处理 POST /api/cloud/groups/{id}/cancel。
func (h *Handlers) cloudCancelGroup(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := h.cloudMgr.CancelGroup(id); err != nil {
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
	if err := h.cloudMgr.DeleteGroup(id); err != nil {
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

	if err := h.cloudMgr.ResumeGroup(id, req.Force); err != nil {
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
	if _, ok := h.cloudMgr.GetGroup(groupID); !ok {
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

	// 确定归档文件名
	archiveName := req.ArchiveName
	if archiveName == "" {
		archiveName = fmt.Sprintf("group-%s-%d.tar.gz", groupID, time.Now().Unix())
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

	// 按子任务目录收集已完成文件（子任务文件实际保存在 .__cloud__/<taskID>/ 下）
	cloudDir := filepath.Join(h.cloudMgr.uploadsDir, cloudDirName)
	var groupFiles []fileWithRelPath
	var skippedTasks []string

	group, ok := h.cloudMgr.GetGroup(groupID)
	if !ok {
		sendJSONResponse(w, CloudArchiveResult{Success: false, Message: "group not found"}, http.StatusNotFound)
		return
	}
	for _, taskID := range group.TaskIDs {
		task, found := h.cloudMgr.SnapshotTask(taskID)
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

		sourceFile := filepath.Join(cloudDir, task.ID, task.Filename)
		// 校验目标文件位于 cloud 根目录内（防御 task.Filename 含路径穿越）
		if !IsPathWithin(sourceFile, cloudDir) {
			h.logger.Error("path traversal detected in group archive",
				"group_id", groupID, "task_id", taskID, "source_file", sourceFile)
			skippedTasks = append(skippedTasks, taskID)
			continue
		}
		// 收集后、打包前文件可能被删除/替换，先确认存在
		if _, statErr := os.Stat(sourceFile); statErr != nil {
			h.logger.Warn("cloud group archive: skipping missing file",
				"group_id", groupID, "task_id", taskID, "source_file", sourceFile, "error", statErr)
			skippedTasks = append(skippedTasks, taskID)
			continue
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

	// 多文件打包
	logger := h.logger.With("archive", "group", "group_id", groupID)
	checksum, err := createMultiFileTarGz(groupFiles, outputPath, logger)
	if err != nil {
		h.logger.Error("failed to create group archive", "group_id", groupID, "error", err)
		sendJSONResponse(w, CloudArchiveResult{
			Success: false, Message: fmt.Sprintf("failed to create archive: %v", err),
		}, http.StatusInternalServerError)
		return
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

// genDefaultFilename 从 URL 中提取默认文件名，遵循 wget 行为。
// 逻辑委托给共享包 pkg/cloudfilename，保证与客户端 (sclient / Web UI) 一致：
//   - 路径末尾为 / 时使用 "index.html"
//   - 查询参数（?后的 raw query）直接附加到文件名后
//   - 路径最后一段做百分号解码
//
// 返回的文件名未经 filepathSafe 处理，调用方应自行 sanitize。
func genDefaultFilename(rawURL string) string {
	return cloudfilename.DefaultFromURL(rawURL)
}

// extractFilename 从 URL 中提取文件名（保留向后兼容）。
// 新代码请使用 genDefaultFilename。
func extractFilename(rawURL string) string {
	return genDefaultFilename(rawURL)
}

// filepathSafe 清理文件名中的路径分隔符，防止路径穿越。
// 逻辑委托给共享包 pkg/cloudfilename，保证双端规则一致。
func filepathSafe(name string) string {
	return cloudfilename.Safe(name)
}
