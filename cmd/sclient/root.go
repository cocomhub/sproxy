// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

var (
	cfgFile     string
	currentDir  string
	cfgProvider *sclientcfg.ViperProvider
)

// rootCmd 是所有子命令的根命令
var rootCmd = &cobra.Command{
	Use:   "sclient",
	Short: "文件上传下载客户端",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		cfgProvider = sclientcfg.New(cfgFile)
		cfgProvider.BindPFlag("server_url", cmd.Flags().Lookup("server"))
		cfgProvider.BindPFlag("chunk_size", cmd.Flags().Lookup("chunk-size"))
		// 加载缓存的当前目录
		loadCurrentDir()
		return nil
	},
	Run: func(cmd *cobra.Command, args []string) {
		_ = cmd.Help()
	},
}

func Execute() error {
	return rootCmd.Execute()
}

func init() {
	// XDG 默认配置路径
	defaultCfgPath, err := xdg.ConfigFile(filepath.Join("sproxy", "sclient.yaml"))
	if err != nil {
		// fallback 到 ~/.sclient.yaml
		home, _ := os.UserHomeDir()
		defaultCfgPath = filepath.Join(home, ".sclient.yaml")
	}

	// 检查旧路径 ~/.sclient.yaml
	oldPath := filepath.Join(func() string {
		home, _ := os.UserHomeDir()
		return home
	}(), ".sclient.yaml")
	if _, statErr := os.Stat(oldPath); statErr == nil {
		// 旧文件存在，优先使用旧文件并打印迁移提示
		if defaultCfgPath != oldPath {
			fmt.Fprintf(os.Stderr, "检测到旧配置 %s，将优先使用；建议迁移到 %s\n", oldPath, defaultCfgPath)
			defaultCfgPath = oldPath
		}
	}

	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", defaultCfgPath, "配置文件路径")

	// 全局选项（persistent flags）
	rootCmd.PersistentFlags().StringP("server", "s", "", "服务器地址 (覆盖配置中的 server_url)")
	rootCmd.PersistentFlags().StringP("output", "o", "", "指定下载文件的输出路径")
	rootCmd.PersistentFlags().BoolP("verbose", "v", false, "显示详细输出")
	rootCmd.PersistentFlags().Bool("chunked", false, "启用分块上传/下载模式")
	rootCmd.PersistentFlags().Int64("chunk-size", 0, "分块大小 (默认 4MB)")
	rootCmd.PersistentFlags().Int("concurrency", 0, "上传/下载并发数 (默认 4)")
	rootCmd.PersistentFlags().Bool("resume", false, "续传模式 (默认启用)")

	// 注册子命令
	rootCmd.AddCommand(uploadCmd)
	rootCmd.AddCommand(downloadCmd)
	rootCmd.AddCommand(deleteCmd)
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)
	rootCmd.AddCommand(tunnelCmd)
	rootCmd.AddCommand(genkeyCmd)
	rootCmd.AddCommand(configCmd)
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(relayCmd)
}

// buildFileClient 根据 cfgProvider 配置和 persistent flag 构造 FileClient。
// 从配置中加载 server_url、tunnel_key、chunk_size 等，persistent flag 可覆盖。
func buildFileClient(cmd *cobra.Command) (*client.FileClient, error) {
	cfg, err := client.LoadFromProvider(cfgProvider)
	if err != nil {
		return nil, fmt.Errorf("加载配置失败: %w", err)
	}

	serverURL := cfg.ServerURL
	if s, _ := cmd.Flags().GetString("server"); s != "" {
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
		// 当 --server flag 显式指定时，绕过隧道直接 HTTP
		if s, _ := cmd.Flags().GetString("server"); s == "" {
			opts = append(opts, client.WithTunnel(cfg.TunnelKey))
		}
	}
	if cs, _ := cmd.Flags().GetInt64("chunk-size"); cs > 0 {
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
