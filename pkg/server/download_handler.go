// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// downloadKindCloudArchive 是云任务归档下载的 kind 值。
// kind 白名单仅此一项——归档是用户主动打包的产出（归档名含随机难枚举），
// 按 owner 隔离存储（.__cloud_archives__/<owner>/）。
const downloadKindCloudArchive = "cloud_archive"

// downloadKindCloudTask 是云任务文件下载的 kind 值。
// 云任务文件存 uploadsDir/.__cloud__/<taskID>/<file>（.__ 内部目录），
// filename 传 <taskID>/<file>，服务端校验任务属于当前 owner 后拼接内部目录。
const downloadKindCloudTask = "cloud_task"

// downloadPathError 携带 HTTP 状态码与消息，统一 /download 与 /download/chunk 的路径解析错误。
type downloadPathError struct {
	status  int
	message string
}

func (e *downloadPathError) Error() string { return e.message }

// writeDownloadPathError 将 resolveDownloadPath 返回的错误映射为统一 JSON 响应。
func writeDownloadPathError(w http.ResponseWriter, err error) {
	var de *downloadPathError
	if errors.As(err, &de) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: de.message}, de.status)
		return
	}
	sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
}

// writeHTTPPathError 将 resolveDownloadPath 返回的错误映射为统一 http.Error 响应
// （供 stat 等非 JSON handler 使用）。
func writeHTTPPathError(w http.ResponseWriter, err error) {
	var de *downloadPathError
	if errors.As(err, &de) {
		http.Error(w, de.message, de.status)
		return
	}
	http.Error(w, "invalid filename", http.StatusBadRequest)
}

