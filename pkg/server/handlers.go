// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/web"
)

// parsePagination 从请求查询参数中解析 offset 和 limit。
// offset 默认 0，limit 默认 1000（上限 10000）。
func parsePagination(r *http.Request) (offset, limit int) {
	if o := r.URL.Query().Get("offset"); o != "" {
		_, _ = fmt.Sscanf(o, "%d", &offset)
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		_, _ = fmt.Sscanf(l, "%d", &limit)
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > 10000 {
		limit = 1000
	}
	return
}

type Handlers struct {
	cfgPtr        *atomic.Pointer[Config]
	version       string
	buildAt       string
	checksumStore ChecksumStoreIface
	uploadStore   UploadStoreIface
	tunnelHandler http.Handler
	logger        *slog.Logger
	metrics       *Metrics
	muxMetrics    *mux.Metrics
	shareStore    *ShareStore
	routeTable    *hub.RouteTable
	handler       http.Handler // mux wrapped with metricsMiddleware
	cloudMgr      *CloudDownloadManager
}

// TunnelUpdater 是隧道处理器密钥热替换接口。
// cmd/sproxy 的 SIGHUP 处理流程通过此接口在运行时替换隧道密钥。
type TunnelUpdater interface {
	UpdateKey(key []byte)
}

// TunnelHandler 返回隧道处理器，用于 SIGHUP 时热替换密钥。
func (h *Handlers) TunnelHandler() http.Handler {
	return h.tunnelHandler
}

// RegisterRoutesOpts 是 RegisterRoutes 的选项参数结构体。
type RegisterRoutesOpts struct {
	Mux        *http.ServeMux
	CfgPtr     *atomic.Pointer[Config]
	Version    string
	BuildAt    string
	TunnelKey  []byte
	Logger     *slog.Logger
	RouteTable *hub.RouteTable
}

// RegisterRoutes 将所有 HTTP 路由注册到 mux 上，并返回 *Handlers。
// 调用方应在进程退出前调用 (*Handlers).Close() 以释放后台 goroutine 与持久化资源。
func RegisterRoutes(_ context.Context, opts RegisterRoutesOpts) *Handlers {
	mux := opts.Mux
	cfg := opts.CfgPtr.Load()
	log := defaultLogger(opts.Logger)

	// 初始化 ChecksumStore
	cs := NewChecksumStore(cfg.UploadsDir, log.With("component", "checksum_store"))

	h := &Handlers{
		cfgPtr:        opts.CfgPtr,
		version:       opts.Version,
		buildAt:       opts.BuildAt,
		checksumStore: cs,
		uploadStore:   NewUploadStore(cfg.UploadsDir, cfg.UploadSessionTTL, log.With("component", "upload_store")),
		logger:        log,
		metrics:       NewMetrics(),
		shareStore:    NewShareStore(),
		routeTable:    opts.RouteTable,
	}

	// 初始化 StorageManager 和 CloudDownloadManager
	sm := NewStorageManager(cfg.UploadsDir, cfg.MaxStorageBytes, cs, log.With("component", "storage"))
	cloudCfg := &CloudDownloadConfig{
		SyncThreshold: cfg.CloudSyncThreshold,
		MaxConcurrent: cfg.CloudMaxConcurrent,
		TaskTTL:       parseDuration(cfg.CloudTaskTTL, 24*time.Hour),
		FailedTaskTTL: parseDuration(cfg.CloudFailedTaskTTL, 1*time.Hour),
	}
	h.cloudMgr = NewCloudDownloadManager(cfg.UploadsDir, sm, cs, log.With("component", "cloud"), cloudCfg)

	// 本地路由子 mux（无 authMiddleware，隧道密钥已提供认证）
	localMux := http.NewServeMux()
	localMux.HandleFunc("POST /upload", h.upload)
	localMux.HandleFunc("GET /download", h.download)
	localMux.HandleFunc("POST /delete", h.delete)
	localMux.HandleFunc("POST /rename", h.rename)
	localMux.HandleFunc("GET /api/files", h.listFiles)
	localMux.HandleFunc("HEAD /api/files/stat", h.stat)
	localMux.HandleFunc("POST /mkdir", h.mkdir)
	localMux.HandleFunc("POST /rmdir", h.rmdir)
	localMux.HandleFunc("GET /api/files/search", h.searchFiles)
	localMux.HandleFunc("POST /api/batch/delete", h.batchDelete)
	localMux.HandleFunc("POST /api/batch/rename", h.batchRename)

	localMux.HandleFunc("POST /api/archive", h.archiveHandler)
	localMux.HandleFunc("GET /api/archive-dir", h.archiveDirHandler)
	localMux.HandleFunc("GET /api/versions", h.listVersionsHandler)
	localMux.HandleFunc("POST /api/versions/restore", h.restoreVersionHandler)
	localMux.HandleFunc("DELETE /api/versions", h.deleteVersionHandler)
	localMux.HandleFunc("GET /api/stats", h.statsHandler)

	// 分块上传/下载路由（本地）
	localMux.HandleFunc("POST /upload/init", h.uploadInit)
	localMux.HandleFunc("POST /upload/chunk", h.uploadChunk)
	localMux.HandleFunc("GET /upload/status", h.uploadStatus)
	localMux.HandleFunc("POST /upload/complete", h.uploadComplete)
	localMux.HandleFunc("GET /download/chunk", h.downloadChunk)

	// gzip + 速率限制 + CORS 中间件链
	var apiHandler http.Handler = localMux
	apiHandler = GzipMiddleware(log.With("component", "gzip"))(apiHandler)
	if cfg.RateLimit.Enabled {
		rl := NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "rate_limiter"))
		apiHandler = rl.Middleware(apiHandler)
	}
	apiHandler = CORSMiddleware(cfg.CORS, log.With("component", "cors"))(apiHandler)

	h.tunnelHandler = tunnel.NewLocalHandler(opts.TunnelKey, requestLogMiddleware(log.With("component", "request"), apiHandler), log.With("component", "tunnel"))

	mux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	mux.HandleFunc("GET /download", h.authMiddleware(h.download))
	mux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	mux.HandleFunc("POST /rename", h.authMiddleware(h.rename))
	mux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	mux.HandleFunc("HEAD /api/files/stat", h.authMiddleware(h.stat))
	mux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	mux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	mux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	mux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	mux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))
	mux.HandleFunc("POST /mkdir", h.authMiddleware(h.mkdir))
	mux.HandleFunc("POST /rmdir", h.authMiddleware(h.rmdir))
	mux.HandleFunc("GET /api/files/search", h.authMiddleware(h.searchFiles))
	mux.HandleFunc("POST /api/batch/delete", h.authMiddleware(h.batchDelete))
	mux.HandleFunc("POST /api/batch/rename", h.authMiddleware(h.batchRename))
	mux.HandleFunc("POST /api/archive", h.authMiddleware(h.archiveHandler))
	mux.HandleFunc("GET /api/archive-dir", h.authMiddleware(h.archiveDirHandler))
	mux.HandleFunc("GET /api/versions", h.authMiddleware(h.listVersionsHandler))
	mux.HandleFunc("POST /api/versions/restore", h.authMiddleware(h.restoreVersionHandler))
	mux.HandleFunc("DELETE /api/versions", h.authMiddleware(h.deleteVersionHandler))
	mux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	mux.HandleFunc("POST /api/share", h.authMiddleware(h.createShareHandler))
	mux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 云端下载 API（localMux：隧道认证）
	localMux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	localMux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	localMux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	localMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	// 云端下载 API（主 mux：Bearer auth）
	mux.HandleFunc("POST /api/cloud/download", h.authMiddleware(h.cloudCreateDownload))
	mux.HandleFunc("GET /api/cloud/tasks", h.authMiddleware(h.cloudListTasks))
	mux.HandleFunc("GET /api/cloud/tasks/{id}", h.authMiddleware(h.cloudGetTask))
	mux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.authMiddleware(h.cloudCancelTask))
	mux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.authMiddleware(h.cloudDeleteTask))

	// Hub 管理 API（中继系统），需鉴权
	if opts.RouteTable != nil {
		mux.HandleFunc("GET /api/hub/nodes", h.authMiddleware(h.hubNodesHandler))
		mux.HandleFunc("DELETE /api/hub/nodes/{id}", h.authMiddleware(h.hubRemoveNodeHandler))
		mux.HandleFunc("GET /api/hub/stats", h.authMiddleware(h.hubStatsHandler))
	}

	mux.HandleFunc("GET /healthz", h.healthz)
	mux.HandleFunc("GET /version", h.versionHandler)
	mux.HandleFunc("GET /metrics", h.MetricsHandler)
	mux.Handle("POST /tunnel", h.tunnelHandler)

	// Web UI
	subFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		h.logger.Error("web static fs sub error", "error", err)
	} else {
		fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(subFS)))
		mux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:;")
			fileServer.ServeHTTP(w, r)
		}))
	}

	// GET / -> /ui/ 重定向
	mux.HandleFunc("GET /", h.webRedirect)

	h.handler = h.metricsMiddleware(mux)

	return h
}

