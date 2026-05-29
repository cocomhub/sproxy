// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Config 是 sclient 的配置文件结构。
type Config struct {
	ServerURL        string `yaml:"server_url"`
	UploadEndpoint   string `yaml:"upload_endpoint"`
	DownloadEndpoint string `yaml:"download_endpoint"`
	DeleteEndpoint   string `yaml:"delete_endpoint"`
	CheckChecksum    bool   `yaml:"check_checksum"`
	Timeout          int    `yaml:"timeout"`
	TunnelKey        string `yaml:"tunnel_key"`
	TunnelEndpoint   string `yaml:"tunnel_endpoint"`
	ChunkSize        int64  `yaml:"chunk_size"` // 分块上传/下载块大小（字节），默认 4 MiB
}

func DefaultConfig() *Config {
	return &Config{
		ServerURL:        "http://localhost:18083",
		UploadEndpoint:   "/upload",
		DownloadEndpoint: "/download",
		DeleteEndpoint:   "/delete",
		CheckChecksum:    true,
		Timeout:          300,
		TunnelKey:        "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef",
		TunnelEndpoint:   "/tunnel",
		ChunkSize:        4 << 20, // 4 MiB
	}
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
	fmt.Printf("ServerURL:      %s\n", cfg.ServerURL)
	fmt.Printf("UploadEndpoint:   %s\n", cfg.UploadEndpoint)
	fmt.Printf("DownloadEndpoint: %s\n", cfg.DownloadEndpoint)
	fmt.Printf("DeleteEndpoint:   %s\n", cfg.DeleteEndpoint)
	fmt.Printf("CheckChecksum:   %v\n", cfg.CheckChecksum)
	fmt.Printf("Timeout:        %d\n", cfg.Timeout)
	maskedKey := cfg.TunnelKey
	if len(maskedKey) > 8 {
		maskedKey = maskedKey[:4] + "****" + maskedKey[len(maskedKey)-4:]
	}
	fmt.Printf("TunnelKey:      %s\n", maskedKey)
	fmt.Printf("TunnelEndpoint: %s\n", cfg.TunnelEndpoint)
	fmt.Printf("ChunkSize:      %d\n", cfg.ChunkSize)
}

func HandleConfigSet(cfg *Config, configPath, key, value string) error {
	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "upload_endpoint":
		cfg.UploadEndpoint = value
	case "download_endpoint":
		cfg.DownloadEndpoint = value
	case "delete_endpoint":
		cfg.DeleteEndpoint = value
	case "check_checksum":
		cfg.CheckChecksum = value == "true"
	case "timeout":
		if _, err := fmt.Sscanf(value, "%d", &cfg.Timeout); err != nil {
			return fmt.Errorf("无效的超时值: %w", err)
		}
	case "tunnel_key":
		cfg.TunnelKey = value
	case "tunnel_endpoint":
		cfg.TunnelEndpoint = value
	case "chunk_size":
		if _, err := fmt.Sscanf(value, "%d", &cfg.ChunkSize); err != nil {
			return fmt.Errorf("无效的分块大小: %w", err)
		}
	default:
		return fmt.Errorf("未知配置键: %s", key)
	}
	return SaveConfig(cfg, configPath)
}
