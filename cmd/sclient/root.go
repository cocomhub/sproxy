// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

var cfgFile string

// contextFlags 保存 --context/--env/--user 三个全局 flag 的解析结果（供
// factory/cfgSvc 后续消费；T3a 只负责解析注入，消费在 T3b）。
// PersistentPreRunE 中从 flag 读取（未显式指定时用 SCLIENT_CONTEXT/
// SCLIENT_ENV/SCLIENT_USER 环境变量作为默认值）。
var contextFlags struct {
	context string
	env     string
	user    string
}

// resolvedContext 是 PersistentPreRunE 解析出的有效上下文（env+user 都解析成功
// 才非 nil）。config.yaml 不存在或解析失败时为 nil（回落旧平铺路径）。
var resolvedContext *contextcfg.Resolved

// configYAMLPath 是新 context 配置文件路径（默认 XDG sproxy/config.yaml）。
// 与 --config 指向的旧 sclient.yaml 平铺路径分离：config.yaml 是 context 模型
// 的单一事实源，--config 保持旧语义（既有用户/测试零破坏）。
var configYAMLPath string

// ConfigProvider 抽象配置加载，供命令工厂函数注入。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// cliConfigProvider 是生产实现的 ConfigProvider，基于 sclientcfg.ViperProvider。
// 用 getProvider 闭包延迟解析 provider：PersistentPreRunE 才初始化 cfgProvider，
// 若像 factory 一样直接捕获指针值，构造时会拿到 nil（config show/set 一直报
// "配置未初始化"的既有 bug）。
type cliConfigProvider struct {
	getProvider func() *sclientcfg.ViperProvider
}

func (c *cliConfigProvider) LoadConfig() (*client.Config, error) {
	// T7：context 模型解析成功（config.yaml 存在且 current 可解析）→ 返回合成视图
	// （server_url/凭据/调优项来自 env+user；零值调优项回落 pkg/client 默认）。
	if resolvedContext != nil {
		return clientfactory.ResolvedToClientConfig(resolvedContext), nil
	}
	if c.getProvider == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	p := c.getProvider()
	if p == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	return client.LoadFromProvider(p)
}

