// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"github.com/cocomhub/sproxy/pkg/plugin"
)

// 类型别名，方便外部使用。
type Plugin[T any] = plugin.Plugin[T]

// Registry 是下载器插件的全局注册表。
// 内置兜底实现为 HTTPDownloader。
var Registry = plugin.New[Downloader]("downloader", &HTTPDownloader{})

// Find 查找第一个 Supports 该 source 的下载器。
// 按注册顺序查找，未找到返回 nil。
func Find(source string) Downloader {
	for _, name := range Registry.Names() {
		d, _ := Registry.Get(name)
		if d.Supports(source) {
			return d
		}
	}
	// 内置兜底
	builtin := Registry.Active()
	if builtin.Supports(source) {
		return builtin
	}
	return nil
}

// Supports 判断是否有已注册下载器支持该 source。
func Supports(source string) bool {
	return Find(source) != nil
}

// NewFromConfig 按配置名称创建下载器。
// 未找到时回退到 Active()（最高优先级已注册实现）。
func NewFromConfig(name string) Downloader {
	if name == "" {
		name = "http"
	}
	d, ok := Registry.Get(name)
	if !ok {
		return Registry.Active()
	}
	return d
}