// Close 释放 Handlers 持有的后台资源：停止 UploadStore 的 persist/cleanup goroutine。
// 在进程退出前应调用一次（通常通过 defer h.Close()）。多次调用是安全的。
func (h *Handlers) Close() error {
	if h.uploadStore != nil {
		h.uploadStore.Stop()
	}
	return nil
}

// Handler 返回包装了 metricsMiddleware 的 HTTP handler，用于 http.Server.Handler。
func (h *Handlers) Handler() http.Handler {
	return h.handler
}

// safePath 在 UploadsDir 下安全拼接 remotePath，结果不越界。
// remotePath 必须已通过 ValidateFilePath 校验。返回安全绝对路径，失败时返回空字符串。
func (h *Handlers) safePath(remotePath string) string {
	if remotePath == "" {
		return ""
	}
	cfg := h.cfgPtr.Load()
	return joinSafePath(cfg.UploadsDir, remotePath)
}

// writeFileAtomically 将 src 原子写入 dstPath，同时计算 SHA-256 哈希。
// 先写到唯一临时文件，再 os.Rename，防止部分写入与并发冲突。
func writeFileAtomically(dstPath string, src io.Reader) (checksum string, written int64, err error) {
	tmpFile, err := os.CreateTemp(filepath.Dir(dstPath), filepath.Base(dstPath)+".tmp.*")
	if err != nil {
		return "", 0, fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()
	defer os.Remove(tmpPath)

	hash := sha256.New()
	mw := io.MultiWriter(tmpFile, hash)
	written, err = io.Copy(mw, src)
	if err != nil {
		tmpFile.Close()
		return "", written, fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return "", written, fmt.Errorf("关闭临时文件失败: %w", err)
	}
	checksum = hex.EncodeToString(hash.Sum(nil))
	if err := atomicRename(tmpPath, dstPath); err != nil {
		return checksum, written, fmt.Errorf("重命名临时文件失败: %w", err)
	}
	return checksum, written, nil
}

// atomicRename 尝试 os.Rename，如果失败（Windows 并发场景），
// 先删除目标再重命名，并使用短退避重试以应对 Windows 句柄释放延迟。
func atomicRename(src, dst string) error {
	// 快速路径：直接重命名
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// 慢速路径：删除目标文件，然后重命名临时文件
	// 使用短退避重试，解决 Windows 上并发 Rename 导致的"Access is denied"
	const maxAttempts = 5
	const baseDelay = 2 * time.Millisecond
	for i := range maxAttempts {
		_ = os.Remove(dst)
		if err := os.Rename(src, dst); err == nil {
			return nil
		}
		time.Sleep(baseDelay << i)
	}
	return os.Rename(src, dst) // 最后一次尝试，返回最终错误
}

// resolveFilePath 校验 filename 并生成安全的 UploadsDir 下完整路径。
// 返回已验证的相对路径和绝对路径。校验失败时返回 false。
func (h *Handlers) resolveFilePath(w http.ResponseWriter, filename string) (remotePath, fullPath string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidFilename}, http.StatusBadRequest)
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, fullPath, true
}

