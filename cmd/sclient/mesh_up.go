// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// mesh_up.go 是虚拟子网用户态接入（roadmap P2 VPN 模式最小集）：
//
//   - `sclient mesh up [-l :port] [--exit <node>]`：启动本地 SOCKS5 代理
//     （复用 socks 命令的 mesh 路由），提示客户端把虚拟子网流量指向该代理
//     （curl --socks5-hostname / 系统代理）——虚拟子网内节点经 mesh 可达。
//   - 无 tun/tap 特权要求（纯用户态；tun/tap 内核虚拟网卡留作系统集成后续）。

import (
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/spf13/cobra"
)

// newCmdMeshUp 构造 `mesh up` 子命令（虚拟子网用户态接入 = SOCKS5 代理）。
// 复用 socks 命令完整实现（cobra 组合：up 的 RunE 委托 socks 的 RunE）。
func newCmdMeshUp(factory clientfactory.Factory, ios cli.IOStreams, cfgSvc ConfigProvider) *cobra.Command {
	socksCmd := newCmdSocks(factory, ios, cfgSvc)
	cmd := &cobra.Command{
		Use:   "up [-l :port] [--exit <node>]",
		Short: "启动虚拟子网接入（本地 SOCKS5 代理，虚拟子网流量经 mesh 路由）",
		Long: `启动本地 SOCKS5 代理作为虚拟子网用户态接入：客户端（curl --socks5-hostname /
系统代理）把虚拟子网目标流量指向本代理，代理经 mesh 路由到 --exit 出口节点
出站。与 socks 命令等价（同一 mesh 路由装配）；up 是虚拟子网语义的封装——
无需内核 tun/tap 特权（纯用户态），系统集成可后续扩展。`,
		RunE: func(cmd *cobra.Command, args []string) error {
			// 委托 socks 命令执行（flag 透传：把本命令 flag 复制到 socks）。
			socksCmd.Flags().AddFlagSet(cmd.Flags())
			socksCmd.SetArgs(args)
			return socksCmd.Execute()
		},
	}
	// socks 同款 flag 族（listen + 出口族 + 认证）。
	cmd.Flags().AddFlagSet(socksCmd.Flags())
	return cmd
}