// validateCloudArchiveName 校验 kind=cloud_archive 的归档名。
// 归档名必须是单文件名（无路径分隔符），拒绝空、绝对路径、..、Windows 非法字符。
// 与 ValidateFilePath 的规则对齐，但不含 .__ 首段拒绝——归档目录本身即 .__ 前缀，
// 服务端在 cloudArchivePath 中负责拼接与防穿越（. __ 内部目录仅此白名单 kind 可达）。
func validateCloudArchiveName(name string) *downloadPathError {
	invalid := &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
	name = strings.TrimSpace(name)
	if name == "" {
		return &downloadPathError{status: http.StatusBadRequest, message: errMsgEmptyFilename}
	}
	if strings.ContainsRune(name, 0) {
		return invalid
	}
	if name[0] == '/' || name[0] == '\\' {
		return invalid
	}
	// 拒绝路径分隔符（/ 与 \）：归档名必须是单文件名，服务端拼接归档目录。
	if strings.ContainsAny(name, `/\`) {
		return invalid
	}
	// 拒绝 . 与 ..；filepath.Base 兜底，防 Windows 盘符/尾分隔符等绕过。
	if filepath.Clean(name) == "." || filepath.Clean(name) == ".." || filepath.Base(name) != name {
		return invalid
	}
	// Windows 非法字符（对齐 ValidateFilePath）。
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		if strings.ContainsAny(name, invalidChars) {
			return invalid
		}
	}
	return nil
}

// downloadPath 是 resolveDownloadPath 的解析结果。
// 普通下载（kind 为空）经租户根打开：tnt 非 nil，rel 为租户根内相对路径（user/<path>）。
// kind=cloud_archive/cloud_task 保持旧布局（任务 13/14 随数据迁移再切换）：tnt 为 nil，
// filePath 为绝对路径。
type downloadPath struct {
	filename string          // 用户可见文件名（Content-Disposition / 日志 / checksum key）
	tnt      *storage.Tenant // 非 nil = 普通下载（经 Tenant.Root 定位，防符号链接逃逸）
	rel      string          // 租户根内相对路径（tnt != nil 时有效，如 user/dir/f.txt）
	filePath string          // 旧布局绝对路径（tnt == nil 时有效，kind=cloud_*）
}

// resolveDownloadPath 解析 /download、/download/chunk 与 /api/files/stat 的文件路径。
// 返回 downloadPath：
//
//   - kind 为空 → 普通下载：ValidateFilePath 校验 + Tenant.UserRel 映射到 user 桶，
//     返回租户与根内相对路径（后续经 tenant.Root().Open/Stat 打开）。租户不可用或
//     UserRel 拒绝（.__/__ 内部前缀 / 非法段名）→ 400（downloadPathError）。
//   - kind=cloud_archive → 归档名（单文件名），旧布局 uploadsDir/.__cloud_archives__/[owner/]/<name>。
//   - kind=cloud_task → 云任务文件（任务归属校验），旧布局 uploadsDir/.__cloud__/<taskID>/<file>。
//   - 其它 kind → 400（白名单，防任意内部目录访问）。
func (h *Handlers) resolveDownloadPath(r *http.Request) (*downloadPath, error) {
	name := r.URL.Query().Get("filename")
	kind := r.URL.Query().Get("kind")

	switch kind {
	case "":
		remotePath, vErr := ValidateFilePath(name)
		if vErr != nil {
			if name == "" {
				return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgEmptyFilename}
			}
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
		}
		// 读取侧守卫（审查 #4 收敛 + 审查 I-1）：普通下载/stat 不得访问非 user 桶路径。
		// UserRel 内部：NormalizeRemote + 逐段 ValidSegmentName（拒绝 .__ 内部前缀、
		// Windows 保留设备名等）+ 首段 __ 遗留前缀拒绝；功能桶名首段合法（user/ 桶内）。
		tnt := h.tenantOf(r)
		if tnt == nil {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		rel, ok := tnt.UserRel(remotePath)
		if !ok {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		return &downloadPath{filename: remotePath, tnt: tnt, rel: rel}, nil
	case downloadKindCloudArchive:
		if aErr := validateCloudArchiveName(name); aErr != nil {
			return nil, aErr
		}
		fullPath := h.cloudArchivePathFor(r, name)
		if fullPath == "" {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		return &downloadPath{filename: name, filePath: fullPath}, nil
	case downloadKindCloudTask:
		remotePath, vErr := ValidateFilePath(name)
		if vErr != nil {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidFilename}
		}
		// 校验任务属于当前 owner + 请求文件 == task.Filename（审查 I3：只允许下载
		// 任务声明的原始文件，防下载任务目录下 .partial/.partial.etag 等残留）。
		// 跨租户任务或文件不匹配 → 404（防枚举，不泄露存在性）。
		// 格式必须为 <taskID>/<file>（含分隔符），否则视为不存在。
		slash := strings.IndexByte(remotePath, '/')
		if slash <= 0 {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		taskID := remotePath[:slash]
		if h.cloudMgr == nil {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		task, ok := h.cloudMgr.SnapshotTask(taskID, ownerFromRequest(r))
		if !ok {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		// 仅允许 taskID/<task.Filename> 精确匹配（防下载任务目录下其它文件）。
		if remotePath != taskID+"/"+task.Filename {
			return nil, &downloadPathError{status: http.StatusNotFound, message: errMsgFileNotFound}
		}
		cfg := h.cfgPtr.Load()
		fullPath := joinSafePath(filepath.Join(cfg.UploadsDir, cloudDirName), remotePath)
		if fullPath == "" {
			return nil, &downloadPathError{status: http.StatusBadRequest, message: errMsgInvalidPath}
		}
		return &downloadPath{filename: remotePath, filePath: fullPath}, nil
	default:
		return nil, &downloadPathError{
			status:  http.StatusBadRequest,
			message: "未知下载 kind: " + kind,
		}
	}
}

// checksumStoreForRead 返回下载/stat/chunk 的 checksum 存储与 key。
// 普通下载（dp.tnt 非 nil）→ per-tenant store + 根内相对路径 rel（无 owner 前缀）；
// cloud 下载（dp.tnt nil）→ 全局 store + owner 作用域 key（旧行为）。
// per-tenant store 不可用时返回 nil（调用方跳过 checksum 响应头）。
func (h *Handlers) checksumStoreForRead(r *http.Request, dp *downloadPath) (ChecksumStoreIface, string) {
	if dp.tnt != nil {
		cs := h.checksumStoreFor(ownerFromRequest(r))
		if cs == nil {
			return nil, ""
		}
		return cs, dp.rel
	}
	return h.checksumStore, h.checksumKeyFor(r, dp.filename)
}

// countingWriter 包装 http.ResponseWriter 并追踪实际写入的字节数。
// 用于 http.ServeContent 写入后记录实际传输字节（而非 Content-Length）。
type countingWriter struct {
	http.ResponseWriter
	count atomic.Int64
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.ResponseWriter.Write(p)
	cw.count.Add(int64(n))
	return n, err
}

func (h *Handlers) download(w http.ResponseWriter, r *http.Request) {
	dp, err := h.resolveDownloadPath(r)
	if err != nil {
		writeDownloadPathError(w, err)
		return
	}

	// 普通下载经租户根打开（root 相对，防符号链接逃逸）；cloud kind 用旧布局绝对路径。
	var file *os.File
	if dp.tnt != nil {
		file, err = dp.tnt.Root().Open(dp.rel)
	} else {
		file, err = os.Open(dp.filePath)
	}
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgFileNotFound}, http.StatusNotFound)
		} else {
			h.logger.Error("打开文件失败", "file_name", dp.filename, "error", err.Error())
			sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgOpenFileFailed}, http.StatusInternalServerError)
		}
		return
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		h.logger.Error("stat 文件失败", "file_name", dp.filename, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "stat 失败"}, http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Disposition", formatContentDisposition(dp.filename))
	w.Header().Set(headerContentType, contentTypeOctetStream)
	w.Header().Set("Accept-Ranges", "bytes")

	// 设置 SHA-256 checksum 响应头：优先从 store 读取，回退实时计算。
	// 回退路径优先复用已打开的文件句柄（零额外 I/O），仅当计算成功后才写入缓存。
	// 普通下载（kind 空）用 per-tenant store + 根内相对路径 key（无 owner 前缀）；
	// cloud 下载（kind 非空）沿用全局 store + owner 作用域 key。
	if csStore, csKey := h.checksumStoreForRead(r, dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		} else {
			// 缓存未命中，从已打开文件句柄计算（复用 file，零额外 I/O）
			_, _ = file.Seek(0, io.SeekStart)
			if cs, err := Checksum(file); err == nil {
				_, _ = file.Seek(0, io.SeekStart)
				csStore.Set(csKey, cs)
				w.Header().Set(headerFileChecksum, cs)
			} else {
				h.logger.Warn("计算文件 checksum 失败", "error", err.Error(), "file_name", dp.filename)
			}
		}
	}

	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))

	// 使用 http.ServeContent 替代 http.ServeFile：
	//   - 自动处理 Range header（返回 206 + Content-Range，旧客户端不带 Range 仍 200 全量）
	//   - 不会根据扩展名嗅探并覆盖已设置的 Content-Type（同步修复缺陷 #12）
	cw := &countingWriter{ResponseWriter: w}
	http.ServeContent(cw, r, info.Name(), info.ModTime(), file)
	if h.metrics != nil {
		h.metrics.RecordDownload(cw.count.Load())
	}
}

// stat 处理 HEAD /api/files/stat?filename=<name>[&kind=cloud_archive]。
// 通过响应头 X-File-Size、X-File-Checksum、X-File-MTime（UnixNano）返回元信息。
// kind 为空走普通文件路径；kind=cloud_archive 解析归档目录（供分块下载前 stat）。
// 文件不存在返回 404；不返回响应体。
func (h *Handlers) stat(w http.ResponseWriter, r *http.Request) {
	dp, err := h.resolveDownloadPath(r)
	if err != nil {
		writeHTTPPathError(w, err)
		return
	}
	var info os.FileInfo
	if dp.tnt != nil {
		info, err = dp.tnt.Root().Stat(dp.rel)
	} else {
		info, err = os.Stat(dp.filePath)
	}
	if err != nil {
		if os.IsNotExist(err) {
			http.Error(w, "not found", http.StatusNotFound)
		} else {
			h.logger.Error("stat 失败", "file_name", dp.filename, "error", err.Error())
			http.Error(w, "stat error", http.StatusInternalServerError)
		}
		return
	}
	if info.IsDir() {
		w.Header().Set("X-File-IsDir", "true")
	}
	w.Header().Set("X-File-Size", fmt.Sprintf("%d", info.Size()))
	w.Header().Set(headerFileMTime, fmt.Sprintf("%d", info.ModTime().UnixNano()))
	if csStore, csKey := h.checksumStoreForRead(r, dp); csStore != nil {
		if cs, ok := csStore.Get(csKey); ok {
			w.Header().Set(headerFileChecksum, cs)
		} else if !info.IsDir() {
			var cs string
			if dp.tnt != nil {
				cs, err = FileChecksumRoot(dp.tnt.Root(), dp.rel)
			} else {
				cs, err = FileChecksum(dp.filePath)
			}
			if err == nil {
				w.Header().Set(headerFileChecksum, cs)
			}
		}
	}
	w.WriteHeader(http.StatusOK)
}
