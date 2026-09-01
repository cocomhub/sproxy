// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"os"
)

// mkdir 创建指定子目录。?dirname=path
// 已迁移到 Tenant API：用户目录映射到 user 桶内（<root>/<owner>/user/<rel>），
// UserRel 逐段段名校验（拒绝 .__ 内部前缀、功能桶引用、保留设备名等），
// 无需再单独内部目录守卫。
func (h *Handlers) mkdir(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(dirname)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名: " + err.Error()}, http.StatusBadRequest)
		return
	}
	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	if err := tnt.Root().MkdirAll(rel, 0755); err != nil {
		h.logger.Error(errMsgCreateDirFailed, "dir", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgCreateDirFailed}, http.StatusInternalServerError)
		return
	}

	h.logger.Info("目录已创建", "dir", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已创建: %s", remotePath)}, http.StatusOK)
}

// rmdir 删除指定目录（含所有内容）。?dirname=path&force=true
// 已迁移到 Tenant API：路径映射到 user 桶（<root>/<owner>/user/<rel>），
// 递归删除用 root.RemoveAll（os.Root 保证符号链接不逃逸，替代手写 removeDirNoFollow）；
// checksum 从 per-tenant store 清理 rel 前缀与 rel 自身。
func (h *Handlers) rmdir(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(dirname)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名: " + err.Error()}, http.StatusBadRequest)
		return
	}
	tnt := h.tenantOf(r)
	if tnt == nil || tnt.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	root := tnt.Root()
	rel, ok := tnt.UserRel(remotePath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	// 使用 Lstat 检查符号链接，拒绝操作（不跟随符号链接）
	stat, err := root.Lstat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
		}
		return
	}
	if stat.Mode()&os.ModeSymlink != 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "不允许删除符号链接"}, http.StatusBadRequest)
		return
	}
	if !stat.IsDir() {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		return
	}

	// 再次检查，确认目录未被替换（TOCTOU 防御）
	stat2, err := root.Lstat(rel)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
		}
		return
	}
	if stat2.Mode()&os.ModeSymlink != 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "不允许删除符号链接"}, http.StatusBadRequest)
		return
	}
	if !stat2.IsDir() {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		return
	}

	// force 必须为 true 才执行删除（避免误删）
	force := r.URL.Query().Get("force") == "true"
	if !force {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请使用 ?force=true 确认删除"}, http.StatusBadRequest)
		return
	}

	// 使用 root.RemoveAll 安全递归删除（os.Root 保证符号链接不逃逸）
	if err := root.RemoveAll(rel); err != nil {
		h.logger.Error("删除目录失败", "dir", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除目录失败"}, http.StatusInternalServerError)
		return
	}

	// 清理 per-tenant checksum store 中该目录下所有文件的记录（key = rel，无 owner 前缀）。
	// 使用 "/" 分隔符，与 ChecksumStore 的 key 格式约定保持一致（所有 key 使用 filepath.ToSlash 格式）。
	if cs := h.checksumStoreFor(ownerFromRequest(r)); cs != nil {
		cs.DeletePrefix(rel + "/")
		// 清理目录自身的 checksum 记录（如果存在）
		cs.Delete(rel)
	}

	h.logger.Info("目录已删除", "dir", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已删除: %s", remotePath)}, http.StatusOK)
}
