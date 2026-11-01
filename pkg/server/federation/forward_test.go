// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package federation

import (
	"context"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// TestFederationForwarder_Forward_LoopGuard：转发器防环单元测试——跳数超限与
// 路径回源均返回 508（DoD 2），且不发起网络请求。覆盖 hubID 配置与不配置两种
// 情形（评审 #2：默认无 node_id 时防环也必须自洽生效）。
func TestFederationForwarder_Forward_LoopGuard(t *testing.T) {
	peer := hub.FederationPeer{ID: "hubB", URL: "http://127.0.0.1:1"} // 不会真正拨号
	for _, tc := range []struct {
		name  string
		hubID string
	}{
		{name: "hubID配置", hubID: "hubA"},
		{name: "hubID缺省", hubID: ""}, // 默认配置：防环依赖下一跳 peer.ID 追加，仍须生效
	} {
		t.Run(tc.name, func(t *testing.T) {
			fwd := NewFederationForwarder(nil, tc.hubID, 2, testutil.DiscardLogger())

			// 跳数超限：incomingHop >= maxHops(2) → 508。
			_, err := fwd.Forward(t.Context(), peer, "node-x", "1.2.3.4:80", 2, nil)
			if !isForwardStatus(err, http.StatusLoopDetected) {
				t.Fatalf("跳数超限应 508, got %v", err)
			}

			// 路径回源：peer.ID("hubB") 已在路径 → 508。
			_, err = fwd.Forward(t.Context(), peer, "node-x", "1.2.3.4:80", 0, []string{"hubA", "hubB"})
			if !isForwardStatus(err, http.StatusLoopDetected) {
				t.Fatalf("路径回源应 508, got %v", err)
			}

			// 跳数边界内、路径不含 peer → 不触发防环（会尝试拨号 127.0.0.1:1 → 网络错误 502）。
			_, err = fwd.Forward(t.Context(), peer, "node-x", "1.2.3.4:80", 1, []string{"hubA"})
			if err == nil || isForwardStatus(err, http.StatusLoopDetected) {
				t.Fatalf("防环不应误伤合法转发, got %v", err)
			}
		})
	}
}

// TestRelayForwardDialer_TLSHandshakeBounded：对端 accept TCP 但永不回复 ServerHello
// （TLS 黑洞）时，握手必须被 deadline/ctx 约束快速失败，而非无限阻塞（评审 #1）。
func TestRelayForwardDialer_TLSHandshakeBounded(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(io.Discard, cn) // 读 ClientHello 但永不回 ServerHello
			}(c)
		}
	}()

	peer := hub.FederationPeer{ID: "hubTLS", URL: "https://" + ln.Addr().String()}
	d := &relayForwardDialer{logger: testutil.DiscardLogger()}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	start := time.Now()
	_, err = d.Dial(ctx, peer, "node-x", "1.2.3.4:80", nil)
	if err == nil {
		t.Fatalf("TLS 黑洞应拨号失败")
	}
	// 2s ctx deadline + 握手 deadline（min(ctx,30s)）→ 应在 5s 内返回（-race 留余量）。
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("TLS 握手应被 deadline 约束，took %v", elapsed)
	}
}

// isForwardStatus 报告 err 是否为携带指定状态的 StatusError。
func isForwardStatus(err error, status int) bool {
	if err == nil {
		return false
	}
	if fse, ok := err.(*StatusError); ok {
		return fse.Status == status
	}
	return false
}
