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
		fmt.Sscanf(o, "%d", &offset)
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		fmt.Sscanf(l, "%d", &limit)
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
	checksumStore *ChecksumStore
	uploadStore   *UploadStore
	tunnelHandler http.Handler
	logger        *slog.Logger
	metrics       *Metrics
	muxMetrics    *mux.Metrics
	shareStore    *ShareStore
	routeTable    *hub.RouteTable
	handler       http.Handler // mux wrapped with metricsMiddleware
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

// RegisterRoutes 将所有 HTTP 路由注册到 mux 上，并返回 *Handlers。
// 调用方应在进程退出前调用 (*Handlers).Close() 以释放后台 goroutine 与持久化资源。
func RegisterRoutes(ctx context.Context, mux *http.ServeMux, cfgPtr *atomic.Pointer[Config], version, buildAt string, tunnelKey []byte, logger *slog.Logger, routeTable *hub.RouteTable) *Handlers {
	cfg := cfgPtr.Load()
	log := defaultLogger(logger)

	// 初始化 ChecksumStore
	cs := NewChecksumStore(cfg.UploadsDir, log.With("component", "checksum_store"))

	h := &Handlers{
		cfgPtr:        cfgPtr,
		version:       version,
		buildAt:       buildAt,
		checksumStore: cs,
		uploadStore:   NewUploadStore(cfg.UploadsDir, cfg.UploadSessionTTL, log.With("component", "upload_store")),
		logger:        log,
		metrics:       NewMetrics(),
		shareStore:    NewShareStore(),
		routeTable:    routeTable,
	}

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

	h.tunnelHandler = tunnel.NewLocalHandler(tunnelKey, requestLogMiddleware(log.With("component", "request"), apiHandler), log.With("component", "tunnel"))

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

	// Hub 管理 API（中继系统），需鉴权
	if routeTable != nil {
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
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
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "Version: %s\nBuildAt: %s\n", h.version, h.buildAt)
}

// hubNodesHandler 返回在线节点列表。
func (h *Handlers) hubNodesHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, "hub not enabled", http.StatusNotFound)
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
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// hubRemoveNodeHandler 踢出指定节点。
func (h *Handlers) hubRemoveNodeHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, "hub not enabled", http.StatusNotFound)
		return
	}
	id := hub.NodeID(r.PathValue("id"))
	if id == "" {
		http.Error(w, "missing node id", http.StatusBadRequest)
		return
	}
	h.routeTable.Remove(id)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "removed", "node": string(id)})
}

// hubStatsHandler 返回中继统计。
func (h *Handlers) hubStatsHandler(w http.ResponseWriter, r *http.Request) {
	if h.routeTable == nil {
		http.Error(w, "hub not enabled", http.StatusNotFound)
		return
	}
	count := h.routeTable.NodeCount()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"nodes_connected": count,
	})
}

