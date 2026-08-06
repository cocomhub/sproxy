// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
)

// mkdir 创建指定子目录。?dirname=path
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

	targetDir := h.safePath(remotePath)
	if targetDir == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(targetDir, 0755); err != nil {
		h.logger.Error(errMsgCreateDirFailed, "dir", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgCreateDirFailed}, http.StatusInternalServerError)
		return
	}

	h.logger.Info("目录已创建", "dir", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已创建: %s", remotePath)}, http.StatusOK)
}

// rmdir 删除指定目录（含所有内容）。?dirname=path&force=true
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

	targetDir := h.safePath(remotePath)
	if targetDir == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	// 使用 Lstat 检查符号链接，拒绝操作
	stat, err := os.Lstat(targetDir)
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

	// force 必须为 true 才执行删除（避免误删）
	force := r.URL.Query().Get("force") == "true"
	if !force {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请使用 ?force=true 确认删除"}, http.StatusBadRequest)
		return
	}

	// 使用 removeDirNoFollow 安全递归删除（不跟随符号链接）
	if err := removeDirNoFollow(targetDir); err != nil {
		h.logger.Error("删除目录失败", "dir", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除目录失败"}, http.StatusInternalServerError)
		return
	}

	// 清理 checksum store 中该目录下所有文件的记录
	h.checksumStore.DeletePrefix(remotePath + "/")
	// 清理目录自身的 checksum 记录（如果存在）
	h.checksumStore.Delete(remotePath)

	h.logger.Info("目录已删除", "dir", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已删除: %s", remotePath)}, http.StatusOK)
}

// removeDirNoFollow 递归删除目录树，遇到符号链接时删除链接本身而非跟随。
