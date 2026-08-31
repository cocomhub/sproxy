// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	mesh "github.com/cocomhub/sproxy/pkg/tunnel/mesh"
)

// meshVIPDial 包装 meshDialFunc，把虚拟 IP 目标（<vip>:<port>）经 vipTable 解析为
// 节点 ID 后回落 base 拨号（webrtc 直连优先 / hub 中继回落 / --gateway 复用已建链路）。
//
// 语义（设计 AD-3/AD-6）：
//   - 目标地址 host ∈ 虚拟子网 → 查 vipTable 定位 node-id，构造 {Node: node-id,
//     Addr: <vip>:<port>} 回落 base（出口侧 DialPolicy 识别 ==selfVIP 改写本机端口）；
//   - 未知虚拟 IP → 报错（不猜测 node-id，防地址注入）；
//   - 非虚拟子网 → 原样回落 base（服务名寻址等既有路径）。
//
// vipTable 只接受认证数据源（hub 节点列表 / mDNS 签名 TXT）填充，防注入。
func meshVIPDial(vipTable *mesh.VipTable, subnet netip.Prefix, base meshDialFunc, _ cli.IOStreams) meshDialFunc {
	if base == nil {
		base = meshDialFunc(mesh.Dial)
	}
	return func(ctx context.Context, svc *client.FileClient, signaler *hub.HubSignaler, target *client.MeshService, localNode string) (*mesh.Result, error) {
		if target == nil {
			return nil, errors.New("mesh 目标为空")
		}
		host, _, err := net.SplitHostPort(target.Addr)
		if err != nil {
			return nil, fmt.Errorf("虚拟 IP 目标地址非法 %q: %w", target.Addr, err)
		}
		vip, ok := mesh.ParseVirtualAddr(host)
		if !ok || !mesh.IsVirtualAddr(vip, subnet) {
			return base(ctx, svc, signaler, target, localNode)
		}
		nodeID, ok := vipTable.NodeByAddr(vip)
		if !ok {
			// R-5：目标节点重连/hub 重启后虚拟 IP 可能变化，静态 target 会持续解析失败；
			// 明确提示重试（重启 mesh connect 重新拉取节点列表）。
			return nil, fmt.Errorf("虚拟 IP %s 未在 mesh 节点列表中找到对应节点（请确认目标节点已在线且 hub 已分配虚拟 IP；若目标节点刚重连导致虚拟 IP 变化，请重试本命令）", vip)
		}
		t2 := *target
		t2.Node = nodeID
		return base(ctx, svc, signaler, &t2, localNode)
	}
}
