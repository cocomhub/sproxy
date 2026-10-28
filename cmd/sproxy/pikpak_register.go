// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/integrity"
	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume/ext/pikpak"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// buildAccountPool 从配置装配多账号会话池（**加密卷**凭据存储 + accounts[] 配额）。
// 返回 (pool, ok, fatalErr)：
//   - 加密存储装配成功 → 池恒创建（**即使零账号**，下载前 RefreshAccounts 对账，CLI
//     运行期 add/remove 无需重启生效——F5/接线 Critical），ok=true；
//   - 装配失败 + **config 声明了账号**（cfg.Pikpak.AccountConfigs 非空）→ fatalErr 非 nil
//     （fail-closed：已配置多账号却无加密卷 → 拒绝静默回落明文单会话，注册侧跳过下载器，
//     亮错误；F2/安全姿态）；
//   - 装配失败 + 无账号配置 → ok=false（回落当前 CLI 登录态，Warn，功能未配置）。
func buildAccountPool(cfg *server.Config, set *registry.Set, log *slog.Logger) (*pikpak.AccountPool, bool, error) {
	// 默认卷根（可用时）供主密钥经 `storage.Root` 落盘：at-rest 加密卷上密钥材料必须密文
	// 存储，且与默认 secrets 卷同口径（两个写入者形态不一致会让对方读到「密文当密钥」）。
	var volRoot *storage.Root
	if set != nil {
		volRoot = set.Root(set.Default().Name)
	}
	store, err := pikpakEncryptedSecretStore(context.Background(), volRoot, cfg.StorageRoot, cfg.Pikpak.SecretsDir, log)
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
// pikpakGCIDStats 是 GCID 校验命中统计（cloud 装配共享实例，决策用）。
var pikpakGCIDStats = &integrity.GCIDVerifyStats{}

func registerPikpakDownloader(cfg *server.Config, set *registry.Set) {
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
		pool, ok, fatalErr := buildAccountPool(cfg, set, slog.Default())
		if fatalErr != nil {
			// F2/fail-closed：config 已配置多账号但凭据加密卷不可用——拒绝静默回落明文
			// 单会话，跳过注册（分享下载报「无下载器」亮错误，operator 须修复加密卷）。
			slog.Error("pikpak 多账号装配失败（已配置账号但加密卷不可用），跳过 pikpak 下载器注册", "err", fatalErr)
			return
		}
		jfactory := progressJournalFactory(cfg.StorageRoot)
		dl, err := pikpak.NewPikpakDownloader(pikpak.DownloaderConfig{
			Cli:         cli2,
			API:         api,
			DownloadDir: cfg.Pikpak.DownloadDir,
			Timeout:     cfg.Pikpak.Timeout,
			// round-10 用户裁决：释放流程收归 RestoreLease（DeletePermanent）——旧下载器
			// 与 hybrid 对齐空间释放（免费账号 6GB；hybrid AutoDelete 默认 true）。该下载器
			// 兼作 hybrid 的 Fallback（匿名路径失败时委托），此时也须释放转存副本。
			AutoDelete:  cfg.Pikpak.AutoDelete || cfg.Pikpak.Hybrid.AutoDelete,
			AccountPool: pool,
			GCIDStats:   pikpakGCIDStats,
			// drive 模式（账号网盘文件）也落进度 WAL（云端转存下载的实际路径）。
			JournalFactory: jfactory,
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

		registerHybridIfEnabled(cfg, api, dl, pool)
		// URL 解析扩展（/api/resolve）：分享 URL → 文件清单（含 hash，供 GCID 校验）。
		// 漏注册会使 /api/resolve 对分享 URL 回落 generic（2026-10-26 实测缺陷）。
		registerPikpakResolver(downloader.DefaultRegistry, cfg.Pikpak.InsecureTLS)
	})
}

// registerHybridIfEnabled 按配置注册 hybrid 下载器（gocognit 收敛：从 registerPikpakDownloader 拆出）。
// 默认启用（Disable 零值 false=开；true 才显式禁用）：分享 URL 优先 hybrid。
// 注册走 RegisterHybridDownloader（可测试化的全局能力：注册表经参数注入，
// 生产传全局 DefaultRegistry，测试传本地 NewRegistry() 实例——R18 策略 2026-10-05）。
func registerHybridIfEnabled(cfg *server.Config, api *pikpak.API, dl *pikpak.PikpakDownloader, pool *pikpak.AccountPool) {
	if cfg.Pikpak.Hybrid.Disable {
		return
	}
	hcfg := pikpak.HybridConfig{
		Resolver:    pikpak.NewShareResolver(pikpak.ShareResolverConfig{}),
		API:         api,
		ChunkSize:   cfg.Pikpak.Hybrid.ChunkSize,
		ShareRatio:  cfg.Pikpak.Hybrid.ShareRatio,
		Concurrency: cfg.Pikpak.Hybrid.Concurrency,
		AutoDelete:  cfg.Pikpak.Hybrid.AutoDelete,
		Logger:      slog.Default(),
		Metrics:     &pikpak.HybridMetrics{},
		GCIDStats:   pikpakGCIDStats,
		// 多账号分片（round-12）：非 nil 时账号区 chunk 分摊到多账号并行（转存串行+下载并行）
		AccountPool: pool,
		// Fallback：匿名分享路径整体失败时降级到旧 PikpakDownloader 完整账号下载
		// （设计 §1.2——resolve 失败/无直链不阻断任务）。
		Fallback: dl,
	}
	if !cfg.Pikpak.Hybrid.DisableProgressJournal {
		hcfg.JournalFactory = progressJournalFactory(cfg.StorageRoot)
	}
	if herr := pikpak.RegisterHybridDownloader(downloader.DefaultRegistry, hcfg); herr != nil {
		slog.Warn("pikpak hybrid downloader not registered", "err", herr)
	} else {
		slog.Info("pikpak hybrid downloader registered", "priority", 11,
			"progress_journal", hcfg.JournalFactory != nil)
	}
}

// progressJournalFactory 返回**节点本地**进度 WAL 工厂（目录 `<storage_root>/meta/state/pikpak-journal`）。
//
// 为什么放在节点本地 meta/state：与卷状态缓存同口径——可重建的节点本地恢复层，
// 不属于用户数据、不随卷迁移；重启后按作用域重放已完成分片（manifest 丢失也能续传）。
func progressJournalFactory(storageRoot string) func(destPath, key string) (pikpak.ProgressJournal, error) {
	if storageRoot == "" {
		return nil
	}
	dir := filepath.Join(storageRoot, "meta", "state", "pikpak-journal")
	return func(destPath, key string) (pikpak.ProgressJournal, error) {
		// key 含目标路径：同一分享可下载到不同目标（各自独立进度）。
		return pikpak.NewFileProgressJournal(dir, key+"|"+destPath)
	}
}

// registerPikpakOnce 保证只注册一次。
var registerPikpakOnce sync.Once

// registerPikpakResolver 注册 PikPak 分享的 URL 解析扩展（通用 /api/resolve 用）。
//
// 设计：解析能力唯一实现在 pkg/volume/ext/pikpak（URLResolver：登录态列举优先、
// 匿名解析回落），装配层只做一次注册；未注册时 /api/resolve 对分享 URL 返回
// 通用回落结果（kind=generic，无文件清单）。
func registerPikpakResolver(reg *downloader.Registry, insecureTLS bool) {
	if reg == nil {
		return
	}
	cfg := pikpak.ShareResolverConfig{}
	var httpClient *http.Client
	if insecureTLS {
		// 本地自签证书（WSL 联调等）：注入跳过校验的 HTTPClient（生产必须 false）。
		httpClient = &http.Client{Transport: insecureTransport(), Timeout: 30 * time.Second}
		cfg.HTTPClient = httpClient
	}
	reg.RegisterResolver(downloader.Plugin[downloader.URLResolver]{
		Name:     pikpak.URLResolverName,
		Instance: pikpak.NewURLResolverWithClient(nil, pikpak.NewShareResolver(cfg), httpClient),
		Priority: 10,
	})
	slog.Info("pikpak url resolver registered", "priority", 10, "insecure_tls", insecureTLS)
}

// insecureTransport 返回跳过 TLS 校验的 Transport（仅本地联调；绝不用于生产）。
func insecureTransport() *http.Transport {
	// 基座 = DefaultTransport.Clone()（保留 ProxyFromEnvironment/连接池调校）——
	// 不裸构造 Transport（门禁 R19：丢默认调校）。
	t, _ := http.DefaultTransport.(*http.Transport)
	if t == nil {
		t = netutil.IsolatedTransport()
	}
	t = t.Clone()
	t.TLSClientConfig = &tls.Config{InsecureSkipVerify: true} //nolint:gosec // 本地联调专用
	return t
}
