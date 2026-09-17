// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package baidupcs 是百度网盘（BaiduPCS）存储后端插件。
//
// 独立 go.mod（见本目录 go.mod）：隔离 BaiduPCS-Go 裁剪库的依赖，避免污染 sproxy 主 module。
// 经 pkg/plugin.Registry[T] 注册为存储后端；装配方（cmd/sproxy）配置启用时构造 Storage。
//
// 执行策略：二进制优先 + 库兜底（cocom 验证模式）——默认调 BaiduPCS-Go 二进制（命令语义稳定、
// 子进程隔离、完整传输器内置、可独立升级），二进制缺失/失败时回退 fork 库实现。
package baidupcs

// StorageFactory 是 plugin 注册的 Storage 工厂接口（最小面）。
type StorageFactory interface {
	New(cfg StorageConfig) (*Storage, error)
}

// factory 是默认工厂实现。
type factory struct{}

// New 用 cfg 构造百度网盘 Storage。
func (factory) New(cfg StorageConfig) (*Storage, error) {
	return NewStorage(cfg)
}

// DefaultFactory 返回内置工厂（plugin 注册用）。
func DefaultFactory() StorageFactory { return factory{} }
