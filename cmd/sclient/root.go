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
	"github.com/spf13/cobra"
)

var cfgFile string

// ConfigProvider 抽象配置加载，供命令工厂函数注入。
type ConfigProvider interface {
	LoadConfig() (*client.Config, error)
}

// cliConfigProvider 是生产实现的 ConfigProvider，基于 sclientcfg.ViperProvider。
type cliConfigProvider struct {
	provider *sclientcfg.ViperProvider
}

func (c *cliConfigProvider) LoadConfig() (*client.Config, error) {
	if c.provider == nil {
		return nil, fmt.Errorf("配置未初始化")
	}
	return client.LoadFromProvider(c.provider)
}

// NewRootCmd 创建完整的 sclient 根命令，包含所有 flags 和子命令。
func NewRootCmd() *cobra.Command {
	var (
		currentDir  string
		cfgProvider *sclientcfg.ViperProvider
		cliState    = &state.State{}
	)
	defaultCfgPath, err := xdg.ConfigFile(filepath.Join("sproxy", "sclient.yaml"))
	if err != nil {
		home, _ := os.UserHomeDir()
		defaultCfgPath = filepath.Join(home, ".sclient.yaml")
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
			cfgProvider.BindPFlag("auth_token", cmd.Flags().Lookup("auth-token"))
			currentDir = loadCurrentDir()
			cliState.CurrentDir = currentDir
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")
	root.PersistentFlags().StringP("server", "s", "", "服务器地址 (覆盖配置中的 server_url)")
	root.PersistentFlags().String("auth-token", "", "Bearer Token 认证令牌 (服务端配置了 auth_token 时需要)")
	root.PersistentFlags().StringP("output", "o", "", "指定下载文件的输出路径")
	root.PersistentFlags().BoolP("verbose", "v", false, "显示详细输出")
	root.PersistentFlags().Bool("chunked", false, "启用分块上传/下载模式")
	root.PersistentFlags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	root.PersistentFlags().Int("concurrency", 0, "上传/下载并发数 (默认 4)")
	root.PersistentFlags().Bool("resume", false, "续传模式 (默认启用)")
	root.PersistentFlags().Bool("json", false, "以 JSON 格式输出")
	root.PersistentFlags().Bool("insecure", false, "跳过 TLS 证书验证（用于自签证书开发/测试环境）")

	// 注册子命令
	ios := cli.SystemIOStreams()
	factory := clientfactory.New(cfgFile, func() clientfactory.CfgBinder { return cfgProvider })
	cfgSvc := &cliConfigProvider{provider: cfgProvider}
	root.AddCommand(NewCmdCd(cliState, ios))
	root.AddCommand(NewCmdPwd(cliState, ios))
	root.AddCommand(NewCmdMkdir(factory, ios, cliState))
	root.AddCommand(NewCmdRmdir(factory, ios, cliState))
	root.AddCommand(NewCmdGenkey(ios))
	root.AddCommand(NewCmdConfig(factory, ios, &cfgFile, cfgSvc))
	root.AddCommand(NewCmdVersion(factory, ios, cfgSvc))
	root.AddCommand(NewCmdStats(factory, ios))
	root.AddCommand(NewCmdDiag(ios))
	root.AddCommand(NewCmdUpload(factory, ios, cliState))
	root.AddCommand(NewCmdDownload(factory, ios, cliState))
	root.AddCommand(NewCmdDelete(factory, ios, cliState))
	root.AddCommand(NewCmdList(factory, ios, cliState))
	root.AddCommand(NewCmdSearch(factory, ios))
	root.AddCommand(NewCmdStat(factory, ios, cliState))
	root.AddCommand(NewCmdMv(factory, ios, cliState))
	root.AddCommand(NewCmdArchive(factory, ios))
	root.AddCommand(NewCmdArchiveDir(factory, ios))
	root.AddCommand(NewCmdBatchDelete(factory, ios, cliState))
	root.AddCommand(NewCmdBatchRename(factory, ios))
	root.AddCommand(NewCmdPreview(factory, ios, cliState, cfgSvc))
	root.AddCommand(NewCmdTunnel(factory, ios))
	root.AddCommand(NewCmdShare(factory, ios))
	root.AddCommand(NewCmdRelay(factory, ios, cfgSvc))
	root.AddCommand(NewCmdCloudDownload(factory, ios, cliState, cfgSvc))

	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}

// initLogger 初始化 sclient 的控制台日志。
func initLogger(verbose bool) *slog.Logger {
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(logger)
	return logger
}
