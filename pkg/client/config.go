// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// Config 是 sclient 的配置文件结构。
type Config struct {
	ServerURL    string `yaml:"server_url" mapstructure:"server_url"`
	NoChecksum   bool   `yaml:"no_checksum" mapstructure:"no_checksum"`
	Timeout      int    `yaml:"timeout" mapstructure:"timeout"`
	TunnelKey    string `yaml:"tunnel_key" mapstructure:"tunnel_key"`
	ChunkSize    int64  `yaml:"chunk_size" mapstructure:"chunk_size"`
	MaxChunkSize int64  `yaml:"max_chunk_size" mapstructure:"max_chunk_size"`
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL:  "http://localhost:18083",
		NoChecksum: false,
		Timeout:    300,
		ChunkSize:  size.DefaultChunkSize, // 4 MiB
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

// LoadFromViper 从 viper 实例解码配置，合并默认值并校验。
func LoadFromViper(v *viper.Viper) (*Config, error) {
	cfg := DefaultConfig()
	if err := v.Unmarshal(cfg); err != nil {
		return nil, fmt.Errorf("配置解码失败: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("获取用户主目录失败: %w", err)
	}
	return filepath.Join(home, ".sclient.yaml"), nil
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
	fmt.Printf("NoChecksum:    %v\n", cfg.NoChecksum)
	fmt.Printf("Timeout:       %d\n", cfg.Timeout)
	maskedKey := cfg.TunnelKey
	if len(maskedKey) > 8 {
		maskedKey = maskedKey[:4] + "****" + maskedKey[len(maskedKey)-4:]
	}
	fmt.Printf("TunnelKey:     %s\n", maskedKey)
	fmt.Printf("ChunkSize:     %d\n", cfg.ChunkSize)
	fmt.Printf("MaxChunkSize:  %d\n", cfg.MaxChunkSize)
}

func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "no_checksum":
		cfg.NoChecksum = value == "true"
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
