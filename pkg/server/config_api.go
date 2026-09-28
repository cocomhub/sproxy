// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
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
	BandwidthEnabled   bool   `json:"bandwidth_enabled"` // rate_limit.bandwidth 生效状态（可观测）
	BandwidthOwnerBPS  int64  `json:"bandwidth_per_owner_bps"`
	MaxStorageBytes    int64  `json:"max_storage_bytes"`
	MaxUploadBytes     int64  `json:"max_upload_bytes"`
	ChunkSize          int64  `json:"chunk_size"`
	UploadSessionTTL   string `json:"upload_session_ttl"`
	VersioningEnabled  bool   `json:"versioning_enabled"`
	VersioningMax      int    `json:"versioning_max_versions"`
	CloudMaxConcurrent int    `json:"cloud_max_concurrent"`
	CloudSyncThreshold int64  `json:"cloud_sync_threshold"`
	HubEnabled         bool   `json:"hub_enabled"`
	TLSEnabled         bool   `json:"tls_enabled"`
	Addr               string `json:"addr"`
	StorageRoot        string `json:"storage_root"` // 相对路径；若配置为绝对路径则返回原值
	WebTunnel          bool   `json:"web_tunnel"`   // web.tunnel：Web UI 领域方法是否默认走加密隧道
}