// resolveFilePathHTTP 供非 JSON handler 使用（如 stat 返回普通 http.Error）。
func (h *Handlers) resolveFilePathHTTP(w http.ResponseWriter, filename string) (remotePath, fullPath string, ok bool) {
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return "", "", false
	}
	fullPath = h.safePath(remotePath)
	if fullPath == "" {
		http.Error(w, "invalid file path", http.StatusBadRequest)
		return "", "", false
	}
	return remotePath, fullPath, true
}

// handleDuplicateFile 检查文件是否存在，处理重复上传和版本管理逻辑。
// 返回 true 表示已处理（调用方应 return）。
func (h *Handlers) handleDuplicateFile(w http.ResponseWriter, filePath, expectedChecksum, remotePath string) bool {
	stat, statErr := os.Stat(filePath)
	if statErr != nil {
		return false // 文件不存在，继续正常上传
	}
	if verifyFileWithChecksum(filePath, expectedChecksum) {
		// 幂等上传：文件已存在且 checksum 匹配，先保存版本后返回
		h.saveVersionBeforeOverwrite(remotePath)
		sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
		return true
	}
	cfg := h.cfgPtr.Load()
	if cfg.Versioning.Enabled {
		// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本
		h.saveVersionBeforeOverwrite(remotePath)
		return false // 继续执行写入流程，用新内容覆盖现有文件
	}
	// checksum 不匹配：冲突，需保留现有文件
	h.logger.Warn("文件已存在，但校验失败", "file_name", remotePath)
	sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
	return true
}

