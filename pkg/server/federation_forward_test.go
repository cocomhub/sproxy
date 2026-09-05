// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// federationForwardTestAK / SK：跨 hub 转发认证测试用的默认 mesh 凭据
// （sk-<32hex>，ParseMesh → 空 mesh）。
const (
	federationForwardTestAK = "sk-0011223344556677"
	federationForwardTestSK = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// newRelayTestHub 启动一个带 hub + 联邦配置的完整 sproxy handler（httptest server）。
// creds 为空时以无认证模式启动；非空时配置 SproxySig 准入（fail-closed）。
// 凭据通过 Ring 注入（取代旧 cfg.AccessKeys）。
func newRelayTestHub(t *testing.T, rt *hub.MeshRouteTable, creds ...testCredPair) (*Handlers, *httptest.Server) {
	t.Helper()
	cfg := Default()
	cfg.Addr = "127.0.0.1:0"
	cfg.StorageRoot = t.TempDir()
	cfg.LogLevel = "error"
	cfg.Hub.Enabled = true
	cfg.Hub.Federation.Enabled = true
	var cfgPtr atomic.Pointer[Config]
	cfgPtr.Store(cfg)
	muxsrv := http.NewServeMux()
	opts := RegisterRoutesOpts{
		Mux:        muxsrv,
		CfgPtr:     &cfgPtr,
		RouteTable: rt,
		Logger:     testutil.DiscardLogger(),
	}
	if len(creds) > 0 {
		withTestCreds(&opts, creds...)
	} else {
		noAuth := defaultNoAuthRegOpts()
		opts.CredentialRing = noAuth.CredentialRing
		opts.AllowInsecureLoopback = noAuth.AllowInsecureLoopback
	}
	h := RegisterRoutes(t.Context(), opts)
	ts := httptest.NewServer(h.Handler())
	t.Cleanup(func() { ts.Close(); _ = h.Close() })
	return h, ts
}

// newRelayEchoLeaf 构造一个带 TCP echo 后端的中继叶子：
// leafMux（RoleListener）运行 relay.Serve（DialResultFrames），callerMux（RoleDialer）
// 供 hub 注册为节点并 Open 流。返回 callerMux（注册用）与 echo 地址。
func newRelayEchoLeaf(t *testing.T) (*mux.Mux, string) {
	t.Helper()
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo: %v", err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn) // echo
			}(c)
		}
	}()

	pipeA, pipeB := xfertest.Pipe()
	callerMux := mux.New(pipeA, mux.RoleDialer)
	leafMux := mux.New(pipeB, mux.RoleListener)
	t.Cleanup(func() {
		_ = callerMux.Close()
		_ = leafMux.Close()
	})
	ctx, cancel := context.WithCancel(t.Context())
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: func(addr string) (string, bool) { return addr, true }, DialResultFrames: true})
	}()
	t.Cleanup(func() { cancel() })
	_ = leafErr
	return callerMux, echoLn.Addr().String()
}

