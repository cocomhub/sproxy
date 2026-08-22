// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"gopkg.in/yaml.v3"
)

// TLSConfig 是 TLS 相关配置，支持三种证书模式：
//   - CertFile + KeyFile：静态文件证书（最高优先级）
//   - ACME.Enabled：ACME 自动证书
//   - AutoTLS：自签证书（默认 fallback）
type TLSConfig struct {
	Enabled  bool       `yaml:"enabled" mapstructure:"enabled"`
	CertFile string     `yaml:"cert_file" mapstructure:"cert_file"`
	KeyFile  string     `yaml:"key_file" mapstructure:"key_file"`
	AutoTLS  bool       `yaml:"auto_tls" mapstructure:"auto_tls"`
	ClientCA string     `yaml:"client_ca" mapstructure:"client_ca"` // mTLS: CA 证书路径，非空时启用客户端证书验证
	ACME     ACMEConfig `yaml:"acme" mapstructure:"acme"`           // ACME 自动证书配置（可选）
}

// ACMEConfig 是 ACME 自动证书的配置。
type ACMEConfig struct {
	Enabled    bool     `yaml:"enabled" mapstructure:"enabled"`
	Domains    []string `yaml:"domains" mapstructure:"domains"`
	Email      string   `yaml:"email" mapstructure:"email"`
	CacheDir   string   `yaml:"cache_dir" mapstructure:"cache_dir"`
	HTTP01     bool     `yaml:"http01" mapstructure:"http_01"`
	HTTP01Port string   `yaml:"http01_port" mapstructure:"http_01_port"`
}

type RateLimitConfig struct {
	Enabled  bool          `yaml:"enabled" mapstructure:"enabled"`
	Requests int           `yaml:"requests" mapstructure:"requests"`
	Window   time.Duration `yaml:"window" mapstructure:"window"`
}

type ServerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header" mapstructure:"read_header"`
	Read       time.Duration `yaml:"read" mapstructure:"read"`
	Write      time.Duration `yaml:"write" mapstructure:"write"`
	Idle       time.Duration `yaml:"idle" mapstructure:"idle"`
	Shutdown   time.Duration `yaml:"shutdown" mapstructure:"shutdown"`
}

type VersionConfig struct {
	Enabled     bool `yaml:"enabled" mapstructure:"enabled"`
	MaxVersions int  `yaml:"max_versions" mapstructure:"max_versions"`
}

// HubConfig 配置 Hub 中继系统。
type HubConfig struct {
	Enabled    bool             `yaml:"enabled" mapstructure:"enabled"`
	NodeID     string           `yaml:"node_id" mapstructure:"node_id"`
	RelayToken string           `yaml:"relay_token" mapstructure:"relay_token"`
	Transports TransportConfigs `yaml:"transports" mapstructure:"transports"`
}

// TransportConfigs 聚合所有可用的传输层配置，当前为预留扩展，暂无产品代码消费。
type TransportConfigs struct {
	WS WSTransportConfig `yaml:"ws" mapstructure:"ws"` // 预留：WebSocket 传输监听配置
}

// WSTransportConfig 配置 WebSocket 传输监听。
// 当前 WS 升级端点挂载到主 HTTP server（与文件服务同端口，路径由 Path 指定），
// 因此 Listen 字段仅作预留（独立端口模式未来扩展），实际监听端口由 addr 决定。
type WSTransportConfig struct {
	Enabled bool   `yaml:"enabled" mapstructure:"enabled"`
	Listen  string `yaml:"listen" mapstructure:"listen"`
	Path    string `yaml:"path" mapstructure:"path"`
}