// parseRenameParams 从请求中提取重命名参数：from、to 和 X-File-Checksum。
func parseRenameParams(r *http.Request) (from, to, checksum string, err error) {
	from = r.URL.Query().Get("from")
	to = r.URL.Query().Get("to")
	if from == "" || to == "" {
		return "", "", "", fmt.Errorf("from 和 to 都不能为空")
	}
	from, err = ValidateFilePath(from)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的源路径")
	}
	to, err = ValidateFilePath(to)
	if err != nil {
		return "", "", "", fmt.Errorf("无效的目标路径")
	}
	checksum = r.Header.Get(headerFileChecksum)
	return from, to, checksum, nil
}

// resolveRenamePaths 计算 from 和 to 对应的安全绝对路径。
func resolveRenamePaths(h *Handlers, w http.ResponseWriter, from, to string) (fromPath, toPath string, ok bool) {
	fromPath = h.safePath(from)
	toPath = h.safePath(to)
	if fromPath == "" || toPath == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgInvalidPath}, http.StatusBadRequest)
		return "", "", false
	}
	return fromPath, toPath, true
}

// renameOpCtx 是 executeRename 的参数集合，用于减少函数参数数量（go:S107）。
type renameOpCtx struct {
	h                *Handlers
	w                http.ResponseWriter
	fromPath         string
	toPath           string
	from             string
	to               string
	expectedChecksum string
	logger           *slog.Logger
}

// executeRename 校验 checksum、执行 Rename、更新 checksumStore。
// 返回 nil 表示成功；返回 error 表示失败（已在内部发送响应）。
func executeRename(ctx renameOpCtx) error {
	if _, err := os.Stat(ctx.fromPath); os.IsNotExist(err) {
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return err
	}
	if _, err := os.Stat(ctx.toPath); err == nil {
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "目标路径已存在"}, http.StatusConflict)
		return err
	}
	if !verifyFileWithChecksum(ctx.fromPath, ctx.expectedChecksum) {
		ctx.logger.Warn("rename checksum 校验失败", "from", ctx.from)
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: errMsgSrcChecksumFailed}, http.StatusBadRequest)
		return fmt.Errorf("checksum mismatch")
	}
	if err := os.MkdirAll(filepath.Dir(ctx.toPath), 0755); err != nil {
		ctx.logger.Error(errMsgCreateParentDirFailed, "to", ctx.to, "error", err.Error())
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: errMsgCreateParentDirFailed}, http.StatusInternalServerError)
		return err
	}
	if err := atomicRename(ctx.fromPath, ctx.toPath); err != nil {
		ctx.logger.Error("重命名失败", "from", ctx.from, "to", ctx.to, "error", err.Error())
		sendJSONResponse(ctx.w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return err
	}
	ctx.h.checksumStore.Rename(ctx.from, ctx.to)
	return nil
}