// relayConnectRaw 以 raw CONNECT 风格向 hub 发起 /api/relay/stream 请求（无认证）。
// 200 后返回可双向读写的字节流（bufio 预读字节保留）。非 200 返回错误（含状态码）。
func relayConnectRaw(t *testing.T, baseURL, target, addr string, headers map[string]string) (net.Conn, error) {
	t.Helper()
	host := strings.TrimPrefix(baseURL, "http://")
	raw, err := net.Dial("tcp", host)
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(RelayStreamRequest{Target: target, Type: "tcp", Addr: addr})
	var b strings.Builder
	fmt.Fprintf(&b, "POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n", host, len(body))
	for k, v := range headers {
		fmt.Fprintf(&b, "%s: %s\r\n", k, v)
	}
	b.WriteString("Connection: close\r\n\r\n")
	if _, werr := io.WriteString(raw, b.String()); werr != nil {
		_ = raw.Close()
		return nil, werr
	}
	if _, werr := raw.Write(body); werr != nil {
		_ = raw.Close()
		return nil, werr
	}
	br := bufio.NewReader(raw)
	statusLine, rerr := br.ReadString('\n')
	if rerr != nil {
		_ = raw.Close()
		return nil, rerr
	}
	parts := strings.SplitN(strings.TrimSpace(statusLine), " ", 3)
	code := 0
	if len(parts) >= 2 {
		code, _ = strconv.Atoi(parts[1])
	}
	if code != http.StatusOK {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		_ = raw.Close()
		return nil, fmt.Errorf("hub 返回 %d %s", code, strings.TrimSpace(string(rest)))
	}
	for {
		line, hdrErr := br.ReadString('\n')
		if hdrErr != nil {
			_ = raw.Close()
			return nil, hdrErr
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	return &testBufConn{Conn: raw, r: br}, nil
}

// testBufConn 包装 bufio.Reader（保留 200 后可能已预读的数据面字节）。
type testBufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *testBufConn) Read(p []byte) (int, error) { return c.r.Read(p) }

// echoRoundTrip 在已建立的中继字节流上做一次写读往返断言。
func echoRoundTrip(t *testing.T, conn net.Conn, payload string) {
	t.Helper()
	if _, err := conn.Write([]byte(payload)); err != nil {
		t.Fatalf("写 payload 失败: %v", err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatalf("读 echo 失败: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}
}

// TestCrossHubRelay_EndToEnd_Echo（DoD 1 核心）：A→hub1→hub2→B 链式中继数据往返。
// hub-A 路由表无 node-b，但经联邦看到 node-b（来自 hub-B），relay 拨号被转发到
// hub-B，由 hub-B 拨 node-b 叶子出站 echo——跨 hub 转发路径闭环。
func TestCrossHubRelay_EndToEnd_Echo(t *testing.T) {
	callerMux, echoAddr := newRelayEchoLeaf(t)

	// hub-B：路由表注册 node-b（叶子）。
	rtB := hub.NewMeshRouteTable()
	rtB.AddNode("", "node-b", callerMux)
	_, tsB := newRelayTestHub(t, rtB)

	// hub-A：空路由表 + 联邦客户端指向 hub-B。
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: tsB.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("hub-A 拉取 hub-B 节点表: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-b", echoAddr, nil)
	if err != nil {
		t.Fatalf("跨 hub relay dial: %v", err)
	}
	defer conn.Close()
	echoRoundTrip(t, conn, "cross-hub-echo-1")
	echoRoundTrip(t, conn, "cross-hub-echo-2")
}

// TestCrossHubRelay_ConcurrentDial：并发跨 hub 拨号（多条中继流同时经 A→B 链），
// -race 稳定；每条流独立 echo 往返。
func TestCrossHubRelay_ConcurrentDial(t *testing.T) {
	callerMux, echoAddr := newRelayEchoLeaf(t)
	rtB := hub.NewMeshRouteTable()
	rtB.AddNode("", "node-b", callerMux)
	_, tsB := newRelayTestHub(t, rtB)

	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: tsB.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("hub-A 拉取 hub-B 节点表: %v", err)
	}
	hA.SetFederationClient(fcA)

	const n = 6
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			conn, err := relayConnectRaw(t, tsA.URL, "node-b", echoAddr, nil)
			if err != nil {
				errCh <- fmt.Errorf("拨号 %d: %w", idx, err)
				return
			}
			defer conn.Close()
			payload := fmt.Sprintf("concurrent-%d", idx)
			if _, werr := conn.Write([]byte(payload)); werr != nil {
				errCh <- fmt.Errorf("写 %d: %w", idx, werr)
				return
			}
			got := make([]byte, len(payload))
			if _, rerr := io.ReadFull(conn, got); rerr != nil {
				errCh <- fmt.Errorf("读 %d: %w", idx, rerr)
				return
			}
			if string(got) != payload {
				errCh <- fmt.Errorf("echo %d 不匹配: got %q want %q", idx, got, payload)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("并发跨 hub 拨号: %v", err)
	}
}

// TestCrossHubRelay_AuthEndToEnd：跨 hub 转发不绕过认证——hub-A/hub-B 均配置
// access_keys，客户端用 hub-A 凭据、hub-A 用 peer 凭据转发到 hub-B，全链路由
// SproxySig 保护；错误 peer 凭据被 hub-B 拒绝（fail-closed）。
func TestCrossHubRelay_AuthEndToEnd(t *testing.T) {
	callerMux, echoAddr := newRelayEchoLeaf(t)
	rtB := hub.NewMeshRouteTable()
	rtB.AddNode("", "node-b", callerMux)
	_, tsB := newRelayTestHub(t, rtB, testCredPair{ak: federationForwardTestAK, sk: federationForwardTestSK})

	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA, testCredPair{ak: federationForwardTestAK, sk: federationForwardTestSK})

	// 正确 peer 凭据：hub-A 用 hub-B 认可的 AK/SK 拉取 + 转发。
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{
		ID: "hubB", URL: tsB.URL,
		AccessKey: federationForwardTestAK, AccessKeySecret: federationForwardTestSK,
	}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("hub-A 拉取 hub-B 节点表（带凭据）: %v", err)
	}
	hA.SetFederationClient(fcA)

	// 客户端经 client.FileClient（SproxySig 签名）拨 hub-A 的 relay stream。
	cl := client.NewFileClient(tsA.URL, client.WithAccessKey(federationForwardTestAK, federationForwardTestSK))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	conn, err := cl.RelayStream(ctx, "node-b", echoAddr)
	if err != nil {
		t.Fatalf("跨 hub relay dial（带认证）: %v", err)
	}
	defer conn.Close()
	echoRoundTrip(t, conn, "auth-cross-hub-echo")
}

// TestRelayStreamHandler_ForwardUpstream401_Maps502：上游对端以 401 拒绝本 hub
// 的转发（peer 凭据错误/未授权）→ 客户端收到 502（网关侧认证失败不误报为客户端
// 未授权；跨 hub 转发 fail-closed，错误沿转发链传播）。
func TestRelayStreamHandler_ForwardUpstream401_Maps502(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	mock.relayStatus = http.StatusUnauthorized // 模拟对端拒绝本 hub 凭据
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", nil)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("上游 401 应传播给客户端")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("上游 401 应映射 502（网关侧认证失败）, got: %v", err)
	}
}

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