func (h *Handlers) upload(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	cfg := h.cfgPtr.Load()
	// 限制请求体大小，防止 OOM；超过 MaxBytesReader 会向客户端返回 413
	r.Body = http.MaxBytesReader(w, r.Body, size.UploadBodyLimit)
	// 仅在内存中缓冲 MultipartBufSize，超出部分由 stdlib 落临时文件
	if err := r.ParseMultipartForm(size.MultipartBufSize); err != nil {
		logger.Warn("解析 multipart 失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体过大或解析失败"}, http.StatusRequestEntityTooLarge)
		return
	}

	file, handler, err := r.FormFile("file")
	if err != nil {
		logger.Error("读取文件失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "读取文件失败"}, http.StatusBadRequest)
		return
	}
	defer file.Close()

	expectedChecksum := r.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		logger.Warn("缺少 X-File-Checksum 请求头")
		sendJSONResponse(w, UploadResponse{Success: false, Message: "缺少 X-File-Checksum 请求头"}, http.StatusBadRequest)
		return
	}

	// 路径校验（支持子目录）
	// Go >=1.26 的 mime/multipart 会对 Content-Disposition 中的 filename 调用
	// filepath.Base，导致 "dir/file.txt" 被截断为 "file.txt"。
	// 因此优先使用 X-File-Path 头获取完整路径，回退到 handler.Filename 兼容旧客户端。
	remotePathStr := r.Header.Get("X-File-Path")
	if remotePathStr == "" {
		remotePathStr = handler.Filename
	}
	remotePath, err := ValidateFilePath(remotePathStr)
	if err != nil {
		logger.Warn("无效的文件名", "file_name", remotePathStr, "error", err)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	logger.Debug("上传路径", "remote_path", remotePath, "header", r.Header.Get("X-File-Path"), "multipart", handler.Filename)

	uploadDir := cfg.UploadsDir
	filePath := filepath.Join(uploadDir, remotePath)
	if err := os.MkdirAll(filepath.Dir(filePath), 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}

	if stat, err := os.Stat(filePath); err == nil {
		if verifyFileWithChecksum(filePath, expectedChecksum) {
			// 幂等上传：文件已存在且 checksum 匹配，先保存版本后返回
			h.saveVersionBeforeOverwrite(remotePath)
			sendJSONResponse(w, UploadResponse{Success: true, Message: fmt.Sprintf("文件已上传成功, size: %d", stat.Size()), Checksum: expectedChecksum}, http.StatusOK)
			return
		}
		if cfg.Versioning.Enabled {
			// 版本管理启用时，checksum 不匹配视为有意覆盖旧版本
			h.saveVersionBeforeOverwrite(remotePath)
			// 继续执行下面的写入流程，用新内容覆盖现有文件
		} else {
			// checksum 不匹配：冲突，需保留现有文件
			logger.Warn("文件已存在，但校验失败", "file_name", remotePath)
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件已存在，但校验失败"}, http.StatusConflict)
			return
		}
	}

	dir := filepath.Dir(filePath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		logger.Error("创建目录失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目录失败"}, http.StatusInternalServerError)
		return
	}
	tempFile, err := os.CreateTemp(dir, filepath.Base(filePath)+".tmp.*")
	if err != nil {
		logger.Error("创建文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建文件失败"}, http.StatusInternalServerError)
		return
	}
	tmpPath := tempFile.Name()
	// .tmp 文件统一在错误路径或 rename 成功后由 defer os.Remove 兜底清理；
	// rename 成功后 .tmp 已不在原位，os.Remove 会无声失败，不影响成品文件。
	// 不使用 defer tempFile.Close()，因为正常路径需要在 rename 前显式 Close，
	// 双 close 在 Windows 上有句柄复用风险。下方错误路径手动 Close 后再 return。
	defer os.Remove(tmpPath)

	// 边写边算 SHA-256，复用同一份字节流
	sha256Hash := sha256.New()
	multiWriter := io.MultiWriter(tempFile, sha256Hash)

	if _, err := io.Copy(multiWriter, file); err != nil {
		_ = tempFile.Close()
		logger.Error("保存文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}
	if err := tempFile.Close(); err != nil {
		logger.Error("关闭临时文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "保存文件失败"}, http.StatusInternalServerError)
		return
	}

	serverChecksum := hex.EncodeToString(sha256Hash.Sum(nil))
	if serverChecksum != expectedChecksum {
		logger.Warn("文件 SHA-256 校验失败", "server", serverChecksum, "client", expectedChecksum, "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	if err := os.Rename(tmpPath, filePath); err != nil {
		// Windows cannot rename over an existing file; remove and retry.
		if rmErr := os.Remove(filePath); rmErr == nil {
			if err2 := os.Rename(tmpPath, filePath); err2 == nil {
				goto afterRename
			}
		}
		logger.Error("重命名文件失败", "error", err.Error(), "file_name", remotePath)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "重命名文件失败"}, http.StatusInternalServerError)
		return
	}
afterRename:

	w.Header().Set("X-File-Checksum", serverChecksum)
	h.checksumStore.Set(remotePath, serverChecksum)

	// 处理文件修改时间
	if mtimeStr := r.Header.Get("X-File-MTime"); mtimeStr != "" {
		var mtimeInt int64
		if _, err := fmt.Sscanf(mtimeStr, "%d", &mtimeInt); err == nil && mtimeInt > 0 {
			modTime := time.Unix(0, mtimeInt)
			if err := os.Chtimes(filePath, modTime, modTime); err != nil {
				logger.Warn("设置文件时间戳失败", "file_name", remotePath, "error", err)
			}
		}
	}

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
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	filePath := filepath.Join(cfg.UploadsDir, remotePath)

	file, err := os.Open(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		} else {
			h.logger.Error("打开文件失败", "file_name", remotePath, "error", err.Error())
			sendJSONResponse(w, UploadResponse{Success: false, Message: "打开文件失败"}, http.StatusInternalServerError)
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
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Accept-Ranges", "bytes")

	// 设置 SHA-256 checksum 响应头：优先从 store 读取，回退实时计算
	if cs, ok := h.checksumStore.Get(remotePath); ok {
		w.Header().Set("X-File-Checksum", cs)
	} else if cs, err := FileChecksum(filePath); err == nil {
		w.Header().Set("X-File-Checksum", cs)
	} else {
		h.logger.Warn("计算文件 checksum 失败", "error", err.Error(), "file_name", remotePath)
	}

	w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))

	// 使用 http.ServeContent 替代 http.ServeFile：
	//   - 自动处理 Range header（返回 206 + Content-Range，旧客户端不带 Range 仍 200 全量）
	//   - 不会根据扩展名嗅探并覆盖已设置的 Content-Type（同步修复缺陷 #12）
	http.ServeContent(w, r, info.Name(), info.ModTime(), file)
	if h.metrics != nil {
		h.metrics.RecordDownload(info.Size())
	}
}

