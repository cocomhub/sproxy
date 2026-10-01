// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"sync"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

// registerPikpakDownloader 把 PikPak 下载器注册进默认注册表，
// 使 cloud download 能按 URL（mypikpak.com/s/ 或 keepshare）自动匹配。
//
// 装配语义（对齐 baidupcs backend 插件注册）：重复注册 → registry panic（编程错误），
// 故用 sync.Once 保护（多装配/多测试并发调 runServer 时避免冲突）。
//
// 注意：下载器需已登录 CLI（binary_path 或 PATH 中的 pikpak 登录态）。
// 未登录时 Download 返回 ErrNotLoggedIn 明确错误（不静默降级）。
func registerPikpakDownloader(cfg *server.Config) {
	registerPikpakOnce.Do(func() {
		cli2, err := pikpak.NewCli(pikpak.CliConfig{
			BinaryPath:  cfg.Pikpak.BinaryPath,
			InstallDir:  cfg.Pikpak.InstallDir,
			AutoInstall: cfg.Pikpak.AutoInstall,
		})
		if err != nil {
			// 装配失败：不注册（cloud download 回落 HTTP，日志告警）。
			// 用户可运行 `sproxy pikpak install` 后重启。
			slog.Warn("pikpak downloader not registered", "err", err)
			return
		}
		api := pikpak.NewAPI(pikpak.APIConfig{}, cli2)
		dl, err := pikpak.NewPikpakDownloader(pikpak.DownloaderConfig{
			Cli:         cli2,
			API:         api,
			DownloadDir: cfg.Pikpak.DownloadDir,
			Timeout:     cfg.Pikpak.Timeout,
			AutoDelete:  cfg.Pikpak.AutoDelete,
		})
		if err != nil {
			slog.Warn("pikpak downloader not registered", "err", err)
			return
		}
		downloader.DefaultRegistry.Register(downloader.Plugin[downloader.Downloader]{
			Name:     "pikpak",
			Instance: dl,
			Priority: 10, // 高于内置 HTTP（0）：分享 URL 优先走 PikPak 完整下载
		})
		slog.Info("pikpak downloader registered", "priority", 10)
	})
}

// registerPikpakOnce 保证只注册一次。
var registerPikpakOnce sync.Once
