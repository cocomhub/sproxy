// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	"github.com/cocomhub/sproxy/web"
)

// Handlers 持有所有 HTTP handler 的依赖。
type Handlers struct {
	cfgPtr         *atomic.Pointer[Config]
	version        string
	buildAt        string
	checksumStore  ChecksumStoreIface
	uploadStore    UploadStoreIface
	tunnelHandler  http.Handler
	logger         *slog.Logger
	metrics        *Metrics
	shareStore     *ShareStore
	routeTable     *hub.RouteTable
	signalBroker   *SignalBroker
	handler        http.Handler
	cloudMgr       *CloudDownloadManager
	storageMgr     *StorageManager
	uploadingFiles sync.Map       // map[string]string — filename → uploadID，追踪正在上传的文件名
	uploadingStop  chan struct{}  // 关闭后通知 uploadingFiles 定期清理 goroutine 退出
	uploadingWg    sync.WaitGroup // 等待 cleanupUploadingFilesLoop 退出
	closeOnce      sync.Once      // 防止 Close() 重复关闭 channel
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
func RegisterRoutes(ctx context.Context, opts RegisterRoutesOpts) *Handlers {
	// TODO: ctx 当前未使用，后续可用于 graceful shutdown 或请求级超时控制
	srvMux := opts.Mux
	cfg := opts.CfgPtr.Load()
	log := defaultLogger(opts.Logger)
	// 用 WithContextHandler 包装：所有 InfoContext/DebugContext(ctx, ...) 日志
	// 自动读取 ctx 中的 SpanContext，带上 trace_id/span_id 实现全链路追踪。
	log = slog.New(tracing.WithContextHandler(log.Handler()))

	// 初始化 ChecksumStore
	cs := NewChecksumStore(cfg.UploadsDir, log.With("component", "checksum_store"))

	h := &Handlers{
		cfgPtr:        opts.CfgPtr,
		version:       opts.Version,
		buildAt:       opts.BuildAt,
		checksumStore: cs,
		uploadStore:   MustNewUploadStore(cfg.UploadsDir, cfg.UploadSessionTTL, log.With("component", "upload_store")),
		logger:        log,
		metrics:       NewMetrics(),
		shareStore:    NewShareStore(log.With("component", "share")),
		routeTable:    opts.RouteTable,
		signalBroker:  NewSignalBroker(opts.RouteTable),
		uploadingStop: make(chan struct{}),
	}

	// 启动 uploadingFiles 定期清理 goroutine（OOM 防范）
	h.uploadingWg.Go(func() {
		h.cleanupUploadingFilesLoop()
	})

	// 初始化 StorageManager 和 CloudDownloadManager
	sm := NewStorageManager(cfg.UploadsDir, cfg.MaxStorageBytes, cs, log.With("component", "storage"))
	cloudCfg := &CloudDownloadConfig{
		SyncThreshold:   cfg.CloudSyncThreshold,
		MaxConcurrent:   cfg.CloudMaxConcurrent,
		MaxBatchURLs:    cfg.CloudMaxBatchURLs,
		TaskTTL:         cfg.CloudTaskTTL,
		FailedTaskTTL:   cfg.CloudFailedTaskTTL,
		AllowPrivate:    cfg.CloudDownloadAllowPrivate,
		DownloadTimeout: cfg.CloudDownloadTimeout,
		IdleTimeout:     cfg.CloudDownloadIdleTimeout,
		MaxRetries:      cfg.CloudMaxRetries,
		RetryDelay:      cfg.CloudRetryDelay,
		Downloader:      cfg.CloudDownloader,
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
	localMux.HandleFunc("GET /api/config", h.configHandler)
	localMux.HandleFunc("PUT /api/config", h.updateConfigHandler)

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

	// 隧道内层请求同样挂 requestLogMiddleware：解析客户端注入的 traceparent，
	// 生成子 span 并把 SpanContext 写入 ctx，使内层 handler 的 InfoContext/DebugContext
	// 日志带 trace_id/span_id，恢复隧道内层 per-request「收到/完成」日志。
	// 注意：这是 requestLogMiddleware 的第二个独立实例（主 mux 外层已用一次），
	// 对隧道路径独立生效，正确。
	h.tunnelHandler = tunnel.NewLocalHandler(opts.TunnelKey, h.requestLogMiddleware(apiHandler), log.With("component", "tunnel"))

	srvMux.HandleFunc("POST /upload", h.authMiddleware(h.upload))
	srvMux.HandleFunc("GET /download", h.authMiddleware(h.download))
	srvMux.HandleFunc("POST /delete", h.authMiddleware(h.delete))
	srvMux.HandleFunc("POST /rename", h.authMiddleware(h.rename))
	srvMux.HandleFunc("GET /api/files", h.authMiddleware(h.listFiles))
	srvMux.HandleFunc("HEAD /api/files/stat", h.authMiddleware(h.stat))
	srvMux.HandleFunc("POST /upload/init", h.authMiddleware(h.uploadInit))
	srvMux.HandleFunc("POST /upload/chunk", h.authMiddleware(h.uploadChunk))
	srvMux.HandleFunc("GET /upload/status", h.authMiddleware(h.uploadStatus))
	srvMux.HandleFunc("POST /upload/complete", h.authMiddleware(h.uploadComplete))
	srvMux.HandleFunc("GET /download/chunk", h.authMiddleware(h.downloadChunk))
	srvMux.HandleFunc("POST /mkdir", h.authMiddleware(h.mkdir))
	srvMux.HandleFunc("POST /rmdir", h.authMiddleware(h.rmdir))
	srvMux.HandleFunc("GET /api/files/search", h.authMiddleware(h.searchFiles))
	srvMux.HandleFunc("POST /api/batch/delete", h.authMiddleware(h.batchDelete))
	srvMux.HandleFunc("POST /api/batch/rename", h.authMiddleware(h.batchRename))
	srvMux.HandleFunc("POST /api/archive", h.authMiddleware(h.archiveHandler))
	srvMux.HandleFunc("GET /api/archive-dir", h.authMiddleware(h.archiveDirHandler))
	srvMux.HandleFunc("GET /api/versions", h.authMiddleware(h.listVersionsHandler))
	srvMux.HandleFunc("POST /api/versions/restore", h.authMiddleware(h.restoreVersionHandler))
	srvMux.HandleFunc("DELETE /api/versions", h.authMiddleware(h.deleteVersionHandler))
	srvMux.HandleFunc("GET /api/stats", h.authMiddleware(h.statsHandler))
	srvMux.HandleFunc("GET /api/config", h.authMiddleware(h.configHandler))
	srvMux.HandleFunc("PUT /api/config", h.authMiddleware(h.updateConfigHandler))
	srvMux.HandleFunc("POST /api/share", h.authMiddleware(h.createShareHandler))
	srvMux.HandleFunc("GET /s/{token}", h.accessShareHandler)

	// 分享管理 API（localMux：隧道内部使用）
	localMux.HandleFunc("GET /api/shares", h.listSharesHandler)
	localMux.HandleFunc("DELETE /api/shares/{token}", h.revokeShareHandler)

	// 分享管理 API（主 mux：Bearer auth）
	srvMux.HandleFunc("GET /api/shares", h.authMiddleware(h.listSharesHandler))
	srvMux.HandleFunc("DELETE /api/shares/{token}", h.authMiddleware(h.revokeShareHandler))

	// 云端下载 API（localMux：隧道认证）
	localMux.HandleFunc("POST /api/cloud/download", h.cloudCreateDownload)
	localMux.HandleFunc("POST /api/cloud/download/batch", h.cloudCreateBatchDownload)
	localMux.HandleFunc("GET /api/cloud/tasks", h.cloudListTasks)
	localMux.HandleFunc("GET /api/cloud/tasks/{id}", h.cloudGetTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.cloudCancelTask)
	localMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.cloudDeleteTask)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.cloudArchiveTask)
	localMux.HandleFunc("POST /api/cloud/archive", h.cloudArchiveBatch)
	localMux.HandleFunc("POST /api/cloud/tasks/{id}/resume", h.cloudResumeTask)
	localMux.HandleFunc("POST /api/cloud/groups", h.cloudCreateGroup)
	localMux.HandleFunc("GET /api/cloud/groups", h.cloudListGroups)
	localMux.HandleFunc("GET /api/cloud/groups/{id}", h.cloudGetGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/cancel", h.cloudCancelGroup)
	localMux.HandleFunc("DELETE /api/cloud/groups/{id}", h.cloudDeleteGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/resume", h.cloudResumeGroup)
	localMux.HandleFunc("POST /api/cloud/groups/{id}/archive", h.cloudArchiveGroup)
	// 云端下载 API（主 mux：Bearer auth）
	srvMux.HandleFunc("POST /api/cloud/download", h.authMiddleware(h.cloudCreateDownload))
	srvMux.HandleFunc("POST /api/cloud/download/batch", h.authMiddleware(h.cloudCreateBatchDownload))
	srvMux.HandleFunc("GET /api/cloud/tasks", h.authMiddleware(h.cloudListTasks))
	srvMux.HandleFunc("GET /api/cloud/tasks/{id}", h.authMiddleware(h.cloudGetTask))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/cancel", h.authMiddleware(h.cloudCancelTask))
	srvMux.HandleFunc("DELETE /api/cloud/tasks/{id}", h.authMiddleware(h.cloudDeleteTask))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/archive", h.authMiddleware(h.cloudArchiveTask))
	srvMux.HandleFunc("POST /api/cloud/archive", h.authMiddleware(h.cloudArchiveBatch))
	srvMux.HandleFunc("POST /api/cloud/tasks/{id}/resume", h.authMiddleware(h.cloudResumeTask))
	srvMux.HandleFunc("POST /api/cloud/groups", h.authMiddleware(h.cloudCreateGroup))
	srvMux.HandleFunc("GET /api/cloud/groups", h.authMiddleware(h.cloudListGroups))
	srvMux.HandleFunc("GET /api/cloud/groups/{id}", h.authMiddleware(h.cloudGetGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/cancel", h.authMiddleware(h.cloudCancelGroup))
	srvMux.HandleFunc("DELETE /api/cloud/groups/{id}", h.authMiddleware(h.cloudDeleteGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/resume", h.authMiddleware(h.cloudResumeGroup))
	srvMux.HandleFunc("POST /api/cloud/groups/{id}/archive", h.authMiddleware(h.cloudArchiveGroup))

	// Hub 管理 API（中继系统），需鉴权
	if opts.RouteTable != nil {
		// 任意 TCP 流中继（SSH/长连接）：升级为双向字节流。
		// 注：旧的 HTTP JSON 中继（POST /api/relay）已删除——被本流中继完全替代。
		// 仅支持直连（srvMux + Bearer）：handler 依赖 http.Hijacker 升级为原始 TCP，
		// 而隧道的 ResponseWriter 包装链（streamRecorder/gzipResponseWriter）不实现
		// Hijacker——经隧道访问必 500（旧版误注册到 localMux 的死路由，已删除）。
		streamHandler := NewRelayStreamHandler(opts.RouteTable, log.With("component", "relay_stream"))
		srvMux.HandleFunc("POST /api/relay/stream", h.authMiddleware(streamHandler.ServeHTTP))
		// TODO(I29)：若未来需要「经隧道做原始 TCP 中继」（链式中继/多跳），正确定位是
		// mux 层 raw-stream（复用 hub relay 模式），而非 http.Hijacker。见
		// .superpowers/sdd/i29-tunnel-hijack-value.md。

		// WebRTC 信令桥：SDP Offer/Answer/Candidate 存转 + 长轮询
		broker := h.signalBroker
		// S44：信令 POST 单独挂限流（独立实例，与文件传输隔离配额），防共享
		// relay_token 下被攻破节点洪泛注入信令；GET poll 长轮询不挂（客户端
		// 高频轮询会误触发限流）。
		var signalPostRL *RateLimiter
		if cfg.RateLimit.Enabled {
			signalPostRL = NewRateLimiter(cfg.RateLimit.Requests, cfg.RateLimit.Window, log.With("component", "signal_rate_limiter"))
		}
		signalPost := func(kind hub.SignalKind) http.HandlerFunc {
			hf := func(w http.ResponseWriter, r *http.Request) {
				broker.handleSignalPost(w, r, kind)
			}
			if signalPostRL == nil {
				return hf
			}
			return signalPostRL.Middleware(http.HandlerFunc(hf)).ServeHTTP
		}
		srvMux.HandleFunc("POST /api/signal/offer", h.authMiddleware(signalPost(hub.SignalOffer)))
		srvMux.HandleFunc("POST /api/signal/answer", h.authMiddleware(signalPost(hub.SignalAnswer)))
		srvMux.HandleFunc("POST /api/signal/candidate", h.authMiddleware(signalPost(hub.SignalCandidate)))
		srvMux.HandleFunc("GET /api/signal/poll/{peer}", h.authMiddleware(broker.handleSignalPoll))
		localMux.HandleFunc("POST /api/signal/offer", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalOffer)
		})
		localMux.HandleFunc("POST /api/signal/answer", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalAnswer)
		})
		localMux.HandleFunc("POST /api/signal/candidate", func(w http.ResponseWriter, r *http.Request) {
			broker.handleSignalPost(w, r, hub.SignalCandidate)
		})
		localMux.HandleFunc("GET /api/signal/poll/{peer}", broker.handleSignalPoll)

		srvMux.HandleFunc("GET /api/hub/nodes", h.authMiddleware(h.hubNodesHandler))
		srvMux.HandleFunc("DELETE /api/hub/nodes/{id}", h.authMiddleware(h.hubRemoveNodeHandler))
		srvMux.HandleFunc("GET /api/hub/stats", h.authMiddleware(h.hubStatsHandler))
		srvMux.HandleFunc("GET /api/hub/services", h.authMiddleware(h.hubServicesHandler))
		// mesh 服务列表暴露 localMux 是有意的：FileClient.MeshServices（client.go）
		// 配置了 tunnelClient 时经 /tunnel 访问 localMux 做 mesh 选路；
		// nodes/stats/remove 为运维管理面，仅 srvMux+Bearer，不暴露隧道。
		localMux.HandleFunc("GET /api/hub/services", h.hubServicesHandler)
	}

	srvMux.HandleFunc("GET /healthz", h.healthz)
	srvMux.HandleFunc("GET /version", h.versionHandler)
	srvMux.HandleFunc("GET /metrics", h.MetricsHandler)
	srvMux.Handle("POST /tunnel", h.tunnelHandler)

	// Web UI
	subFS, err := fs.Sub(web.StaticFS, "static")
	if err != nil {
		h.logger.Error("web static fs sub error", "error", err)
	} else {
		fileServer := http.StripPrefix("/ui/", http.FileServer(http.FS(subFS)))
		srvMux.Handle("GET /ui/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy",
				"default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:;")
			fileServer.ServeHTTP(w, r)
		}))
	}

	// GET / -> /ui/ 重定向。
	// 用 "{$}" 只精确匹配根路径：Go 1.22+ ServeMux 中 "GET /" 是 catch-all，
	// 会把任意未匹配路径（如 /foobar）也 301 到 /ui/；{$} 使未知路径返回 404。
	// （实测 /ui 无尾斜杠在 {$} 下返回 307 到 /ui/，浏览器自动跟随。）
	srvMux.HandleFunc("GET /{$}", h.webRedirect)

	h.handler = h.metricsMiddleware(h.requestLogMiddleware(srvMux))

	return h
}

