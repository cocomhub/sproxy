// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"time"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
	AutoTLS  bool   `yaml:"auto_tls"`
}

type RateLimitConfig struct {
	Enabled  bool          `yaml:"enabled"`
	Requests int           `yaml:"requests"`
	Window   time.Duration `yaml:"window"`
}

type ServerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header"`
	Read       time.Duration `yaml:"read"`
	Write      time.Duration `yaml:"write"`
	Idle       time.Duration `yaml:"idle"`
	Shutdown   time.Duration `yaml:"shutdown"`
}

type VersionConfig struct {
	Enabled     bool `yaml:"enabled" mapstructure:"enabled"`
	MaxVersions int  `yaml:"max_versions" mapstructure:"max_versions"`
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
	MaxChunkSize     int64         `yaml:"max_chunk_size" mapstructure:"max_chunk_size"` // 仅 sclient 使用；服务端按 DefaultChunkBodyLimit 限制
	UploadSessionTTL time.Duration `yaml:"upload_session_ttl" mapstructure:"upload_session_ttl"`

	// 文件版本管理（默认关闭）
	Versioning VersionConfig `yaml:"versioning" mapstructure:"versioning"`

	// API 密钥配置
	APIKeys APIKeyConfig `yaml:"api_keys" mapstructure:"api_keys"`
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
			AutoTLS: false,
		},
		CORS: CORSConfig{
			MaxAge: 86400,
		},
		ChunkSize:        size.DefaultChunkSize,
		UploadSessionTTL: 24 * time.Hour,
	}
}

// Validate 校验配置合理性，设置零值字段为默认值。
func (c *Config) Validate() error {
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
	if c.TunnelKey != "" {
		// 同时校验长度与 hex 格式，避免运行时 hex.DecodeString 报错才发现。
		// 复用 pkg/tunnel.ParseKey 保持单一来源。
		if _, err := tunnel.ParseKey(c.TunnelKey); err != nil {
			return fmt.Errorf("tunnel_key 校验失败（必须是 64 位十六进制字符 0-9a-fA-F）: %w", err)
		}
	}
	return nil
}

// LoadFromViper 从 viper 实例解码配置，合并默认值并校验。
func LoadFromViper(v *viper.Viper) (*Config, error) {
	cfg := Default()
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func LoadConfig(path string) (*Config, error) {
	cfg := Default()
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
