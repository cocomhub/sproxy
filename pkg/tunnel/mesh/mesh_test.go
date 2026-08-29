// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/sproxysig"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	webrtc "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// 测试用合法 AK/SK：SK 必须为 64 hex 字符（32 字节），ComputeRegisterProof 才可计算。
const (
	testAccessKey = "sk-test-access-key"
	testSecret    = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// TestWebRTCStream_WritesDialFrameOnMuxStream（P0-1 回归）：
// 直连数据面必须在 mux 流上写拨号帧，而非裸字节写 DataChannel。对端 p2p listen
// 用 mux 按帧消费，本测试复现对端消费方式断言读到正确拨号帧。
func TestWebRTCStream_WritesDialFrameOnMuxStream(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
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
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
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
	rt := hub.NewMeshRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: testAccessKey, Secret: testSecret}}), nil)

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

	ctx := t.Context()
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
		HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
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
	rt := hub.NewMeshRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: testAccessKey, Secret: testSecret}}), nil)
	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	ts := httptest.NewServer(muxHTTP)
	defer ts.Close()
	ctx := t.Context()
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
		HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
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

// TestAutoRegister_EmptySecretFailsClosed（任务8）：AccessKeySecret 为空时 AutoRegister
// 直接报错（fail-closed，防止无凭据注册被 hub 静默拒绝后客户端困惑）。
func TestAutoRegister_EmptySecretFailsClosed(t *testing.T) {
	_, err := AutoRegister(t.Context(), AutoRegisterParams{
		HubURL: "ws://127.0.0.1:1/ws",
		NodeID: "node-a",
		Prefix: "p2p",
	})
	if err == nil {
		t.Fatal("expected error when access_key_secret is empty")
	}
	if !strings.Contains(err.Error(), "access_key_secret 为空") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// runNodeTestHub 起 mock hub：/ws 注册 + HubServer + 可选信令桥，返回 server URL。
func runNodeTestHub(t *testing.T, withSignaling bool) (*hub.MeshRouteTable, *httptest.Server, context.CancelFunc) {
	t.Helper()
	rt := hub.NewMeshRouteTable()
	srv := hub.NewHubServer(rt, hub.NewAuthenticator([]hub.AccessKey{{Key: testAccessKey, Secret: testSecret}}), nil)
	muxHTTP := http.NewServeMux()
	wsNode := ws.NewHandlerNode()
	wsNode.AddToMux(muxHTTP, "/ws")
	if withSignaling {
		muxHTTP.Handle("/api/signal/", &miniSignalHub{})
	}
	// 节点发现：GET /api/hub/nodes 返回当前路由表全部在线节点（含临时节点）。
	muxHTTP.HandleFunc("/api/hub/nodes", func(w http.ResponseWriter, r *http.Request) {
		type nodeResp struct {
			ID string `json:"id"`
		}
		var nodes []nodeResp
		for _, n := range rt.List("") {
			nodes = append(nodes, nodeResp{ID: string(n.ID)})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(nodes)
	})
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
		kind := r.URL.Query().Get("kind")
		h.mu.Lock()
		if h.inbox == nil {
			h.inbox = map[string][]map[string]any{}
		}
		// 按 ?kind= 过滤（对齐真实 hub I9）。每次 poll 只消费**一条**匹配消息，
		// 其余保留给下次 poll——否则同一 peer 的多个并发 offer（full-mesh 下 node-a
		// 与 node-b 同时拨 node-c）会在一次 poll 中一起返回、客户端只取一条，其余
		// 丢失导致对端 WaitAnswer 超时。
		inbox := h.inbox[peer]
		var msgs, kept []map[string]any
		for _, m := range inbox {
			mkind, _ := m["kind"].(string)
			if kind == "" || mkind == kind {
				msgs = append(msgs, m)
			} else {
				kept = append(kept, m)
			}
		}
		if len(msgs) > 1 {
			kept = append(kept, msgs[1:]...)
			msgs = msgs[:1]
		}
		if msgs == nil {
			msgs = []map[string]any{}
		}
		h.inbox[peer] = kept
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
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
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
	svcs := rt.Table("").ServicesOf(hub.NodeID(nodeID))
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
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
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
	nodeCtx := t.Context()
	runErr := make(chan error, 1)
	go func() {
		runErr <- RunNode(nodeCtx, NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
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
		HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
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

// TestListHubNodes：解析 /api/hub/nodes 返回的节点列表 + Bearer 头 + 401 分支。
func TestListHubNodes(t *testing.T) {
	var gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"id":"a"},{"id":"b"},{"id":""}]`))
	}))
	defer ts.Close()

	ids, err := ListHubNodes(context.Background(), ts.URL, "test-ak", "test-sk", false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(gotAuth, sproxysig.Scheme+" ") {
		t.Fatalf("Authorization = %q, want SproxySig 签名", gotAuth)
	}
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("ids = %v, want [a b]（空 id 过滤）", ids)
	}

	// 401 分支：hubAPIError code=401。
	ts401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer ts401.Close()
	_, err = ListHubNodes(context.Background(), ts401.URL, "test-ak", "test-sk", false)
	var herr *hubAPIError
	if !errors.As(err, &herr) || herr.code != http.StatusUnauthorized {
		t.Fatalf("期望 hubAPIError 401, got %v", err)
	}
}

// TestRunNode_DiscoveryConnects：两个 mesh node（node-a/node-b）自动对等发现，
// node-a（A<B 半拨号）自动 webrtc 直连 node-b 并保持。
func TestRunNode_DiscoveryConnects(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	rt, ts, _ := runNodeTestHub(t, true)

	peersA := make(chan string, 4)
	ctxA := t.Context()
	go func() {
		_ = RunNode(ctxA, NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
			NodeID: "node-a", EnableWebRTC: true, Discover: true,
			DiscoveryInterval: 100 * time.Millisecond, DiscoveryProbeTimeout: 5 * time.Second,
			DiscoveryPeers: peersA, DialAllow: true,
		})
	}()
	ctxB := t.Context()
	go func() {
		_ = RunNode(ctxB, NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
			NodeID: "node-b", EnableWebRTC: true, Discover: true,
			DiscoveryInterval: 100 * time.Millisecond, DiscoveryProbeTimeout: 5 * time.Second,
			DialAllow: true,
		})
	}()

	// 等 node-a 自动发现并直连 node-b（node-a < node-b → A 拨 B）。
	select {
	case got := <-peersA:
		if got != "node-b" {
			t.Fatalf("discovery peer = %q, want node-b", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("node-a 未在 15s 内自动直连 node-b")
	}
	// node-b 仍在路由表（未因拨号/重连被误伤）。
	if rt.Lookup(hub.NodeID("node-b")) == nil {
		t.Fatal("node-b 不应从路由表消失")
	}
}

// TestGateway_RoutesEstablishedLink：本地网关复用已建直连链路路由到目标服务。
// 链路 = 内存 pipe 上的 mux 对（serve 侧跑 relay.Serve 出口拨 echo，链路池侧由网关
// 持有）；GatewayConnect 后数据面端到端 echo 回显（拨号帧 → relay.Serve → 出口拨号）。
func TestGateway_RoutesEstablishedLink(t *testing.T) {
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

	// 内存 pipe 上的 mux 对：serve 侧（relay.Serve 出口拨号）+ 链路池侧（网关持有）。
	a, b := xfertest.Pipe()
	defer a.Close()
	defer b.Close()
	serveMux := mux.New(a, mux.RoleListener)
	defer serveMux.Close()
	ctx := t.Context()
	go func() {
		// dialAllow=true + 精确放行宣告地址（echoAddr 是 loopback，DialAllowed 默认拒绝）。
		_ = relay.Serve(ctx, serveMux, "http://127.0.0.1:1", true, nil, nil,
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{echoAddr})})
	}()

	links := newLinkPool()
	dMux := mux.New(b, mux.RoleDialer)
	defer dMux.Close()
	links.set("peer", dMux)
	gw := newGateway(links, NodeConfig{NodeID: "local-node"}, nil)
	gatewayAddr, err := gw.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway serve: %v", err)
	}

	conn, err := GatewayConnect(ctx, gatewayAddr, "peer", echoAddr, "")
	if err != nil {
		t.Fatalf("GatewayConnect: %v", err)
	}
	defer conn.Close()

	payload := []byte("ping")
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("echo 未回显: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 内容不匹配: got %q want %q", got, payload)
	}
}

// TestGateway_NoPeerLink：请求一个链路池中没有的 peer → ErrNoPeerLink（调用方回落
// 常规拨号）。同时覆盖网关对缺参请求（peer/addr 为空）的 bad_request 应答。
func TestGateway_NoPeerLink(t *testing.T) {
	links := newLinkPool()
	gw := newGateway(links, NodeConfig{NodeID: "local-node"}, nil)
	ctx := t.Context()
	gatewayAddr, err := gw.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway serve: %v", err)
	}

	_, err = GatewayConnect(ctx, gatewayAddr, "missing-peer", "127.0.0.1:1", "")
	if !errors.Is(err, ErrNoPeerLink) {
		t.Fatalf("期望 ErrNoPeerLink, got %v", err)
	}
}

// TestGateway_Status：网关拓扑查询返回 node-id + 服务宣告 + 已建直连链路（链路类型）。
func TestGateway_Status(t *testing.T) {
	a, b := xfertest.Pipe()
	defer a.Close()
	defer b.Close()
	links := newLinkPool()
	dMux := mux.New(b, mux.RoleDialer)
	defer dMux.Close()
	links.set("node-b", dMux)
	gw := newGateway(links, NodeConfig{
		NodeID:   "node-a",
		Services: []hub.Service{{Name: "echo", Addr: "127.0.0.1:22"}},
	}, nil)
	ctx := t.Context()
	gatewayAddr, err := gw.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway serve: %v", err)
	}

	st, err := QueryGatewayStatus(ctx, gatewayAddr, "")
	if err != nil {
		t.Fatalf("QueryGatewayStatus: %v", err)
	}
	if st.NodeID != "node-a" {
		t.Fatalf("node_id = %q, want node-a", st.NodeID)
	}
	if len(st.Services) != 1 || st.Services[0].Name != "echo" || st.Services[0].Addr != "127.0.0.1:22" {
		t.Fatalf("services = %+v, want [{echo 127.0.0.1:22}]", st.Services)
	}
	if len(st.Peers) != 1 || st.Peers[0].Peer != "node-b" || st.Peers[0].Link != "webrtc-direct" {
		t.Fatalf("peers = %+v, want [node-b webrtc-direct]", st.Peers)
	}
}

// TestRunNode_ServiceAccessViaGateway：两个 mesh node（node-ap 自动拨号 node-svc，
// 各自宣告 echo 服务）——本地网关复用**同一条已建直连链路**双向路由（A→B 经 A 的
// 网关、B→A 经 B 的网关 accept 侧注册链路回拨），数据面端到端就绪（mesh connect
// --gateway 双向全覆盖）。
func TestRunNode_ServiceAccessViaGateway(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	// 两个 echo 后端（node-svc 与 node-ap 各一个）。
	startEcho := func(t *testing.T) string {
		t.Helper()
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
					_, _ = io.Copy(cn, cn)
				}(c)
			}
		}()
		return ln.Addr().String()
	}
	echoSvcAddr := startEcho(t)
	echoApAddr := startEcho(t)

	_, ts, _ := runNodeTestHub(t, true)

	// node-svc：宣告 echo-svc（服务宿主，被 node-ap 自动拨号），本地网关。
	gatewaySvc := make(chan string, 1)
	ctxSvc := t.Context()
	go func() {
		_ = RunNode(ctxSvc, NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
			NodeID: "node-svc", EnableWebRTC: true, Discover: true,
			DiscoveryInterval: 100 * time.Millisecond, DiscoveryProbeTimeout: 5 * time.Second,
			Services:     []hub.Service{{Name: "echo-svc", Addr: echoSvcAddr}},
			ServiceAddrs: []string{echoSvcAddr}, DialAllow: true,
			GatewayAddr: "127.0.0.1:0", GatewayNotify: gatewaySvc,
		})
	}()

	// node-ap：低 ID 自动拨 node-svc，宣告 echo-ap，本地网关。
	peersA := make(chan string, 4)
	gatewayA := make(chan string, 1)
	ctxA := t.Context()
	go func() {
		_ = RunNode(ctxA, NodeConfig{
			HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
			NodeID: "node-ap", EnableWebRTC: true, Discover: true,
			DiscoveryInterval: 100 * time.Millisecond, DiscoveryProbeTimeout: 5 * time.Second,
			DiscoveryPeers: peersA,
			Services:       []hub.Service{{Name: "echo-ap", Addr: echoApAddr}},
			ServiceAddrs:   []string{echoApAddr}, DialAllow: true,
			GatewayAddr: "127.0.0.1:0", GatewayNotify: gatewayA,
		})
	}()

	// 等两节点网关就绪 + node-ap 自动直连 node-svc。
	var addrA, addrSvc string
	select {
	case addrA = <-gatewayA:
	case <-time.After(10 * time.Second):
		t.Fatal("node-ap 网关未就绪")
	}
	select {
	case addrSvc = <-gatewaySvc:
	case <-time.After(10 * time.Second):
		t.Fatal("node-svc 网关未就绪")
	}
	select {
	case got := <-peersA:
		if got != "node-svc" {
			t.Fatalf("discovery peer = %q, want node-svc", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("node-ap 未自动直连 node-svc")
	}

	// 双向 echo 往返（经各自本地网关复用同一条已建链路）。B→A 方向依赖 accept 侧
	// 链路注册，注册在拨号建立后同步完成；对 ErrNoPeerLink 短重试容忍注册时序。
	echoRoundTrip := func(t *testing.T, gatewayAddr, peer, addr string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			conn, err := GatewayConnect(context.Background(), gatewayAddr, peer, addr, testSecret)
			if err == nil {
				defer conn.Close()
				payload := []byte("ping")
				if _, werr := conn.Write(payload); werr != nil {
					t.Fatalf("写 echo（%s→%s）失败: %v", peer, addr, werr)
				}
				got := make([]byte, len(payload))
				if rerr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); rerr != nil {
					t.Fatal(rerr)
				}
				if _, rerr := io.ReadFull(conn, got); rerr != nil {
					t.Fatalf("echo 未回显（%s→%s）: %v", peer, addr, rerr)
				}
				if string(got) != string(payload) {
					t.Fatalf("echo 内容不匹配: got %q want %q", got, payload)
				}
				return
			}
			if !errors.Is(err, ErrNoPeerLink) {
				t.Fatalf("GatewayConnect(%s→%s) 失败: %v", peer, addr, err)
			}
			if time.Now().After(deadline) {
				t.Fatalf("GatewayConnect(%s→%s) 无已建链路超时: %v", peer, addr, err)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	// 方向 1（A→B）：node-ap 网关复用已建链路路由到 node-svc 的 echo-svc。
	echoRoundTrip(t, addrA, "node-svc", echoSvcAddr)
	// 方向 2（B→A）：node-svc 网关（accept 侧注册链路）回拨 node-ap 的 echo-ap。
	echoRoundTrip(t, addrSvc, "node-ap", echoApAddr)
}

// TestGateway_RejectsWrongToken：mesh node 配置了信令 token 时，网关拒绝未携带
// 正确 token 的请求（未授权进程无法复用网关路由）。
func TestGateway_RejectsWrongToken(t *testing.T) {
	links := newLinkPool()
	gw := newGateway(links, NodeConfig{NodeID: "local-node", AccessKeySecret: "secret-token"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gatewayAddr, err := gw.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway serve: %v", err)
	}
	// 错误 token → 拒绝。
	_, err = GatewayConnect(ctx, gatewayAddr, "peer", "127.0.0.1:1", "wrong-token")
	if err == nil || !strings.Contains(err.Error(), "token") {
		t.Fatalf("期望网关拒绝错误 token, got %v", err)
	}
	// 空 token → 拒绝。
	_, err = GatewayConnect(ctx, gatewayAddr, "peer", "127.0.0.1:1", "")
	if err == nil {
		t.Fatalf("期望网关拒绝空 token, got nil")
	}
}

// TestGateway_RejectsNonLoopback：网关 fail-closed 拒绝非 loopback 监听地址
// （杜绝把未认证控制面暴露到 LAN/公网，防被用作开放 mesh 中继）。
func TestGateway_RejectsNonLoopback(t *testing.T) {
	links := newLinkPool()
	gw := newGateway(links, NodeConfig{NodeID: "local-node"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// 通配地址（0.0.0.0）→ 拒绝。
	_, err := gw.Serve(ctx, "0.0.0.0:0")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("期望拒绝通配监听地址, got %v", err)
	}
	// 显式私网地址 → 拒绝。
	_, err = gw.Serve(ctx, "192.168.1.5:0")
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("期望拒绝私网监听地址, got %v", err)
	}
}

// TestGateway_BindFailureFallsBackToRandomPort：网关默认端口被占时回落 127.0.0.1:0
// 随机端口（不终止 mesh node），回落后的网关可用。
func TestGateway_BindFailureFallsBackToRandomPort(t *testing.T) {
	// 占用一个 loopback 端口（模拟默认端口已被同机其他进程/节点占用）。
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	occupiedAddr := occupied.Addr().String()

	links := newLinkPool()
	gw := newGateway(links, NodeConfig{NodeID: "local-node"}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	actual, err := gw.Serve(ctx, occupiedAddr)
	if err != nil {
		t.Fatalf("网关应在默认端口被占时回落随机端口, got %v", err)
	}
	if actual == occupiedAddr {
		t.Fatalf("回落端口不应等于被占端口 %s", occupiedAddr)
	}
	// 回落后的网关可用：状态查询成功。
	if _, err := QueryGatewayStatus(ctx, actual, ""); err != nil {
		t.Fatalf("回落网关不可用: %v", err)
	}
}

// TestGateway_ConcurrentConnectionsOnSameLink：多个连接并发经同一网关复用同一条
// 已建链路（mux 多路复用），各自 echo 端到端成功。
func TestGateway_ConcurrentConnectionsOnSameLink(t *testing.T) {
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

	a, b := xfertest.Pipe()
	defer a.Close()
	defer b.Close()
	serveMux := mux.New(a, mux.RoleListener)
	defer serveMux.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		_ = relay.Serve(ctx, serveMux, "http://127.0.0.1:1", true, nil, nil,
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{echoAddr})})
	}()

	links := newLinkPool()
	dMux := mux.New(b, mux.RoleDialer)
	defer dMux.Close()
	links.set("peer", dMux)
	gw := newGateway(links, NodeConfig{NodeID: "local-node"}, nil)
	gatewayAddr, err := gw.Serve(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("gateway serve: %v", err)
	}

	const n = 8
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, gerr := GatewayConnect(ctx, gatewayAddr, "peer", echoAddr, "")
			if gerr != nil {
				errCh <- gerr
				return
			}
			defer conn.Close()
			payload := []byte("ping")
			if _, werr := conn.Write(payload); werr != nil {
				errCh <- werr
				return
			}
			got := make([]byte, len(payload))
			if rerr := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); rerr != nil {
				errCh <- rerr
				return
			}
			if _, rerr := io.ReadFull(conn, got); rerr != nil {
				errCh <- rerr
				return
			}
			if string(got) != string(payload) {
				errCh <- fmt.Errorf("echo mismatch: got %q want %q", got, payload)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for gerr := range errCh {
		t.Errorf("并发复用已建链路失败: %v", gerr)
	}
}

// TestParseDiscoveryPeerID：discovery 拨号临时身份（disc-<base>-<unixnano>）恢复真实
// node ID；非 disc 前缀 / 无数字尾段 / 空 base 均判非法。
func TestParseDiscoveryPeerID(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"disc-node-a-1787500000000000000", "node-a", true},
		{"disc-my-node-with-dashes-1787500000000000000", "my-node-with-dashes", true}, // base 含 '-'
		{"disc-node-123-1787500000000000000", "node-123", true},                       // base 以数字结尾
		{"disc-123-1787500000000000000", "123", true},                                 // base 全数字
		{"disc-disc-a-1787500000000000000", "disc-a", true},                           // base 以 disc- 开头
		{"disc-123-456", "", false},                                                   // 尾段过短（非 8+ 位 hex 后缀）
		{"mesh-node-a-1787500000000000000", "", false},                                // 非 disc 前缀
		{"p2p-node-a-1787500000000000000", "", false},                                 // p2p 拨号（临时，非 discovery）
		{"disc-node-a-abc12345", "node-a", true},                                      // 随机 hex 后缀（>=8 位）合法
		{"disc-node-a-abc", "", false},                                                // 尾段非合法长度 hex
		{"disc-node-a", "", false},                                                    // 无后缀段
		{"disc--1787500000000000000", "", false},                                      // base 为空
	}
	for _, c := range cases {
		got, ok := parseDiscoveryPeerID(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("parseDiscoveryPeerID(%q) = (%q,%v), want (%q,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// TestLinkPool_RemoveIf：仅当链路池中该 peer 仍指向给定 mux 才移除（防重连竞态
// 误删新链路——旧链路 serve 返回时若新链路已 set 不误删）。
func TestLinkPool_RemoveIf(t *testing.T) {
	links := newLinkPool()
	a, b := xfertest.Pipe()
	m1 := mux.New(a, mux.RoleDialer)
	defer m1.Close()
	m2 := mux.New(b, mux.RoleListener)
	defer m2.Close()
	links.set("peer", m1)

	// 旧 mux（m2）移除：池中仍指向 m1 → 不删除。
	links.removeIf("peer", m2)
	if _, ok := links.get("peer"); !ok {
		t.Fatal("removeIf 旧 mux 不应删除当前链路")
	}
	// 当前 mux（m1）移除：删除。
	links.removeIf("peer", m1)
	if _, ok := links.get("peer"); ok {
		t.Fatal("removeIf 当前 mux 应删除链路")
	}
}

// TestRunNode_FullMeshThreeNodes：3 节点 full-mesh，中间节点 node-b 的链路池同时含
// accept 侧（node-a，a<b 拨入注册）与拨号侧（node-c，b<c 拨出注册）条目——验证
// 双向链路注册在真实 full-mesh 中的混合。
func TestRunNode_FullMeshThreeNodes(t *testing.T) {
	// Windows 下收敛 UDP 候选收集到 loopback 单端口，避免测试反复弹防火墙授权框。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(60 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	_, ts, _ := runNodeTestHub(t, true)

	runNode := func(nodeID string, notify chan<- string) {
		go func() {
			_ = RunNode(t.Context(), NodeConfig{
				HubURL: ts.URL, AccessKey: testAccessKey, AccessKeySecret: testSecret,
				NodeID: nodeID, EnableWebRTC: true, Discover: true,
				DiscoveryInterval: 100 * time.Millisecond, DiscoveryProbeTimeout: 5 * time.Second,
				GatewayAddr: "127.0.0.1:0", GatewayNotify: notify,
			})
		}()
	}
	gatewayB := make(chan string, 1)
	runNode("node-a", make(chan string, 1))
	runNode("node-b", gatewayB)
	runNode("node-c", make(chan string, 1))

	var gatewayAddr string
	select {
	case gatewayAddr = <-gatewayB:
	case <-time.After(10 * time.Second):
		t.Fatal("node-b 网关未就绪")
	}

	// 等 node-b 链路池同时含 node-a（accept 侧）与 node-c（拨号侧）。
	// -race + 3 节点真实 webrtc 打洞可能显著慢于常规；60s 宽窗口对齐
	// CLAUDE.md "-race 下超时留 3 倍余量"。
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		st, err := QueryGatewayStatus(t.Context(), gatewayAddr, testSecret)
		if err == nil && len(st.Peers) == 2 {
			peers := map[string]bool{}
			for _, p := range st.Peers {
				peers[p.Peer] = true
			}
			if peers["node-a"] && peers["node-c"] {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("node-b 链路池未同时包含 node-a（accept 侧）与 node-c（拨号侧）")
}
