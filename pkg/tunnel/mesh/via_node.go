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
// 数据面经 X：本地 → hub → X（RelayStream）+ X 出站拨 T（relay.Serve 出口语义）。
type viaNodeProvider struct{}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

// Expand 展开为每个候选中间节点 X 的候选（ListHubNodes ∩ outbound-dial）。
func (p viaNodeProvider) Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate {
	if svc == nil || target == nil {
		return nil
	}
	nodes, err := svc.ListHubNodes(ctx)
	if err != nil {
		return nil // 发现失败：无 via-node 候选（direct/relay 仍参与竞速）
	}
	out := make([]Candidate, 0, maxViaNodes)
	for _, n := range nodes {
		if len(out) >= maxViaNodes {
			break
		}
		if n.ID == target.Node || !slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
			continue
		}
		xID := n.ID
		out = append(out, Candidate{
			ID:       "via-node:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
				target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
				start := time.Now()
				conn, err := svc.RelayStream(ctx, xID, target.Addr)
				if err != nil {
					return nil, fmt.Errorf("via-node(%s): %w", xID, err)
				}
				return &Result{Conn: conn, Kind: KindViaNode, Latency: time.Since(start)}, nil
			},
		})
	}
	return out
}
