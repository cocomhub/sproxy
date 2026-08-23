// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
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

// runNodeTestHub 起 mock hub：/ws 注册 + HubServer + 可选信令桥，返回 server URL。
func runNodeTestHub(t *testing.T, withSignaling bool) (*hub.RouteTable, *httptest.Server, context.CancelFunc) {
	t.Helper()
	rt := hub.NewRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator("relay-token"), nil)
	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	if withSignaling {
		muxHTTP.Handle("/api/signal/", &miniSignalHub{})
	}
	ts := httptest.NewServer(muxHTTP)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			c, aerr := wsNode.Accept(ctx)
			if aerr != nil {
				return
			}
			go func(cc xfer.Conn) { _ = srv.HandleConn(ctx, cc) }(c)
		}
	}()
	t.Cleanup(func() { cancel(); ts.Close() })
	return rt, ts, cancel
}

// miniSignalHub 是最小信令桥：POST /api/signal/{kind} 存到收件箱，
// GET /api/signal/poll/{peer} 返回该 peer 的消息并消费（非长轮询，客户端周期重 poll）。
type miniSignalHub struct {
	mu    sync.Mutex
	inbox map[string][]map[string]any
}

func (h *miniSignalHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/api/signal/poll/"):
		peer := strings.TrimPrefix(r.URL.Path, "/api/signal/poll/")
		h.mu.Lock()
		if h.inbox == nil {
			h.inbox = map[string][]map[string]any{}
		}
		msgs := h.inbox[peer]
		if msgs == nil {
			msgs = []map[string]any{}
		}
		h.inbox[peer] = nil
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(msgs)
	case strings.HasPrefix(r.URL.Path, "/api/signal/"):
		var msg map[string]any
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		to, _ := msg["to"].(string)
		h.mu.Lock()
		if h.inbox == nil {
			h.inbox = map[string][]map[string]any{}
		}
		h.inbox[to] = append(h.inbox[to], msg)
		h.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	default:
		http.NotFound(w, r)
	}
}

// TestRunNode_RegistersServicesAndRelays：mesh node 单进程注册 + 服务宣告 +
// 中继路径（hub 侧 mux Open 流写 dial 帧 → 出口拨号 echo）。EnableWebRTC=false。
func TestRunNode_RegistersServicesAndRelays(t *testing.T) {
	// echo 后端（127.0.0.1 回环）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn)
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	rt, ts, _ := runNodeTestHub(t, false)
	nodeID := "mesh-node-relay"
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- RunNode(nodeCtx, NodeConfig{
			HubURL: ts.URL, RelayToken: "relay-token", SignalToken: "signal-token",
			NodeID: nodeID, Services: []hub.Service{{Name: "echo", Addr: echoAddr}},
			ServiceAddrs: []string{echoAddr}, DialAllow: true, LocalAddr: "http://127.0.0.1:1",
			EnableWebRTC: false,
		})
	}()

	// 等注册 + 服务宣告。
	deadline := time.Now().Add(5 * time.Second)
	for rt.Lookup(hub.NodeID(nodeID)) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if rt.Lookup(hub.NodeID(nodeID)) == nil {
		nodeCancel()
		t.Fatal("mesh node 未注册")
	}
	svcs := rt.ServicesOf(hub.NodeID(nodeID))
	if len(svcs) != 1 || svcs[0].Name != "echo" || svcs[0].Addr != echoAddr {
		nodeCancel()
		t.Fatalf("服务宣告不对: %+v", svcs)
	}

	// 中继路径：hub 侧 mux Open 流 + 写 dial 帧 → mesh node relay.Serve 出口拨 echo。
	hubMux := rt.Lookup(hub.NodeID(nodeID))
	stream, err := hubMux.Open(context.Background())
	if err != nil {
		nodeCancel()
		t.Fatal(err)
	}
	defer stream.Close()
	head, _ := json.Marshal(hub.DialRequest{Dial: echoAddr})
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, err := stream.Write(lenBuf); err != nil {
		nodeCancel()
		t.Fatal(err)
	}
	if _, err := stream.Write(head); err != nil {
		nodeCancel()
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		nodeCancel()
		t.Fatal(err)
	}

	// 读回：先消费拨号结果帧，再读 echo 回显。
	gotCh := make(chan string, 1)
	go func() {
		var all []byte
		buf := make([]byte, 64)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				all = append(all, buf[:n]...)
				if bytes.Contains(all, []byte("ping")) {
					gotCh <- string(all)
					return
				}
			}
			if rerr != nil {
				gotCh <- string(all)
				return
			}
		}
	}()
	select {
	case got := <-gotCh:
		if !bytes.Contains([]byte(got), []byte("ping")) {
			nodeCancel()
			t.Fatalf("中继 echo 未回显: %q", got)
		}
	case <-time.After(5 * time.Second):
		nodeCancel()
		t.Fatal("中继 echo 超时")
	}

	nodeCancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Fatalf("RunNode 应在 ctx 取消后返回 nil, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunNode 未在 ctx 取消后返回")
	}
}