// TestRelayStreamHandler_ForwardHopLimit_Returns508：服务器级——客户端带超限
// X-Relay-Hop 请求联邦目标 → hub 拒绝 508，且不向对端转发（mock 未被接触）。
func TestRelayStreamHandler_ForwardHopLimit_Returns508(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", map[string]string{relayForwardHopHeader: "99"})
	if err == nil {
		_ = conn.Close()
		t.Fatalf("跳数超限应被拒绝")
	}
	if !strings.Contains(err.Error(), "508") {
		t.Fatalf("跳数超限应 508, got: %v", err)
	}
	if mock.relayHits.Load() != 0 {
		t.Fatalf("跳数超限不应向对端转发, relay hits = %d", mock.relayHits.Load())
	}
}

// TestRelayStreamHandler_ForwardPathLoop_Returns508：服务器级——客户端带含对端 ID
// 的 X-Relay-Path 请求联邦目标 → hub 检测到回源拒绝 508，不向对端转发。
func TestRelayStreamHandler_ForwardPathLoop_Returns508(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", map[string]string{relayForwardPathHeader: "hubA,hubB"})
	if err == nil {
		_ = conn.Close()
		t.Fatalf("路径回源应被拒绝")
	}
	if !strings.Contains(err.Error(), "508") {
		t.Fatalf("路径回源应 508, got: %v", err)
	}
	if mock.relayHits.Load() != 0 {
		t.Fatalf("路径回源不应向对端转发, relay hits = %d", mock.relayHits.Load())
	}
}

