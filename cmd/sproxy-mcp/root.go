// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/cocomhub/buildinfo"
	"github.com/cocomhub/sproxy/internal/buildmeta"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/mcp"
	"github.com/spf13/cobra"
)

// Version / BuildAt 由构建期 -ldflags -X 注入（Makefile GO_LD_FLAGS_X），
// 与 cmd/sproxy、cmd/sclient 同构，**不要手工改这些常量**。
var (
	Version = "dev"
	BuildAt = "unknown"
)

const (
	flagServer          = "server"
	flagAccessKey       = "access-key"
	flagAccessKeySecret = "access-key-secret"
	flagAccessKeyID     = "access-key-id"
	flagVolume          = "volume"
)

var rootCmd = &cobra.Command{
	Use:   "sproxy-mcp",
	Short: "MCP (Model Context Protocol) server——把 sproxy 文件能力暴露为 MCP 工具（stdio）",
	Long: "sproxy-mcp 通过 stdio 传输提供 MCP server（JSON-RPC 2.0）：AI CLI " +
		"（Claude Code / Codex 等）可经它调用 read_file/write_file/list_files/search/stat/" +
		"mkdir/delete/share_create/cloud_download_create 等工具操作 sproxy 服务端文件。",
	Args: cobra.NoArgs,
	RunE: runServer,
}

func init() {
	addFlags(rootCmd)
	rootCmd.AddCommand(newVersionSubcommand())
}

// newRootCmd 返回根命令的新实例（flag 本地化，避免 cobra 全局状态与包级变量的
// 跨用例串扰；测试与 main 共用同一构造与登记）。
func newRootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "sproxy-mcp",
		Short: "MCP (Model Context Protocol) server——把 sproxy 文件能力暴露为 MCP 工具（stdio）",
		Long: "sproxy-mcp 通过 stdio 传输提供 MCP server（JSON-RPC 2.0）：AI CLI " +
			"（Claude Code / Codex 等）可经它调用 read_file/write_file/list_files/search/stat/" +
			"mkdir/delete/share_create/cloud_download_create 等工具操作 sproxy 服务端文件。",
		Args: cobra.NoArgs,
		RunE: runServer,
	}
	addFlags(cmd)
	return cmd
}

// addFlags 登记 sproxy-mcp 的全部命令行 flag（测试与 init 共用同一登记，防漂移）。
// flag 值按命令实例本地存储（String 而非 StringVar），RunE 经 GetString 读取——
// 不引入包级可变状态，并行测试可安全执行。
func addFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String(flagServer, "", "sproxy 服务端地址（必填），如 https://127.0.0.1:18083")
	f.String(flagAccessKey, "", "SproxySig AccessKey（公开标识）")
	f.String(flagAccessKeySecret, "", "SproxySig AccessKeySecret（本地密钥，仅计算签名，永不上线）")
	f.String(flagAccessKeyID, "", "SproxySig SK 条目 ID（skey-id）")
	f.String(flagVolume, "", "可选卷上下文（缺省 auto）")
}

// newVersionSubcommand 创建 version 子命令（与 cmd/sproxy、cmd/sclient 同构）。
func newVersionSubcommand() *cobra.Command {
	info := buildinfo.Default()
	info.Version = Version
	info.BuiltAt = BuildAt
	info.DirtyInfo = buildmeta.DirtyInfo()
	cmd := buildinfo.NewVersionCmd(info)
	return cmd
}

// buildFileClient 按参数装配 FileClient：server URL 必填；凭据三要素
// （access-key / access-key-secret / access-key-id）可选——全部省略时
// 走服务端免认证场景（无凭据 Ring / api_keys 关闭的部署形态）。
func buildFileClient(serverURL, ak, sk, skID string) (*client.FileClient, error) {
	var opts []client.Option
	if ak != "" {
		opts = append(opts, client.WithAccessKey(ak, sk))
	}
	if skID != "" {
		opts = append(opts, client.WithAccessKeyID(skID))
	}
	fc := client.NewFileClient(serverURL, opts...)
	if err := fc.InitError(); err != nil {
		return nil, fmt.Errorf("FileClient 初始化失败: %w", err)
	}
	return fc, nil
}

// runServer 是根命令的执行体：装配 FileClient → ToolRegistry → MCP Server，
// 以 stdin/stdout 作为 stdio 传输运行 Serve 直到 EOF 或 exit 通知。
func runServer(cmd *cobra.Command, _ []string) error {
	f := cmd.Flags()
	serverURL, err := f.GetString(flagServer)
	if err != nil {
		return err
	}
	if serverURL == "" {
		return fmt.Errorf("缺少必填 flag --server（sproxy 服务端地址）")
	}
	ak, _ := f.GetString(flagAccessKey)
	sk, _ := f.GetString(flagAccessKeySecret)
	skID, _ := f.GetString(flagAccessKeyID)
	volume, _ := f.GetString(flagVolume)

	fc, err := buildFileClient(serverURL, ak, sk, skID)
	if err != nil {
		return err
	}
	registry := mcp.NewToolRegistry(fc, volume)
	srv := mcp.NewServer(os.Stdin, os.Stdout, registry)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx)
}
