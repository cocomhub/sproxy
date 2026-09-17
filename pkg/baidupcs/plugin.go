// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// StorageFactory 是 plugin 注册的 Storage 工厂接口（最小面）。
type StorageFactory interface {
	New(cfg StorageConfig) (*Storage, error)
}

// factory 是默认工厂实现。
type factory struct{}

// New 用 cfg 构造百度网盘 Storage（Adapter 为空时默认双路径装配）。
func (factory) New(cfg StorageConfig) (*Storage, error) {
	return NewStorage(cfg)
}

// DefaultFactory 返回内置工厂（plugin 注册用）。
func DefaultFactory() StorageFactory { return factory{} }