// TestRelayStreamHandler_Forward_FailoverToSecondPeer：多对端上报同一节点时，首个
// 对端失败（502）自动尝试第二个（成功）——故障转移（评审 #5，对齐 MeshConnect 多候选回退）。
func TestRelayStreamHandler_Forward_FailoverToSecondPeer(t *testing.T) {
	mockDown := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	mockDown.relayStatus = http.StatusBadGateway
	mockUp := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)

	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "hubDown", URL: mockDown.srv.URL},
		{ID: "hubUp", URL: mockUp.srv.URL},
	}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", nil)
	if err != nil {
		t.Fatalf("第二个对端应接管成功: %v", err)
	}
	defer conn.Close()
	echoRoundTrip(t, conn, "failover-echo")
	if mockDown.relayHits.Load() != 1 {
		t.Errorf("宕机对端应被尝试一次, got %d", mockDown.relayHits.Load())
	}
	if mockUp.relayHits.Load() != 1 {
		t.Errorf("健康对端应被尝试一次, got %d", mockUp.relayHits.Load())
	}
}

// TestRelayStreamHandler_Forward_FailoverPastLoop（Minor-1 回归）：请求经对端 X 来
// （路径含 X），X 命中防环 508——**508 不阻断故障转移**，继续尝试上报同节点的对端 Y
// 并成功（每个 Forward 独立防环；旧实现 break 会跳过能真实服务的 Y）。
func TestRelayStreamHandler_Forward_FailoverPastLoop(t *testing.T) {
	mockX := newMockFedPeer(t, `[{"id":"node-z","addr":"10.0.0.9:9000","mesh":""}]`)
	mockY := newMockFedPeer(t, `[{"id":"node-z","addr":"10.0.0.9:9000","mesh":""}]`)

	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{
		{ID: "hubX", URL: mockX.srv.URL},
		{ID: "hubY", URL: mockY.srv.URL},
	}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	// 请求带 X-Relay-Path: hubX（模拟经 hubX 转发而来）→ X 防环 508，Y 应接管。
	conn, err := relayConnectRaw(t, tsA.URL, "node-z", "1.2.3.4:80", map[string]string{
		relayForwardPathHeader: "hubX",
	})
	if err != nil {
		t.Fatalf("508 后应故障转移到 Y 成功: %v", err)
	}
	defer conn.Close()
	echoRoundTrip(t, conn, "past-loop-failover-echo")
	if mockX.relayHits.Load() != 0 {
		t.Errorf("X 在路径中应被防环拦截（不实际拨号）, hits = %d", mockX.relayHits.Load())
	}
	if mockY.relayHits.Load() != 1 {
		t.Errorf("Y 应被实际拨号一次, got %d", mockY.relayHits.Load())
	}
}

// TestRelayStreamHandler_ForwardErrorPropagation_502：上游对端返回 502 → 客户端
// 收到 502（错误沿转发链传播，不静默挂起）。
func TestRelayStreamHandler_ForwardErrorPropagation_502(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	mock.relayStatus = http.StatusBadGateway
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", nil)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("上游 502 应传播给客户端")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("上游 502 应映射 502, got: %v", err)
	}
	if mock.relayHits.Load() != 1 {
		t.Fatalf("上游应被请求一次, got %d", mock.relayHits.Load())
	}
}

