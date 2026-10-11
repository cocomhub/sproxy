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
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files"
)

// listTrashHandler 列出回收站条目。
// trashEntry 是回收站列表条目（TrashRel 含分层路径、Name 解码原 rel）。
type trashEntry struct {
	TrashRel string `json:"trash_rel"`
	Name     string `json:"name"`
}

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
	var out []trashEntry
	// C-MAJOR-2（用户裁定）：trash 条目为**目录镜像 + 文件名编码**（trash/user/<目录>/
	// <文件名>.__deleted__，目录段保留原文、文件名 base64）——递归遍历收集文件条目
	// （跳过 meta sidecar 条目；目录不列）。
	_ = filepath.WalkDir(trashAbs, h.listTrashWalk(trashAbs, &out))
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": out})
}

// listTrashWalk 是 listTrashHandler 的 WalkDir 回调（S107 拆分降 gocognit）：收集
// 主文件条目（跳过 meta sidecar 与目录），TrashRel 含分层路径、Name 解码原 rel。
func (h *Handlers) listTrashWalk(trashAbs string, out *[]trashEntry) fs.WalkDirFunc {
	return func(absPath string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(trashAbs, absPath)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		idx := strings.Index(rel, files.TrashDeletedSuffix())
		if idx < 0 {
			return nil
		}
		// 跳过 meta sidecar 条目（软删随迁的服务端内部文件；trashMetaMarker
		// 在删除标记前命中 → 不含主文件条目——主文件条目名无 .meta 段）。
		if strings.Contains(rel[:idx], files.TrashMetaMarker()) {
			return nil
		}
		*out = append(*out, trashEntry{
			TrashRel: "trash/" + rel,
			Name:     files.UnflattenTrashRel(rel[:idx]),
		})
		return nil
	}
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
