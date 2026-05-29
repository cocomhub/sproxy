// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type TLSConfig struct {
	Enabled  bool   `yaml:"enabled"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
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
}

type Config struct {
	Addr           string          `yaml:"addr"`
	UploadsDir     string          `yaml:"uploads_dir"`
	MaxUploadBytes int64           `yaml:"max_upload_bytes"`
	ServerTimeouts ServerTimeouts  `yaml:"server_timeouts"`
	LogLevel       string          `yaml:"log_level"`
	LogFormat      string          `yaml:"log_format"`
	MaxHeaderBytes int             `yaml:"max_header_bytes"`
	TunnelKey      string          `yaml:"tunnel_key"`
	TLS            TLSConfig       `yaml:"tls"`
	AuthToken      string          `yaml:"auth_token"`
	RateLimit      RateLimitConfig `yaml:"rate_limit"`

	// 分块上传配置
	ChunkSize           int64         `yaml:"chunk_size"`             // 每块大小，默认 4 MiB
	MaxChunkUploadBytes int64         `yaml:"max_chunk_upload_bytes"` // 单块请求体最大限制，默认 8 MiB
	UploadSessionTTL    time.Duration `yaml:"upload_session_ttl"`     // 未完成上传会话保留时间，默认 24h
}

func Default() *Config {
	return &Config{
		Addr:           ":18083",
		UploadsDir:     "./uploads",
		MaxUploadBytes: 1 << 30, // 1 GiB
		RateLimit: RateLimitConfig{
			Requests: 10,
			Window:   time.Second,
		},
		ChunkSize:           4 << 20,        // 4 MiB
		MaxChunkUploadBytes: 8 << 20,        // 8 MiB
		UploadSessionTTL:    24 * time.Hour, // 24h
	}
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
