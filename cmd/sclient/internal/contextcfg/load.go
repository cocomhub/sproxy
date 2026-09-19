// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package contextcfg

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// configFilePerm 是配置文件权限（凭据 access_key_secret 明文，600）。
const configFilePerm = 0o600

// Load 读取配置文件；文件不存在或为空时返回空模型（不报错，不创建文件）。
// 解析失败/校验失败返回错误（fail-closed，不静默用默认值掩盖配置损坏）。
func Load(path string) (*Config, error) {
	if path == "" {
		return NewDefault(), nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return NewDefault(), nil
		}
		return nil, fmt.Errorf("读取配置文件 %s 失败: %w", path, err)
	}
	if len(data) == 0 {
		return NewDefault(), nil
	}
	cfg := NewDefault()
	if err := yaml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 %s 失败: %w", path, err)
	}
	// 旧格式（无 apiVersion/kind）也允许解析——兼容迁移中间态；但引用自洽必须校验。
	if cfg.APIVersion == "" {
		cfg.APIVersion = APIVersion
	}
	if cfg.Kind == "" {
		cfg.Kind = Kind
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("配置文件 %s 校验失败: %w", path, err)
	}
	return cfg, nil
}

// Save 原子写配置文件（0600）：先写同目录临时文件再 rename，避免半写状态。
func Save(cfg *Config, path string) error {
	if cfg == nil {
		return fmt.Errorf("配置为空，无法保存")
	}
	if path == "" {
		return fmt.Errorf("配置文件路径为空")
	}
	// 内存构造的配置可能缺版本/类型标识（程序内 NewDefault 已带；手写结构体补全）。
	if cfg.APIVersion == "" {
		cfg.APIVersion = APIVersion
	}
	if cfg.Kind == "" {
		cfg.Kind = Kind
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("保存前校验失败: %w", err)
	}
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("序列化配置失败: %w", err)
	}
	dir := filepath.Dir(path)
	if mkErr := os.MkdirAll(dir, 0o700); mkErr != nil {
		return fmt.Errorf("创建配置目录 %s 失败: %w", dir, mkErr)
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("创建临时配置文件失败: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // 失败路径清理；成功后 rename 使该路径失效
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("写入临时配置文件失败: %w", err)
	}
	if err := tmp.Chmod(configFilePerm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("设置配置文件权限失败: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("关闭临时配置文件失败: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("原子替换配置文件 %s 失败: %w", path, err)
	}
	return nil
}

// SetCurrentContext 更新配置文件的 current-context 指针（读→改→写，原子）。
// name 不存在于 contexts 时返回错误（fail-closed，不写半状态）。
func SetCurrentContext(path, name string) error {
	cfg, err := Load(path)
	if err != nil {
		return err
	}
	if name != "" && cfg.FindContext(name) == nil {
		return fmt.Errorf("context %q 不存在", name)
	}
	cfg.CurrentContext = name
	return Save(cfg, path)
}
