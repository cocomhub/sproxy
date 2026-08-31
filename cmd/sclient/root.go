// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/spf13/cobra"
)

var cfgFile string

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

	root := &cobra.Command{
		Use:   "sclient",
		Short: "文件上传下载客户端",
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			cfgProvider = sclientcfg.New(cfgFile)
			cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup("server"))
			cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup("chunk-size"))
			cfgProvider.BindPFlag("access_key", cmd.Flags().Lookup("access-key"))
			cfgProvider.BindPFlag("access_key_secret", cmd.Flags().Lookup("access-key-secret"))
			currentDir = loadCurrentDir()
			cliState.CurrentDir = currentDir

			verbose, _ := cmd.Flags().GetBool("verbose")
			initLogger(verbose)
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")
	root.PersistentFlags().StringP("server", "s", "", "服务器地址 (覆盖配置中的 server_url)")
	root.PersistentFlags().String("access-key", "", "SproxySig 认证 AccessKey (服务端配置了 access_keys 时需要)")
	root.PersistentFlags().String("access-key-secret", "", "SproxySig 认证 AccessKeySecret (本地密钥，仅计算签名，永不上线)")
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
	factory := clientfactory.New(cfgFile, func() clientfactory.CfgBinder { return cfgProvider })
	cfgSvc := &cliConfigProvider{getProvider: func() *sclientcfg.ViperProvider { return cfgProvider }}
	root.AddCommand(NewCmdCd(cliState, ios))
	root.AddCommand(NewCmdPwd(cliState, ios))
	root.AddCommand(NewCmdMkdir(factory, ios, cliState))
	root.AddCommand(NewCmdRmdir(factory, ios, cliState))
	root.AddCommand(NewCmdGenkey(ios))
	root.AddCommand(NewCmdAccessKey(ios))
	root.AddCommand(NewCmdIdentity(ios))
	root.AddCommand(NewCmdConfig(factory, ios, &cfgFile, cfgSvc))
	root.AddCommand(NewCmdVersion(ios))
	root.AddCommand(NewCmdStats(factory, ios))
	root.AddCommand(NewCmdDiag(ios))
	root.AddCommand(NewCmdUpload(factory, ios, cliState))
	root.AddCommand(NewCmdDownload(factory, ios, cliState))
	root.AddCommand(NewCmdDelete(factory, ios, cliState))
	root.AddCommand(NewCmdList(factory, ios, cliState))
	root.AddCommand(NewCmdSearch(factory, ios))
	root.AddCommand(NewCmdStat(factory, ios, cfgSvc))
	root.AddCommand(NewCmdMv(factory, ios, cliState))
	root.AddCommand(NewCmdArchive(factory, ios))
	root.AddCommand(NewCmdArchiveDir(factory, ios))
	root.AddCommand(NewCmdBatchDelete(factory, ios, cliState))
	root.AddCommand(NewCmdBatchRename(factory, ios))
	root.AddCommand(NewCmdPreview(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdTunnel(factory, ios))
	root.AddCommand(NewCmdShare(factory, ios))
	root.AddCommand(NewCmdRelay(factory, ios, cfgSvc))
	root.AddCommand(NewCmdP2P(ios, cfgSvc))
	root.AddCommand(NewCmdMesh(factory, ios, cfgSvc))
	root.AddCommand(newCmdSocks(factory, ios, cfgSvc))
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

// initLogger 初始化 sclient 的控制台日志。
// verbose 时同时把 pion 底层打洞日志（candidate/STUN/DTLS 明细）调到 TRACE，
// 供打洞失败排障使用；默认保持 Error，常驻无噪音。
func initLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
		webrtc.SetVerbose(true)
	}
	logger := slog.New(tracing.WithContextHandler(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
	slog.SetDefault(logger)
	return logger
}