// NewRootCmd 创建完整的 sclient 根命令，包含所有 flags 和子命令。
func NewRootCmd() *cobra.Command {
	var (
		currentDir  string
		cfgProvider *sclientcfg.ViperProvider
		cliState    = &state.State{}
	)
	// P2-配置2：多环境支持——SCLIENT_ENV 环境变量选择 env 后缀配置文件
	// （如 SCLIENT_ENV=prod → sclient.prod.yaml）。为空用默认 sclient.yaml。
	// 便于同一台机器维护 prod/staging/dev 多套 hub/server/token 配置。
	cfgBase := "sclient.yaml"
	if envName := os.Getenv("SCLIENT_ENV"); envName != "" {
		cfgBase = "sclient." + envName + ".yaml"
	}
	defaultCfgPath, err := xdg.ConfigFile(filepath.Join("sproxy", cfgBase))
	if err != nil {
		home, _ := os.UserHomeDir()
		defaultCfgPath = filepath.Join(home, "."+cfgBase)
	}
	if envName := os.Getenv("SCLIENT_ENV"); envName != "" {
		fmt.Fprintf(os.Stderr, "使用环境配置: %s（SCLIENT_ENV=%s）\n", defaultCfgPath, envName)
	}

	// 检查旧路径 ~/.sclient.yaml
	oldPath := filepath.Join(func() string {
		home, _ := os.UserHomeDir()
		return home
	}(), ".sclient.yaml")
	if _, statErr := os.Stat(oldPath); statErr == nil {
		if defaultCfgPath != oldPath {
			fmt.Fprintf(os.Stderr, "检测到旧配置 %s，将优先使用；建议迁移到 %s\n", oldPath, defaultCfgPath)
			defaultCfgPath = oldPath
		}
	}

	// context 模型配置文件（新单一事实源，与 --config 平铺路径分离）。
	configYAMLPath = resolveConfigYAMLPath()

	root := &cobra.Command{
		Use:   "sclient",
		Short: "文件上传下载客户端",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfgProvider = sclientcfg.New(cfgFile)
			cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup("server"))
			cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup("chunk-size"))
			cfgProvider.BindPFlag("access_key", cmd.Flags().Lookup("access-key"))
			cfgProvider.BindPFlag("access_key_secret", cmd.Flags().Lookup("access-key-secret"))
			cfgProvider.BindPFlag("access_key_id", cmd.Flags().Lookup("access-key-id"))
			cfgProvider.BindPFlag("volume", cmd.Flags().Lookup("volume"))
			currentDir = loadCurrentDir()
			cliState.CurrentDir = currentDir

			// context 解析注入（T3a）：--context/--env/--user flag 优先，环境变量
			// 为默认值；config.yaml 首次缺失且存在旧平铺 → 自动迁移。解析失败（无
			// config.yaml 且无旧文件）→ 空模型不报错，resolvedContext 置 nil（回落
			// 旧平铺路径，T3b 消费）。
			resolveAndMigrateContext(cmd, cmd.ErrOrStderr())

			verbose, _ := cmd.Flags().GetBool("verbose")
			initLogger(verbose)
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")
	// context 模型全局 flag：脚本化指定环境/用户/上下文（T3a 解析注入，T3b 消费）。
	root.PersistentFlags().StringVar(&contextFlags.context, "context", os.Getenv("SCLIENT_CONTEXT"), "上下文名（environments+users 组合，优先于 --env/--user 与 current-context）")
	root.PersistentFlags().StringVar(&contextFlags.env, "env", os.Getenv("SCLIENT_ENV"), "环境名（--context 未指定时覆盖 current-context 的环境）")
	root.PersistentFlags().StringVar(&contextFlags.user, "user", os.Getenv("SCLIENT_USER"), "用户名（--context 未指定时覆盖 current-context 的用户）")
	root.PersistentFlags().StringP("server", "s", "", "服务器地址 (覆盖配置中的 server_url)")
	root.PersistentFlags().String("access-key", "", "SproxySig 认证 AccessKey（服务端凭据 Ring 登记了对应 AK/SK 时需要）")
	root.PersistentFlags().String("access-key-secret", "", "SproxySig 认证 AccessKeySecret (本地密钥，仅计算签名，永不上线)")
	root.PersistentFlags().String("access-key-id", "", "SproxySig SK 条目 ID（skey-id，v2 协议必传；`trust renew` 回填）")
	root.PersistentFlags().String("volume", "", "存储卷上下文（默认空 = auto；upload/download/list/meta/delete/mv 等文件操作限定到指定卷）")
	root.PersistentFlags().StringP("output", "o", "", "指定下载文件的输出路径")
	root.PersistentFlags().BoolP("verbose", "v", false, "显示详细输出")
	root.PersistentFlags().Bool("chunked", false, "启用分块上传/下载模式")
	root.PersistentFlags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	root.PersistentFlags().Int("concurrency", 0, "上传/下载并发数 (默认 4)")
	root.PersistentFlags().Bool("resume", false, "续传模式 (默认启用)")
	root.PersistentFlags().Bool("json", false, "以 JSON 格式输出")
	// 审查 M-1：--insecure 双语义——HTTP 直连面（WithInsecureTLS）无 loopback 限制
	// （既有）；xfer tcp+tls 面（buildXferClientTLSConfig）**仅限 loopback hub**
	// （fail-closed，远程需 --ca-file）。文案区分两者避免误导。
	root.PersistentFlags().Bool("insecure", false, "跳过 TLS 证书验证：HTTP 直连面不限地址；xfer tcp+tls 面仅限 loopback hub（远程需 --ca-file 信任其证书）")
	root.PersistentFlags().String("ca-file", "", "xfer tcp+tls 传输的受信 CA 文件路径（PEM；服务端为自签证书时使用，与 --insecure 互斥）")
	root.PersistentFlags().String("client-cert", "", "mTLS 客户端证书路径（PEM 格式）")
	root.PersistentFlags().String("client-key", "", "mTLS 客户端私钥路径（PEM 格式）")
	root.PersistentFlags().Bool("client-cert-allow-missing", false, "当客户端证书加载失败时，不中断程序执行")
	root.PersistentFlags().Bool("allow-transport-fallback", false, "允许隧道/xfer 初始化失败时回退到直连模式（默认严格模式）")

	// 注册子命令
	ios := cli.SystemIOStreams()
	factory := clientfactory.NewWithContext(cfgFile, func() clientfactory.CfgBinder { return cfgProvider }, func() *contextcfg.Resolved { return resolvedContext })
	cfgSvc := &cliConfigProvider{getProvider: func() *sclientcfg.ViperProvider { return cfgProvider }}
	root.AddCommand(NewCmdCd(cliState, ios))
	root.AddCommand(NewCmdPwd(cliState, ios))
	root.AddCommand(NewCmdMkdir(factory, ios, cliState))
	root.AddCommand(NewCmdRmdir(factory, ios, cliState))
	root.AddCommand(NewCmdGenkey(ios))
	root.AddCommand(NewCmdTrust(factory, ios, cfgSvc, &cfgFile))
	root.AddCommand(NewCmdIdentity(ios))
	root.AddCommand(newCmdTrash(factory, ios))
	root.AddCommand(newCmdQuota(factory, ios))
	root.AddCommand(NewCmdConfig(factory, ios, &cfgFile, cfgSvc))
	root.AddCommand(newCmdContext(&cfgFile))
	root.AddCommand(newEnvCommand(&cfgFile))
	root.AddCommand(newUserCommand(&cfgFile))
	root.AddCommand(NewCmdVersion(ios))
	root.AddCommand(NewCmdUpgrade(ios))
	root.AddCommand(NewCmdStats(factory, ios))
	root.AddCommand(NewCmdDu(factory, ios, cliState))
	root.AddCommand(NewCmdDF(factory, ios))
	root.AddCommand(NewCmdDiag(ios))
	root.AddCommand(NewCmdUpload(factory, ios, cliState))
	root.AddCommand(NewCmdUploadDirect(factory, ios, cliState))
	root.AddCommand(NewCmdDownload(factory, ios, cliState))
	root.AddCommand(NewCmdDelete(factory, ios, cliState))
	root.AddCommand(NewCmdList(factory, ios, cliState))
	root.AddCommand(NewCmdVolumes(factory, ios))
	root.AddCommand(NewCmdVolume(factory, ios, cliState))
	root.AddCommand(NewCmdBackup(factory, ios))
	root.AddCommand(NewCmdMigrate(factory, ios))
	root.AddCommand(NewCmdSearch(factory, ios))
	root.AddCommand(NewCmdStat(factory, ios, cfgSvc))
	root.AddCommand(NewCmdMv(factory, ios, cliState))
	root.AddCommand(NewCmdArchive(factory, ios))
	root.AddCommand(NewCmdArchiveDir(factory, ios))
	root.AddCommand(NewCmdBatchDelete(factory, ios, cliState))
	root.AddCommand(NewCmdBatchRename(factory, ios))
	root.AddCommand(NewCmdPreview(factory, ios, cliState))
	root.AddCommand(NewCmdTunnel(factory, ios))
	root.AddCommand(NewCmdShare(factory, ios))
	root.AddCommand(NewCmdRelay(factory, ios, cfgSvc))
	root.AddCommand(NewCmdP2P(ios, cfgSvc))
	root.AddCommand(NewCmdMesh(factory, ios, cfgSvc))
	root.AddCommand(newCmdSocks(factory, ios, cfgSvc))
	root.AddCommand(newCmdHTTPProxy(factory, ios, cfgSvc))
	root.AddCommand(newCmdUDP(factory, ios, cfgSvc))
	root.AddCommand(NewCmdCloudDownload(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdCloudDownloadGroup(factory, ios, cfgSvc))
	root.AddCommand(NewCmdSync(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdMeta(factory, ios, cliState))

	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}

// resolveConfigYAMLPath 返回 context 模型配置文件路径（XDG sproxy/config.yaml）。
// 与 --config（旧平铺 sclient.yaml）分离：config.yaml 是 environments/users/
// contexts 三件套的单一事实源。
func resolveConfigYAMLPath() string {
	configHome := xdg.ConfigHome
	if configHome == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			configHome = dir
		}
	}
	if configHome != "" {
		return filepath.Join(configHome, "sproxy", "config.yaml")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".sproxy", "config.yaml")
}

