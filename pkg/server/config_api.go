// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"
)

// configMu 保护 rebuildLogger 和 updateConfigHandler 的并发访问。
// rebuildLogger 调用 slog.SetDefault 有全局副作用；
// updateConfigHandler 对同一 Config 对象做字段读写。
var configMu sync.Mutex

// 日志级别字符串映射，用于运行时切换日志级别。
var levelStrings = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// rebuildLogger 根据配置重建 slog.Logger 并替换全局默认值和 Handlers.logger。
func (h *Handlers) rebuildLogger(cfg *Config) {
	// 注意：调用方必须持有 configMu（updateConfigHandler 已在入口处加锁）。

	level := slog.LevelInfo
	if l, ok := levelStrings[cfg.LogLevel]; ok {
		level = l
	}
	opts := &slog.HandlerOptions{Level: level}
	var handler slog.Handler
	switch cfg.LogFormat {
	case "json":
		handler = slog.NewJSONHandler(os.Stdout, opts)
	default:
		handler = slog.NewTextHandler(os.Stdout, opts)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	h.logger = logger
	SetResponseLogger(logger)
}

// configResponse 是 GET /api/config 的响应体，脱敏返回运行时配置。
type configResponse struct {
	LogLevel           string `json:"log_level"`
	LogFormat          string `json:"log_format"`
	AccessKeysSet      bool   `json:"access_keys_set"` // 是否已配置 AccessKey（SproxySig 认证）
	RateLimitRequests  int    `json:"rate_limit_requests"`
	RateLimitWindow    string `json:"rate_limit_window"` // Duration 字符串
	MaxStorageBytes    int64  `json:"max_storage_bytes"`
	ChunkSize          int64  `json:"chunk_size"`
	UploadSessionTTL   string `json:"upload_session_ttl"`
	VersioningEnabled  bool   `json:"versioning_enabled"`
	VersioningMax      int    `json:"versioning_max_versions"`
	CloudMaxConcurrent int    `json:"cloud_max_concurrent"`
	CloudSyncThreshold int64  `json:"cloud_sync_threshold"`
	HubEnabled         bool   `json:"hub_enabled"`
	TLSEnabled         bool   `json:"tls_enabled"`
	Addr               string `json:"addr"`
	UploadsDir         string `json:"uploads_dir"` // 相对路径；若配置为绝对路径则返回原值
	WebTunnel          bool   `json:"web_tunnel"`  // web.tunnel：Web UI 领域方法是否默认走加密隧道
}

// configHandler 处理 GET /api/config，返回当前运行时配置（脱敏）。
func (h *Handlers) configHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	resp := configResponse{
		LogLevel:           cfg.LogLevel,
		LogFormat:          cfg.LogFormat,
		AccessKeysSet:      len(cfg.AccessKeys) > 0,
		RateLimitRequests:  cfg.RateLimit.Requests,
		RateLimitWindow:    cfg.RateLimit.Window.String(),
		MaxStorageBytes:    cfg.MaxStorageBytes,
		ChunkSize:          cfg.ChunkSize,
		UploadSessionTTL:   cfg.UploadSessionTTL.String(),
		VersioningEnabled:  cfg.Versioning.Enabled,
		VersioningMax:      cfg.Versioning.MaxVersions,
		CloudMaxConcurrent: cfg.CloudMaxConcurrent,
		CloudSyncThreshold: cfg.CloudSyncThreshold,
		HubEnabled:         cfg.Hub.Enabled,
		TLSEnabled:         cfg.TLS.Enabled,
		Addr:               cfg.Addr,
		UploadsDir:         cfg.UploadsDir,
		WebTunnel:          cfg.Web.Tunnel,
	}

	sendJSONResponse(w, resp, http.StatusOK)
}

// updateConfigRequest 是 PUT /api/config 的请求体。
type updateConfigRequest struct {
	LogLevel        *string `json:"log_level,omitempty"`
	LogFormat       *string `json:"log_format,omitempty"`
	RateLimitReq    *int    `json:"rate_limit_requests,omitempty"`
	RateLimitWin    *string `json:"rate_limit_window,omitempty"`
	MaxStorageBytes *int64  `json:"max_storage_bytes,omitempty"`
	WebTunnel       *bool   `json:"web_tunnel,omitempty"`
}

