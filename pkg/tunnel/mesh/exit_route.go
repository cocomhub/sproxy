// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// exit_route.go 提供「本地直连优先 → 回退出口」与「自动选出口」的路由装配：
//   - NewLocalOrExitDial：本地 net.Dialer 直连目标（有界超时），失败/超时回退注入的 exit 拨号闭包；
//   - NewAutoExitDial：本地直连失败后从 hub 节点列表自动选出口（outbound-dial 能力优先 +
//     exclude 排除名单，候选 failover），目标由出口节点拨号策略把关。
//
// 签名与 pkg/httpproxy.DialFunc / pkg/socks5.DialFunc 兼容（func(ctx, addr) (net.Conn, error)），
// 本包不 import pkg/httpproxy（R1 分层），仅靠签名一致。二期竞速升级（并行双候选 + TTL 缓存）
// 签名不变，见设计文档 §5。
package mesh

import (
	"context"
	"fmt"
	"net"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// DefaultLocalDialTimeout 是本地直连探测默认超时（被墙 TCP 黑洞可感知的合理上界）。
const DefaultLocalDialTimeout = 3 * time.Second

// NewLocalOrExitDial 构造「本地直连优先 → 回退出口」拨号函数。
// localTimeout 是本地直连探测超时（0 = 不试本地，直接 exit）；exit 为 nil 时退化为纯本地直连
// （等价 Config.Dial=nil，本机出口语义）。
// 签名兼容二期竞速升级（并行双候选 + TTL 缓存，见设计文档 §5）。
func NewLocalOrExitDial(localTimeout time.Duration, exit func(ctx context.Context, addr string) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error) {
	local := func(ctx context.Context, addr string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "tcp", addr)
	}
	if exit == nil {
		return local
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		if localTimeout > 0 {
			ctx2, cancel := context.WithTimeout(ctx, localTimeout)
			conn, err := local(ctx2, addr)
			cancel()
			if err == nil {
				return conn, nil
			}
		}
		return exit(ctx, addr)
	}
}

// NewAutoExitDial 构造「本地直连优先 → 回退自动选出口」拨号函数。
// nodeLister 注入候选源（生产 = svc.ListHubNodes，测试 = 桩）；
// exitDialFor(nodeID) 构造经该节点的出口拨号闭包；
// exclude 是出口候选排除名单（精确 node-id 匹配命中跳过——被排除节点仍可被 SmartDial
// via-node 选为中转中间节点，「能中转但不出站」）。
// 候选判据：Capabilities 含 outbound-dial 优先；无则回落全部在线节点减 exclude。
// 顺序尝试候选（失败跳过下一个）；全部不可达才报错。
func NewAutoExitDial(
	localTimeout time.Duration,
	nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error),
	exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error),
	exclude []string,
) func(ctx context.Context, addr string) (net.Conn, error) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		if nodeLister == nil {
			return nil, fmt.Errorf("auto-exit: 无候选源")
		}
		nodes, err := nodeLister(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-exit: 拉取节点列表失败: %w", err)
		}
		// 候选 = outbound-dial 能力优先，无则全部在线节点；再减排除名单。
		var candidates []client.HubNodeInfo
		for _, n := range nodes {
			if slices.Contains(exclude, n.ID) {
				continue
			}
			if slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
				candidates = append(candidates, n)
			}
		}
		if len(candidates) == 0 {
			for _, n := range nodes {
				if !slices.Contains(exclude, n.ID) {
					candidates = append(candidates, n)
				}
			}
		}
		var lastErr error
		for _, n := range candidates {
			if exitDialFor == nil {
				return nil, fmt.Errorf("auto-exit: exitDialFor 未注入")
			}
			conn, derr := exitDialFor(n.ID)(ctx, addr)
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr != nil {
			return nil, fmt.Errorf("auto-exit: 全部候选不可达: %w", lastErr)
		}
		return nil, fmt.Errorf("auto-exit: 无可用出口节点")
	}
	return NewLocalOrExitDial(localTimeout, exit)
}
