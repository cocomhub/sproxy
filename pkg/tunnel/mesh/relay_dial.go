// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// dialRelay 是「经 hub 中继到目标节点」的单一实现（mesh connect 固定路径 +
// SmartDial 竞速 relay 候选共用，避免多套入口漂移）：
//
//   - 非 E2E：RelayStream（hub 写普通 dial 帧，叶子出口拨号）
//   - E2E（opts.E2E != nil）：RelayStreamE2E 让 hub 写 e2e 首帧 +
//     DialE2EHandshake 完成 ECDH 握手（对齐 hub-relay-e2e 设计）
//
// 返回 Result（Kind=KindRelay，EndToEnd=opts.E2E!=nil，Latency=整体建连耗时）。
// 修复历史：smart.go relayDial 曾忽略 opts.E2E 走明文（静默明文降级，安全红线），
// 抽公共函数后单一实现杜绝此类遗漏。
func dialRelay(ctx context.Context, svc *client.FileClient, target *client.MeshService, opts DialOptions) (*Result, error) {
	start := time.Now()
	if opts.E2E != nil {
		conn, err := svc.RelayStreamE2E(ctx, target.Node, target.Addr, true, "")
		if err != nil {
			return nil, fmt.Errorf("relay: %w", err)
		}
		e2eConn, derr := DialE2EHandshake(ctx, conn, *opts.E2E)
		if derr != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("relay E2E 握手失败: %w", derr)
		}
		return &Result{Conn: e2eConn, Kind: KindRelay, EndToEnd: true, Latency: time.Since(start)}, nil
	}
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	return &Result{Conn: conn, Kind: KindRelay, Latency: time.Since(start)}, nil
}