// processBatchRenameItem 处理单条批量重命名操作。
func (h *Handlers) processBatchRenameItem(op BatchRenameOp, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: op.From + " -> " + op.To}
	from, err := ValidateFilePath(op.From)
	if err != nil {
		result.Message = "无效的源路径"
		return result
	}
	to, err := ValidateFilePath(op.To)
	if err != nil {
		result.Message = "无效的目标路径"
		return result
	}
	if from == to {
		result.Success = true
		result.Message = "源与目标相同，无需移动"
		return result
	}
	fromPath := h.safePath(from)
	toPath := h.safePath(to)
	if fromPath == "" || toPath == "" {
		result.Message = "无效的文件路径"
		return result
	}
	if _, err := os.Stat(fromPath); os.IsNotExist(err) {
		result.Message = "源文件不存在"
		return result
	}
	if _, err := os.Stat(toPath); err == nil {
		result.Message = "目标路径已存在"
		return result
	}
	if op.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	if !verifyFileWithChecksum(fromPath, op.Checksum) {
		logger.Warn("batch rename checksum 不匹配", "from", op.From)
		result.Message = errMsgSrcChecksumFailed
		return result
	}
	if err := os.MkdirAll(filepath.Dir(toPath), 0755); err != nil {
		logger.Error(errMsgCreateParentDirFailed, "to", to, "error", err.Error())
		result.Message = "创建父目录失败"
		return result
	}
	if err := atomicRename(fromPath, toPath); err != nil {
		logger.Error("batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
		result.Message = "重命名失败"
		return result
	}
	h.checksumStore.Rename(from, to)
	return BatchOperationResult{
		Filename: op.From + " -> " + op.To,
		Success:  true,
		Message:  "重命名成功",
	}
}

// resolveListDir 处理 listFiles 的 subdir 参数，返回目标目录。
func (h *Handlers) resolveListDir(w http.ResponseWriter, r *http.Request) (targetDir string, ok bool) {
	cfg := h.cfgPtr.Load()
	targetDir = cfg.UploadsDir
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := ValidateFilePath(subdir); err != nil {
			h.logger.Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
			return "", false
		}
		targetDir = h.safePath(subdir)
		if targetDir == "" {
			h.logger.Warn("无效的子目录路径", "subdir", subdir)
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
			return "", false
		}
	}
	return targetDir, true
}

// sortFileEntries 按指定字段和顺序排序文件条目。
func sortFileEntries(entries []fileInfo, sortBy, sortOrder string) {
	switch sortBy {
	case "size":
		sort.SliceStable(entries, func(i, j int) bool {
			if sortOrder == "desc" {
				return entries[i].Size > entries[j].Size
			}
			return entries[i].Size < entries[j].Size
		})
	case "time":
		sort.SliceStable(entries, func(i, j int) bool {
			if sortOrder == "desc" {
				return entries[i].ModTime > entries[j].ModTime
			}
			return entries[i].ModTime < entries[j].ModTime
		})
	default: // "name"
		if sortOrder == "desc" {
			sort.SliceStable(entries, func(i, j int) bool {
				return entries[i].Name > entries[j].Name
			})
		} else {
			sort.SliceStable(entries, func(i, j int) bool {
				return entries[i].Name < entries[j].Name
			})
		}
	}
}

// paginateEntries 对文件列表进行分页。
func paginateEntries(entries []fileInfo, offset, limit int) []fileInfo {
	total := len(entries)
	start := min(offset, total)
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	return entries[start:end]
}

// collectSearchResults 递归搜索 uploads_dir 下文件名包含 queryLower 的文件。
func (h *Handlers) collectSearchResults(rootsDir, queryLower string, csMap map[string]string) []fileInfo {
	var results []fileInfo
	_ = filepath.WalkDir(rootsDir, func(path string, d fs.DirEntry, err error) error {
		return h.searchWalkDirCallback(rootsDir, path, d, err, queryLower, csMap, &results)
	})
	return results
}

// searchWalkDirCallback 是 collectSearchResults 中 filepath.WalkDir 的回调函数。
func (h *Handlers) searchWalkDirCallback(rootsDir, path string, d fs.DirEntry, err error, queryLower string, csMap map[string]string, results *[]fileInfo) error {
	if err != nil {
		return nil
	}
	rel, _ := filepath.Rel(rootsDir, path)
	if rel == "." {
		return nil
	}
	if d.Name() == ".checksums.json" || d.Name() == chunkedDirName || d.Name() == versionsDirName || d.Name() == cloudDirName || d.Name() == ".__downloads__" {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if !strings.Contains(strings.ToLower(d.Name()), queryLower) {
		return nil
	}
	if d.IsDir() {
		*results = append(*results, fileInfo{
			Name:  filepath.ToSlash(rel),
			IsDir: true,
		})
		return nil
	}
	info, err := d.Info()
	if err != nil {
		return nil
	}
	fi := fileInfo{
		Name:    filepath.ToSlash(rel),
		Size:    info.Size(),
		ModTime: info.ModTime().UnixNano(),
	}
	if cs, ok := csMap[filepath.ToSlash(rel)]; ok {
		fi.Checksum = cs
	}
	*results = append(*results, fi)
	return nil
}

// requestLogMiddleware 记录 HTTP 请求的基本信息：方法、路径、远程地址、耗时。
func requestLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("请求", "method", r.Method, "path", r.URL.Path,
			"remote_addr", r.RemoteAddr, "user_agent", r.UserAgent(), "duration", time.Since(start))
	})
}