// TestRelayStreamHandler_Forward_HeadersSent：转发请求携带防环头与对端认证头
// （SproxySig），下游可据此防环与鉴权。
func TestRelayStreamHandler_Forward_HeadersSent(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{
		ID: "hubB", URL: mock.srv.URL,
		AccessKey: federationForwardTestAK, AccessKeySecret: federationForwardTestSK,
	}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", nil)
	if err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	_ = conn.Close()

	hdr := mock.lastHeaders()
	if hdr == nil {
		t.Fatalf("mock 未收到转发请求")
	}
	if hdr["hop"] != "1" {
		t.Errorf("转发请求 X-Relay-Hop 应为 1, got %q", hdr["hop"])
	}
	// 下一跳 peer.ID（hubB）必须追加进路径：即使 hubID 未配置（默认），防环路径
	// 检查也自洽生效（评审 #2 修复：不再依赖 node_id 配置）。
	if hdr["path"] != "hubB" {
		t.Errorf("转发请求 X-Relay-Path 应为下一跳 peer ID hubB（hubID 未配置）, got %q", hdr["path"])
	}
	if !strings.HasPrefix(hdr["authorization"], "SproxySig ") {
		t.Errorf("转发请求应带 SproxySig 认证头, got %q", hdr["authorization"])
	}
}

// TestRelayStreamHandler_ForwardPath_Accumulation：中间 hub 转发时 hop/path 逐跳
// 递增——客户端请求带 hop=2 / path=hubA,hubB（已途经两 hub），本 hub 转发到下一跳
// 对端 hubC 时，hop 递增为 3、路径追加下一跳 peer.ID → hubA,hubB,hubC（评审 #3：
// 多跳路径追加逻辑的服务器级覆盖）。
func TestRelayStreamHandler_ForwardPath_Accumulation(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubC", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", map[string]string{
		relayForwardHopHeader:  "2",
		relayForwardPathHeader: "hubA,hubB",
	})
	if err != nil {
		t.Fatalf("转发应成功: %v", err)
	}
	_ = conn.Close()

	hdr := mock.lastHeaders()
	if hdr == nil {
		t.Fatalf("mock 未收到转发请求")
	}
	if hdr["hop"] != "3" {
		t.Errorf("X-Relay-Hop 应递增为 3, got %q", hdr["hop"])
	}
	if hdr["path"] != "hubA,hubB,hubC" {
		t.Errorf("X-Relay-Path 应追加下一跳 hubC → hubA,hubB,hubC, got %q", hdr["path"])
	}
}

// TestRelayStreamHandler_ForwardOversizedPath_400：X-Relay-Path 超限 → 400 拒绝
// （防头部放大，评审 #6）。
func TestRelayStreamHandler_ForwardOversizedPath_400(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	bigPath := strings.Repeat("a,", (maxRelayPathBytes+1024)/2) // 远超 64KiB
	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", map[string]string{relayForwardPathHeader: bigPath})
	if err == nil {
		_ = conn.Close()
		t.Fatalf("超限路径应被拒绝")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("超限路径应 400, got: %v", err)
	}
	if mock.relayHits.Load() != 0 {
		t.Fatalf("超限路径不应触发转发, hits = %d", mock.relayHits.Load())
	}
}

// TestRelayStreamHandler_ForwardUnknownTarget_404：目标非本地且非联邦候选 → 404。
func TestRelayStreamHandler_ForwardUnknownTarget_404(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":""}]`)
	rtA := hub.NewMeshRouteTable()
	hA, tsA := newRelayTestHub(t, rtA)
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}
	hA.SetFederationClient(fcA)

	conn, err := relayConnectRaw(t, tsA.URL, "node-unknown", "1.2.3.4:80", nil)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("未知目标应 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("未知目标应 404, got: %v", err)
	}
	if mock.relayHits.Load() != 0 {
		t.Fatalf("未知目标不应触发转发, hits = %d", mock.relayHits.Load())
	}
}

// TestRelayStreamHandler_ForwardNoFederation_404：未配置联邦转发器时，目标非本地
// 保持 404（与旧行为一致，联邦转发不改变无联邦语义）。
func TestRelayStreamHandler_ForwardNoFederation_404(t *testing.T) {
	rtA := hub.NewMeshRouteTable()
	_, tsA := newRelayTestHub(t, rtA) // 不 SetFederationClient
	conn, err := relayConnectRaw(t, tsA.URL, "node-x", "1.2.3.4:80", nil)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("无联邦转发器时未知目标应 404")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("无联邦转发器应 404, got: %v", err)
	}
}

