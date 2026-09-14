// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_load.go 是**加载与保存**：LoadFromProvider（从 provider 解码 + 默认值 + 校验）、
// LoadConfig（配置文件路径版本，保留供测试兼容）、SaveConfig（0600 落盘，含敏感信息策略说明）。
//
// 拆分说明见 config.go 顶部。

package server

import (
	"fmt"
	"os"

	"github.com/cocomhub/sproxy/pkg/provider"
	"gopkg.in/yaml.v3"
)

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
	// 敏感信息策略（原 TODO「AuthToken 脱敏」的审计结论，2026-09-14）：
	//   - 落盘**必须**含明文密钥（tunnel_key / access_keys[].secret / api_keys[].key 等），
	//     否则重启后无法工作；故配置文件本身不脱敏，靠 0600 权限约束（见下方 os.WriteFile）。
	//   - 需脱敏的是对外**展示**面：客户端配置回显走 pkg/client.HandleConfigShow（S49 凭据全掩）；
	//     服务端当前无 config dump 通道，若将来新增（如 `sproxy config show`）必须复用同一掩码约定。
	//   - 日志面：装配层只打印白名单字段（cmd/sproxy/root.go 的 config loaded / TLS enabled），
	//     不整体 dump Config，故无密钥入日志路径。
	// 另：本文件已无 AuthToken 字段（术语已由 AccessKey 替代），旧 TODO 所指对象不存在。
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("写入配置文件失败: %w", err)
	}

	return nil
}