// updateConfigHandler 处理 PUT /api/config，更新运行时配置项。
// 只更新请求体中的字段，不修改未指定的字段。
func (h *Handlers) updateConfigHandler(w http.ResponseWriter, r *http.Request) {
	configMu.Lock()
	defer configMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB

	// auditDenied 记录一次被拒绝的配置变更（含操作主体，便于追责）。
	auditDenied := func(detail string) {
		h.RecordAudit(r.Context(), AuditEvent{
			Action: "config_update", ObjectType: "config",
			Result: AuditResultDenied, Detail: detail,
		})
	}

	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		auditDenied("invalid request body")
		sendJSONResponse(w, map[string]any{"success": false, "message": "invalid request body"}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		auditDenied("请求体哈希校验失败")
		sendJSONResponse(w, UploadResponse{Success: false, Message: "请求体校验失败"}, http.StatusBadRequest)
		return
	}

	// 检查是否所有字段均为 nil，拒绝空请求体（{}）
	if req.LogLevel == nil && req.LogFormat == nil &&
		req.RateLimitReq == nil && req.RateLimitWin == nil && req.MaxStorageBytes == nil && req.WebTunnel == nil {
		auditDenied("empty request body: no fields to update")
		sendJSONResponse(w, map[string]any{"success": false, "message": "empty request body: no fields to update"}, http.StatusBadRequest)
		return
	}

	// Copy-on-Write: 浅拷贝 Config 后修改副本，避免与并发读取的 goroutine 产生 data race。
	// Config 当前字段均为值类型（string、int、struct、time.Duration），浅拷贝安全。
	cfg := *h.cfgPtr.Load()
	changed := false

	if req.LogLevel != nil {
		validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
		if !validLevels[*req.LogLevel] {
			auditDenied("invalid log_level: " + *req.LogLevel)
			sendJSONResponse(w, map[string]any{"success": false, "message": "invalid log_level, must be debug/info/warn/error"}, http.StatusBadRequest)
			return
		}
		cfg.LogLevel = *req.LogLevel
		changed = true
	}

	if req.LogFormat != nil {
		if *req.LogFormat != "text" && *req.LogFormat != "json" {
			auditDenied("invalid log_format: " + *req.LogFormat)
			sendJSONResponse(w, map[string]any{"success": false, "message": "invalid log_format, must be text/json"}, http.StatusBadRequest)
			return
		}
		cfg.LogFormat = *req.LogFormat
		changed = true
	}

	if req.RateLimitReq != nil {
		if *req.RateLimitReq <= 0 {
			auditDenied("invalid rate_limit_requests")
			sendJSONResponse(w, map[string]any{"success": false, "message": "rate_limit_requests must be non-negative"}, http.StatusBadRequest)
			return
		}
		cfg.RateLimit.Requests = *req.RateLimitReq
		changed = true
	}

	if req.RateLimitWin != nil {
		d, err := time.ParseDuration(*req.RateLimitWin)
		if err != nil || d <= 0 {
			auditDenied("invalid rate_limit_window duration")
			sendJSONResponse(w, map[string]any{"success": false, "message": "invalid rate_limit_window duration"}, http.StatusBadRequest)
			return
		}
		cfg.RateLimit.Window = d
		changed = true
	}

	if req.MaxStorageBytes != nil {
		if *req.MaxStorageBytes < 0 {
			auditDenied("invalid max_storage_bytes")
			sendJSONResponse(w, map[string]any{"success": false, "message": "max_storage_bytes must be non-negative"}, http.StatusBadRequest)
			return
		}
		cfg.MaxStorageBytes = *req.MaxStorageBytes
		if h.storageMgr != nil {
			h.storageMgr.SetMaxBytes(*req.MaxStorageBytes)
		}
		changed = true
	}

	if req.WebTunnel != nil {
		cfg.Web.Tunnel = *req.WebTunnel
		changed = true
	}

	if changed {
		// Copy-on-Write: 存储新配置的副本
		h.cfgPtr.Store(&cfg)

		// RateLimiter 热更新：需要批次 C/D 添加 h.rateLimiter 字段和 UpdateConfig 方法
		// TODO: 当 h.rateLimiter 字段可用时，在此处调用 h.rateLimiter.UpdateConfig(cfg.RateLimit.Requests, cfg.RateLimit.Window)

		// 日志级别或格式变更时，立即重建 logger 使生效
		if req.LogLevel != nil || req.LogFormat != nil {
			h.rebuildLogger(&cfg)
		}
	}

	h.RecordAudit(r.Context(), AuditEvent{
		Action: "config_update", ObjectType: "config",
		Result: AuditResultSuccess,
	})
	sendJSONResponse(w, map[string]any{
		"success": true,
		"changed": changed,
	}, http.StatusOK)
}
