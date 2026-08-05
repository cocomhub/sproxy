// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"github.com/cocomhub/sproxy/pkg/plugin"
)

// 类型别名，方便外部使用。
type Plugin[T any] = plugin.Plugin[T]

// Registry 是下载器插件注册表。
// 使用前通过 NewRegistry() 创建，或使用 DefaultRegistry 全局实例。
type Registry struct {
	inner *plugin.Registry[Downloader]
}

// NewRegistry 创建下载器注册表。
// 内置兜底实现为 HTTPDownloader。
func NewRegistry() *Registry {
	return &Registry{inner: plugin.New[Downloader]("downloader", NewHTTPDownloader())}
}

// DefaultRegistry 是全局默认注册表实例。
var DefaultRegistry = NewRegistry()

// 向后兼容：全局函数操作 DefaultRegistry。

// Find 查找第一个 Supports 该 source 的下载器。
// 按注册顺序查找，未找到返回 nil。
// TODO: 支持通过选项/context 控制查找行为，例如按优先级或标签筛选。
func Find(source string) Downloader {
	return DefaultRegistry.Find(source)
}

// Supports 判断是否有已注册下载器支持该 source。
func Supports(source string) bool {
	return DefaultRegistry.Supports(source)
}

// NewFromConfig 按配置名称创建下载器。
// 未找到时回退到 Active()（最高优先级已注册实现）。
// TODO: 当 name == "" 时调用方意图不明确，考虑使用 DefaultDownloader()。
func NewFromConfig(name string) Downloader {
	return DefaultRegistry.NewFromConfig(name)
}

// DefaultDownloader 返回默认（内置 HTTP）下载器。
// 相比 NewFromConfig("") 语义更清晰。
func DefaultDownloader() Downloader {
	return DefaultRegistry.DefaultDownloader()
}

// --- Registry 方法 ---

// Register 注册一个下载器插件。
func (r *Registry) Register(p Plugin[Downloader]) {
	r.inner.Register(p)
}

// Get 按名称查找下载器。
func (r *Registry) Get(name string) (Downloader, bool) {
	return r.inner.Get(name)
}

// Active 返回最高优先级的已注册下载器。
func (r *Registry) Active() Downloader {
	return r.inner.Active()
}

// Names 返回所有已注册下载器的名称列表（按注册顺序）。
func (r *Registry) Names() []string {
	return r.inner.Names()
}

// Clear 移除所有已注册的下载器，恢复为仅内置兜底的状态。
// 仅用于测试；生产代码不应调用。
func (r *Registry) Clear() {
	r.inner.Clear()
}

// Find 查找第一个 Supports 该 source 的下载器。
func (r *Registry) Find(source string) Downloader {
	for _, name := range r.Names() {
		d, _ := r.Get(name)
		if d.Supports(source) {
			return d
		}
	}
	// 内置兜底
	builtin := r.Active()
	if builtin.Supports(source) {
		return builtin
	}
	return nil
}

// Supports 判断是否有已注册下载器支持该 source。
func (r *Registry) Supports(source string) bool {
	return r.Find(source) != nil
}

// NewFromConfig 按配置名称创建下载器。
func (r *Registry) NewFromConfig(name string) Downloader {
	if name == "" {
		name = "http"
	}
	d, ok := r.Get(name)
	if !ok {
		return r.Active()
	}
	return d
}

// DefaultDownloader 返回默认（内置 HTTP）下载器。
func (r *Registry) DefaultDownloader() Downloader {
	return r.Active()
}