// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package plugin 提供通用泛型注册框架 Registry[T]。
// 各子系统（xfer、hub、tracing）基于 Registry[T] 定义自己的插件注册表。
package plugin

import (
	"fmt"
	"sync"
)

// Plugin 描述一个已注册的插件。
type Plugin[T any] struct {
	Name     string
	Instance T
	Priority int // 优先级，高者优先。内置默认为 0，外部插件应 > 0
}

// Registry 是类型安全的插件注册表。
// T 是插件接口类型，由各子系统定义。
// 零值不可用，必须通过 New 创建。
type Registry[T any] struct {
	name    string
	mu      sync.RWMutex
	builtin T
	plugins map[string]Plugin[T]
}

// New 创建一个新的注册表。
// name 用于日志/调试标识；builtin 是内置兜底实现。
func New[T any](name string, builtin T) *Registry[T] {
	return &Registry[T]{
		name:    name,
		builtin: builtin,
		plugins: make(map[string]Plugin[T]),
	}
}

// Register 注册一个插件。
// 同名插件以最后一次注册为准（后注册覆盖前注册）。
func (r *Registry[T]) Register(p Plugin[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Name == "" {
		panic(fmt.Sprintf("plugin[%s]: Register called with empty name", r.name))
	}
	r.plugins[p.Name] = p
}

// Active 返回最高优先级的已注册实现。
// 如果没有已注册插件，返回内置兜底。
func (r *Registry[T]) Active() T {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *Plugin[T]
	for _, p := range r.plugins {
		if best == nil || p.Priority > best.Priority {
			best = &p
		}
	}
	if best != nil {
		return best.Instance
	}
	return r.builtin
}

// Get 按名称查找插件。返回其实例和 true；未找到时返回零值和 false。
func (r *Registry[T]) Get(name string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	if !ok {
		var zero T
		return zero, false
	}
	return p.Instance, true
}

// Names 返回所有已注册插件的名称列表。
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	return names
}

// IsDefault 返回当前是否使用内置兜底实现（即无外部插件注册）。
func (r *Registry[T]) IsDefault() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins) == 0
}

// Clear 移除所有已注册的插件，恢复为仅内置兜底的状态。
// 仅用于测试；生产代码不应调用。
func (r *Registry[T]) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = make(map[string]Plugin[T])
}
