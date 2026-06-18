// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package sproxycfg 提供 Viper 封装的配置提供者，供 cmd/sproxy 使用。
//
// cmd/sproxy 通过此包间接使用 Viper，不直接 import viper，
// 从而允许 pkg/server 的配置接口不依赖 Viper 类型。
package sproxycfg

import (
	"errors"
	"log/slog"
	"os"

	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// ViperProvider 包装 *viper.Viper，实现 provider.Provider 和 provider.Refresher。
type ViperProvider struct {
	v *viper.Viper
}

// New 创建并初始化 ViperProvider。
// cfgFile 是 YAML 配置文件路径；如果文件不存在则不报错。
func New(cfgFile string) *ViperProvider {
	v := viper.New()
	v.SetConfigFile(cfgFile)
	v.SetConfigType("yaml")
	v.SetEnvPrefix("SPROXY")
	v.AutomaticEnv()

	if err := v.ReadInConfig(); err != nil {
		var cfnfe viper.ConfigFileNotFoundError
		if !errors.As(err, &cfnfe) && !os.IsNotExist(err) {
			slog.Warn("读取配置文件出错", "path", cfgFile, "error", err)
		}
	}
	return &ViperProvider{v: v}
}

// Unmarshal 解码 Viper 中的配置到目标结构体。
func (p *ViperProvider) Unmarshal(obj any) error {
	return p.v.Unmarshal(obj)
}

// Refresh 重新读取配置文件。
func (p *ViperProvider) Refresh() error {
	return p.v.ReadInConfig()
}

// BindPFlag 绑定 cobra flag 到 Viper key。
func (p *ViperProvider) BindPFlag(key string, flag *pflag.Flag) {
	_ = p.v.BindPFlag(key, flag)
}

// Set 直接设置 Viper key 的值，主要用于测试。
func (p *ViperProvider) Set(key string, value any) {
	p.v.Set(key, value)
}

// compile-time interface checks
var _ provider.Provider = (*ViperProvider)(nil)
var _ provider.Refresher = (*ViperProvider)(nil)
