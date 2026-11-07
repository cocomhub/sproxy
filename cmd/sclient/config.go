// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/contextcfg"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// NewCmdConfig 创建独立的 config 命令工厂函数，接收 IOStreams 用于输出。
// cfgFile 为配置文件路径指针，用于 config set 时的回写。
func NewCmdConfig(factory clientfactory.Factory, ios cli.IOStreams, cfgFile *string, cfgSvc ConfigProvider) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config [show|set <key> <value>|remote]",
		Short: "配置管理",
		Long:  "查看或修改 sclient 配置（统一 context 模型 config.yaml）。\n\n可用配置项:\n  server_url      服务器地址 (如 https://127.0.0.1:18083)\n  access_key      SproxySig 认证 AccessKey（服务端凭据 Ring 登记了该 AK/SK 时需要）\n  access_key_secret  SproxySig 认证 AccessKeySecret（本地密钥，仅计算签名，永不上线）\n  access_key_id   SproxySig SK 条目 ID（可选；trust renew 成功后自动回填）\n  timeout         HTTP 超时秒数\n  chunk_size      分块上传/下载块大小 (字节)\n  max_chunk_size  最大分块大小 (字节)\n  hub_url         mesh/relay/p2p 共用的 hub 地址 (如 wss://hub.example.com/ws)\n  node_id         本节点默认 ID (mesh/relay/p2p 信令来源；为空回落主机名)\n  peer_fingerprints  对端身份指纹 pinning 列表（逗号分隔，64 hex 或 sha256:<64 hex>；配置后 xfer 隧道握手 fail-closed 校验对端身份）\n\n统一 context 后所有读写都在 config.yaml（environments/users/contexts 三件套）。",
		RunE: func(cmd *cobra.Command, args []string) error {
			// 统一 context：config.yaml（context 模型）是唯一配置源。无配置/损坏 → fail-closed。
			cc, err := loadContextConfig(*cfgFile)
			if err != nil {
				ios.WriteErrLine("加载配置失败: %v", err)
				return fmt.Errorf("加载配置失败: %w", err)
			}
			if len(args) == 0 || args[0] == "show" {
				return configShow(ios, cc)
			}
			if args[0] == "set" {
				if len(args) < 3 {
					return fmt.Errorf("用法: sclient config set <键> <值>")
				}
				return configSet(ios, cc, cfgFile, args[1], args[2])
			}
			ios.WriteErrLine("未知的 config 子命令: %s", args[0])
			return fmt.Errorf("用法: sclient config [show|set <键> <值>|remote]")
		},
	}
	cmd.AddCommand(NewCmdConfigRemote(factory, ios))
	return cmd
}

// configShow 显示当前配置：Resolve 得到合成视图（env+user 扁平）。
func configShow(ios cli.IOStreams, cc *contextcfg.Config) error {
	resolved, rerr := contextcfg.Resolve(cc, contextcfg.ResolveArgs{})
	if rerr != nil {
		ios.WriteErrLine("解析当前 context 失败: %v", rerr)
		return fmt.Errorf("解析当前 context 失败: %w", rerr)
	}
	cfg := clientfactory.ResolvedToClientConfig(resolved)
	client.HandleConfigShow(cfg, ios.Out)
	return nil
}

// configSet 设置配置：写入 context 模型对应段（env/user/context）后 Save。
func configSet(ios cli.IOStreams, cc *contextcfg.Config, cfgFile *string, key, value string) error {
	if err := applyConfigSetContext(cc, key, value); err != nil {
		ios.WriteErrLine("设置配置失败: %v", err)
		return fmt.Errorf("设置配置失败: %w", err)
	}
	if err := contextcfg.Save(cc, *cfgFile); err != nil {
		ios.WriteErrLine("保存配置失败: %v", err)
		return fmt.Errorf("保存配置失败: %w", err)
	}
	fmt.Fprintf(ios.Out, "配置已更新: %s = %s（写入当前 context）\n", key, value)
	return nil
}