func (h *Handlers) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	if h.uploadStore != nil {
		if err := h.uploadStore.Health(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("UploadStore: " + err.Error()))
			return
		}
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

func (h *Handlers) versionHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(headerContentType, contentTypeTextPlain)
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Version: %s\nBuildAt: %s\n", h.version, h.buildAt)
}

// hubNodesHandler 返回在线节点列表。
func (h *Handlers) hubNodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	nodes := h.routeTable.List()
	type nodeResp struct {
		ID        string `json:"id"`
		Addr      string `json:"addr,omitempty"`
		Connected string `json:"connected,omitempty"`
	}
	resp := make([]nodeResp, 0, len(nodes))
	for _, n := range nodes {
		resp = append(resp, nodeResp{
			ID:        string(n.ID),
			Addr:      n.Addr,
			Connected: n.Connected.Format("2006-01-02T15:04:05Z07:00"),
		})
	}
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(resp)
}

// hubRemoveNodeHandler 踢出指定节点。
func (h *Handlers) hubRemoveNodeHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	id := hub.NodeID(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing node id", http.StatusBadRequest)
		return
	}
	h.routeTable.Remove(id)
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": string(id)})
}

// hubStatsHandler 返回中继统计。
func (h *Handlers) hubStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, errMsgHubNotEnabled, http.StatusNotFound)
		return
	}
	count := h.routeTable.NodeCount()
	w.Header().Set(headerContentType, contentTypeJSON)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"nodes_connected": count,
	})
}

// parseUploadMultipart 解析上传请求的 multipart 表单，返回文件、文件信息、期望的 checksum 和错误。
func (h *Handlers) parseUploadMultipart(w http.ResponseWriter, r *http.Request, logger *slog.Logger) (file multipart.File, handler *multipart.FileHeader, expectedChecksum string, ok bool) {
	r.Body = http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
	if err := r.ParseMultipartForm(size.MultipartBufSize); err != nil {
		logger.Warn("解析 multipart 失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体过大或解析失败"}, http.StatusRequestEntityTooLarge)
		return nil, nil, "", false
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.Error("读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return nil, nil, "", false
	}

	expectedChecksum = r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		file.Close()
		logger.Warn("缺少 X-File-Checksum 请求头")
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return nil, nil, "", false
	}
	return file, handler, expectedChecksum, true
}

// setUploadResponseHeaders 设置上传成功后的响应头（checksum、mtime）。
func (h *Handlers) setUploadResponseHeaders(w http.ResponseWriter, r *http.Request, remotePath, filePath, serverChecksum string, logger *slog.Logger) {
	w.Header().Set(headerFileChecksum, serverChecksum)
	h.checksumStore.Set(remotePath, serverChecksum)

	// 处理文件修改时间
	if mtimeStr := r.Header.Get(headerFileMTime); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(filePath, modTime, modTime); err != nil {
				logger.Warn("设置文件时间戳失败", "file_name", remotePath, "error", err)
			}
		}
	}
}

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	file, handler, expectedChecksum, ok := h.parseUploadMultipart(w, r, logger)
	if !ok {
		return
	}
	defer file.Close()

	// 路径校验（支持子目录）
	remotePathStr := r.Header.Get("X-File-Path")
	if remotePathStr == "" {
		remotePathStr = handler.Filename
	}
	remotePath, filePath, ok := h.resolveFilePath(w, remotePathStr)
	if !ok {
		return
	}
	logger.Debug("上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	// 重复检测与版本管理
	if h.handleDuplicateFile(w, filePath, expectedChecksum, remotePath) {
		return
	}

	// 原子写入 + 流式哈希
	serverChecksum, _, err := writeFileAtomically(filePath, file)
	if err != nil {
		logger.Error("保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgSaveFailed}, http.StatusInternalServerError)
		return
	}

	if serverChecksum != expectedChecksum {
		os.Remove(filePath)
		logger.Warn("文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	h.setUploadResponseHeaders(w, r, remotePath, filePath, serverChecksum, logger)

	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件上传成功, size: %d", handler.Size),
		Checksum: serverChecksum,
	}, http.StatusOK)
	if h.metrics != nil {
		h.metrics.RecordUpload(handler.Size)
	}
}

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

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

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
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	expectedChecksum := r.Header.Get(headerFileChecksum)
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		logger.Warn("X-File-Checksum 为空", "file_name", remotePath)
		return
	}

	if !verifyFileWithChecksum(filePath, expectedChecksum) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件校验失败"}, http.StatusBadRequest)
		logger.Warn("文件校验失败", "file_name", remotePath)
		return
	}

	if err := os.Remove(filePath); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "删除文件失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Delete(remotePath)
	if h.metrics != nil {
		h.metrics.RecordDelete()
	}
	sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件删除成功: %s", remotePath)}, http.StatusOK)
}

