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

// 本文件锁定 (*Tunnel).Serve 的返回值契约（见 tunnel_mux.go 文档注释）：
//
//	ctx 取消（正常关闭，含握手中断）→ nil；返回非 nil ⟺ 终止性错误。
//
// 真失败一侧另由既有门禁覆盖：TestECDHHandshake_KeyedListenerNilDialerFails
// （ecdh_test.go）断言 keyed listener 握手失败（父 ctx 存活）必须返回非 nil——
// 它同时是「不得把真握手失败归零」的反向保险。

// TestTunnelServe_CtxCancelReturnsNil 断言两条退出路径在 ctx 取消时都返回 nil：
// 未配置 key 时的 accept 循环、keyed listener 的握手中断。
func TestTunnelServe_CtxCancelReturnsNil(t *testing.T) {
	key, err := ParseKey(testHexKey)
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	tests := []struct {
		name string
		key  []byte
	}{
		{"accept 循环（无 key）", nil},
		{"握手中断（keyed listener）", key},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, b := xfertest.Pipe()
			m := mux.New(b, mux.RoleListener)
			defer func() { _ = m.Close() }()
			tun := NewTunnel(m, tc.key)

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()

			errCh := make(chan error, 1)
			go func() {
				errCh <- tun.Serve(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
			}()

			time.Sleep(50 * time.Millisecond) // 等 Serve 进入 Accept/握手等待
			cancel()

			select {
			case err := <-errCh:
				if err != nil {
					t.Fatalf("ctx 取消应返回 nil（正常关闭），实际返回 %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Serve 未在 ctx 取消后返回")
			}
		})
	}
}

// TestTunnelServe_MuxCloseReturnsError 断言：ctx 仍存活时 mux 被关闭属终止性错误，
// Serve 必须返回非 nil——cmd/sproxy 的 `sErr != nil && ctx.Err() == nil` 告警守卫
// 依赖这条语义。
func TestTunnelServe_MuxCloseReturnsError(t *testing.T) {
	t.Parallel()
	_, b := xfertest.Pipe()
	m := mux.New(b, mux.RoleListener)
	tun := NewTunnel(m, nil) // 无 key：跳过握手，直接进 accept 循环

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- tun.Serve(ctx, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}()

	time.Sleep(50 * time.Millisecond)
	if err := m.Close(); err != nil {
		t.Fatalf("mux.Close: %v", err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("ctx 存活时 mux 关闭应返回非 nil（终止性错误），实际返回 nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 mux 关闭后返回")
	}
}
