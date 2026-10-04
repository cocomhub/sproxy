// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
)

// buildAccountPool 从配置装配多账号会话池（**加密卷**凭据存储 + accounts[] 配额）。
// 返回 (pool, ok, fatalErr)：
//   - 加密存储装配成功 → 池恒创建（**即使零账号**，下载前 RefreshAccounts 对账，CLI
//     运行期 add/remove 无需重启生效——F5/接线 Critical），ok=true；
//   - 装配失败 + **config 声明了账号**（cfg.Pikpak.AccountConfigs 非空）→ fatalErr 非 nil
//     （fail-closed：已配置多账号却无加密卷 → 拒绝静默回落明文单会话，注册侧跳过下载器，
//     亮错误；F2/安全姿态）；
//   - 装配失败 + 无账号配置 → ok=false（回落当前 CLI 登录态，Warn，功能未配置）。
func buildAccountPool(cfg *server.Config, log *slog.Logger) (*pikpak.AccountPool, bool, error) {
	store, err := pikpakEncryptedSecretStore(context.Background(), cfg.StorageRoot, cfg.Pikpak.SecretsDir, log)
	if err != nil {
		if len(cfg.Pikpak.AccountConfigs) > 0 {
			return nil, false, fmt.Errorf("pikpak 已配置多账号但凭据加密卷不可用: %w", err)
		}
		slog.Warn("pikpak 凭据加密卷不可用（无账号配置，回落当前 CLI 登录态）", "err", err)
		return nil, false, nil
	}
	pool, err := pikpak.NewAccountPool(pikpak.AccountPoolConfig{
		Secrets: store,
		Logger:  log,
		// CredentialsDir 不覆盖：默认 ~/.pikpak——真 pikpak CLI 硬编码读 ~/.pikpak/
		// .credentials.json（设计附录 A.2 实测：HOME/USERPROFILE/config-dir 均无效、
		// Cli.run 以 os.Environ() 继承 HOME），Use 必须写该路径会话切换才生效。
		// 覆盖为其它目录会让真 CLI 读不到选中账号凭据（会话切换失效）。
	})
	if err != nil {
		return nil, false, fmt.Errorf("pikpak account pool: %w", err)
	}
	if err := pool.LoadAccounts(context.Background()); err != nil {
		return nil, false, fmt.Errorf("pikpak load accounts: %w", err)
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
	return pool, true, nil
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
		pool, ok, fatalErr := buildAccountPool(cfg, slog.Default())
		if fatalErr != nil {
			// F2/fail-closed：config 已配置多账号但凭据加密卷不可用——拒绝静默回落明文
			// 单会话，跳过注册（分享下载报「无下载器」亮错误，operator 须修复加密卷）。
			slog.Error("pikpak 多账号装配失败（已配置账号但加密卷不可用），跳过 pikpak 下载器注册", "err", fatalErr)
			return
		}
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
		if ok && pool != nil {
			slog.Info("pikpak multi-account pool wired", "accounts", len(pool.Accounts()))
		}
		downloader.DefaultRegistry.Register(downloader.Plugin[downloader.Downloader]{
			Name:     "pikpak",
			Instance: dl,
			Priority: 10, // 高于内置 HTTP（0）：分享 URL 优先走 PikPak 完整下载
		})
		slog.Info("pikpak downloader registered", "priority", 10)

		// hybrid 默认启用（Enabled 零值 = true；显式 false 关闭）：分享 URL 优先 hybrid。
		if !cfg.Pikpak.Hybrid.Disable {
			hybridDL, herr := pikpak.NewHybridDownloader(pikpak.HybridConfig{
				Resolver:    pikpak.NewShareResolver(pikpak.ShareResolverConfig{}),
				API:         api,
				ChunkSize:   cfg.Pikpak.Hybrid.ChunkSize,
				ShareRatio:  cfg.Pikpak.Hybrid.ShareRatio,
				Concurrency: cfg.Pikpak.Hybrid.Concurrency,
				AutoDelete:  cfg.Pikpak.Hybrid.AutoDelete,
				Logger:      slog.Default(),
				Metrics:     &pikpak.HybridMetrics{},
			})
			if herr != nil {
				slog.Warn("pikpak hybrid downloader not registered", "err", herr)
			} else {
				downloader.DefaultRegistry.Register(downloader.Plugin[downloader.Downloader]{
					Name:     "pikpak-hybrid",
					Instance: hybridDL,
					Priority: 11, // 高于旧 pikpak（10）：分享 URL 优先 hybrid
				})
				slog.Info("pikpak hybrid downloader registered", "priority", 11)
			}
		}
	})
}

// registerPikpakOnce 保证只注册一次。
var registerPikpakOnce sync.Once
