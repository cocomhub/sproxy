// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// RegisterHybridDownloader 把 hybrid 下载器注册进**指定**注册表。
//
// 全局能力可测试化（R18 策略，2026-10-05）：注册目标经参数注入——
//   - 生产路径：cmd/sproxy 传全局 `downloader.DefaultRegistry`（内部全局变量，保留）；
//   - 测试路径：传本地 `downloader.NewRegistry()` 实例（可导出全局方法），
//     不触碰全局 → 测试可并行（t.Parallel）。
//
// 返回错误而非内部 panic/log：注册失败由调用方决定告警等级，不静默。
func RegisterHybridDownloader(reg *downloader.Registry, cfg HybridConfig) error {
	if reg == nil {
		return fmt.Errorf("hybrid: registry required")
	}
	dl, err := NewHybridDownloader(cfg)
	if err != nil {
		return err
	}
	reg.Register(downloader.Plugin[downloader.Downloader]{
		Name:     "pikpak-hybrid",
		Instance: dl,
		Priority: 11, // 高于旧 pikpak（10）：分享 URL 优先 hybrid
	})
	return nil
}
