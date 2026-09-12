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

	"github.com/cocomhub/sproxy/pkg/pathguard"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
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
	owner := normalizeOwner(ownerFromRequest(r))
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

			// 跨卷定位（T6b）：归档源文件按 owner 卷视图定位（addFileToTar 经 os.Root 相对
			// 打开——中间目录符号链接不逃逸，TOCTOU 交叉校验保留）。非默认卷文件可打包；
			// 默认卷被 ACL 排除时默认卷遗留不可见 → 跳过（不读取无权卷内容，fail-closed）。
			root, userRel := h.archiveFileRootFor(owner, relPath)
			if root == nil {
				logger.Error("归档添加文件失败：文件不在视图", "path", relPath)
				continue
			}
			if err := addFileToTar(tw, root, userRel, relPath, logger); err != nil {
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
		relPath, err := pathguard.ValidateFilePath(f)
		if err != nil {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件路径: " + f}, http.StatusBadRequest)
			return nil, false
		}
		// 读取侧守卫（审查 #4 收敛）：归档源不得引用服务端内部目录（.__ 前缀为服务端
		// 保留；只可经 kind 白名单由服务端按 owner 拼接）。UserRel 虽会拒绝 .__ 段，
		// 但归档源在流式输出后才解析，需在响应开始前拦截并给明确 400。
		if pathguard.HasServiceInternalPrefix(relPath) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "不能访问服务端内部目录（.__ 前缀为服务端保留）: " + f}, http.StatusBadRequest)
			return nil, false
		}
		validated = append(validated, relPath)
	}
	return validated, true
}

// archiveFileRootFor 返回归档源文件 relPath 在 owner 卷视图内的 root 与 user 桶 rel。
// 未命中（视图全无）或默认卷被 ACL 排除（遗留不可见）返回 (nil, "")——调用方跳过，
// 绝不读取无权卷内容（fail-closed）。volSet nil（旧装配）回落默认租户（唯一根）。
func (h *Handlers) archiveFileRootFor(owner, relPath string) (*storage.Root, string) {
	tnt := h.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, ""
	}
	userRel, ok := tnt.UserRel(relPath)
	if !ok {
		return nil, ""
	}
	if h.volSet != nil {
		if loc, found := h.locateOwnerFile(owner, userRel); found && loc != nil && loc.tenant != nil && loc.tenant.Root() != nil {
			return loc.tenant.Root(), userRel
		}
		if !h.defaultVolumeAllows(owner) {
			return nil, ""
		}
	}
	return tnt.Root(), userRel
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
	relPath, err := pathguard.ValidateFilePath(dirname)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录名"}, http.StatusBadRequest)
		return
	}

	// 源目录按请求者租户 user 桶解析（<root>/<tenant>/user/<path>）。
	owner := normalizeOwner(ownerFromRequest(r))
	tnt0 := h.tenantFor(owner)
	if tnt0 == nil || tnt0.Root() == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}
	userRel, ok := tnt0.UserRel(relPath)
	if !ok {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目录路径"}, http.StatusBadRequest)
		return
	}

	// 多卷（T6b）：目录可跨卷并存（换卷目录不一定每卷都有），收集 owner 视图内存在该目录的
	// 卷租户逐卷打包；默认卷被 ACL 排除时不参与（fail-closed，不打包无权卷遗留）。
	type dirSrc struct {
		tnt     *storage.Tenant
		userRel string
	}
	var srcs []dirSrc
	if h.volSet == nil {
		// 旧装配路径：唯一根 stat 校验目录存在 + 是目录（与单卷既有错误语义一致）。
		info, statErr := tnt0.Root().Stat(userRel)
		if statErr != nil {
			if os.IsNotExist(statErr) {
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
		srcs = append(srcs, dirSrc{tnt: tnt0, userRel: userRel})
	} else {
		// F6（review 边界成文）：跨卷「同名文件/目录并存」（rel 在一卷是目录、另一卷是文件）时，
		// 策略为**打包存在的目录、忽略同名文件条目**并返回 200——目录可跨卷并存、不受 AD-4 文件
		// 唯一性约束，rel 在视图内存在目录即可打包。这与单卷「指定路径不是目录 → 400」有语义差异
		// （单卷 rel 唯一，不可能目录/文件同址；多卷仅异常共存时触发），属刻意取舍，非漏洞。
		// 仅当视图内**完全没有该 rel 的目录**且存在同名文件时才回落 400（notDir 且无目录）。
		notDir := false
		for _, v := range volume.AllowedVolumes(h.volSet.All(), owner) {
			exists, vErr := h.volumeFileExists(v.Name, owner, userRel)
			if vErr != nil || !exists {
				continue
			}
			tnt := h.volumeTenant(v.Name, owner)
			if tnt == nil || tnt.Root() == nil {
				continue
			}
			info, statErr := tnt.Root().Stat(userRel)
			if statErr != nil {
				continue
			}
			if !info.IsDir() {
				notDir = true
				continue
			}
			srcs = append(srcs, dirSrc{tnt: tnt, userRel: userRel})
		}
		if notDir && len(srcs) == 0 {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "指定路径不是目录"}, http.StatusBadRequest)
			return
		}
	}
	if len(srcs) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "目录不存在"}, http.StatusNotFound)
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

		for _, src := range srcs {
			// 检查客户端是否断开连接
			select {
			case <-r.Context().Done():
				pipeErr = r.Context().Err()
				return
			default:
			}

			if err := addFileToTarDepth(tw, src.tnt.Root(), src.userRel, filepath.ToSlash(relPath), h.logger, 0); err != nil {
				pipeErr = err
			}
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