func (h *Handlers) delete(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	filename := r.URL.Query().Get("filename")
	if filename == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件名不能为空"}, http.StatusBadRequest)
		return
	}
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的文件名"}, http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	filePath := filepath.Join(cfg.UploadsDir, remotePath)
	if _, err := os.Stat(filePath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "文件不存在"}, http.StatusNotFound)
		return
	}

	expectedChecksum := r.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "缺少 X-File-Checksum 请求头"}, http.StatusBadRequest)
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
	cfg := h.cfgPtr.Load()
	logger := h.logger.With("batch", "delete")
	results := make([]BatchOperationResult, 0, len(req.Files))
	for _, f := range req.Files {
		result := BatchOperationResult{Filename: f.Filename}
		remotePath, err := ValidateFilePath(f.Filename)
		if err != nil {
			result.Message = "无效的文件名"
			results = append(results, result)
			continue
		}
		filePath := filepath.Join(cfg.UploadsDir, remotePath)
		if _, err := os.Stat(filePath); os.IsNotExist(err) {
			// 文件不存在视为成功（幂等删除）
			result.Success = true
			result.Message = "文件不存在（幂等删除）"
			results = append(results, result)
			continue
		}
		if f.Checksum == "" {
			result.Message = "缺少 checksum"
			results = append(results, result)
			continue
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
		results = append(results, result)
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
	cfg := h.cfgPtr.Load()
	logger := h.logger.With("batch", "rename")
	results := make([]BatchOperationResult, 0, len(req.Operations))
	for _, op := range req.Operations {
		result := BatchOperationResult{Filename: op.From + " -> " + op.To}
		from, err := ValidateFilePath(op.From)
		if err != nil {
			result.Message = "无效的源路径"
			results = append(results, result)
			continue
		}
		to, err := ValidateFilePath(op.To)
		if err != nil {
			result.Message = "无效的目标路径"
			results = append(results, result)
			continue
		}
		if from == to {
			result.Success = true
			result.Message = "源与目标相同，无需移动"
			results = append(results, result)
			continue
		}
		fromPath := filepath.Join(cfg.UploadsDir, from)
		toPath := filepath.Join(cfg.UploadsDir, to)
		if _, err := os.Stat(fromPath); os.IsNotExist(err) {
			result.Message = "源文件不存在"
			results = append(results, result)
			continue
		}
		if _, err := os.Stat(toPath); err == nil {
			result.Message = "目标路径已存在"
			results = append(results, result)
			continue
		}
		if op.Checksum == "" {
			result.Message = "缺少 checksum"
			results = append(results, result)
			continue
		}
		if !verifyFileWithChecksum(fromPath, op.Checksum) {
			logger.Warn("batch rename checksum 不匹配", "from", op.From)
			result.Message = "源文件 SHA-256 校验失败"
			results = append(results, result)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(toPath), 0755); err != nil {
			logger.Error("创建目标父目录失败", "to", to, "error", err.Error())
			result.Message = "创建父目录失败"
			results = append(results, result)
			continue
		}
		if err := os.Rename(fromPath, toPath); err != nil {
			logger.Error("batch rename 失败", "from", op.From, "to", op.To, "error", err.Error())
			result.Message = "重命名失败"
			results = append(results, result)
			continue
		}
		h.checksumStore.Rename(from, to)
		results = append(results, BatchOperationResult{
			Filename: op.From + " -> " + op.To,
			Success:  true,
			Message:  "重命名成功",
		})
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
	cfg := h.cfgPtr.Load()

	// 支持按层级查询：?subdir=path 列出指定子目录，默认列出根目录
	targetDir := cfg.UploadsDir
	if subdir := strings.TrimPrefix(r.URL.Query().Get("subdir"), "/"); subdir != "" {
		if _, err := ValidateFilePath(subdir); err != nil {
			h.logger.Warn("无效的子目录", "subdir", subdir, "error", err.Error())
			sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusOK)
			return
		}
		targetDir = filepath.Join(cfg.UploadsDir, subdir)
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
	allFiles := make([]fileInfo, 0, len(entries))
	for _, e := range entries {
		if e.Name() == ".checksums.json" || e.Name() == chunkedDirName {
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
		if subdir := r.URL.Query().Get("subdir"); subdir != "" {
			cleaned, _ := ValidateFilePath(subdir)
			relName = filepath.ToSlash(filepath.Join(cleaned, e.Name()))
		}
		if cs, ok := csMap[relName]; ok {
			fi.Checksum = cs
		}
		allFiles = append(allFiles, fi)
	}

	// 排序
	switch sortBy {
	case "size":
		sort.SliceStable(allFiles, func(i, j int) bool {
			if sortOrder == "desc" {
				return allFiles[i].Size > allFiles[j].Size
			}
			return allFiles[i].Size < allFiles[j].Size
		})
	case "time":
		sort.SliceStable(allFiles, func(i, j int) bool {
			if sortOrder == "desc" {
				return allFiles[i].ModTime > allFiles[j].ModTime
			}
			return allFiles[i].ModTime < allFiles[j].ModTime
		})
	default: // "name"
		if sortOrder == "desc" {
			sort.SliceStable(allFiles, func(i, j int) bool {
				return allFiles[i].Name > allFiles[j].Name
			})
		} else {
			sort.SliceStable(allFiles, func(i, j int) bool {
				return allFiles[i].Name < allFiles[j].Name
			})
		}
	}

	total := len(allFiles)

	// 分页
	var files []fileInfo
	start := min(offset, total)
	end := total
	if limit > 0 && start+limit < end {
		end = start + limit
	}
	files = allFiles[start:end]
	sendJSONResponse(w, listResponse{Files: files, Total: total, Offset: offset, Limit: limit}, http.StatusOK)
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

	var results []fileInfo
	if err := filepath.WalkDir(cfg.UploadsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip inaccessible entries
		}
		rel, _ := filepath.Rel(cfg.UploadsDir, path)
		if rel == "." {
			return nil
		}
		// 跳过内部目录
		if d.Name() == ".checksums.json" || d.Name() == chunkedDirName {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.Contains(strings.ToLower(d.Name()), qLower) {
			return nil
		}
		if d.IsDir() {
			results = append(results, fileInfo{
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
		results = append(results, fi)
		return nil
	}); err != nil {
		h.logger.Error("搜索文件失败", "error", err.Error())
		sendJSONResponse(w, map[string]any{"files": []fileInfo{}}, http.StatusInternalServerError)
		return
	}
	sendJSONResponse(w, map[string]any{"files": results}, http.StatusOK)
}

// rename 处理 POST /rename?from=<old>&to=<new>。
// 与 delete 对称，要求 X-File-Checksum 头校验源文件，避免误覆盖。
// 目标路径已存在时返回 409；服务端会自动 mkdir -p 中间目录。
func (h *Handlers) rename(w http.ResponseWriter, r *http.Request) {
	reqID := r.Header.Get("X-Request-ID")
	if reqID == "" {
		reqID = fmt.Sprintf("%d", time.Now().UnixNano())
	}
	logger := h.logger.With("req_id", reqID)

	fromRaw := r.URL.Query().Get("from")
	toRaw := r.URL.Query().Get("to")
	if fromRaw == "" || toRaw == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "from 和 to 都不能为空"}, http.StatusBadRequest)
		return
	}
	from, err := ValidateFilePath(fromRaw)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的源路径"}, http.StatusBadRequest)
		return
	}
	to, err := ValidateFilePath(toRaw)
	if err != nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "无效的目标路径"}, http.StatusBadRequest)
		return
	}
	if from == to {
		sendJSONResponse(w, UploadResponse{Success: true, Message: "源与目标相同，无需移动"}, http.StatusOK)
		return
	}

	expectedChecksum := r.Header.Get("X-File-Checksum")
	if expectedChecksum == "" {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "缺少 X-File-Checksum 请求头"}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	fromPath := filepath.Join(cfg.UploadsDir, from)
	toPath := filepath.Join(cfg.UploadsDir, to)

	if _, err := os.Stat(fromPath); os.IsNotExist(err) {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源文件不存在"}, http.StatusNotFound)
		return
	}
	if _, err := os.Stat(toPath); err == nil {
		sendJSONResponse(w, UploadResponse{Success: false, Message: "目标路径已存在"}, http.StatusConflict)
		return
	}

	if !verifyFileWithChecksum(fromPath, expectedChecksum) {
		logger.Warn("rename checksum 校验失败", "from", from)
		sendJSONResponse(w, UploadResponse{Success: false, Message: "源文件 SHA-256 校验失败"}, http.StatusBadRequest)
		return
	}

	if err := os.MkdirAll(filepath.Dir(toPath), 0755); err != nil {
		logger.Error("创建目标父目录失败", "to", to, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "创建目标父目录失败"}, http.StatusInternalServerError)
		return
	}
	if err := os.Rename(fromPath, toPath); err != nil {
		logger.Error("重命名失败", "from", from, "to", to, "error", err.Error())
		sendJSONResponse(w, UploadResponse{Success: false, Message: "重命名失败"}, http.StatusInternalServerError)
		return
	}
	h.checksumStore.Rename(from, to)

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
	remotePath, err := ValidateFilePath(filename)
	if err != nil {
		http.Error(w, "invalid filename", http.StatusBadRequest)
		return
	}
	cfg := h.cfgPtr.Load()
	fullPath := filepath.Join(cfg.UploadsDir, remotePath)
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
	w.Header().Set("X-File-MTime", fmt.Sprintf("%d", info.ModTime().UnixNano()))
	if cs, ok := h.checksumStore.Get(remotePath); ok {
		w.Header().Set("X-File-Checksum", cs)
	} else if !info.IsDir() {
		if cs, err := FileChecksum(fullPath); err == nil {
			w.Header().Set("X-File-Checksum", cs)
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
