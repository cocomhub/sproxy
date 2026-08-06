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
	"strings"
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

	// 根据请求文件列表推导归档名：单文件保留原文件名，多文件用公共前缀目录名
	if len(validated) == 1 {
		baseName := filepath.Base(validated[0])
		if baseName == "" || baseName == "." {
			baseName = "file"
		}
		w.Header().Set("Content-Disposition", formatContentDisposition(baseName+".tar.gz"))
	} else {
		name := commonArchiveName(validated)
		w.Header().Set("Content-Disposition", formatContentDisposition(name+".tar.gz"))
	}
	w.WriteHeader(http.StatusOK)

	// 流式打包：io.Pipe 中 tar + gzip
	pr, pw := io.Pipe()
	go func() {
		var pipeErr error
		defer func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		}()
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)

		for _, relPath := range validated {
			// 检查客户端是否断开连接，避免 goroutine 泄漏
			select {
			case <-r.Context().Done():
				pipeErr = r.Context().Err()
				return
			default:
			}

			fullPath := h.safePath(relPath)
			if fullPath == "" {
				logger.Error("归档添加文件失败：无效的文件路径", "path", relPath)
				continue
			}
			if err := addFileToTar(tw, fullPath, relPath, logger); err != nil {
				logger.Error("归档添加文件失败", "path", relPath, "error", err)
				pipeErr = err
			}
		}

		// 按序关闭
		if err := tw.Close(); err != nil {
			logger.Error("tar writer 关闭失败", "error", err)
			pipeErr = err
		}
		if err := gw.Close(); err != nil {
			logger.Error("gzip writer 关闭失败", "error", err)
			pipeErr = err
		}
	}()

	_, _ = io.Copy(w, pr)
}

// commonArchiveName 从文件路径列表中推导公共归档名。
func commonArchiveName(paths []string) string {
	if len(paths) == 0 {
		return "archive"
	}
	if len(paths) == 1 {
		base := filepath.Base(paths[0])
		if base == "" || base == "." {
			return "archive"
		}
		return strings.TrimSuffix(base, filepath.Ext(base))
	}
	// 尝试取公共前缀目录
	dir := filepath.Dir(paths[0])
	for _, p := range paths[1:] {
		for !strings.HasPrefix(p, dir) {
			parent := filepath.Dir(dir)
			if parent == "." || parent == "/" || parent == dir {
				return "archive"
			}
			dir = parent
		}
	}
	if dir == "." || dir == "/" {
		return "archive"
	}
	return filepath.Base(dir)
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
// TOCTOU 防护：Linux 使用 O_NOFOLLOW 不跟随符号链接打开，
// 交叉验证 os.SameFile 确保 lstat 和 open 后文件一致。
func addFileToTar(tw *tar.Writer, fullPath, relPath string, logger *slog.Logger) error {
	// 使用 Lstat 检测符号链接，拒绝跟随
	info, err := os.Lstat(fullPath)
	if err != nil {
		return fmt.Errorf("stat 失败: %w", err)
	}

	// 检测符号链接，拒绝归档
	if info.Mode()&os.ModeSymlink != 0 {
		logger.Warn("跳过符号链接", "path", relPath)
		return nil
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

	// 打开文件：Linux 用 O_NOFOLLOW 不跟随符号链接
	file, err := openFileNoFollow(fullPath)
	if err != nil {
		return fmt.Errorf("打开文件失败: %w", err)
	}
	defer file.Close()

	// 交叉验证：lstat 得到的文件信息与打开后的文件信息一致
	openedInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat 已打开文件失败: %w", err)
	}
	if !os.SameFile(info, openedInfo) {
		return fmt.Errorf("文件在 lstat 和 open 之间被替换（TOCTOU）: %s", relPath)
	}

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
	w.Header().Set("Content-Disposition", formatContentDisposition(archiveName))
	w.WriteHeader(http.StatusOK)

	pr, pw := io.Pipe()
	go func() {
		var pipeErr error
		defer func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		}()
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)

		// 检查客户端是否断开连接
		select {
		case <-r.Context().Done():
			pipeErr = r.Context().Err()
			return
		default:
		}

		if err := addFileToTar(tw, fullPath, filepath.ToSlash(relPath), h.logger); err != nil {
			pipeErr = err
		}
		if err := tw.Close(); err != nil {
			pipeErr = err
		}
		if err := gw.Close(); err != nil {
			pipeErr = err
		}
	}()
	_, _ = io.Copy(w, pr)
}
