// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash.go 是回收站 HTTP 端点（roadmap P2 回收站）：
//   - GET /api/trash[?volume=<卷>]：列出回收站条目（trash 桶内文件 + 解析原路径）。
//     **缺省 = 汇总**（owner 可见的全部本地卷，每条带 `volume` 字段）；`?volume=` 只列该卷。
//   - POST /api/trash/restore?file=<trashRel>[&volume=<卷>]：恢复回 user/ 原路径；
//     缺省跨卷定位（条目只存在于软删时所在卷）。
//   - POST /api/trash/empty[?volume=<卷>|all=1]：清空回收站——`volume` 只清该卷、`all=1` 清全部卷、
//     **二者皆无仍只清默认卷**（既有语义，破坏性默认不静默扩大）。
//   - 软删经 delete?soft=true（files.DeleteFile SoftDelete 透传）。
//
// **多卷语义（用户裁定 2026-10-19 ③：各卷独立管理 + 汇总展示）**：软删落**文件所在卷**的 trash
// ⇒ 列表/恢复/清空必须按卷遍历（此前只看默认卷 ⇒ 其它卷条目不可见、不可恢复）。
//
// 域实现见 pkg/files/trash.go（softDeleteToTrash/RestoreTrash/EmptyTrash/CleanupTrash）。

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/internal/httpjson"
	"github.com/cocomhub/sproxy/pkg/files"
)

// listTrashHandler 列出回收站条目。
// trashEntry 是回收站列表条目（TrashRel 含分层路径、Name 解码原 rel）。
type trashEntry struct {
	TrashRel string `json:"trash_rel"`
	Name     string `json:"name"`
	// Volume 是条目所在卷（多卷汇总视图必需；单卷客户端可忽略——纯新增字段，向后兼容）。
	Volume string `json:"volume"`
}

func (h *Handlers) listTrashHandler(w http.ResponseWriter, r *http.Request) {
	owner := ownerFromRequest(r)
	volumeName := r.URL.Query().Get("volume")
	roots, rerr := h.fileService().TrashRoots(owner, volumeName)
	if rerr != nil {
		if he, ok := rerr.(*files.HTTPError); ok {
			sendJSONResponse(w, map[string]any{"entries": []string{}, "message": he.Message}, he.Status)
			return
		}
		sendJSONResponse(w, map[string]any{"entries": []string{}}, http.StatusInternalServerError)
		return
	}
	var out []trashEntry
	// C-MAJOR-2（用户裁定）：trash 条目为**目录镜像 + 文件名编码**（trash/user/<目录>/
	// <文件名>.__deleted__，目录段保留原文、文件名 base64）——递归遍历收集文件条目
	// （跳过 meta sidecar 条目；目录不列）。**逐卷遍历**（缺省 = 汇总全部本地卷）。
	for _, vr := range roots {
		trashAbs, ok := vr.Root.Abs("trash")
		if !ok {
			continue
		}
		_ = filepath.WalkDir(trashAbs, h.listTrashWalk(trashAbs, vr.Volume, &out))
	}
	httpjson.Send(w, http.StatusOK, map[string]any{"entries": out})
}

// listTrashWalk 是 listTrashHandler 的 WalkDir 回调（S107 拆分降 gocognit）：收集
// 主文件条目（跳过 meta sidecar 与目录），TrashRel 含分层路径、Name 解码原 rel。
func (h *Handlers) listTrashWalk(trashAbs, volumeName string, out *[]trashEntry) fs.WalkDirFunc {
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
			Volume:   volumeName,
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
	// volume 缺省 ⇒ 跨卷定位（条目只存在于软删时所在卷）。
	if err := h.fileService().RestoreTrashIn(r.Context(), ownerFromRequest(r), r.URL.Query().Get("volume"), file); err != nil {
		if he, ok := err.(*files.HTTPError); ok {
			sendJSONResponse(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		// 非 HTTPError 的真实失败必须留痕（此前只回通用文案 ⇒ 排查无线索；多卷跨卷定位后
		// 更易出现「某个卷上不可预期的错误」，需可诊断）。
		h.logger.Error("恢复回收站条目失败", "file", file, "volume", r.URL.Query().Get("volume"), "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "恢复失败"}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: "已恢复"}, http.StatusOK)
}

// emptyTrashHandler 清空回收站。
func (h *Handlers) emptyTrashHandler(w http.ResponseWriter, r *http.Request) {
	// volume=<卷> ⇒ 只清该卷；all=1 ⇒ 清全部卷；二者皆无 ⇒ 只清默认卷（既有语义，零回归）。
	volumeName := r.URL.Query().Get("volume")
	all := r.URL.Query().Get("all") == "1"
	if err := h.fileService().EmptyTrashIn(r.Context(), ownerFromRequest(r), volumeName, all); err != nil {
		if he, ok := err.(*files.HTTPError); ok {
			sendJSONResponse(w, UploadResponse{Success: false, Message: he.Message}, he.Status)
			return
		}
		sendJSONResponse(w, UploadResponse{Success: false, Message: "清空失败"}, http.StatusInternalServerError)
		return
	}
	msg := "回收站已清空"
	if all {
		msg = "全部卷回收站已清空"
	} else if volumeName != "" {
		msg = "该卷回收站已清空"
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: msg}, http.StatusOK)
}

var _ = filepath.Join
