// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub_test

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 tcp 传输
)

// testAccessKey / testAccessKeySecret 是测试用 SproxySig AK/SK。
const (
	testAccessKey       = "sk-test-00000000000000000000000000"
	testAccessKeySecret = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// startHubTCP 启动一个仅裸 TCP 传输的 hub（无 WS），返回 hub 地址与路由表。
func startHubTCP(t *testing.T, ctx context.Context, aks []hub.AccessKey) (*hub.MeshRouteTable, string) {
	t.Helper()
	rt := hub.NewMeshRouteTable()
	auth := hub.NewAuthenticator(aks)
	hs := hub.NewHubServer(rt, auth, testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = hs.AcceptTCP(ctx, ln)
	}()
	addr := ln.(interface{ Addr() net.Addr }).Addr().String()
	return rt, addr
}

// registerLeafTCP 通过裸 TCP 注册一个叶子节点并返回其连接（保持存活）。
func registerLeafTCP(t *testing.T, ctx context.Context, hubAddr, nodeID, ak, sk string) (xfer.Conn, *mux.Mux) {
	t.Helper()
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	conn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, err := hub.ComputeRegisterProof(sk, nodeID, ts, nonce)
	if err != nil {
		t.Fatal(err)
	}
	frame := hub.NewRegisterFrame(nodeID, ak, proof, ts, nonce, hub.Meta{Addr: "127.0.0.1:0"}, hub.CapabilityPerNodeSecret)
	if serr := conn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	ack, rerr := conn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, aerr := hub.ParseRegisterAck(string(ack)); aerr != nil {
		t.Fatal(aerr)
	}
	m := mux.New(conn, mux.RoleListener)
	t.Cleanup(func() { _ = m.Close() })
	return conn, m
}

// TestHubTCP_RegisterViaTCP 验证叶子经裸 TCP 注册后进入路由表（无 WS 场景）。
func TestHubTCP_RegisterViaTCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt, hubAddr := startHubTCP(t, ctx, []hub.AccessKey{{Key: testAccessKey, Secret: testAccessKeySecret}})
	_, _ = registerLeafTCP(t, ctx, hubAddr, "leaf-tcp", testAccessKey, testAccessKeySecret)

	if !rt.Has("leaf-tcp") {
		t.Fatal("expected leaf-tcp to be registered via TCP")
	}
	if info, ok := rt.LookupInfo("leaf-tcp"); !ok {
		t.Fatal("expected LookupInfo to find leaf-tcp")
	} else if info.Addr != "127.0.0.1:0" {
		t.Fatalf("expected addr 127.0.0.1:0, got %q", info.Addr)
	}
}

// TestHubTCP_InvalidAccessKeyRejected 验证错误的 AK/注册证明被拒绝（REG_ERR），
// 且节点不进入路由表（fail-closed）。
func TestHubTCP_InvalidAccessKeyRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt, hubAddr := startHubTCP(t, ctx, []hub.AccessKey{{Key: testAccessKey, Secret: testAccessKeySecret}})

	tp := xfer.Get("tcp")
	conn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// 用错误 AK（未命中 accessKeys）发送注册帧
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, _ := hub.ComputeRegisterProof("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "evil", ts, nonce)
	frame := hub.NewRegisterFrame("evil", "sk-unknown-key", proof, ts, nonce, hub.Meta{})
	if serr := conn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	ack, rerr := conn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, aerr := hub.ParseRegisterAck(string(ack)); aerr == nil {
		t.Fatal("expected REG_ERR for invalid access key")
	}
	if rt.Has("evil") {
		t.Fatal("invalid key node should not be registered")
	}
}