type Config struct {
	Addr       string `yaml:"addr" mapstructure:"addr"`
	UploadsDir string `yaml:"uploads_dir" mapstructure:"uploads_dir"`
	// MaxUploadBytes 已移至 internal/size.UploadBodyLimit（1 GiB 硬限制），不可配置。
	// MaxChunkUploadBytes 已移至 internal/size.DefaultChunkBodyLimit（64 MiB 硬限制），不可配置。
	ServerTimeouts ServerTimeouts  `yaml:"server_timeouts" mapstructure:"server_timeouts"`
	LogLevel       string          `yaml:"log_level" mapstructure:"log_level"`
	LogFormat      string          `yaml:"log_format" mapstructure:"log_format"`
	MaxHeaderBytes int             `yaml:"max_header_bytes" mapstructure:"max_header_bytes"`
	TunnelKey      string          `yaml:"tunnel_key" mapstructure:"tunnel_key"`
	TLS            TLSConfig       `yaml:"tls" mapstructure:"tls"`
	AuthToken      string          `yaml:"auth_token" mapstructure:"auth_token"`
	RateLimit      RateLimitConfig `yaml:"rate_limit" mapstructure:"rate_limit"`
	CORS           CORSConfig      `yaml:"cors" mapstructure:"cors"`

	// 分块上传配置
	ChunkSize        int64         `yaml:"chunk_size" mapstructure:"chunk_size"`
	UploadSessionTTL time.Duration `yaml:"upload_session_ttl" mapstructure:"upload_session_ttl"`

	// 文件版本管理（默认关闭）
	Versioning VersionConfig `yaml:"versioning" mapstructure:"versioning"`

	// API 密钥配置
	APIKeys APIKeyConfig `yaml:"api_keys" mapstructure:"api_keys"`

	// Hub 中继系统（默认关闭）
	Hub HubConfig `yaml:"hub" mapstructure:"hub"`

	// 存储空间控制
	MaxStorageBytes int64 `yaml:"max_storage_bytes" mapstructure:"max_storage_bytes"` // 存储上限（字节），0 = 不限制

	// 云端下载配置
	CloudSyncThreshold        int64         `yaml:"cloud_sync_threshold" mapstructure:"cloud_sync_threshold"`
	CloudDownloader           string        `yaml:"cloud_downloader" mapstructure:"cloud_downloader"`
	CloudTaskTTL              time.Duration `yaml:"cloud_task_ttl" mapstructure:"cloud_task_ttl"`
	CloudFailedTaskTTL        time.Duration `yaml:"cloud_failed_task_ttl" mapstructure:"cloud_failed_task_ttl"`
	CloudMaxConcurrent        int           `yaml:"cloud_max_concurrent" mapstructure:"cloud_max_concurrent"`
	CloudMaxBatchURLs         int           `yaml:"cloud_max_batch_urls" mapstructure:"cloud_max_batch_urls"`
	CloudDownloadAllowPrivate bool          `yaml:"cloud_download_allow_private" mapstructure:"cloud_download_allow_private"`
	CloudDownloadTimeout      time.Duration `yaml:"cloud_download_timeout" mapstructure:"cloud_download_timeout"`
	CloudDownloadIdleTimeout  time.Duration `yaml:"cloud_download_idle_timeout" mapstructure:"cloud_download_idle_timeout"`
	CloudMaxRetries           int           `yaml:"cloud_max_retries" mapstructure:"cloud_max_retries"`
	CloudRetryDelay           time.Duration `yaml:"cloud_retry_delay" mapstructure:"cloud_retry_delay"`
	// CloudArchiveMaxBytes 单次云归档允许的最大字节数（原始文件大小总和），0 = 不限制（仍受 max_storage_bytes 与 TryReserve 兜底）。
	CloudArchiveMaxBytes int64 `yaml:"cloud_archive_max_bytes" mapstructure:"cloud_archive_max_bytes"`
}

func Default() *Config {
	return &Config{
		Addr:       ":18083",
		UploadsDir: "./uploads",
		ServerTimeouts: ServerTimeouts{
			Shutdown: 30 * time.Second,
		},
		RateLimit: RateLimitConfig{
			Requests: 10,
			Window:   time.Second,
		},
		TLS: TLSConfig{
			Enabled: true,
			AutoTLS: true,
		},
		CORS: CORSConfig{
			MaxAge: defaultMaxAge,
		},
		ChunkSize:                 size.DefaultChunkSize,
		UploadSessionTTL:          24 * time.Hour,
		CloudSyncThreshold:        20 * 1024 * 1024, // 20 MiB
		CloudDownloader:           "http",
		CloudTaskTTL:              24 * time.Hour,
		CloudFailedTaskTTL:        1 * time.Hour,
		CloudMaxConcurrent:        3,
		CloudMaxBatchURLs:         100,
		CloudDownloadAllowPrivate: false,
		CloudDownloadTimeout:      30 * time.Minute,
		CloudDownloadIdleTimeout:  1 * time.Minute,
		CloudMaxRetries:           10,
		CloudRetryDelay:           10 * time.Second,
	}
}

