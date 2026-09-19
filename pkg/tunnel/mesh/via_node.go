// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// maxViaNodes 是 via-node 展开的中间节点候选上限（受全局 MaxCandidates 约束）。
const maxViaNodes = 3

// viaNodeProvider 实现 P4 多跳：经中间节点 X 中转（X 出站拨号到目标 T）。
// 每个 X 展开**双候选**（平级竞速，端到端 RTT 择优）：
//   - via-relay:<X>  —— 数据面经 hub 中继（RelayStream(X, T)）
//   - via-direct:<X> —— 数据面 webrtc 直连 X（DialWebRTC(HubSignaler(X))，X 出口拨 T）
type viaNodeProvider struct{}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

// Expand 展开为每个候选中间节点 X 的双候选（ListHubNodes ∩ outbound-dial）。
func (p viaNodeProvider) Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate {
	if svc == nil || target == nil {
		return nil
	}
	nodes, err := svc.ListHubNodes(ctx)
	if err != nil {
		return nil // 发现失败：无 via-node 候选（direct/relay 仍参与竞速）
	}
	out := make([]Candidate, 0, maxViaNodes*2)
	for _, n := range nodes {
		if len(out) >= maxViaNodes*2 {
			break
		}
		if n.ID == target.Node || !slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
			continue
		}
		xID := n.ID
		// via-relay:<X>：数据面经 hub 中继（现有路径）。
		out = append(out, Candidate{
			ID:       "via-relay:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
				target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
				start := time.Now()
				conn, err := svc.RelayStream(ctx, xID, target.Addr)
				if err != nil {
					return nil, fmt.Errorf("via-relay(%s): %w", xID, err)
				}
				return &Result{Conn: conn, Kind: KindViaNode, Latency: time.Since(start)}, nil
			},
		})
		// via-direct:<X>：数据面 webrtc 直连 X（信令器打洞到 X，X 出口拨 T）。
		out = append(out, Candidate{
			ID:       "via-direct:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
				target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
				return viaDirectXDial(ctx, signaler, xID, target, opts)
			},
		})
	}
	return out
}

// KindViaDirect 表示经中间节点 X 的 webrtc 直连数据面（L→X 不经 hub 字节）。
const KindViaDirect = "via-direct"

// viaDirectXDial 是 via-direct:X 候选的拨号函数：信令器打洞到 X（webrtc 直连），
// mux 流写 DialRequest(T) → X 的 relay.Serve 出口拨 T → 数据面 pump。
//
// 数据面路径：L ⇄(webrtc 打洞)⇄ X ⇄ T（不经 hub 字节；hub 只承载信令控制面）。
// 信令前提：signaler 须为 *hub.HubSignaler（可对任意已注册节点 X 打洞）——
// mDNS DirectSignaler 或 nil 无法寻址 X，返回错误（fail-closed，via-relay:X 仍参与竞速）。
func viaDirectXDial(ctx context.Context, signaler webrtc.Signaler, xID string,
	target *client.MeshService, opts DialOptions) (*Result, error) {
	start := time.Now()
	if !SignalerUsable(signaler) {
		return nil, fmt.Errorf("via-direct(%s): 无可用信令器（需 hub 信令桥打洞到 X）", xID)
	}
	// DialWebRTC 内部：webrtc 打洞到 xID（信令经 hub 桥）+ WebRTCStream 开 mux 流
	// 写 DialRequest(target.Addr) → X 的 relay.Serve 出口拨 T → pump。
	conn, err := DialWebRTC(ctx, signaler, &client.MeshService{Node: xID, Addr: target.Addr}, opts.ICE)
	if err != nil {
		return nil, fmt.Errorf("via-direct(%s): %w", xID, err)
	}
	return &Result{Conn: conn, Kind: KindViaDirect, Latency: time.Since(start)}, nil
}