// configHandler 处理 GET /api/config，返回当前运行时配置（脱敏）。
func (h *Handlers) configHandler(w http.ResponseWriter, r *http.Request) {
	cfg := h.cfgPtr.Load()

	resp := configResponse{
		LogLevel:           cfg.LogLevel,
		LogFormat:          cfg.LogFormat,
		AccessKeysSet:      h.credentialRing != nil && h.credentialRing.Len() > 0,
		RateLimitRequests:  cfg.RateLimit.Requests,
		RateLimitWindow:    cfg.RateLimit.Window.String(),
		BandwidthEnabled:   cfg.RateLimit.Bandwidth.Enabled,
		BandwidthOwnerBPS:  cfg.RateLimit.Bandwidth.PerOwnerBPS,
		MaxStorageBytes:    cfg.MaxStorageBytes,
		MaxUploadBytes:     int64(cfg.MaxUploadBytes),
		ChunkSize:          cfg.ChunkSize,
		UploadSessionTTL:   cfg.UploadSessionTTL.String(),
		VersioningEnabled:  cfg.Versioning.Enabled,
		VersioningMax:      cfg.Versioning.MaxVersions,
		CloudMaxConcurrent: cfg.CloudMaxConcurrent,
		CloudSyncThreshold: cfg.CloudSyncThreshold,
		HubEnabled:         cfg.Hub.Enabled,
		TLSEnabled:         cfg.TLS.Enabled,
		Addr:               cfg.Addr,
		StorageRoot:        resolveDefaultVolumeRoot(cfg),
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
	MaxUploadBytes  *int64  `json:"max_upload_bytes,omitempty"`
	WebTunnel       *bool   `json:"web_tunnel,omitempty"`
}

// updateConfigHandler 处理 PUT /api/config，更新运行时配置项。
// 只更新请求体中的字段，不修改未指定的字段。
func (h *Handlers) updateConfigHandler(w http.ResponseWriter, r *http.Request) {
	configMu.Lock()
	defer configMu.Unlock()

	r.Body = http.MaxBytesReader(w, r.Body, 1<<10) // 1 KiB

	var req updateConfigRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.auditConfigDenied(r, msgInvalidRequestBody)
		sendJSONResponse(w, map[string]any{"success": false, "message": msgInvalidRequestBody}, http.StatusBadRequest)
		return
	}
	// I-3：读完全部 body 触发 bodyValidator EOF 哈希校验（Decode 不读到 EOF）。
	if err := drainAndVerifyBody(r); err != nil {
		h.auditConfigDenied(r, "请求体哈希校验失败")
		sendJSONResponse(w, UploadResponse{Success: false, Message: msgBadRequest}, http.StatusBadRequest)
		return
	}

	// 检查是否所有字段均为 nil，拒绝空请求体（{}）
	if req.LogLevel == nil && req.LogFormat == nil &&
		req.RateLimitReq == nil && req.RateLimitWin == nil && req.MaxStorageBytes == nil &&
		req.MaxUploadBytes == nil && req.WebTunnel == nil {
		h.auditConfigDenied(r, "empty request body: no fields to update")
		sendJSONResponse(w, map[string]any{"success": false, "message": "empty request body: no fields to update"}, http.StatusBadRequest)
		return
	}

	// Copy-on-Write: 浅拷贝 Config 后修改副本，避免与并发读取的 goroutine 产生 data race。
	// Config 当前字段均为值类型（string、int、struct、time.Duration），浅拷贝安全。
	cfg := *h.cfgPtr.Load()
	changed, applyErr := h.applyConfigUpdates(r, &req, &cfg)
	if applyErr != nil {
		sendJSONResponse(w, map[string]any{"success": false, "message": applyErr.Error()}, http.StatusBadRequest)
		return
	}

	if changed {
		// Copy-on-Write: 存储新配置的副本 + RateLimiter 热更新 + 日志 logger 重建。
		h.commitConfigUpdate(&cfg, &req)
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

// auditConfigDenied 记录一次被拒绝的配置变更（含操作主体，便于追责）。
func (h *Handlers) auditConfigDenied(r *http.Request, detail string) {
	h.RecordAudit(r.Context(), AuditEvent{
		Action: "config_update", ObjectType: "config",
		Result: AuditResultDenied, Detail: detail,
	})
}

// applyConfigUpdates 把请求体字段逐项应用到配置副本（含校验）。任一字段校验失败 →
// 记录审计拒绝并返回错误（message 直接作 HTTP 响应体）；全部合法返回是否有变更。
// 按逻辑分组委托给 applyLoggingUpdates / applyQuotaUpdates 与 WebTunnel 单字段。
func (h *Handlers) applyConfigUpdates(r *http.Request, req *updateConfigRequest, cfg *Config) (changed bool, updateErr error) {
	logChanged, err := h.applyLoggingUpdates(r, req, cfg)
	if err != nil {
		return false, err
	}
	quotaChanged, err := h.applyQuotaUpdates(r, req, cfg)
	if err != nil {
		return false, err
	}
	if req.WebTunnel != nil {
		cfg.Web.Tunnel = *req.WebTunnel
		changed = true
	}
	return logChanged || quotaChanged || changed, nil
}

// applyLoggingUpdates 应用日志级别/格式字段（含校验；非法值记审计拒绝并返回错误）。
func (h *Handlers) applyLoggingUpdates(r *http.Request, req *updateConfigRequest, cfg *Config) (changed bool, updateErr error) {
	if req.LogLevel != nil {
		validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
		if !validLevels[*req.LogLevel] {
			h.auditConfigDenied(r, "invalid log_level: "+*req.LogLevel)
			return false, fmt.Errorf("invalid log_level, must be debug/info/warn/error")
		}
		cfg.LogLevel = *req.LogLevel
		changed = true
	}

	if req.LogFormat != nil {
		if *req.LogFormat != "text" && *req.LogFormat != "json" {
			h.auditConfigDenied(r, "invalid log_format: "+*req.LogFormat)
			return false, fmt.Errorf("invalid log_format, must be text/json")
		}
		cfg.LogFormat = *req.LogFormat
		changed = true
	}
	return changed, nil
}

// applyQuotaUpdates 应用限流/容量字段（rate_limit_requests / rate_limit_window /
// max_upload_bytes；max_storage_bytes 委托 applyMaxStorageUpdate）。
func (h *Handlers) applyQuotaUpdates(r *http.Request, req *updateConfigRequest, cfg *Config) (changed bool, updateErr error) {
	if req.RateLimitReq != nil {
		if *req.RateLimitReq <= 0 {
			h.auditConfigDenied(r, "invalid rate_limit_requests")
			return false, fmt.Errorf("rate_limit_requests must be non-negative")
		}
		cfg.RateLimit.Requests = *req.RateLimitReq
		changed = true
	}

	if req.RateLimitWin != nil {
		d, err := time.ParseDuration(*req.RateLimitWin)
		if err != nil || d <= 0 {
			h.auditConfigDenied(r, "invalid rate_limit_window duration")
			return false, fmt.Errorf("invalid rate_limit_window duration")
		}
		cfg.RateLimit.Window = d
		changed = true
	}

	storageChanged, err := h.applyMaxStorageUpdate(r, req, cfg)
	if err != nil {
		return false, err
	}
	changed = changed || storageChanged

	if req.MaxUploadBytes != nil {
		if *req.MaxUploadBytes < 0 {
			h.auditConfigDenied(r, "invalid max_upload_bytes")
			return false, fmt.Errorf("max_upload_bytes must be non-negative")
		}
		cfg.MaxUploadBytes = ByteSize(*req.MaxUploadBytes)
		changed = true
	}
	return changed, nil
}

// applyMaxStorageUpdate 应用 max_storage_bytes 字段（校验 + storageMgr/globalPool
// 联动 SetMaxBytes）。
func (h *Handlers) applyMaxStorageUpdate(r *http.Request, req *updateConfigRequest, cfg *Config) (bool, error) {
	if req.MaxStorageBytes == nil {
		return false, nil
	}
	if *req.MaxStorageBytes < 0 {
		h.auditConfigDenied(r, "invalid max_storage_bytes")
		return false, fmt.Errorf("max_storage_bytes must be non-negative")
	}
	cfg.MaxStorageBytes = *req.MaxStorageBytes
	if h.storageMgr != nil {
		h.storageMgr.SetMaxBytes(*req.MaxStorageBytes)
	}
	if h.globalPool != nil {
		h.globalPool.SetMaxBytes(*req.MaxStorageBytes)
	}
	return true, nil
}

// commitConfigUpdate 提交已应用的配置变更：Copy-on-Write 存储新副本 + RateLimiter
// 热更新（同一实例复用 mu，不重建 handler 链）+ 日志 logger 重建。调用方保证
// changed=true。
func (h *Handlers) commitConfigUpdate(cfg *Config, req *updateConfigRequest) {
	h.cfgPtr.Store(cfg)

	// RateLimiter 热更新：同一实例复用 mu 更新参数（enabled/limit/window），
	// 不重建 handler 链（xfer LocalHandler 已持有构造期引用），不清空时间戳。
	// 启动未启用限流（cfg.RateLimit.Enabled=false）时字段为 nil：PUT 不能开启
	// （无 middleware 挂载，开也无效），静默跳过以对齐现有「不可热开启」语义。
	// 注意：nil 分支的字段访问与启动分支读 cfg 位置一致，均值在 cfgPtr.Store 之后
	// 读取的均是已更新副本，无竞态。
	if h.rateLimiter != nil {
		h.rateLimiter.UpdateConfig(cfg.RateLimit.Enabled, cfg.RateLimit.Requests, cfg.RateLimit.Window)
		// 新维度（per-endpoint 规则 + 全局并发上限）随热更新同步（roadmap 12.1-6 片 2）：
		// 每次 PUT 都全量同步 cfgPtr 里的当前值——装配期与热更新共用同一 UpdateDimensions。
		h.rateLimiter.UpdateDimensions(cfg.RateLimit.Endpoints, cfg.RateLimit.EndpointDefault, cfg.RateLimit.MaxConcurrent)
		// 协调后端随热更新重建（coordinated 开关 / backend 变更即时生效）；
		// 失败回退 local + 警告。
		if cfg.RateLimit.Coordinated {
			if coord, cerr := newCoordinator(cfg.RateLimit.Backend, int64(cfg.RateLimit.Requests), cfg.RateLimit.Window, h.globalRoot.AbsPath(), h.logger); cerr != nil {
				h.logger.Warn("rate limit coordinator rebuild failed, fallback to local", "error", cerr)
				h.rateLimiter.SetCoordinator(nil)
			} else {
				h.rateLimiter.SetCoordinator(coord)
			}
		} else {
			h.rateLimiter.SetCoordinator(nil)
		}
	}
	if h.signalPostRL != nil {
		h.signalPostRL.UpdateConfig(cfg.RateLimit.Enabled, cfg.RateLimit.Requests, cfg.RateLimit.Window)
	}
	// 日志级别或格式变更时，立即重建 logger 使生效
	if req.LogLevel != nil || req.LogFormat != nil {
		h.rebuildLogger(cfg)
	}
}
