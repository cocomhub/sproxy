// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"os"
)

func (h *Handlers) download(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgEmptyFilename}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return
	}
	filePath := h.safePath(remotePath)
	if filePath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return
	}

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			h.logger.Error("打开文件失败", "file_name", remotePath, "error", err.Error())
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		h.logger.Error("stat 文件失败", "file_name", remotePath, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "stat 失败"}, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", remotePath))
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Accept-Ranges", "bytes")

	// 设置 SHA-256 checksum 响应头：优先从 store 读取，回退实时计算
	if cs, ok := h.checksumStore.Get(remotePath); ok {
		w.Header().Set(headerFileChecksum, cs)
	} else if cs, err := FileChecksum(filePath); err == nil {
		w.Header().Set(headerFileChecksum, cs)
	} else {
		h.logger.Warn("计算文件 checksum 失败", "error", err.Error(), "file_name", remotePath)
	}

	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))

	// 使用 http.ServeContent 替代 http.ServeFile：
	//   - 自动处理 Range header（返回 206 + Content-Range，旧客户端不带 Range 仍 200 全量）
	//   - 不会根据扩展名嗅探并覆盖已设置的 Content-Type（同步修复缺陷 #12）
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	if h.metrics != nil {
		h.metrics.RecordDownload(info.Size())
	}
}

// stat 处理 HEAD /api/files/stat?filename=<name>。
// 通过响应头 X-File-Size、X-File-Checksum、X-File-MTime（UnixNano）返回元信息。
// 文件不存在返回 404；不返回响应体。
func (h *Handlers) stat(w http.ResponseWriter, r *http.Request) {
	filename := r.URL.Query().Get("filename")
	if filename == "" {
		http.Error(w, "missing filename", http.StatusBadRequest)
		return
	}
	remotePath, fullPath, ok := h.resolveFilePathHTTP(w, filename)
	if !ok {
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			h.logger.Error("stat 失败", "file_name", remotePath, "error", err.Error())
			http.Error(w, "stat error", http.StatusInternalServerError)
		}
		return
	}
	if info.IsDir() {
		w.Header().Set("X-File-IsDir", "true")
	}
	w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))
	if cs, ok := h.checksumStore.Get(remotePath); ok {
		w.Header().Set(headerFileChecksum, cs)
	} else if !info.IsDir() {
		if cs, err := FileChecksum(fullPath); err == nil {
			w.Header().Set(headerFileChecksum, cs)
		}
	}
	w.WriteHeader(http.StatusOK)
}