// legacyConfigPath 返回旧平铺配置文件候选路径（按优先级）：
//  1. SCLIENT_ENV 对应的 sclient.<env>.yaml（XDG sproxy/）；
//  2. 默认 sclient.yaml（XDG sproxy/）；
//  3. 历史路径 ~/.sclient.yaml。
//
// 返回第一个存在的；都不存在返回空（无旧配置可迁移）。
// 同样基于 xdg.ConfigHome（对齐 resolveConfigYAMLPath 的测试语义）。
func legacyConfigPath() string {
	configHome := xdg.ConfigHome
	if configHome == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			configHome = dir
		}
	}
	if configHome != "" {
		base := filepath.Join(configHome, "sproxy")
		// M-1 修复：SCLIENT_ENV 对应的 sclient.<env>.yaml 优先于默认 sclient.yaml
		// （与旧 cfgBase 选择语义 / 设计 §5 SCLIENT_ENV 兼容一致）。
		if envName := os.Getenv("SCLIENT_ENV"); envName != "" {
			if p := filepath.Join(base, "sclient."+envName+".yaml"); fileExists(p) {
				return p
			}
		}
		if p := filepath.Join(base, "sclient.yaml"); fileExists(p) {
			return p
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		oldPath := filepath.Join(home, ".sclient.yaml")
		if fileExists(oldPath) {
			return oldPath
		}
	}
	return ""
}

