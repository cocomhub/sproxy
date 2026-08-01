// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strconv"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"gopkg.in/yaml.v3"
)

// Config 是 sclient 的配置文件结构。
type Config struct {
	ServerURL              string `yaml:"server_url" mapstructure:"server_url"`
	Timeout                int    `yaml:"timeout" mapstructure:"timeout"`
	TunnelKey              string `yaml:"tunnel_key" mapstructure:"tunnel_key"`
	ChunkSize              int64  `yaml:"chunk_size" mapstructure:"chunk_size"`
	MaxChunkSize           int64  `yaml:"max_chunk_size" mapstructure:"max_chunk_size"`
	AuthToken              string `yaml:"auth_token" mapstructure:"auth_token"`
	AllowTransportFallback bool   `yaml:"allow_transport_fallback" mapstructure:"allow_transport_fallback"`
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL:    "https://127.0.0.1:18083",
		Timeout:      300,
		ChunkSize:    size.DefaultChunkSize,    // 4 MiB
		MaxChunkSize: size.DefaultMaxChunkSize, // 64 MiB
	}
}

// SetDefaults 设置零值字段为默认值（副作用）。调用 Validate 前必须调用此方法。
func (c *Config) SetDefaults() {
	if c == nil {
		return
	}
	if c.ServerURL == "" {
		c.ServerURL = "https://127.0.0.1:18083"
	}
	if c.Timeout <= 0 {
		c.Timeout = 300
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.MaxChunkSize <= 0 {
		c.MaxChunkSize = size.DefaultMaxChunkSize
	}
}

// Validate 校验配置合理性，不修改字段值。
// 设置默认值请在调用 Validate 前先调用 SetDefaults()。
func (c *Config) Validate() error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	if !isValidServerURL(c.ServerURL) {
		return fmt.Errorf("server_url 无效: %s", c.ServerURL)
	}
	if c.Timeout <= 0 {
		return fmt.Errorf("timeout 必须大于 0")
	}
	if c.ChunkSize <= 0 {
		return fmt.Errorf("chunk_size 必须大于 0")
	}
	if c.TunnelKey != "" && len(c.TunnelKey) != 64 {
		return fmt.Errorf("tunnel_key 必须是 64 位 hex 字符")
	}
	return nil
}

// LoadFromProvider 从 provider.Provider 解码配置，设置默认值并校验。
func LoadFromProvider(p provider.Provider) (*Config, error) {
	cfg := DefaultConfig()
	if err := p.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil // 返回默认配置，不创建文件
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
		return nil, err
	}
	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	if containsPathTraversal(path) {
		return fmt.Errorf("配置路径包含非法路径穿越: %s", path)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

// isValidServerURL 校验 server URL 格式是否合法。
func isValidServerURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && u.Scheme != "" && u.Host != ""
}

func HandleConfigShow(cfg *Config, w io.Writer) {
	if cfg == nil {
		return
	}
	fmt.Fprintf(w, "ServerURL:     %s\n", cfg.ServerURL)
	fmt.Fprintf(w, "Timeout:       %d\n", cfg.Timeout)
	maskedKey := cfg.TunnelKey
	if len(maskedKey) > 4 {
		maskedKey = maskedKey[:4] + "****"
	} else if len(maskedKey) > 0 {
		maskedKey = "****"
	}
	fmt.Fprintf(w, "TunnelKey:     %s\n", maskedKey)
	maskedToken := cfg.AuthToken
	if len(maskedToken) > 4 {
		maskedToken = maskedToken[:4] + "****"
	} else if len(maskedToken) > 0 {
		maskedToken = "****"
	}
	fmt.Fprintf(w, "AuthToken:     %s\n", maskedToken)
	fmt.Fprintf(w, "ChunkSize:     %d\n", cfg.ChunkSize)
	fmt.Fprintf(w, "MaxChunkSize:  %d\n", cfg.MaxChunkSize)
	fmt.Fprintf(w, "AllowTransportFallback: %v\n", cfg.AllowTransportFallback)
}

// ApplyConfigSet 在内存中更新配置，不写文件。返回更新后的配置和错误。
func ApplyConfigSet(cfg *Config, key, value string) error {
	switch key {
	case "server_url":
		if !isValidServerURL(value) {
			return fmt.Errorf("无效的服务器地址: %s", value)
		}
		cfg.ServerURL = value
	case "auth_token":
		cfg.AuthToken = value
	case "timeout":
		if timeout, err := strconv.Atoi(value); err != nil {
			return fmt.Errorf("无效的超时值: %w", err)
		} else {
			cfg.Timeout = timeout
		}
	case "tunnel_key":
		cfg.TunnelKey = value
	case "chunk_size":
		chunkSize, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的分块大小: %w", err)
		}
		cfg.ChunkSize = chunkSize
	case "max_chunk_size":
		maxChunkSize, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return fmt.Errorf("无效的最大分块大小: %w", err)
		}
		cfg.MaxChunkSize = maxChunkSize
	default:
		return fmt.Errorf("未知配置键: %s", key)
	}
	return nil
}

// HandleConfigSet 更新配置并写入文件。
func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	if err := ApplyConfigSet(cfg, key, value); err != nil {
		return err
	}
	return SaveConfig(cfg, configPath)
}

// ConfigResponse 是 GET /api/config 的响应结构体。
type ConfigResponse struct {
	LogLevel           string `json:"log_level"`
	LogFormat          string `json:"log_format"`
	AuthTokenSet       bool   `json:"auth_token_set"`
	TunnelKeySet       bool   `json:"tunnel_key_set"`
	RateLimitRequests  int    `json:"rate_limit_requests"`
	RateLimitWindow    string `json:"rate_limit_window"`
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

// GetConfig 获取远程服务器配置。
func (c *FileClient) GetConfig(ctx context.Context) (*ConfigResponse, error) {
	var cfg ConfigResponse
	if err := c.doJSON(ctx, "GET", "/api/config", nil, &cfg); err != nil {
		return nil, fmt.Errorf("获取配置失败: %w", err)
	}
	return &cfg, nil
}

// UpdateConfig 更新远程服务器运行时配置。
// 只更新指定的字段，未指定的字段保持不变。
// 可更新的字段：log_level, log_format, auth_token, rate_limit_requests, rate_limit_window。
func (c *FileClient) UpdateConfig(ctx context.Context, updates map[string]any) error {
	var result doJSONResp
	if err := c.doJSON(ctx, "PUT", "/api/config", updates, &result); err != nil {
		return fmt.Errorf("更新配置失败: %w", err)
	}
	return nil
}
