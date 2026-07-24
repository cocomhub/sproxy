// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// 日志级别字符串映射，用于运行时切换日志级别。
var levelStrings = map[string]slog.Level{
	"debug": slog.LevelDebug,
	"info":  slog.LevelInfo,
	"warn":  slog.LevelWarn,
	"error": slog.LevelError,
}

// rebuildLogger 根据配置重建 slog.Logger 并替换全局默认值和 Handlers.logger。
func (h *Handlers) rebuildLogger(cfg *Config) {
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
}

// configResponse 是 GET /api/config 的响应体，脱敏返回运行时配置。
type configResponse struct {
	LogLevel           string `json:"log_level"`
	LogFormat          string `json:"log_format"`
	AuthTokenSet       bool   `json:"auth_token_set"` // 是否已设置 token
	TunnelKeySet       bool   `json:"tunnel_key_set"` // 是否已设置 tunnel key
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
	UploadsDir         string `json:"uploads_dir"`
}

// configHandler 处理 GET /api/config，返回当前运行时配置（脱敏）。
func (h *Handlers) configHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	resp := configResponse{
		LogLevel:           cfg.LogLevel,
		LogFormat:          cfg.LogFormat,
		AuthTokenSet:       cfg.AuthToken != "",
		TunnelKeySet:       cfg.TunnelKey != "",
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
	}

	sendJSONResponse(w, resp, http.StatusOK)
}

// updateConfigRequest 是 PUT /api/config 的请求体。
type updateConfigRequest struct {
	LogLevel        *string `json:"log_level,omitempty"`
	LogFormat       *string `json:"log_format,omitempty"`
	AuthToken       *string `json:"auth_token,omitempty"`
	RateLimitReq    *int    `json:"rate_limit_requests,omitempty"`
	RateLimitWin    *string `json:"rate_limit_window,omitempty"`
	MaxStorageBytes *int64  `json:"max_storage_bytes,omitempty"`
}

// updateConfigHandler 处理 PUT /api/config，更新运行时配置项。
// 只更新请求体中的字段，不修改未指定的字段。
func (h *Handlers) updateConfigHandler(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB

	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		sendJSONResponse(w, map[string]string{"error": "invalid request body"}, http.StatusBadRequest)
		return
	}

	cfg := h.cfgPtr.Load()
	changed := false

	if req.LogLevel != nil {
		validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
		if !validLevels[*req.LogLevel] {
			sendJSONResponse(w, map[string]string{"error": "invalid log_level, must be debug/info/warn/error"}, http.StatusBadRequest)
			return
		}
		cfg.LogLevel = *req.LogLevel
		changed = true
	}

	if req.LogFormat != nil {
		if *req.LogFormat != "text" && *req.LogFormat != "json" {
			sendJSONResponse(w, map[string]string{"error": "invalid log_format, must be text/json"}, http.StatusBadRequest)
			return
		}
		cfg.LogFormat = *req.LogFormat
		changed = true
	}

	if req.AuthToken != nil {
		cfg.AuthToken = *req.AuthToken
		changed = true
	}

	if req.RateLimitReq != nil {
		if *req.RateLimitReq < 0 {
			sendJSONResponse(w, map[string]string{"error": "rate_limit_requests must be non-negative"}, http.StatusBadRequest)
			return
		}
		cfg.RateLimit.Requests = *req.RateLimitReq
		changed = true
	}

	if req.RateLimitWin != nil {
		d, err := time.ParseDuration(*req.RateLimitWin)
		if err != nil || d <= 0 {
			sendJSONResponse(w, map[string]string{"error": "invalid rate_limit_window duration"}, http.StatusBadRequest)
			return
		}
		cfg.RateLimit.Window = d
		changed = true
	}

	if req.MaxStorageBytes != nil {
		if *req.MaxStorageBytes < 0 {
			sendJSONResponse(w, map[string]string{"error": "max_storage_bytes must be non-negative"}, http.StatusBadRequest)
			return
		}
		cfg.MaxStorageBytes = *req.MaxStorageBytes
		if h.storageMgr != nil {
			h.storageMgr.SetMaxBytes(*req.MaxStorageBytes)
		}
		changed = true
	}

	if changed {
		h.cfgPtr.Store(cfg)
		// 日志级别或格式变更时，立即重建 logger 使生效
		if req.LogLevel != nil || req.LogFormat != nil {
			h.rebuildLogger(cfg)
		}
	}

	sendJSONResponse(w, map[string]any{
		"success": true,
		"changed": changed,
	}, http.StatusOK)
}
