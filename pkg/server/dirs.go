// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"os"
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
	// 写入侧守卫（审查 #4 收敛）：不得创建服务端内部目录名（.__cloud__ 等保留给服务端）。
	if isInternalDirPathPrefix(remotePath) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "不能创建服务端内部目录（.__ 前缀为服务端保留）"}, http.StatusBadRequest)
		return
	}

	targetDir := h.safePathFor(r, remotePath)
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
	// 写入侧守卫（审查 #4 收敛）：不得删除服务端内部目录（防 rmdir 删除 .__cloud__ 等）。
	if isInternalDirPathPrefix(remotePath) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "不能删除服务端内部目录（.__ 前缀为服务端保留）"}, http.StatusBadRequest)
		return
	}

	targetDir := h.safePathFor(r, remotePath)
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

	// 再次检查，确认目录未被替换（TOCTOU 防御）
	stat2, err := os.Lstat(targetDir)
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

	// 使用 removeDirNoFollow 安全递归删除（不跟随符号链接）
	if err := removeDirNoFollow(targetDir); err != nil {
		h.logger.Error("删除目录失败", "dir", remotePath, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除目录失败"}, http.StatusInternalServerError)
		return
	}

	// 清理 checksum store 中该目录下所有文件的记录（owner 作用域 key）
	// 使用 "/" 分隔符，与 ChecksumStore 的 key 格式约定保持一致（所有 key 使用 filepath.ToSlash 格式）
	dirKey := h.checksumKeyFor(r, remotePath)
	h.checksumStore.DeletePrefix(dirKey + "/")
	// 清理目录自身的 checksum 记录（如果存在）
	h.checksumStore.Delete(dirKey)

	h.logger.Info("目录已删除", "dir", remotePath)
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("目录已删除: %s", remotePath)}, http.StatusOK)
}

// removeDirNoFollow 递归删除目录树，遇到符号链接时删除链接本身而非跟随。
// 使用深度优先后序遍历确保子目录先于父目录被删除。
func removeDirNoFollow(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := dir + string(os.PathSeparator) + entry.Name()
		// 符号链接：删除链接本身，不递归进入
		if entry.Type()&os.ModeSymlink != 0 {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		if entry.IsDir() {
			if err := removeDirNoFollow(path); err != nil {
				return err
			}
		} else {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return os.Remove(dir)
}
