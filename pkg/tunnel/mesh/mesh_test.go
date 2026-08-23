// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
)

// TestWebRTCStream_WritesDialFrameOnMuxStream（P0-1 回归）：
// 直连数据面必须在 mux 流上写拨号帧，而非裸字节写 DataChannel。对端 p2p listen
// 用 mux 按帧消费，本测试复现对端消费方式断言读到正确拨号帧。
func TestWebRTCStream_WritesDialFrameOnMuxStream(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	signal := webrtc.NewSignal()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	frameCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		conn, err := webrtc.Listen(signal)
		if err != nil {
			errCh <- err
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer m.Close()
		stream, err := m.Accept(ctx)
		if err != nil {
			errCh <- err
			return
		}
		defer stream.Close()
		lenBuf := make([]byte, 4)
		if _, err := io.ReadFull(stream, lenBuf); err != nil {
			errCh <- err
			return
		}
		meta := make([]byte, binary.BigEndian.Uint32(lenBuf))
		if _, err := io.ReadFull(stream, meta); err != nil {
			errCh <- err
			return
		}
		frameCh <- string(meta)
	}()

	conn, err := webrtc.Dial(signal)
	if err != nil {
		t.Fatalf("dial webrtc: %v", err)
	}
	defer conn.Close()
	res, err := WebRTCStream(ctx, conn, "127.0.0.1:22")
	if err != nil {
		t.Fatalf("WebRTCStream: %v", err)
	}
	if res.Kind != KindWebRTC {
		t.Fatalf("kind = %q, want webrtc", res.Kind)
	}

	select {
	case f := <-frameCh:
		var d hub.DialRequest
		if err := json.Unmarshal([]byte(f), &d); err != nil || d.Dial != "127.0.0.1:22" {
			t.Fatalf("对端收到的拨号帧 = %q, want {\"dial\":\"127.0.0.1:22\"}", f)
		}
	case err := <-errCh:
		t.Fatalf("对端读取拨号帧失败: %v", err)
	case <-ctx.Done():
		t.Fatal("等待对端读取拨号帧超时")
	}
}

// TestDial_FallsBackToRelay：webrtc 打洞失败（不可达信令）时回落 hub 中继。
func TestDial_FallsBackToRelay(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/relay/stream" {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	svc := client.NewFileClient(ts.URL)
	target := &client.MeshService{Name: "svc", Node: "node-a", Addr: "127.0.0.1:22"}
	// 不可达 hub 信令器 → webrtc 必然失败 → 回落中继。
	signaler := hub.NewHubSignaler("http://127.0.0.1:1", "", "local-node")

	_, err := Dial(context.Background(), svc, signaler, target, "local-node")
	if err == nil {
		t.Fatal("expected error: webrtc 失败且中继失败应报错")
	}
	// 错误应来自中继（webrtc 已回落），而非 webrtc 本身：用 "RelayStream" 前缀判定
	// （svc.RelayStream 的错误均以该前缀包装），覆盖 502/连接重置/超时等 relay 错误。
	if !strings.Contains(err.Error(), "RelayStream") {
		t.Fatalf("expected relay fallback error, got: %v", err)
	}
}

// TestAutoRegister_GetsSecretAndCleanup：经 mock hub 自动注册拿到 per-node secret，
// 信令请求携带 X-Node-Secret / X-Node-ID（B2/B3），closer 移除临时节点。
func TestAutoRegister_GetsSecretAndCleanup(t *testing.T) {
	rt := hub.NewRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator("relay-token"), nil)

	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	var mu sync.Mutex
	var gotSecret, gotNodeID string
	muxHTTP.HandleFunc("/api/signal/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotSecret = r.Header.Get("X-Node-Secret")
		gotNodeID = r.Header.Get("X-Node-ID")
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	ts := httptest.NewServer(muxHTTP)
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// hub 侧：接受 ws 连接并交给 HandleConn 注册。
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) { _ = srv.HandleConn(ctx, cc) }(c)
		}
	}()

	reg, err := AutoRegister(ctx, AutoRegisterParams{
		HubURL: ts.URL, RelayToken: "relay-token", SignalToken: "signal-token",
		NodeID: "node-a", Prefix: "p2p", ExactNode: false, Insecure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Closer() })
	if !strings.HasPrefix(reg.TempNode, "p2p-node-a-") {
		t.Fatalf("temp node %q should start with p2p-node-a-", reg.TempNode)
	}
	info, ok := rt.LookupInfo(hub.NodeID(reg.TempNode))
	if !ok {
		t.Fatalf("temp node %q not registered", reg.TempNode)
	}
	if info.Secret == "" {
		t.Fatal("per-node secret should not be empty")
	}
	// 信令携带 X-Node-Secret / X-Node-ID（B2/B3 身份校验前置）。
	if sperr := reg.Signaler.SendOffer("peer-node", "sdp"); sperr != nil {
		t.Fatal(sperr)
	}
	mu.Lock()
	secret, nodeID := gotSecret, gotNodeID
	mu.Unlock()
	if secret != info.Secret || nodeID != reg.TempNode {
		t.Fatalf("signaling headers mismatch: secret=%q node=%q (want %q/%q)", secret, nodeID, info.Secret, reg.TempNode)
	}
	// closer 关闭注册连接 → hub 移除临时节点（防 WS 泄漏核心保证，D4）。
	if cerr := reg.Closer(); cerr != nil {
		t.Fatal(cerr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rt.Has(hub.NodeID(reg.TempNode)) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Has(hub.NodeID(reg.TempNode)) {
		t.Fatalf("closer 后节点 %q 应被 hub 移除", reg.TempNode)
	}
}

// TestAutoRegister_ExactNode（D1 回归）：exact 模式注册成 nodeID 原样（p2p listen
// 的被寻址方需稳定 ID 供 --peer 寻址），closer 移除节点。
func TestAutoRegister_ExactNode(t *testing.T) {
	rt := hub.NewRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator("relay-token"), nil)
	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	ts := httptest.NewServer(muxHTTP)
	defer ts.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) { _ = srv.HandleConn(ctx, cc) }(c)
		}
	}()

	reg, err := AutoRegister(ctx, AutoRegisterParams{
		HubURL: ts.URL, RelayToken: "relay-token", SignalToken: "signal-token",
		NodeID: "node-b", Prefix: "p2p", ExactNode: true, Insecure: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Closer() })
	// exact 模式：注册成 nodeID 原样（不加临时前缀）。
	if reg.TempNode != "node-b" {
		t.Fatalf("exact 模式应注册成 node-b 原样, got %q", reg.TempNode)
	}
	if _, ok := rt.LookupInfo(hub.NodeID("node-b")); !ok {
		t.Fatal("exact node-b 未注册（p2p listen 无法被 --peer 寻址）")
	}
	// closer 移除节点。
	if cerr := reg.Closer(); cerr != nil {
		t.Fatal(cerr)
	}
	deadline := time.Now().Add(3 * time.Second)
	for rt.Has(hub.NodeID("node-b")) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rt.Has(hub.NodeID("node-b")) {
		t.Fatal("closer 后 exact node-b 应被移除")
	}
}
