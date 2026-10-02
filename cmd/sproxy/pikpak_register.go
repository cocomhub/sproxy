// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

// buildAccountPool 从配置装配多账号会话池（secrets 目录 + accounts[] 配额）。
// 返回 (pool, ok)：配置无账号/装配失败时 ok=false（下载器退回当前 CLI 登录态）。
func buildAccountPool(cfg *server.Config, log *slog.Logger) (*pikpak.AccountPool, bool) {
	dir := cfg.Pikpak.SecretsDir
	if dir == "" {
		var homeErr error
		dir, homeErr = pikpakSecretsDir("") // 与 CLI account 子命令一致：sproxy 配置目录
		if homeErr != nil {
			slog.Warn("pikpak secrets dir unavailable", "err", homeErr)
			return nil, false // 取不到 home：不回退公开临时目录，禁用多账号池
		}
	}
	store, err := pikpak.NewDirSecretStore(dir)
	if err != nil {
		slog.Warn("pikpak secrets store unavailable", "err", err, "dir", dir)
		return nil, false
	}
	pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{
		Secrets:        store,
		CredentialsDir: filepath.Join(dir, "..", "pikpak-credentials"),
		Logger:         log,
	})
	if err != nil {
		slog.Warn("pikpak account pool unavailable", "err", err)
		return nil, false
	}
	if err := pool.LoadAccounts(context.Background()); err != nil {
		slog.Warn("pikpak load accounts", "err", err)
		return nil, false // 无法重建账号列表 → 退回单一登录态（不注册池）
	}
	if len(pool.Accounts()) == 0 {
		return nil, false // 无账号 → 无需池
	}
	// 配置里每账号显式配额覆盖默认（LoadAccounts 已恢复持久化配额）。
	ctx := context.Background()
	for _, acct := range cfg.Pikpak.AccountConfigs {
		if acct.DailyQuota > 0 {
			if err := pool.SetDailyQuota(ctx, acct.Name, acct.DailyQuota); err != nil {
				slog.Warn("pikpak account quota", "name", acct.Name, "err", err)
			}
		}
	}
	return pool, true
}

// registerPikpakDownloader 把 PikPak 下载器注册进默认注册表，
// 使 cloud download 能按 URL（mypikpak.com/s/ 或 keepshare）自动匹配。
//
// 装配语义：同名重复注册以最后一次为准（plugin.Registry 覆盖语义），
// 故用 sync.Once 保护避免重复装配（多装配/多测试并发调 runServer 时只注册一次）。
//
// 注意：下载器需已登录 CLI（binary_path 或 PATH 中的 pikpak 登录态）。
// 未登录时 API 调用返回 ErrNotLoggedIn 明确错误（不静默降级、不假绿）。
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
		pool, _ := buildAccountPool(cfg, slog.Default())
		dl, err := pikpak.NewPikpakDownloader(pikpak.DownloaderConfig{
			Cli:         cli2,
			API:         api,
			DownloadDir: cfg.Pikpak.DownloadDir,
			Timeout:     cfg.Pikpak.Timeout,
			AutoDelete:  cfg.Pikpak.AutoDelete,
			AccountPool: pool,
		})
		if err != nil {
			slog.Warn("pikpak downloader not registered", "err", err)
			return
		}
		if pool != nil {
			slog.Info("pikpak multi-account pool wired", "accounts", len(pool.Accounts()))
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