// Close 释放 Handlers 持有的后台资源：停止 UploadStore 的 persist/cleanup goroutine 和 StorageManager 的定期扫描。
// 在进程退出前应调用一次（通常通过 defer h.Close()）。多次调用是安全的。
// 关闭顺序：先关 uploadingFiles 清理 goroutine，再关 UploadStore（后者可能还有 uploading 操作引用其 session）。
// TODO: 当前始终返回 nil；后续可收集各子组件关闭的错误，合并后返回。
func (h *Handlers) Close() error {
	// 先关闭 uploadingFiles 清理 goroutine，确保不再引用 uploadStore session
	h.closeOnce.Do(func() {
		close(h.uploadingStop)
	})
	h.uploadingWg.Wait()

	if h.uploadStore != nil {
		h.uploadStore.Stop()
	}
	if h.storageMgr != nil {
		h.storageMgr.Stop()
	}
	if h.cloudMgr != nil {
		h.cloudMgr.Close()
	}
	if h.shareStore != nil {
		h.shareStore.Stop()
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
	if cfg == nil {
		return ""
	}
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

// cleanupUploadingFilesLoop 定期清理 uploadingFiles 中已过期（不存在对应 session）的条目。
// 普通 upload 条目 value 为 "upload"（无 session），直接跳过。
// 作为 goroutine 在 RegisterRoutes 中启动，由 Close() 通过关闭 uploadingStop 停止。
func (h *Handlers) cleanupUploadingFilesLoop() {
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-h.uploadingStop:
			return
		case <-ticker.C:
			h.uploadingFiles.Range(func(key, value any) bool {
				filename, ok := key.(string)
				if !ok {
					return true
				}
				uploadID, ok := value.(string)
				if !ok {
					return true
				}
				// 普通 upload 条目 value 为 "upload"，无对应 session，跳过
				if uploadID == "upload" {
					return true
				}
				if h.uploadStore != nil && h.uploadStore.GetSession(uploadID) == nil {
					h.uploadingFiles.Delete(filename)
				}
				return true
			})
		}
	}
}