// batchDelete 处理 POST /api/batch/delete。
// 请求体 JSON：{"files": [{"file_name": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余文件。
// processBatchDeleteItem 处理单条文件删除操作。
func (h *Handlers) processBatchDeleteItem(f BatchDeleteFile, logger *slog.Logger) BatchOperationResult {
	result := BatchOperationResult{Filename: f.Filename}
	remotePath, err := ValidateFilePath(f.Filename)
	if err != nil {
		result.Message = errMsgInvalidFilename
		return result
	}
	filePath := h.safePath(remotePath)
	if filePath == "" {
		result.Message = "无效的文件路径"
		return result
	}
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		result.Success = true
		result.Message = "文件不存在（幂等删除）"
		return result
	}
	if f.Checksum == "" {
		result.Message = "缺少 checksum"
		return result
	}
	// 校验 checksum
	valid := verifyFileWithChecksum(filePath, f.Checksum)
	// 仍然执行删除，但标记校验失败
	if err := os.Remove(filePath); err != nil {
		result.Message = "删除失败"
	} else {
		h.checksumStore.Delete(remotePath)
		result.Success = true
		result.Message = "删除成功"
		if !valid {
			result.Message = "删除成功（checksum 不匹配，文件内容可能已变更）"
			logger.Warn("删除时 checksum 不匹配", "file_name", remotePath)
		}
	}
	return result
}

// batchDelete 处理 POST /api/batch/delete。
// 请求体 JSON：{"files": [{"file_name": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余文件。
func (h *Handlers) batchDelete(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	if len(req.Files) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "files 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "delete")
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		results = append(results, h.processBatchDeleteItem(f, logger))
	}
	sendJSONResponse(w, BatchDeleteResponse{Results: results}, http.StatusOK)
}

// batchRename 处理 POST /api/batch/rename。
// 请求体 JSON：{"operations": [{"from": "...", "to": "...", "checksum": "..."}]}
// 继续处理模式：单条失败不影响其余操作。
func (h *Handlers) batchRename(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB
	var req BatchRenameRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无法解析请求体"}, http.StatusBadRequest)
		return
	}
	if len(req.Operations) == 0 {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "operations 不能为空"}, http.StatusBadRequest)
		return
	}
	logger := h.logger.With("batch", "rename")
	results := make([]BatchOperationResult, 0, len(req.Operations))
	for _, op := range req.Operations {
		result := h.processBatchRenameItem(op, logger)
		results = append(results, result)
	}
	sendJSONResponse(w, BatchRenameResponse{Results: results}, http.StatusOK)
}

type fileInfo struct {
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	ModTime  int64  `json:"mod_time"` // UnixNano
	IsDir    bool   `json:"is_dir"`   // 是否为目录
}

type listResponse struct {
	Files  []fileInfo `json:"files"`
	Total  int        `json:"total"`
	Offset int        `json:"offset"`
	Limit  int        `json:"limit"`
}

