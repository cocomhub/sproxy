// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// TestTunnelLongLived_StreamStaysOpen 守护 SSH/反向隧道长连接场景：
// 服务器端 handler 写应答后仍存活一段（模拟 SSH 会话），且客户端提前关闭响应体
// 不会误杀整个 mux——该流关闭后 mux 还能继续开新流。
// 这是对“兜底关闭”修复的重要回归保护：不允许为了解死锁而把正常长连接一起掐断。
func TestTunnelLongLived_StreamStaysOpen(t *testing.T) {
	a, b := xfertest.Pipe()
	muxA := mux.New(a, mux.RoleDialer)
	muxB := mux.New(b, mux.RoleListener)
	defer muxA.Close()
	defer muxB.Close()

	tunA := NewTunnel(muxA, nil)
	tunB := NewTunnel(muxB, nil)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	srvErr := make(chan error, 1)
	go func() {
		srvErr <- tunB.Serve(ctx, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 模拟 SSH 应答：写 meta + 少量数据，然后保持运行（模拟会话中）
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			time.Sleep(800 * time.Millisecond)
		}))
	}()

	time.Sleep(150 * time.Millisecond)

	req, _ := http.NewRequestWithContext(ctx, "GET", "/ssh", nil)
	resp, err := tunA.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	// 客户端立即关闭响应体（模拟短会话），然后再开一条流验证 mux 健康
	resp.Body.Close()

	// 服务端 800ms 后仍在跑（未被提前关闭），mux 还能再开一条流
	select {
	case serveEr := <-srvErr:
		t.Fatalf("Serve 提前退出: %v", serveEr)
	case <-time.After(900 * time.Millisecond):
	}

	stream2, err := muxA.Open(ctx)
	if err != nil {
		t.Fatalf("客户端关闭响应体后 mux 不能开新流(误杀长连接?): %v", err)
	}
	stream2.Close()
}
