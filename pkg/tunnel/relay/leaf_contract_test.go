// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package relay

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 本文件锁定 Serve 的返回值契约（见 leaf.go 文档注释）：
//
//	ctx 取消（正常关闭）→ nil；返回非 nil ⟺ 终止性错误。
//
// 两侧都必须被断言：只测「取消返回 nil」会让真失败被静默归零而无人察觉；只测
// 「出错返回非 nil」会重新引入「把 ctx 取消包成 error」的旧缺陷（所有调用方的
// 判空重新变成恒真，staticcheck SA4023）。

// TestServe_CtxCancelReturnsNil 断言：ctx 取消是正常关闭，Serve 返回 nil。
func TestServe_CtxCancelReturnsNil(t *testing.T) {
	t.Parallel()
	_, b := xfertest.Pipe()
	m := mux.New(b, mux.RoleListener)
	defer func() { _ = m.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, m, "http://127.0.0.1:1", false, http.DefaultClient, testLogger())
	}()

	time.Sleep(50 * time.Millisecond) // 等 Serve 进入 Accept 等待
	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ctx 取消应返回 nil（正常关闭），实际返回 %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve 未在 ctx 取消后返回")
	}
}

// TestServe_MuxCloseReturnsError 断言：ctx 仍存活时 mux 被关闭属终止性错误，
// Serve 必须返回非 nil——调用方的 `if err != nil` 守卫依赖这条语义。
func TestServe_MuxCloseReturnsError(t *testing.T) {
	t.Parallel()
	_, b := xfertest.Pipe()
	m := mux.New(b, mux.RoleListener)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		errCh <- Serve(ctx, m, "http://127.0.0.1:1", false, http.DefaultClient, testLogger())
	}()

	time.Sleep(50 * time.Millisecond) // 等 Serve 进入 Accept 等待
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