// loadContextConfig 加载 config.yaml（context 模型）。返回 (*Config, nil) 表示成功
// （config.yaml 存在且有 current-context）；失败（无配置/损坏/引用缺失）→ fail-closed 报错。
func loadContextConfig(cfgPath string) (*contextcfg.Config, error) {
	if cfgPath == "" {
		return nil, errConfigNotFound
	}
	cc, err := contextcfg.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if len(cc.Contexts) == 0 || cc.CurrentContext == "" {
		return nil, errConfigNotFound
	}
	if _, rerr := contextcfg.Resolve(cc, contextcfg.ResolveArgs{}); rerr != nil {
		return nil, rerr
	}
	return cc, nil
}

// applyConfigSetContext 把 config set 写入 context 模型对应段：
//   - server_url / hub_url / node_id / timeout / chunk_size / tls 等 → 当前 env 段；
//   - access_key / access_key_secret / access_key_id → 当前 user 段；
//   - volume → 当前 context 段；
//   - 其它键 → 报错指引 `sclient context set`。
func applyConfigSetContext(cc *contextcfg.Config, key, value string) error {
	if cc == nil {
		return fmt.Errorf("当前 context 无可写配置（请先 sclient context use <name>）")
	}
	resolved, rerr := contextcfg.Resolve(cc, contextcfg.ResolveArgs{})
	if rerr != nil {
		return rerr
	}
	// 先借 client.ApplyConfigSet 做键合法性与值校验（合成视图上应用）。
	view := clientfactory.ResolvedToClientConfig(resolved)
	if err := client.ApplyConfigSet(view, key, value); err != nil {
		return err
	}
	// 按段回写（env / user / context）。
	switch key {
	case "server_url":
		resolved.Environment.ServerURL = view.ServerURL
	case "hub_url":
		resolved.Environment.HubURL = view.HubURL
	case "node_id":
		resolved.Environment.NodeID = view.NodeID
	case "timeout":
		resolved.Environment.Timeout = view.Timeout
	case "chunk_size":
		resolved.Environment.ChunkSize = view.ChunkSize
	case "access_key":
		if resolved.User == nil {
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user-name）")
		}
		resolved.User.AccessKey = view.AccessKey
	case "access_key_secret":
		if resolved.User == nil {
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user-name）")
		}
		resolved.User.AccessKeySecret = view.AccessKeySecret
	case "access_key_id":
		if resolved.User == nil {
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user-name）")
		}
		resolved.User.AccessKeyID = view.AccessKeyID
	case "volume":
		resolved.Volume = strings.TrimSpace(value)
	default:
		return fmt.Errorf("config set 不支持 context 键 %q（用 sclient context set 管理；或查看 config show）", key)
	}
	return nil
}

// NewCmdConfigRemote 创建独立的 config remote 命令工厂函数。
func NewCmdConfigRemote(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "查看或修改远程服务器配置",
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}

			cfg, err := svc.GetConfig(cmd.Context())
			if err != nil {
				ios.WriteErrLine("获取远程配置失败: %v", err)
				return fmt.Errorf("获取远程配置失败: %w", err)
			}
			fm := buildFormatterWithWriter(ios.Out, cmd)
			fm.PrintConfig(cfg)
			return nil
		},
	}
	cmd.AddCommand(NewCmdConfigRemoteSet(factory, ios))
	return cmd
}

// NewCmdConfigRemoteSet 创建独立的 config remote set 命令工厂函数。
func NewCmdConfigRemoteSet(factory clientfactory.Factory, ios cli.IOStreams) *cobra.Command {
	return &cobra.Command{
		Use:   "set <key> <value>",
		Short: "更新远程服务器运行时配置",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			svc, err := factory.NewClient(cmd)
			if err != nil {
				return err
			}
			key := args[0]
			value := args[1]
			updates := map[string]any{key: value}
			if err := svc.UpdateConfig(cmd.Context(), updates); err != nil {
				ios.WriteErrLine("更新远程配置失败: %v", err)
				return fmt.Errorf("更新远程配置失败: %w", err)
			}
			fmt.Fprintf(ios.Out, "远程配置已更新: %s = %s\n", key, value)
			return nil
		},
	}
}
