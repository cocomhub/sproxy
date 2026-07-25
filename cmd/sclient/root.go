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

const (
	flagServer    = "server"
	flagChunkSize = "chunk-size"
)

var (
	cfgFile     string
	currentDir  string
	cfgProvider *sclientcfg.ViperProvider
	cliState    = &state.State{}
)

// NewRootCmd 创建完整的 sclient 根命令，包含所有 flags 和子命令。
func NewRootCmd() *cobra.Command {
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
			cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup(flagServer))
			cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup(flagChunkSize))
			cfgProvider.BindPFlag("auth_token", cmd.Flags().Lookup("auth-token"))
			loadCurrentDir()
			cliState.CurrentDir = currentDir
			return nil
		},
		Run: func(cmd *cobra.Command, args []string) {
			_ = cmd.Help()
		},
	}

	root.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")
	root.PersistentFlags().StringP(flagServer, "s", "", "服务器地址 (覆盖配置中的 server_url)")
	root.PersistentFlags().String("auth-token", "", "Bearer Token 认证令牌 (服务端配置了 auth_token 时需要)")
	root.PersistentFlags().StringP("output", "o", "", "指定下载文件的输出路径")
	root.PersistentFlags().BoolP("verbose", "v", false, "显示详细输出")
	root.PersistentFlags().Bool("chunked", false, "启用分块上传/下载模式")
	root.PersistentFlags().Int64(flagChunkSize, 0, "分块大小 (默认 4MB)")
	root.PersistentFlags().Int("concurrency", 0, "上传/下载并发数 (默认 4)")
	root.PersistentFlags().Bool("resume", false, "续传模式 (默认启用)")
	root.PersistentFlags().Bool("json", false, "以 JSON 格式输出")

	// 注册子命令
	ios := cli.SystemIOStreams()
	factory := clientfactory.New(cfgFile, func() clientfactory.CfgBinder { return cfgProvider })
	root.AddCommand(NewCmdCd(cliState, ios))
	root.AddCommand(NewCmdPwd(cliState, ios))
	root.AddCommand(NewCmdMkdir(factory, ios, cliState))
	root.AddCommand(NewCmdRmdir(factory, ios, cliState))
	root.AddCommand(NewCmdGenkey(ios))
	root.AddCommand(NewCmdConfig(factory, ios, &cfgFile))
	root.AddCommand(NewCmdVersion(factory, ios))
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
	root.AddCommand(NewCmdPreview(factory, ios, cliState))
	root.AddCommand(NewCmdTunnel(factory, ios))
	root.AddCommand(NewCmdShare(factory, ios))
	root.AddCommand(NewCmdRelay(factory, ios))
	root.AddCommand(NewCmdCloudDownload(factory, ios, cliState))

	return root
}

func Execute() error {
	return NewRootCmd().Execute()
}

// buildFileClient 根据 cfgProvider 配置和 persistent flag 构造 FileClient。
func buildFileClient(cmd *cobra.Command) (*client.FileClient, error) {
	cfg, err := client.LoadFromProvider(cfgProvider)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	serverURL := cfg.ServerURL
	if s, _ := cmd.Flags().GetString(flagServer); s != "" {
		serverURL = s
	}

	verbose, _ := cmd.Flags().GetBool("verbose")
	logger := initLogger(verbose)

	opts := []client.Option{
		client.WithLogger(logger),
		client.WithProgress(func(label string, read, total int64) {
			if total > 0 {
				percent := float64(read) / float64(total) * 100
				fmt.Fprintf(os.Stderr, "\r%s: %.1f%% (%s/%s)  ", label, percent,
					client.FormatByte(float64(read)), client.FormatByte(float64(total)))
			} else {
				fmt.Fprintf(os.Stderr, "\r%s: %s  ", label, client.FormatByte(float64(read)))
			}
			if read == total {
				fmt.Fprintf(os.Stderr, "\n")
			}
		}),
	}
	if cfg.TunnelKey != "" {
		if s, _ := cmd.Flags().GetString(flagServer); s == "" {
			opts = append(opts, client.WithTunnel(cfg.TunnelKey))
		}
	}
	if cs, _ := cmd.Flags().GetInt64(flagChunkSize); cs > 0 {
		opts = append(opts, func(c *client.FileClient) {
			c.ChunkSize = cs
		})
	} else if cfg.ChunkSize > 0 {
		opts = append(opts, func(c *client.FileClient) {
			c.ChunkSize = cfg.ChunkSize
		})
	}
	if ms := cfg.MaxChunkSize; ms > 0 {
		opts = append(opts, client.WithMaxChunkSize(ms))
	}
	if cfg.AuthToken != "" {
		opts = append(opts, client.WithAuthToken(cfg.AuthToken))
	}
	if t, _ := cmd.Flags().GetString("auth-token"); t != "" {
		opts = append(opts, client.WithAuthToken(t))
	}

	return client.NewFileClient(serverURL, opts...), nil
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