// TestHubTCP_EmptyAccessKeysFailClosed 验证 hub 未配置 access_keys（nil auth）时
// 拒绝所有注册（fail-closed，防"遗漏传 auth"走向开放注册）。
func TestHubTCP_EmptyAccessKeysFailClosed(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt := hub.NewMeshRouteTable()
	// auth 传 nil → NewAuthenticator(nil) → fail-closed
	hs := hub.NewHubServer(rt, nil, testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { _ = hs.AcceptTCP(ctx, ln) }()
	hubAddr := ln.(interface{ Addr() net.Addr }).Addr().String()

	tp := xfer.Get("tcp")
	conn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, _ := hub.ComputeRegisterProof(testAccessKeySecret, "nope", ts, nonce)
	frame := hub.NewRegisterFrame("nope", testAccessKey, proof, ts, nonce, hub.Meta{})
	if serr := conn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	ack, rerr := conn.Receive(ctx)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if _, aerr := hub.ParseRegisterAck(string(ack)); aerr == nil {
		t.Fatal("expected REG_ERR when auth is fail-closed")
	}
}

// TestHubTCP_DisconnectRemovesNode 验证叶子断开 TCP 后节点从路由表移除。
func TestHubTCP_DisconnectRemovesNode(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt, hubAddr := startHubTCP(t, ctx, []hub.AccessKey{{Key: testAccessKey, Secret: testAccessKeySecret}})
	conn, m := registerLeafTCP(t, ctx, hubAddr, "leaf-disc", testAccessKey, testAccessKeySecret)

	if !rt.Has("leaf-disc") {
		t.Fatal("expected leaf-disc registered before disconnect")
	}
	// 关闭连接（mux.Close 关闭底层 conn），hub 的 HandleConn 应感知并移除节点
	_ = m.Close()
	_ = conn.Close()

	// 轮询等待移除（异步）
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !rt.Has("leaf-disc") {
			return // 已移除
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("expected leaf-disc to be removed after disconnect")
}

// TestHubTCP_ConcurrentRegistrations 验证多个叶子并发经 TCP 注册均成功。
func TestHubTCP_ConcurrentRegistrations(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	rt, hubAddr := startHubTCP(t, ctx, []hub.AccessKey{{Key: testAccessKey, Secret: testAccessKeySecret}})

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		i := i
		go func() {
			defer wg.Done()
			id := string(rune('n' + i))
			tp := xfer.Get("tcp")
			conn, err := tp.Dial(ctx, hubAddr)
			if err != nil {
				errCh <- err
				return
			}
			defer conn.Close()
			ts := time.Now().UnixMilli()
			nonce := hub.NewRegisterNonce()
			proof, perr := hub.ComputeRegisterProof(testAccessKeySecret, id, ts, nonce)
			if perr != nil {
				errCh <- perr
				return
			}
			frame := hub.NewRegisterFrame(id, testAccessKey, proof, ts, nonce, hub.Meta{Addr: "127.0.0.1:0"})
			if serr := conn.Send(ctx, frame); serr != nil {
				errCh <- serr
				return
			}
			ack, rerr := conn.Receive(ctx)
			if rerr != nil {
				errCh <- rerr
				return
			}
			if _, aerr := hub.ParseRegisterAck(string(ack)); aerr != nil {
				errCh <- aerr
				return
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent registration failed: %v", err)
	}
	for i := range n {
		id := string(rune('n' + i))
		if !rt.Has(hub.NodeID(id)) {
			t.Fatalf("expected node %q registered", id)
		}
	}
}

// TestHubTCP_AcceptCtxCancel 验证 ctx 取消后 AcceptTCP 正常返回（不泄漏 goroutine）。
func TestHubTCP_AcceptCtxCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: testAccessKey, Secret: testAccessKeySecret}}), testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	acceptDone := make(chan error, 1)
	subCtx, subCancel := context.WithCancel(ctx)
	go func() { acceptDone <- hs.AcceptTCP(subCtx, ln) }()
	time.Sleep(100 * time.Millisecond)
	subCancel()
	select {
	case err := <-acceptDone:
		if err != nil {
			t.Fatalf("AcceptTCP should return nil on ctx cancel, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("AcceptTCP blocked forever after ctx cancel")
	}
}
