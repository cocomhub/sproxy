// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"gopkg.in/yaml.v3"
)

type SclientConfig struct {
	ServerURL        string `yaml:"server_url"`
	UploadEndpoint   string `yaml:"upload_endpoint"`
	DownloadEndpoint string `yaml:"download_endpoint"`
	DeleteEndpoint   string `yaml:"delete_endpoint"`
	CheckMD5         bool   `yaml:"check_md5"`
	Timeout          int    `yaml:"timeout"`
}

func DefaultConfig() *SclientConfig {
	return &SclientConfig{
		ServerURL:        "http://localhost:18080",
		UploadEndpoint:   "/upload",
		DownloadEndpoint: "/download",
		DeleteEndpoint:   "/delete",
		CheckMD5:         true,
		Timeout:          300,
	}
}

func configFilePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".sclient.yaml"), nil
}

func LoadConfig(path string) (*SclientConfig, error) {
	if path == "" {
		var err error
		path, err = configFilePath()
		if err != nil {
			return nil, fmt.Errorf("获取配置文件路径失败: %w", err)
		}
	}

	cfg := DefaultConfig()

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

	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件失败: %w", err)
	}

	return cfg, nil
}

func SaveConfig(cfg *SclientConfig, path string) error {
	if path == "" {
		var err error
		path, err = configFilePath()
		if err != nil {
			return fmt.Errorf("获取配置文件路径失败: %w", err)
		}
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	return nil
}

func HandleConfigShow(cfg *SclientConfig) {
	fmt.Printf("server_url: %s\n", cfg.ServerURL)
	fmt.Printf("upload_endpoint: %s\n", cfg.UploadEndpoint)
	fmt.Printf("download_endpoint: %s\n", cfg.DownloadEndpoint)
	fmt.Printf("delete_endpoint: %s\n", cfg.DeleteEndpoint)
	fmt.Printf("check_md5: %v\n", cfg.CheckMD5)
	fmt.Printf("timeout: %d\n", cfg.Timeout)
}

func HandleConfigSet(cfg *SclientConfig, configPath, key, value string) error {
	switch key {
	case "server_url":
		cfg.ServerURL = value
	case "upload_endpoint":
		cfg.UploadEndpoint = value
	case "download_endpoint":
		cfg.DownloadEndpoint = value
	case "delete_endpoint":
		cfg.DeleteEndpoint = value
	case "check_md5":
		switch value {
		case "true", "1", "yes":
			cfg.CheckMD5 = true
		case "false", "0", "no":
			cfg.CheckMD5 = false
		default:
			return fmt.Errorf("check_md5 的值必须是 true 或 false")
		}
	case "timeout":
		v, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("timeout 的值必须是整数")
		}
		if v <= 0 {
			return fmt.Errorf("timeout 的值必须大于 0")
		}
		cfg.Timeout = v
	default:
		return fmt.Errorf("未知的配置项: %s，支持的配置项: server_url, upload_endpoint, download_endpoint, delete_endpoint, check_md5, timeout", key)
	}

	if err := SaveConfig(cfg, configPath); err != nil {
		return err
	}

	fmt.Printf("配置已更新: %s = %s\n", key, value)
	return nil
}