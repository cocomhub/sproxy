// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/telemetry"
	"github.com/cocomhub/sproxy/pkg/tunnel"
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

// ConfigProvider 抽象配置加载，供命令工厂函数注入。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// SealfileConfigProvider 是 sealfile 命令族的配置回落读取接口（CLI flag 优先于它）。
type SealfileConfigProvider interface {
	// LoadSealfileConfig 把 sealfile 配置段（viper 键 sealfile.*）解码到 out。
	LoadSealfileConfig(out any) error
}

// errConfigNotFound 是「未找到配置文件」哨兵：统一 context 配置后，任何需要
// server_url/凭据的命令在无 config.yaml 时 fail-closed 报错并指引创建（不再静默
// 回落旧平铺或默认值——v1 双源分裂的根治）。定义在 clientfactory（工厂 fail-closed
// 判定处），cmd/sclient 复用同一哨兵。
var errConfigNotFound = clientfactory.ErrConfigNotFound

// cliConfigProvider 是生产实现的 ConfigProvider：单一事实源 = context 模型
// config.yaml（cfgFile）。resolvedContext 非 nil（PersistentPreRunE 解析成功）时
// LoadConfig 返回合成视图（env+user 扁平）；否则 fail-closed 报错（无配置文件/损坏
// /引用缺失都不静默用默认值）。
type cliConfigProvider struct {
	cfgFile string
}

func (c *cliConfigProvider) LoadConfig() (*client.Config, error) {
	// T7：context 模型解析成功（config.yaml 存在且 current 可解析）→ 返回合成视图
	// （server_url/凭据/调优项来自 env+user；零值调优项回落 pkg/client 默认）。
	if resolvedContext != nil {
		return clientfactory.ResolvedToClientConfig(resolvedContext), nil
	}
	return nil, errConfigNotFound
}

// LoadSealfileConfig 把 sealfile 配置段（config.yaml 顶层 `sealfile:` 段）解码到
// out（*sealfileCfg）。sealfile 命令族（本地加解密，不连服务端）经此读取配置回落值；
// CLI flag 优先于它。config.yaml 无 sealfile 段 → 返回空配置（回落默认，不报错）。
func (c *cliConfigProvider) LoadSealfileConfig(out any) error {
	sfc, ok := out.(*sealfileCfg)
	if !ok {
		return fmt.Errorf("LoadSealfileConfig 目标类型错误（应为 *sealfileCfg）")
	}
	if c.cfgFile == "" {
		return errConfigNotFound
	}
	cfg, err := contextcfg.Load(c.cfgFile)
	if err != nil {
		return err
	}
	if cfg.Sealfile != nil {
		*sfc = sealfileCfg(*cfg.Sealfile)
	}
	return nil
}