// TestRelayStreamHandler_ForwardMeshIsolation_404：目标联邦候选在其它 mesh →
// 404（跨 hub 转发不绕过 mesh 隔离）。hub-A 的请求者 mesh 为 "A"，候选 node-x
// 属于 mesh "B" → PeerForNode 不命中 → 404。
func TestRelayStreamHandler_ForwardMeshIsolation_404(t *testing.T) {
	mock := newMockFedPeer(t, `[{"id":"node-x","addr":"10.0.0.9:9000","mesh":"B"}]`)
	rtA := hub.NewMeshRouteTable()
	fcA, _ := hub.NewFederationClient([]hub.FederationPeer{{ID: "hubB", URL: mock.srv.URL}}, 30*time.Second, 5*time.Second, testutil.DiscardLogger())
	t.Cleanup(fcA.Close)
	if err := fcA.SyncAll(context.Background()); err != nil {
		t.Fatalf("SyncAll: %v", err)
	}

	// 直接构造 mesh=A 的请求（绕过 authMiddleware，直接测 handler 的 mesh 校验）。
	h := NewRelayStreamHandler(rtA, testutil.DiscardLogger())
	h.SetFederation(fcA, "hubA")
	req := httptest.NewRequest(http.MethodPost, "/api/relay/stream",
		strings.NewReader(`{"target":"node-x","type":"tcp","addr":"1.2.3.4:80"}`))
	req = req.WithContext(withMesh(req.Context(), "A"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("跨 mesh 联邦目标应 404, got %d (body=%q)", w.Code, w.Body.String())
	}
	if mock.relayHits.Load() != 0 {
		t.Fatalf("跨 mesh 不应触发转发, hits = %d", mock.relayHits.Load())
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

// mockFedPeer 是模拟联邦对端：/api/hub/federation/nodes 返回固定节点表，
// /api/relay/stream 记录请求并返回 relayStatus（200 时 CONNECT 升级 + echo）。
type mockFedPeer struct {
	srv         *httptest.Server
	nodes       string
	relayStatus int // 非 0 且非 200 时 relay stream 返回该状态
	relayHits   atomic.Int32
	last        atomic.Value // map[string]string（hop/path/authorization）
}

func newMockFedPeer(t *testing.T, nodes string) *mockFedPeer {
	t.Helper()
	m := &mockFedPeer{nodes: nodes}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/hub/federation/nodes":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, m.nodes)
		case "/api/relay/stream":
			m.relayHits.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			m.last.Store(map[string]string{
				"hop":           r.Header.Get(relayForwardHopHeader),
				"path":          r.Header.Get(relayForwardPathHeader),
				"authorization": r.Header.Get("Authorization"),
			})
			if m.relayStatus != 0 && m.relayStatus != http.StatusOK {
				http.Error(w, "mock relay error", m.relayStatus)
				return
			}
			hj, ok := w.(http.Hijacker)
			if !ok {
				http.Error(w, "mock no hijacker", http.StatusInternalServerError)
				return
			}
			conn, rw, err := hj.Hijack()
			if err != nil {
				return
			}
			defer conn.Close()
			_, _ = fmt.Fprintf(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
			_ = rw.Flush()
			_, _ = io.Copy(conn, conn) // echo
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockFedPeer) lastHeaders() map[string]string {
	v := m.last.Load()
	if v == nil {
		return nil
	}
	return v.(map[string]string)
}

// isForwardStatus 报告 err 是否为携带指定状态的 forwardStatusError。
func isForwardStatus(err error, status int) bool {
	if err == nil {
		return false
	}
	if fse, ok := err.(*forwardStatusError); ok {
		return fse.status == status
	}
	return false
}
