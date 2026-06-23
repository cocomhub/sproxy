// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
)

// ArchiveRequest 是 POST /api/archive 的请求体。
type ArchiveRequest struct {
	Files []string `json:"files"`
}

// archiveHandler 处理 POST /api/archive。
// 接收 JSON {"files": ["file1.txt", "dir/file2.txt"]}，
// 返回 application/tar+gzip 流式归档文件。
// 使用 io.Pipe 实现流式打包，不占用额外磁盘空间。
func (h *Handlers) archiveHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req ArchiveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}

	logger := h.logger.With("archive", "create")

	validated, ok := validateArchiveFiles(req.Files, w)
	if !ok {
		return
	}

	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", "attachment; filename=\"archive.tar.gz\"")
	w.WriteHeader(http.StatusOK)

	// 流式打包：io.Pipe 中 tar + gzip
	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)

		for _, relPath := range validated {
			fullPath := h.safePath(relPath)
			if fullPath == "" {
				logger.Error("归档添加文件失败：无效的文件路径", "path", relPath)
				continue
			}
			if err := addFileToTar(tw, fullPath, relPath, logger); err != nil {
				logger.Error("归档添加文件失败", "path", relPath, "error", err)
			}
		}

		// 按序关闭
		if err := tw.Close(); err != nil {
			logger.Error("tar writer 关闭失败", "error", err)
		}
		if err := gw.Close(); err != nil {
			logger.Error("gzip writer 关闭失败", "error", err)
		}
		pw.Close()
	}()

	_, _ = io.Copy(w, pr)
}

// validateArchiveFiles 验证归档请求中的文件路径，返回有效路径列表。
// 如果校验失败，已发送错误响应。
func validateArchiveFiles(files []string, w http.ResponseWriter) ([]string, bool) {
	validated := make([]string, 0, len(files))
	for _, f := range files {
		relPath, err := ValidateFilePath(f)
		if err != nil {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件路径: " + f}, http.StatusBadRequest)
			return nil, false
		}
		validated = append(validated, relPath)
	}
	return validated, true
}

// addFileToTar 将单个文件（或目录）添加到 tar writer 中。
// 如果是目录则递归添加。
func addFileToTar(tw *tar.Writer, fullPath, relPath string, logger *slog.Logger) error {
	info, err := os.Stat(fullPath)
	if err != nil {
		return fmt.Errorf("stat 失败: %w", err)
	}

	if info.IsDir() {
		// 递归添加目录内容
		var entries []os.DirEntry
		entries, err = os.ReadDir(fullPath)
		if err != nil {
			return fmt.Errorf("读取目录失败: %w", err)
		}
		for _, entry := range entries {
			childRel := filepath.ToSlash(filepath.Join(relPath, entry.Name()))
			childFull := filepath.Join(fullPath, entry.Name())
			if err = addFileToTar(tw, childFull, childRel, logger); err != nil {
				logger.Warn("归档添加子文件失败", "path", childRel, "error", err)
			}
		}
		return nil
	}

	file, err := os.Open(fullPath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("创建 tar header 失败: %w", err)
	}
	header.Name = filepath.ToSlash(relPath)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("写入 tar header 失败: %w", err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("写入文件内容失败: %w", err)
	}
	return nil
}

// archiveDirHandler 处理 GET /api/archive-dir?dirname=xxx。
// 将指定目录及其内容打包下载。
func (h *Handlers) archiveDirHandler(w http.ResponseWriter, r *http.Request) {
	dirname := r.URL.Query().Get("dirname")
	if dirname == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "dirname 不能为空"}, http.StatusBadRequest)
		return
	}
	relPath, err := ValidateFilePath(dirname)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名"}, http.StatusBadRequest)
		return
	}

	fullPath := h.safePath(relPath)
	if fullPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	info, err := os.Stat(fullPath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
		} else {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "访问目录失败"}, http.StatusInternalServerError)
		}
		return
	}
	if !info.IsDir() {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
		return
	}

	archiveName := filepath.Base(relPath) + ".tar.gz"
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", archiveName))
	w.WriteHeader(http.StatusOK)

	pr, pw := io.Pipe()
	go func() {
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)
		_ = addFileToTar(tw, fullPath, filepath.ToSlash(relPath), h.logger)
		_ = tw.Close()
		_ = gw.Close()
		pw.Close()
	}()
	_, _ = io.Copy(w, pr)
}