// NewRootCmd 创建完整的 sclient 根命令，包含所有 flags 和子命令。
func NewRootCmd() *cobra.Command {
	var (
		currentDir string
		cliState   = &state.State{}
	)
	// 统一 context 配置：唯一配置文件 = config.yaml（context 模型单一事实源）。
	// SCLIENT_ENV 仅作 --env 默认值（覆盖 current-context 的 environment），不再
	// 影响配置文件路径（v1 双源分裂——login 写 config.yaml、其他命令读
	// sclient.<env>.yaml——的根除）。
	defaultCfgPath := resolveDefaultConfigPath()
	if envName := os.Getenv("SCLIENT_ENV"); envName != "" {
		fmt.Fprintf(os.Stderr, "使用环境 %s（SCLIENT_ENV=%s）\n", defaultCfgPath, envName)
	}

	// 检查旧路径 ~/.sclient.yaml（历史遗留，仍提示；不再作为配置来源）。
	oldPath := filepath.Join(homeDir(), ".sclient.yaml")
	if _, statErr := os.Stat(oldPath); statErr == nil {
		fmt.Fprintf(os.Stderr, "检测到旧配置 %s（已不再使用）；请用 config.yaml 或 sclient context 管理配置\n", oldPath)
	}

	// context 模型配置文件路径 = 默认 config.yaml（--config 同义；统一单一事实源）。
	_ = resolveDefaultConfigPath()

	root := &cobra.Command{
		Use:   "sclient",
		Short: "文件上传下载客户端",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return rootPreRunE(cmd, &currentDir, cliState)
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
	root.PersistentFlags().String("download-proxy", "", "下载回退代理地址（直连服务端失败/超时自动经此代理重发；如 http://127.0.0.1:1080；空=关闭回退）")
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
	root.PersistentFlags().String("protocol-salt-key", "", "协议域分离盐自定义密钥（64 hex；派生替换默认 sproxy 盐防协议指纹识别。⚠️ 必须与握手对端使用相同 key，否则握手失败；stealth 部署配合加密内嵌的派生盐）")
	root.PersistentFlags().String("client-cert", "", "mTLS 客户端证书路径（PEM 格式）")
	root.PersistentFlags().String("client-key", "", "mTLS 客户端私钥路径（PEM 格式）")
	root.PersistentFlags().Bool("client-cert-allow-missing", false, "当客户端证书加载失败时，不中断程序执行")
	root.PersistentFlags().Bool("allow-transport-fallback", false, "允许隧道/xfer 初始化失败时回退到直连模式（默认严格模式）")

	// 注册子命令
	ios := cli.SystemIOStreams()
	factory := clientfactory.NewWithContext(cfgFile, nil, func() *contextcfg.Resolved { return resolvedContext })
	cfgSvc := &cliConfigProvider{cfgFile: cfgFile}

	root.AddCommand(NewCmdCd(cliState, ios))
	root.AddCommand(NewCmdPwd(cliState, ios))
	root.AddCommand(NewCmdMkdir(factory, ios, cliState))
	root.AddCommand(NewCmdRmdir(factory, ios, cliState))
	root.AddCommand(NewCmdGenkey(ios))
	root.AddCommand(NewCmdChecksum(factory, ios))
	root.AddCommand(newClusterCredentialCmd(ios))
	root.AddCommand(NewCmdTrust(factory, ios, cfgSvc, &cfgFile))
	root.AddCommand(NewCmdSecret(factory, ios, cfgSvc))
	root.AddCommand(NewCmdSealfile(ios, cfgSvc))
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
	root.AddCommand(NewCmdLs(factory, ios, cliState))
	root.AddCommand(NewCmdTree(factory, ios, cliState))
	root.AddCommand(NewCmdVolumes(factory, ios))
	root.AddCommand(NewCmdVolume(factory, ios, cliState))
	root.AddCommand(NewCmdBackup(factory, ios))
	root.AddCommand(NewCmdMigrate(factory, ios))
	root.AddCommand(NewCmdSearch(factory, ios))
	root.AddCommand(NewCmdStat(factory, ios, cfgSvc))
	root.AddCommand(NewCmdMv(factory, ios, cliState))
	root.AddCommand(NewCmdCp(factory, ios, cliState))
	root.AddCommand(NewCmdMeta(factory, ios, cliState))
	root.AddCommand(NewCmdArchive(factory, ios))
	root.AddCommand(NewCmdArchiveDir(factory, ios))
	root.AddCommand(NewCmdBatch(factory, ios, cliState))
	root.AddCommand(NewCmdBatchDelete(factory, ios, cliState))
	root.AddCommand(NewCmdBatchRename(factory, ios))
	root.AddCommand(NewCmdPreview(factory, ios, cliState))
	root.AddCommand(NewCmdTunnel(factory, ios))
	root.AddCommand(NewCmdShare(factory, ios, cliState))
	root.AddCommand(NewCmdRelay(factory, ios, cfgSvc))
	root.AddCommand(NewCmdP2P(factory, ios, cfgSvc))
	root.AddCommand(NewCmdMesh(factory, ios, cfgSvc))
	root.AddCommand(newCmdSocks(factory, ios, cfgSvc))
	root.AddCommand(newCmdHTTPProxy(factory, ios, cfgSvc))
	root.AddCommand(newCmdUDP(factory, ios, cfgSvc))
	root.AddCommand(NewCmdCloudDownload(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdCloudDownloadGroup(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdSync(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdMeta(factory, ios, cliState))

	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}

// rootPreRunE 是 NewRootCmd 的 PersistentPreRunE 主体（降 CC）：加载当前目录，
// 执行 context 解析注入（统一 config.yaml 单一事实源）与协议盐设置。不再装配
// viper 平铺 provider（统一 context 后无平铺回落）。
func rootPreRunE(cmd *cobra.Command, currentDir *string, cliState *state.State) error {
	*currentDir = loadCurrentDir()
	cliState.CurrentDir = *currentDir
	cliState.CurrentVolume = loadCurrentVolume()

	// context 解析注入：--context/--env/--user flag 优先，环境变量为默认值；
	// config.yaml 首次缺失且存在旧平铺 → 一次性自动迁移。失败（损坏/引用缺失）
	// 记录解析状态（resolvedContext 置 nil），命令级门禁据此 fail-closed。
	resolveAndMigrateContext(cmd, cmd.ErrOrStderr())

	// 协议盐自定义（防协议指纹识别）：CLI flag 优先，回落配置（Resolved.Env）。
	// ⚠️ 必须与握手对端（stealth 二进制/远端节点）使用相同 key，否则握手失败。
	saltKey, _ := cmd.Flags().GetString("protocol-salt-key")
	if saltKey == "" && resolvedContext != nil && resolvedContext.Environment != nil {
		saltKey = resolvedContext.Environment.ProtocolSaltKey
	}
	if saltKey != "" {
		if sk, derr := hex.DecodeString(saltKey); derr == nil && len(sk) == 32 {
			tunnel.SetProtocolSalts(tunnel.DeriveProtocolSalts(sk))
		} else {
			return fmt.Errorf("--protocol-salt-key 应为 64 hex（32B 密钥）")
		}
	}

	verbose, _ := cmd.Flags().GetBool("verbose")
	initLogger(verbose)
	return nil
}

// resolveDefaultConfigPath 返回统一后的默认配置文件路径（XDG sproxy/config.yaml）。
// 与 --config 指向同一文件（context 模型单一事实源）。
func resolveDefaultConfigPath() string {
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

// legacyConfigPath 返回旧平铺配置文件候选路径（**仅用于一次性迁移**）：
//  1. 默认 sclient.yaml（XDG sproxy/）；
//  2. 历史路径 ~/.sclient.yaml。
//
// 返回第一个存在的；都不存在返回空（无旧配置可迁移）。
// SCLIENT_ENV 对应的 sclient.<env>.yaml 不再参与（统一 context 后仅 env 名覆盖）。
func legacyConfigPath() string {
	configHome := xdg.ConfigHome
	if configHome == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			configHome = dir
		}
	}
	if configHome != "" {
		base := filepath.Join(configHome, "sproxy")
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

// homeDir 返回用户主目录（失败时为空串）。
func homeDir() string {
	home, _ := os.UserHomeDir()
	return home
}

// resolveAndMigrateContext 在 PersistentPreRunE 中执行 context 模型解析（统一后
// 单一配置源 = cfgFile 指向的 config.yaml）：
//   - config.yaml 存在 → 读取并按 flag/环境变量解析（--context > --env/--user >
//     current-context）；成功则 resolvedContext 非 nil；
//   - config.yaml 不存在但存在旧平铺 sclient.yaml → MigrateLegacy 一次性自动导入为
//     config.yaml（打印迁移提示，旧文件保留），并立即解析为新 context；
//   - 两者都不存在 → resolvedContext 置 nil（命令级门禁据此 fail-closed 报错指引创建）。
//
// 解析失败（config.yaml 损坏/引用缺失）不阻断命令，但同样置 nil——需要配置的
// 命令会经 ConfigProvider/factory 得到明确错误（不再静默回落默认值）。
func resolveAndMigrateContext(cmd *cobra.Command, errW io.Writer) {
	cfgPath := cfgFile
	// --context/--env/--user flag 值（flag 注册时已用环境变量作为默认值）。
	ctxName, _ := cmd.Flags().GetString("context")
	envName, _ := cmd.Flags().GetString("env")
	userName, _ := cmd.Flags().GetString("user")

	cfg, err := contextcfg.Load(cfgPath)
	if err != nil {
		// config.yaml 存在但损坏：记录警告，不阻断（门禁在命令级）。
		fmt.Fprintf(errW, "context 配置文件解析失败: %v\n", err)
		resolvedContext = nil
		return
	}
	if len(cfg.Environments) == 0 && len(cfg.Contexts) == 0 {
		// config.yaml 不存在或为空 → 尝试一次性迁移旧平铺。
		if legacy := legacyConfigPath(); legacy != "" {
			envForName := envName
			migrated, merr := contextcfg.MigrateLegacy(legacy, envForName)
			if merr != nil {
				fmt.Fprintf(errW, "旧配置迁移失败: %v\n", merr)
				resolvedContext = nil
				return
			}
			if serr := contextcfg.Save(migrated, cfgPath); serr != nil {
				fmt.Fprintf(errW, "旧配置迁移保存失败: %v\n", serr)
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
		// 仍无配置（无 config.yaml 且无旧文件）：空模型，resolvedContext 置 nil——
		// 需要配置的命令（factory/ConfigProvider）fail-closed 报错指引创建。
		resolvedContext = nil
		return
	}
	resolved, rerr := contextcfg.Resolve(cfg, contextcfg.ResolveArgs{
		Context:     ctxName,
		Environment: envName,
		User:        userName,
	})
	if rerr != nil {
		// 解析失败（如 current 未设置）：记录警告，resolvedContext 置 nil（fail-closed）。
		fmt.Fprintf(errW, "context 解析失败: %v\n", rerr)
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
