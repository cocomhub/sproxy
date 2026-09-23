// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash.go 是回收站 HTTP 端点（roadmap P2 回收站）：
//   - GET /api/trash：列出回收站条目（trash 桶内文件 + 解析原路径）。
//   - POST /api/trash/restore?file=<trashRel>：恢复回 user/ 原路径。
//   - POST /api/trash/empty：清空回收站。
//   - 软删经 delete?soft=true（files.DeleteFile SoftDelete 透传）。
//
// 域实现见 pkg/files/trash.go（softDeleteToTrash/RestoreTrash/EmptyTrash/CleanupTrash）。

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
)

// listTrashHandler 列出回收站条目。
func (h *Handlers) listTrashHandler(w http.ResponseWriter, r *http.Request) {
	owner := ownerFromRequest(r)
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, map[string]any{"entries": []string{}}, http.StatusOK)
		return
	}
	root := tnt.Root()
	trashAbs, ok := root.Abs("trash")
	if !ok {
		sendJSONResponse(w, map[string]any{"entries": []string{}}, http.StatusOK)
		return
	}
	entries, err := os.ReadDir(trashAbs)
	if err != nil {
		sendJSONResponse(w, map[string]any{"entries": []string{}}, http.StatusOK)
		return
	}
	type trashEntry struct {
		TrashRel string `json:"trash_rel"`
		Name     string `json:"name"`
	}
	var out []trashEntry
	for _, e := range entries {
		name := e.Name()
		if idx := strings.Index(name, files.TrashDeletedSuffix()); idx >= 0 {
			out = append(out, trashEntry{
				TrashRel: "trash/" + name,
				Name:     strings.ReplaceAll(name[:idx], "_", "/"),
			})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": out})
}

// restoreTrashHandler 恢复回收站条目。
func (h *Handlers) restoreTrashHandler(w http.ResponseWriter, r *http.Request) {
	file := r.URL.Query().Get("file")
	if file == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "file 不能为空"}, http.StatusBadRequest)
		return
	}
	if err := h.fileService().RestoreTrash(r.Context(), ownerFromRequest(r), file); err != nil {
		if he, ok := err.(*files.HTTPError); ok {
			sendJSONResponse(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复失败"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: "已恢复"}, http.StatusOK)
}

// emptyTrashHandler 清空回收站。
func (h *Handlers) emptyTrashHandler(w http.ResponseWriter, r *http.Request) {
	if err := h.fileService().EmptyTrash(r.Context(), ownerFromRequest(r)); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "清空失败"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: "回收站已清空"}, http.StatusOK)
}

var _ = filepath.Join
