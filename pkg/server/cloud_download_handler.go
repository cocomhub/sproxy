// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/server/downloader"
)

// cloudCreateDownload 处理 POST /api/cloud/download。
func (h *Handlers) cloudCreateDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL      string `json:"url"`
		Filename string `json:"filename,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}

	cleanedURL, cleanedFilename, err := validateCloudDownloadURL(req.URL, req.Filename)
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
	snapshot, _ := h.cloudMgr.SnapshotTask(task.ID)
	sendJSONResponse(w, snapshot, http.StatusOK)
}

// validateCloudDownloadURL 校验下载 URL 和可选的文件名。
// 执行 scheme 检查、SSRF 防护、文件名提取和路径穿越防护。
// 返回 (cleanedURL, cleanedFilename, error)。
func validateCloudDownloadURL(rawURL, rawFilename string) (string, string, error) {
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
	// SSRF 深层防护：检查 host 不解析到内部 IP
	if hostErr := downloader.ValidateURLHost(rawURL); hostErr != nil {
		return "", "", fmt.Errorf("unsafe URL: %w", hostErr)
	}

	filename := rawFilename
	if filename == "" {
		filename = extractFilename(rawURL)
	}
	// 路径穿越防护：清理文件名中的路径分隔符
	filename = filepathSafe(filename)

	return rawURL, filename, nil
}

// cloudCreateBatchDownload 处理 POST /api/cloud/download/batch。
// 批量创建下载任务，始终异步执行。部分失败不中断，每项返回独立结果。
func (h *Handlers) cloudCreateBatchDownload(w http.ResponseWriter, r *http.Request) {
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
		cleanedURL, cleanedFilename, err := validateCloudDownloadURL(entry.URL, entry.Filename)
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
		snapshot, _ := h.cloudMgr.SnapshotTask(task.ID)
		results = append(results, CloudBatchTaskResult{
			ID:       snapshot.ID,
			URL:      cleanedURL,
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
	task, ok := h.cloudMgr.GetTask(id)
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
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusBadRequest)
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

// extractFilename 从 URL 中提取文件名。
func extractFilename(rawURL string) string {
	for i := len(rawURL) - 1; i >= 0; i-- {
		if rawURL[i] == '/' {
			name := rawURL[i+1:]
			for j := 0; j < len(name); j++ {
				if name[j] == '?' || name[j] == '#' {
					name = name[:j]
					break
				}
			}
			if name != "" {
				return name
			}
			break
		}
	}
	return "download"
}

// filepathSafe 清理文件名中的路径分隔符，防止路径穿越。
func filepathSafe(name string) string {
	name = strings.NewReplacer("\\", "_", "/", "_").Replace(name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "download"
	}
	return name
}
