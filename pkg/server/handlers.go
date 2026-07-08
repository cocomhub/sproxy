// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/web"
)

// Handlers 持有所有 HTTP handler 的依赖。
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
	handler       http.Handler
	cloudMgr      *CloudDownloadManager
	storageMgr    *StorageManager
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
	h.storageMgr = sm

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
	localMux.HandleFunc("PUT /api/storage/config", h.storageConfigHandler)

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
	mux.HandleFunc("PUT /api/storage/config", h.authMiddleware(h.storageConfigHandler))
	mux.HandleFunc("POST /api/share", h.authMiddleware(h.createShareHandler))
	mux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 云端下载 API（localMux：隧道认证）
	localMux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	localMux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	localMux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	localMux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	localMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	// 云端下载 API（主 mux：Bearer auth）
	mux.HandleFunc("POST /api/cloud/download", h.authMiddleware(h.cloudCreateDownload))
	mux.HandleFunc("POST /api/cloud/download/batch", h.authMiddleware(h.cloudCreateBatchDownload))
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

// Close 释放 Handlers 持有的后台资源：停止 UploadStore 的 persist/cleanup goroutine 和 StorageManager 的定期扫描。
// 在进程退出前应调用一次（通常通过 defer h.Close()）。多次调用是安全的。
func (h *Handlers) Close() error {
	if h.uploadStore != nil {
		h.uploadStore.Stop()
	}
	if h.storageMgr != nil {
		h.storageMgr.Stop()
	}
	return nil
}

// Handler 返回包装了 metricsMiddleware 的 HTTP handler，用于 http.Server.Handler。
func (h *Handlers) Handler() http.Handler {
	return h.handler
}

// safePath 在 UploadsDir 下安全拼接 remotePath，结果不越界。
func (h *Handlers) safePath(remotePath string) string {
	if remotePath == "" {
		return ""
	}
	cfg := h.cfgPtr.Load()
	return joinSafePath(cfg.UploadsDir, remotePath)
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

func (h *Handlers) webRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
}

// requestLogMiddleware 记录 HTTP 请求的基本信息。
func requestLogMiddleware(logger *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		logger.Info("请求", "method", r.Method, "path", r.URL.Path,
			"remote_addr", r.RemoteAddr, "user_agent", r.UserAgent(), "duration", time.Since(start))
	})
}
