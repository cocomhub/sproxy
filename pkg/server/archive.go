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
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/storage"
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
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
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
	defer pr.Close()
	var closeOnce sync.Once
	go func() {
		var pipeErr error
		defer closeOnce.Do(func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		})
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

			// 源文件按请求者租户 user 桶解析（<root>/<tenant>/user/<path>），
			// addFileToTar 经 os.Root 相对打开（中间目录符号链接不逃逸，TOCTOU 交叉校验保留）。
			tnt := h.tenantOf(r)
			if tnt == nil {
				logger.Error("归档添加文件失败：租户不可用", "path", relPath)
				continue
			}
			userRel, ok := tnt.UserRel(relPath)
			if !ok {
				logger.Error("归档添加文件失败：无效的文件路径", "path", relPath)
				continue
			}
			if err := addFileToTar(tw, tnt.Root(), userRel, relPath, logger); err != nil {
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

	_, copyErr := io.Copy(w, pr)
	if copyErr != nil {
		logger.Warn("archive response copy interrupted", "error", copyErr)
	}
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
		for !strings.HasPrefix(p, dir+"/") {
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
		// 读取侧守卫（审查 #4 收敛）：归档源不得引用服务端内部目录（.__ 前缀为服务端
		// 保留；只可经 kind 白名单由服务端按 owner 拼接）。UserRel 虽会拒绝 .__ 段，
		// 但归档源在流式输出后才解析，需在响应开始前拦截并给明确 400。
		if hasServiceInternalPrefix(relPath) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "不能访问服务端内部目录（.__ 前缀为服务端保留）: " + f}, http.StatusBadRequest)
			return nil, false
		}
		validated = append(validated, relPath)
	}
	return validated, true
}

// addFileToTar 将 root 内 rel 对应的文件（或目录）添加到 tar writer 中（tar 条目名为 tarRel）。
// 如果是目录则递归添加。
// 安全边界：所有读取经 os.Root 相对路径（root.Lstat/Open/ReadDir），os.Root 对每路径分量
// 强制 O_NOFOLLOW，中间目录符号链接指向 root 外即报错（不逃逸）；TOCTOU 交叉验证
// os.SameFile 确保 lstat 和 open 后文件一致（最终组件符号链接被 Lstat 拒绝跟随）。
func addFileToTar(tw *tar.Writer, root *storage.Root, rel, tarRel string, logger *slog.Logger) error {
	return addFileToTarDepth(tw, root, rel, tarRel, logger, 0)
}

// addFileToTarDepth 是 addFileToTar 内部实现，带 depth 参数防止递归过深。
func addFileToTarDepth(tw *tar.Writer, root *storage.Root, rel, tarRel string, logger *slog.Logger, depth int) error {
	if depth > 100 {
		return fmt.Errorf("目录深度超过限制: %s", tarRel)
	}

	// 使用 Lstat 检测符号链接，拒绝跟随
	info, err := root.Lstat(rel)
	if err != nil {
		return fmt.Errorf("stat 失败: %w", err)
	}

	// 检测符号链接，拒绝归档
	if info.Mode()&os.ModeSymlink != 0 {
		logger.Warn("跳过符号链接", "path", tarRel)
		return nil
	}

	if info.IsDir() {
		// 递归添加目录内容（root 相对读取，防中间目录符号链接逃逸）
		entries, readErr := root.ReadDir(rel)
		if readErr != nil {
			return fmt.Errorf("读取目录失败: %w", readErr)
		}
		for _, entry := range entries {
			childRel := path.Join(rel, entry.Name())
			childTarRel := filepath.ToSlash(filepath.Join(tarRel, entry.Name()))
			if err = addFileToTarDepth(tw, root, childRel, childTarRel, logger, depth+1); err != nil {
				logger.Warn("归档添加子文件失败", "path", childTarRel, "error", err)
			}
		}
		return nil
	}

	// 单个文件大小限制
	if info.Size() > defaultMaxArchiveSize {
		return fmt.Errorf("文件 %s 大小 (%d) 超过归档限制 (%d)，请直接下载该文件", tarRel, info.Size(), defaultMaxArchiveSize)
	}

	// 打开文件：os.Root 保证不跟随符号链接（最终组件 + 中间目录均 O_NOFOLLOW）
	file, err := root.Open(rel)
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
		return fmt.Errorf("文件在 lstat 和 open 之间被替换（TOCTOU）: %s", tarRel)
	}

	header, err := tar.FileInfoHeader(info, "")
	if err != nil {
		return fmt.Errorf("创建 tar header 失败: %w", err)
	}
	header.Name = filepath.ToSlash(tarRel)

	if err := tw.WriteHeader(header); err != nil {
		return fmt.Errorf("写入 tar header 失败: %w", err)
	}
	if _, err := io.Copy(tw, file); err != nil {
		return fmt.Errorf("写入文件内容失败: %w", err)
	}
	return nil
}

// defaultMaxArchiveSize 是归档中单个文件的最大大小（100MB）。
const defaultMaxArchiveSize int64 = 100 * 1024 * 1024

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

	// 源目录按请求者租户 user 桶解析（<root>/<tenant>/user/<path>）。
	tnt := h.tenantOf(r)
	if tnt == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	userRel, ok := tnt.UserRel(relPath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	info, err := tnt.Root().Stat(userRel)
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
	defer pr.Close()
	var closeOnce sync.Once
	go func() {
		var pipeErr error
		defer closeOnce.Do(func() {
			if pipeErr != nil {
				pw.CloseWithError(pipeErr)
			} else {
				pw.Close()
			}
		})
		gw := gzip.NewWriter(pw)
		tw := tar.NewWriter(gw)

		// 检查客户端是否断开连接
		select {
		case <-r.Context().Done():
			pipeErr = r.Context().Err()
			return
		default:
		}

		if err := addFileToTarDepth(tw, tnt.Root(), userRel, filepath.ToSlash(relPath), h.logger, 0); err != nil {
			pipeErr = err
		}
		if err := tw.Close(); err != nil {
			pipeErr = err
		}
		if err := gw.Close(); err != nil {
			pipeErr = err
		}
	}()
	_, copyErr := io.Copy(w, pr)
	if copyErr != nil {
		h.logger.Warn("archive dir response copy interrupted", "error", copyErr)
	}
}