// fileExists 报告路径存在（非目录）。
func fileExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir()
}

// resolveAndMigrateContext 在 PersistentPreRunE 中执行 context 模型解析：
//   - config.yaml 存在 → 读取并按 flag/环境变量解析（--context > --env/--user >
//     current-context）；成功则 resolvedContext 非 nil；
//   - config.yaml 不存在但存在旧平铺 sclient.yaml → MigrateLegacy 自动导入为
//     config.yaml（打印迁移提示，旧文件保留），并立即解析为新 context；
//   - 两者都不存在 → 空模型，resolvedContext 置 nil，不报错（首启零配置）。
//
// 解析失败（config.yaml 损坏/引用缺失）不阻断命令——回落旧平铺路径（T3b
// 消费 resolvedContext nil 时走 cfgProvider 平铺），仅记录解析状态。
func resolveAndMigrateContext(cmd *cobra.Command, errW io.Writer) {
	cfgPath := configYAMLPath
	// --context/--env/--user flag 值（flag 注册时已用环境变量作为默认值）。
	ctxName, _ := cmd.Flags().GetString("context")
	envName, _ := cmd.Flags().GetString("env")
	userName, _ := cmd.Flags().GetString("user")

	cfg, err := contextcfg.Load(cfgPath)
	if err != nil {
		// config.yaml 存在但损坏：记录警告，回落旧路径（不阻断）。
		fmt.Fprintf(errW, "context 配置文件解析失败（回落旧配置）: %v\n", err)
		resolvedContext = nil
		return
	}
	if len(cfg.Environments) == 0 && len(cfg.Contexts) == 0 {
		// config.yaml 不存在或为空 → 尝试迁移旧平铺。
		if legacy := legacyConfigPath(); legacy != "" {
			envForName := envName
			migrated, merr := contextcfg.MigrateLegacy(legacy, envForName)
			if merr != nil {
				fmt.Fprintf(errW, "旧配置迁移失败（继续使用旧平铺）: %v\n", merr)
				resolvedContext = nil
				return
			}
			if serr := contextcfg.Save(migrated, cfgPath); serr != nil {
				fmt.Fprintf(errW, "旧配置迁移保存失败（继续使用旧平铺）: %v\n", serr)
				resolvedContext = nil
				return
			}
			// C-1 修复：迁移后不覆盖 ctxName——无 flag 时 Resolve 自动 fallback
			// cfg.CurrentContext（迁移已设为 default/envName）；用户显式 flag 优先。
			fmt.Fprintf(errW, "已导入旧配置为 context %q（%s）；旧文件保留，可手动删除\n", migrated.CurrentContext, legacy)
			cfg = migrated
		}
	}
	if len(cfg.Environments) == 0 && len(cfg.Contexts) == 0 {
		// 仍无配置（无 config.yaml 且无旧文件）：空模型不报错，回落旧路径。
		resolvedContext = nil
		return
	}
	resolved, rerr := contextcfg.Resolve(cfg, contextcfg.ResolveArgs{
		Context:     ctxName,
		Environment: envName,
		User:        userName,
	})
	if rerr != nil {
		// 解析失败（如 current 未设置）：记录警告，回落旧路径（T3b 消费 nil）。
		fmt.Fprintf(errW, "context 解析失败（回落旧配置）: %v\n", rerr)
		resolvedContext = nil
		return
	}
	resolvedContext = resolved
}

// initLogger 初始化 sclient 的控制台日志。
// verbose 时同时把 pion 底层打洞日志（candidate/STUN/DTLS 明细）调到 TRACE，
// 供打洞失败排障使用；默认保持 Error，常驻无噪音。
func initLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
		webrtc.SetVerbose(true)
	}
	logger := slog.New(telemetry.WithContextHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	slog.SetDefault(logger)
	return logger
}