func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	// 支持按层级查询：?subdir=path 列出指定子目录，默认列出根目录
	targetDir, ok := h.resolveListDir(w, r)
	if !ok {
		return
	}

	// 分页参数
	offset, limit := parsePagination(r)

	// 排序参数
	sortBy := r.URL.Query().Get("sort")
	sortOrder := r.URL.Query().Get("order")
	if sortOrder != "desc" {
		sortOrder = "asc"
	}

	entries, err := os.ReadDir(targetDir)
	h.logger.Info("读取目录", "dir", targetDir)
	if os.IsNotExist(err) {
		sendJSONResponse(w, listResponse{Files: []fileInfo{}, Total: 0, Offset: offset, Limit: limit}, http.StatusOK)
		return
	}
	if err != nil {
		h.logger.Error("读取上传目录失败", "error", err.Error())
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusInternalServerError)
		return
	}

	csMap := h.checksumStore.GetAll()

	// 收集所有条目（跳过内部目录）
	subdir := r.URL.Query().Get("subdir")
	allFiles := h.buildFileListEntries(entries, csMap, subdir)

	// 排序
	sortFileEntries(allFiles, sortBy, sortOrder)

	total := len(allFiles)

	// 分页
	files := paginateEntries(allFiles, offset, limit)
	sendJSONResponse(w, listResponse{Files: files, Total: total, Offset: offset, Limit: limit}, http.StatusOK)
}

// buildFileListEntries 从目录条目构建文件信息列表，排除内部目录并附加 checksum。
func (h *Handlers) buildFileListEntries(entries []os.DirEntry, csMap map[string]string, subdir string) []fileInfo {
	allFiles := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".checksums.json" || e.Name() == chunkedDirName || e.Name() == versionsDirName || e.Name() == cloudDirName || e.Name() == ".__downloads__" {
			continue
		}
		if e.IsDir() {
			allFiles = append(allFiles, fileInfo{
				Name:  e.Name(),
				IsDir: true,
			})
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		fi := fileInfo{
			Name:    e.Name(),
			Size:    info.Size(),
			ModTime: info.ModTime().UnixNano(),
		}
		relName := e.Name()
		if subdir != "" {
			cleaned, _ := ValidateFilePath(subdir)
			relName = filepath.ToSlash(filepath.Join(cleaned, e.Name()))
		}
		if cs, ok := csMap[relName]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}
	return allFiles
}

func (h *Handlers) webRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}

// searchFiles 处理 GET /api/files/search?q=keyword。
// 递归搜索 uploads_dir 下文件名包含 q 的文件，不区分大小写。
// 返回与 listFiles 相同的 fileInfo 结构。
func (h *Handlers) searchFiles(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
		return
	}
	qLower := strings.ToLower(q)

	cfg := h.cfgPtr.Load()
	csMap := h.checksumStore.GetAll()

	results := h.collectSearchResults(cfg.UploadsDir, qLower, csMap)
	sendJSONResponse(w, map[string]any{"files": results}, http.StatusOK)
}

// rename 处理 POST /rename?from=<old>&to=<new>。
// 与 delete 对称，要求 X-File-Checksum 头校验源文件，避免误覆盖。
// 目标路径已存在时返回 409；服务端会自动 mkdir -p 中间目录。
func (h *Handlers) rename(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get(headerRequestID)
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	from, to, expectedChecksum, err := parseRenameParams(r)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: err.Error()}, http.StatusBadRequest)
		return
	}

	if from == to {
		sendJSONResponse(w, UploadResponse{Success: true, Message: "源与目标相同，无需移动"}, http.StatusOK)
		return
	}

	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: errMsgMissingChecksum}, http.StatusBadRequest)
		return
	}

	fromPath, toPath, ok := resolveRenamePaths(h, w, from, to)
	if !ok {
		return
	}

	if err := executeRename(renameOpCtx{
		h:                h,
		w:                w,
		fromPath:         fromPath,
		toPath:           toPath,
		from:             from,
		to:               to,
		expectedChecksum: expectedChecksum,
		logger:           logger,
	}); err != nil {
		return
	}

	logger.Info("文件已重命名", "from", from, "to", to, "checksum", expectedChecksum)
	sendJSONResponse(w, UploadResponse{
		Success:  true,
		Message:  fmt.Sprintf("文件已重命名: %s -> %s", from, to),
		Checksum: expectedChecksum,
	}, http.StatusOK)
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

func verifyFileWithChecksum(filePath, expectedChecksum string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyChecksum(expectedChecksum, f)
}