// TestRunNode_WebRTCDirect：mesh node webrtc 直连环接受拨号方打洞直连，
// 直连数据面（dial 帧→出口拨号 echo）经 relay.Serve 分发。EnableWebRTC=true。
func TestRunNode_WebRTCDirect(t *testing.T) {
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	// echo 后端。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn)
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	rt, ts, _ := runNodeTestHub(t, true)
	nodeID := "mesh-node-direct"
	nodeCtx, nodeCancel := context.WithCancel(context.Background())
	defer nodeCancel()
	runErr := make(chan error, 1)
	go func() {
		runErr <- RunNode(nodeCtx, NodeConfig{
			HubURL: ts.URL, RelayToken: "relay-token", SignalToken: "signal-token",
			NodeID: nodeID, Services: []hub.Service{{Name: "echo", Addr: echoAddr}},
			ServiceAddrs: []string{echoAddr}, DialAllow: true, LocalAddr: "http://127.0.0.1:1",
			EnableWebRTC: true,
		})
	}()

	// 等 mesh node 注册（webrtc 环依赖注册 + secret）。
	deadline := time.Now().Add(5 * time.Second)
	for rt.Lookup(hub.NodeID(nodeID)) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if rt.Lookup(hub.NodeID(nodeID)) == nil {
		t.Fatal("mesh node 未注册")
	}

	// 拨号方：临时节点注册拿 signaler → webrtc 直连 nodeID → mux 流写 dial 帧 → echo。
	dialer, err := AutoRegister(context.Background(), AutoRegisterParams{
		HubURL: ts.URL, RelayToken: "relay-token", SignalToken: "signal-token",
		NodeID: "dialer", Prefix: "p2p", ExactNode: false,
	})
	if err != nil {
		t.Fatalf("拨号方注册失败: %v", err)
	}
	defer func() { _ = dialer.Closer() }()

	dialCtx, dialCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer dialCancel()
	conn, err := webrtc.DialWithSignalerCtx(dialCtx, nodeID, dialer.Signaler)
	if err != nil {
		t.Fatalf("webrtc 直连失败: %v", err)
	}
	defer conn.Close()
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleDialer)
	defer m.Close()
	stream, err := m.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := WriteDialFrame(stream, echoAddr); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}

	gotCh := make(chan string, 1)
	go func() {
		var all []byte
		buf := make([]byte, 64)
		for {
			n, rerr := stream.Read(buf)
			if n > 0 {
				all = append(all, buf[:n]...)
				if bytes.Contains(all, []byte("ping")) {
					gotCh <- string(all)
					return
				}
			}
			if rerr != nil {
				gotCh <- string(all)
				return
			}
		}
	}()
	select {
	case got := <-gotCh:
		if !bytes.Contains([]byte(got), []byte("ping")) {
			t.Fatalf("直连 echo 未回显: %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("直连 echo 超时")
	}

	_ = runErr
}
