// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package store 提供通用记录存储：字节级 KV 接口 + 插件注册表。
// 默认实现为 store/file（JSON 文件 + 原子写）；未来可扩展 MySQL/Redis 等后端。
package store

import (
	"fmt"
	"sort"
	"sync"
)

// Store 是字节级键值存储接口，插件友好（MySQL/Redis 等只需实现字节 KV）。
// key 使用 / 分隔的协议路径；Set 必须原子写（tmp + rename + 锁）。
type Store interface {
	// Get 读取 key 对应的值；key 不存在时返回 os.ErrNotExist。
	Get(key string) ([]byte, error)
	// Set 原子写入 key 的值。
	Set(key string, value []byte) error
	// Delete 删除 key（幂等：key 不存在时返回 nil）。
	Delete(key string) error
	// List 返回指定前缀下的全部值（不含 key）；空前缀 = 遍历全部。
	List(prefix string) ([][]byte, error)
	// Close 释放底层资源（file 实现无资源需释放）。
	Close() error
}

// StoreConfig 是 Store 后端的通用配置。
type StoreConfig struct {
	Root string // 存储根目录（file 实现使用）
}

// Factory 创建 Store 的工厂函数，供插件注册表使用。
type Factory func(cfg StoreConfig) (Store, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register 注册一个 Store 后端工厂。同名后注册覆盖先注册。
func Register(name string, f Factory) {
	if name == "" {
		panic("store: Register 不允许空名称")
	}
	if f == nil {
		panic("store: Register 不允许 nil 工厂")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[name] = f
}

// Open 按名称打开 Store 后端；未知名称返回错误。
func Open(name string, cfg StoreConfig) (Store, error) {
	registryMu.RLock()
	f, ok := registry[name]
	registryMu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("store: 未知存储后端 %q（已注册: %v）", name, knownNames())
	}
	return f(cfg)
}

// knownNames 返回已注册后端名称（排序，便于报错）。
func knownNames() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
