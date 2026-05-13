// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package handlers

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ServerTimeouts struct {
	ReadHeader time.Duration `yaml:"read_header"`
	Read       time.Duration `yaml:"read"`
	Write      time.Duration `yaml:"write"`
	Idle       time.Duration `yaml:"idle"`
}

type Config struct {
	Addr           string         `yaml:"addr"`
	UploadsDir     string         `yaml:"uploads_dir"`
	AllowedHosts   []string       `yaml:"allowed_hosts"`
	ServerTimeouts ServerTimeouts `yaml:"server_timeouts"`
	ClientTimeout  time.Duration  `yaml:"client_timeout"`
	LogLevel       string         `yaml:"log_level"`
	LogFormat      string         `yaml:"log_format"`
	MaxHeaderBytes int            `yaml:"max_header_bytes"`
	TunnelKey      string         `yaml:"tunnel_key"`
}

func Default() *Config {
	return &Config{
		Addr:       ":18083",
		UploadsDir: "./uploads",
		TunnelKey:  "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
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
