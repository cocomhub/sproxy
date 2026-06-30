// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
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
	if req.URL == "" {
		sendJSONResponse(w, map[string]string{"error": "url is required"}, http.StatusBadRequest)
		return
	}

	// SSRF 防护：校验 URL scheme 和 host
	parsed, err := url.Parse(req.URL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		sendJSONResponse(w, map[string]string{"error": "only http/https URLs are allowed"}, http.StatusBadRequest)
		return
	}
	if parsed.Host == "" {
		sendJSONResponse(w, map[string]string{"error": "invalid URL: missing host"}, http.StatusBadRequest)
		return
	}
	// SSRF 深层防护：检查 host 不解析到内部 IP
	if hostErr := downloader.ValidateURLHost(req.URL); hostErr != nil {
		sendJSONResponse(w, map[string]string{"error": "unsafe URL: " + hostErr.Error()}, http.StatusBadRequest)
		return
	}

	if req.Filename == "" {
		req.Filename = extractFilename(req.URL)
	}
	// 路径穿越防护：清理文件名中的路径分隔符
	req.Filename = filepathSafe(req.Filename)

	// 创建任务并启动下载（同步模式使用 r.Context()）
	task, err := h.cloudMgr.SubmitAndStart("url", req.URL, req.Filename, -1, r.Context())
	if err != nil {
		sendJSONResponse(w, map[string]string{"error": err.Error()}, http.StatusInsufficientStorage)
		return
	}

	// 返回任务快照（避免并发修改 data race）
	snapshot, _ := h.cloudMgr.SnapshotTask(task.ID)
	sendJSONResponse(w, snapshot, http.StatusOK)
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