// SetDefaults 设置零值字段为默认值。
func (c *Config) SetDefaults() {
	if c.Addr == "" {
		c.Addr = ":18083"
	}
	if c.UploadsDir == "" {
		c.UploadsDir = "./uploads"
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.UploadSessionTTL <= 0 {
		c.UploadSessionTTL = 24 * time.Hour
	}
	if c.ServerTimeouts.Shutdown <= 0 {
		c.ServerTimeouts.Shutdown = 30 * time.Second
	}
	if c.CloudSyncThreshold <= 0 {
		c.CloudSyncThreshold = 20 * 1024 * 1024
	}
	if c.CloudDownloader == "" {
		c.CloudDownloader = "http"
	}
	if c.CloudDownloadTimeout <= 0 {
		c.CloudDownloadTimeout = 30 * time.Minute
	}
	if c.CloudDownloadIdleTimeout <= 0 {
		c.CloudDownloadIdleTimeout = 1 * time.Minute
	}
	if c.CloudMaxRetries < 1 {
		c.CloudMaxRetries = 10
	}
	if c.CloudRetryDelay <= 0 {
		c.CloudRetryDelay = 10 * time.Second
	}
	if c.CloudMaxConcurrent <= 0 {
		c.CloudMaxConcurrent = 3
	}
	if c.CloudMaxBatchURLs == 0 {
		c.CloudMaxBatchURLs = 100
	}
	if c.CloudTaskTTL <= 0 {
		c.CloudTaskTTL = 24 * time.Hour
	}
	if c.CloudFailedTaskTTL <= 0 {
		c.CloudFailedTaskTTL = 1 * time.Hour
	}
}

// Validate 校验配置合理性。
func (c *Config) Validate() error {
	if c.Addr == "" {
		return fmt.Errorf("addr 为空，请配置监听地址")
	}
	if c.UploadsDir == "" {
		return fmt.Errorf("uploads_dir 为空，请配置上传目录")
	}
	if c.TunnelKey == "" && !c.TLS.Enabled {
		return fmt.Errorf("tunnel_key 为空且 TLS 未启用，传输将完全明文，请配置 tunnel_key 或启用 TLS")
	}
	if c.TunnelKey != "" {
		if _, err := tunnel.ParseKey(c.TunnelKey); err != nil {
			return fmt.Errorf("tunnel_key 校验失败（必须是 64 位十六进制字符 0-9a-fA-F）: %w", err)
		}
	}
	if c.APIKeys.Enabled && len(c.APIKeys.Keys) == 0 {
		return fmt.Errorf("api_keys.enabled=true 但未配置任何密钥，认证将拒绝所有请求")
	}
	for i, k := range c.APIKeys.Keys {
		if k.Key == "" {
			return fmt.Errorf("api_keys[%d].key 为空，密钥不能为空字符串", i)
		}
		switch k.Permission {
		case PermissionRead, PermissionWrite, "":
		default:
			return fmt.Errorf("api_keys[%d].permission=%q 无效，仅允许 %q 或 %q", i, k.Permission, PermissionRead, PermissionWrite)
		}
	}
	if c.RateLimit.Enabled && c.RateLimit.Requests <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 requests=%d 无效，请设置大于 0 的值", c.RateLimit.Requests)
	}
	if c.RateLimit.Enabled && c.RateLimit.Window <= 0 {
		return fmt.Errorf("rate_limit.enabled=true 但 window=%s 无效，请设置大于 0 的 duration", c.RateLimit.Window)
	}
	return nil
}

// LoadFromProvider 从 provider.Provider 解码配置，设置默认值并校验。
func LoadFromProvider(p provider.Provider) (*Config, error) {
	cfg := Default()
	if err := p.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// LoadConfig 加载配置文件。路径为空或文件不存在时返回默认配置，不自动创建文件。
func LoadConfig(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败: %w", err)
	}
	if len(data) == 0 {
		return cfg, nil
	}

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置校验失败: %w", err)
	}

	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	// TODO: 后续优化敏感信息管理（TunnelKey/AuthToken 脱敏）
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	return nil
}
