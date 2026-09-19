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
		Long:  "查看或修改 sclient 配置。\n\n可用配置项:\n  server_url      服务器地址 (如 https://127.0.0.1:18083)\n  access_key      SproxySig 认证 AccessKey（服务端凭据 Ring 登记了该 AK/SK 时需要）\n  access_key_secret  SproxySig 认证 AccessKeySecret（本地密钥，仅计算签名，永不上线）\n  access_key_id   SproxySig SK 条目 ID（可选；trust renew 成功后自动回填）\n  timeout         HTTP 超时秒数\n  chunk_size      分块上传/下载块大小 (字节)\n  max_chunk_size  最大分块大小 (字节)\n  hub_url         mesh/relay/p2p 共用的 hub 地址 (如 wss://hub.example.com/ws)\n  node_id         本节点默认 ID (mesh/relay/p2p 信令来源；为空回落主机名)\n  peer_fingerprints  对端身份指纹 pinning 列表（逗号分隔，64 hex 或 sha256:<64 hex>；配置后 xfer 隧道握手 fail-closed 校验对端身份）\n\n多环境：SCLIENT_ENV=prod 时默认加载 sclient.prod.yaml。",
		RunE: func(cmd *cobra.Command, args []string) error {
			// T7 双模式：config.yaml（context 模型）存在且可解析 → 读写 context 段；
			// 否则回落旧平铺（cfgSvc / client.SaveConfig，既有零破坏）。
			var cc *contextcfg.Config
			contextMode := false
			if cfgFile != nil {
				cc, contextMode = loadContextConfig(*cfgFile)
			}
			if len(args) == 0 || args[0] == "show" {
				if contextMode {
					// 合成视图：server_url/凭据/调优项来自当前 context 的 env+user。
					resolved, rerr := contextcfg.Resolve(cc, contextcfg.ResolveArgs{})
					if rerr != nil {
						ios.WriteErrLine("解析当前 context 失败: %v", rerr)
						return fmt.Errorf("解析当前 context 失败: %w", rerr)
					}
					cfg := clientfactory.ResolvedToClientConfig(resolved)
					client.HandleConfigShow(cfg, ios.Out)
					return nil
				}
				cfg, err := cfgSvc.LoadConfig()
				if err != nil {
					ios.WriteErrLine("加载配置失败: %v", err)
					return fmt.Errorf("加载配置失败: %w", err)
				}
				client.HandleConfigShow(cfg, ios.Out)
				return nil
			}

			if args[0] == "set" {
				if len(args) < 3 {
					return fmt.Errorf("用法: sclient config set <键> <值>")
				}
				if contextMode {
					if err := applyConfigSetContext(cc, args[1], args[2]); err != nil {
						ios.WriteErrLine("设置配置失败: %v", err)
						return fmt.Errorf("设置配置失败: %w", err)
					}
					if err := contextcfg.Save(cc, *cfgFile); err != nil {
						ios.WriteErrLine("保存配置失败: %v", err)
						return fmt.Errorf("保存配置失败: %w", err)
					}
					fmt.Fprintf(ios.Out, "配置已更新: %s = %s（写入当前 context）\n", args[1], args[2])
					return nil
				}
				cfg, err := cfgSvc.LoadConfig()
				if err != nil {
					ios.WriteErrLine("加载配置失败: %v", err)
					return fmt.Errorf("加载配置失败: %w", err)
				}
				if err := client.ApplyConfigSet(cfg, args[1], args[2]); err != nil {
					ios.WriteErrLine("设置配置失败: %v", err)
					return fmt.Errorf("设置配置失败: %w", err)
				}
				if err := client.SaveConfig(cfg, *cfgFile); err != nil {
					ios.WriteErrLine("保存配置失败: %v", err)
					return fmt.Errorf("保存配置失败: %w", err)
				}
				fmt.Fprintf(ios.Out, "配置已更新: %s = %s\n", args[1], args[2])
				return nil
			}

			ios.WriteErrLine("未知的 config 子命令: %s", args[0])
			return fmt.Errorf("用法: sclient config [show|set <键> <值>|remote]")
		},
	}
	cmd.AddCommand(NewCmdConfigRemote(factory, ios))
	return cmd
}

// loadContextConfig 加载 config.yaml（context 模型）。返回 (*Config, true) 表示
// context 模式可用（config.yaml 存在且有 current-context）；否则 (nil, false)
// 回落旧平铺路径。show 时用 Resolve 得到合成视图；set 时直接改模型段后 Save。
func loadContextConfig(cfgPath string) (*contextcfg.Config, bool) {
	if cfgPath == "" {
		return nil, false
	}
	cc, err := contextcfg.Load(cfgPath)
	if err != nil || len(cc.Contexts) == 0 || cc.CurrentContext == "" {
		return nil, false
	}
	if _, rerr := contextcfg.Resolve(cc, contextcfg.ResolveArgs{}); rerr != nil {
		return nil, false
	}
	return cc, true
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
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user）")
		}
		resolved.User.AccessKey = view.AccessKey
	case "access_key_secret":
		if resolved.User == nil {
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user）")
		}
		resolved.User.AccessKeySecret = view.AccessKeySecret
	case "access_key_id":
		if resolved.User == nil {
			return fmt.Errorf("当前 context 无 user（请先 sclient context set --user）")
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
