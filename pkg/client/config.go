// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"gopkg.in/yaml.v3"
)

// Config 是 sclient 的配置文件结构。
type Config struct {
	ServerURL    string `yaml:"server_url" mapstructure:"server_url"`
	Timeout      int    `yaml:"timeout" mapstructure:"timeout"`
	TunnelKey    string `yaml:"tunnel_key" mapstructure:"tunnel_key"`
	ChunkSize    int64  `yaml:"chunk_size" mapstructure:"chunk_size"`
	MaxChunkSize int64  `yaml:"max_chunk_size" mapstructure:"max_chunk_size"`
	AuthToken    string `yaml:"auth_token" mapstructure:"auth_token"`
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL: "http://localhost:18083",
		Timeout:   300,
		ChunkSize: size.DefaultChunkSize, // 4 MiB
	}
}

// Validate 校验配置合理性，设置零值字段为默认值。
func (c *Config) Validate() error {
	if c.ServerURL == "" {
		c.ServerURL = "http://localhost:18083"
	}
	if c.Timeout <= 0 {
		c.Timeout = 300
	}
	if c.ChunkSize <= 0 {
		c.ChunkSize = size.DefaultChunkSize
	}
	if c.TunnelKey != "" && len(c.TunnelKey) != 64 {
		return fmt.Errorf("tunnel_key 必须是 64 位 hex 字符")
	}
	return nil
}

// LoadFromProvider 从 provider.Provider 解码配置，合并默认值并校验。
func LoadFromProvider(p provider.Provider) (*Config, error) {
	cfg := DefaultConfig()
	if err := p.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
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
			if saveErr := SaveConfig(cfg, path); saveErr != nil {
				return nil, fmt.Errorf("创建默认配置文件失败: %w", saveErr)
			}
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
	return cfg, nil
}

func SaveConfig(cfg *Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}
	return nil
}

func HandleConfigShow(cfg *Config) {
	fmt.Printf("ServerURL:     %s\n", cfg.ServerURL)
	fmt.Printf("Timeout:       %d\n", cfg.Timeout)
	maskedKey := cfg.TunnelKey
	if len(maskedKey) > 8 {
		maskedKey = maskedKey[:4] + "****" + maskedKey[len(maskedKey)-4:]
	}
	fmt.Printf("TunnelKey:     %s\n", maskedKey)
	maskedToken := cfg.AuthToken
	if len(maskedToken) > 8 {
		maskedToken = maskedToken[:4] + "****" + maskedToken[len(maskedToken)-4:]
	}
	fmt.Printf("AuthToken:     %s\n", maskedToken)
	fmt.Printf("ChunkSize:     %d\n", cfg.ChunkSize)
	fmt.Printf("MaxChunkSize:  %d\n", cfg.MaxChunkSize)
}

func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "auth_token":
		cfg.AuthToken = value
	case "timeout":
		if _, err := fmt.Sscanf(value, "%d", &cfg.Timeout); err != nil {
			return fmt.Errorf("无效的超时值: %w", err)
		}
	case "tunnel_key":
		cfg.TunnelKey = value
	case "chunk_size":
		if _, err := fmt.Sscanf(value, "%d", &cfg.ChunkSize); err != nil {
			return fmt.Errorf("无效的分块大小: %w", err)
		}
	case "max_chunk_size":
		if _, err := fmt.Sscanf(value, "%d", &cfg.MaxChunkSize); err != nil {
			return fmt.Errorf("无效的最大分块大小: %w", err)
		}
	default:
		return fmt.Errorf("未知配置键: %s", key)
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
	resp, err := c.doRequest(ctx, "GET", "/api/config", nil, nil)
	if err != nil {
		return nil, fmt.Errorf("获取配置失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("获取配置失败 (HTTP %d): %s", resp.StatusCode, string(body))
	}

	var cfg ConfigResponse
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}
	return &cfg, nil
}

// UpdateConfig 更新远程服务器运行时配置。
// 只更新指定的字段，未指定的字段保持不变。
// 可更新的字段：log_level, log_format, auth_token, rate_limit_requests, rate_limit_window。
func (c *FileClient) UpdateConfig(ctx context.Context, updates map[string]interface{}) error {
	body, err := json.Marshal(updates)
	if err != nil {
		return fmt.Errorf("序列化请求体失败: %w", err)
	}

	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	resp, err := c.doRequest(ctx, "PUT", "/api/config", bytes.NewReader(body), headers)
	if err != nil {
		return fmt.Errorf("更新配置失败: %w", err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("更新配置失败 (HTTP %d): %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Success bool `json:"success"`
		Changed bool `json:"changed"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("解析响应失败: %s", string(respBody))
	}
	if !result.Success {
		return fmt.Errorf("更新配置失败")
	}
	return nil
}
