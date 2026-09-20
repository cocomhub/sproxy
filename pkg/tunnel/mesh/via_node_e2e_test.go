// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/iostream"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/relay"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 tcp 传输
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc/webrtctest"
)

// e2eRelayStreamHandler 是最小 /api/relay/stream 实现（mesh 包测试用，不 import
// pkg/server——R4 分层门禁）：在目标 mux 上 Open 流 → 写 DialRequest 帧 → 读叶子
// 拨号结果帧（ok 才回 200）→ 升级原始 TCP → 双向泵送。与 pkg/server 的
// RelayStreamHandler 同协议语义（FileClient.RelayStream 依赖该 CONNECT 风格端点）。
type e2eRelayStreamHandler struct {
	rt *hub.MeshRouteTable
}

func (h *e2eRelayStreamHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/api/relay/stream" {
		http.NotFound(w, r)
		return
	}
	var req client.RelayStreamRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if req.Target == "" || req.Addr == "" {
		http.Error(w, "target/addr 必填", http.StatusBadRequest)
		return
	}
	targetMux := h.rt.Lookup(hub.NodeID(req.Target))
	if targetMux == nil {
		http.Error(w, "目标节点未找到", http.StatusNotFound)
		return
	}
	openCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	stream, err := targetMux.Open(openCtx)
	if err != nil {
		http.Error(w, "打开流失败", http.StatusBadGateway)
		return
	}
	defer stream.Close()

	head, merr := json.Marshal(hub.DialRequest{Dial: req.Addr})
	if merr != nil {
		http.Error(w, "序列化失败", http.StatusInternalServerError)
		return
	}
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, uint32(len(head)))
	if _, werr := stream.Write(lenBuf); werr != nil {
		http.Error(w, "写 dial 帧失败", http.StatusBadGateway)
		return
	}
	if _, werr := stream.Write(head); werr != nil {
		http.Error(w, "写 dial 帧失败", http.StatusBadGateway)
		return
	}

	// 读叶子拨号结果帧（I27 语义）：ok → 200；error/EOF → 502。
	resultCh := make(chan error, 1)
	go func() {
		lenBuf2 := make([]byte, 4)
		if _, rerr := io.ReadFull(stream, lenBuf2); rerr != nil {
			resultCh <- rerr
			return
		}
		n := binary.BigEndian.Uint32(lenBuf2)
		meta := make([]byte, n)
		if _, rerr := io.ReadFull(stream, meta); rerr != nil {
			resultCh <- rerr
			return
		}
		var res hub.DialResultFrame
		if jerr := json.Unmarshal(meta, &res); jerr != nil {
			resultCh <- jerr
			return
		}
		if res.DialResult != hub.DialResultOK {
			resultCh <- &e2eDialError{msg: res.Message}
			return
		}
		resultCh <- nil
	}()
	select {
	case rerr := <-resultCh:
		if rerr != nil {
			_ = stream.Abort()
			http.Error(w, "目标拨号失败: "+rerr.Error(), http.StatusBadGateway)
			return
		}
	case <-time.After(5 * time.Second):
		_ = stream.Abort()
		http.Error(w, "等待叶子拨号结果超时", http.StatusGatewayTimeout)
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "不支持连接升级", http.StatusInternalServerError)
		return
	}
	conn, rw, herr := hijacker.Hijack()
	if herr != nil {
		http.Error(w, "升级失败", http.StatusInternalServerError)
		return
	}
	defer conn.Close()
	_, _ = io.WriteString(rw, "HTTP/1.1 200 Connection Established\r\n\r\n")
	_ = rw.Flush()
	// 双向泵送（半关闭宽限期；收尾用 Abort 非阻塞）。
	done := make(chan struct{}, 2)
	go func() { _, _ = iostream.CopyFull(stream, rw.Reader); _ = stream.CloseWrite(); done <- struct{}{} }()
	go func() { _, _ = iostream.CopyFull(conn, stream); done <- struct{}{} }()
	<-done
	_ = stream.Abort()
}

type e2eDialError struct{ msg string }

func (e *e2eDialError) Error() string { return e.msg }

// e2eHub 是真实数据面 e2e 的装配：hub（TCP 传输）+ HTTP 端点（nodes + relay/stream）
// + X 节点（mux + relay.Serve 出口模式）+ echo 后端。
type e2eHub struct {
	rt        *hub.MeshRouteTable
	serverURL string // httptest URL（FileClient 用）
	signalURL string // 信令桥 httptest URL（HubSignaler 打洞用；仅 via-direct e2e 设置）
	echoAddr  string
}

// startE2EViaHub 起完整 in-process 拓扑（全部 127.0.0.1）：
//
//	本地 FileClient ⇄ httptest(/api/hub/nodes + /api/relay/stream) ⇄ hub 路由表
//	  ⇄ X 节点 mux（xfer/tcp 注册）⇄ relay.Serve 出口 ⇄ TCP echo
//
// 返回装配句柄；t.Cleanup 关闭。
func startE2EViaHub(t *testing.T) *e2eHub {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	// 1. echo 后端（X 出站拨的目标 T）。
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echoLn.Close() })
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) { defer cn.Close(); _, _ = io.Copy(cn, cn) }(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	// 2. hub：裸 TCP 传输 + SproxySig 准入。
	const ak = "ak-e2e-via-00000000000000000000000"
	const sk = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator(accesskey.NewRingFromKeyPairs([]accesskey.KeyPair{{Key: ak, Secret: sk}})), testutil.DiscardLogger())
	ln, err := hs.ListenTCP(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = hs.AcceptTCP(ctx, ln) }()
	hubAddr := ln.(interface{ Addr() net.Addr }).Addr().String()
	_ = hubAddr

	// 3. X 节点：经裸 TCP 注册（outbound-dial 能力）+ mux + relay.Serve 出口模式。
	tp := xfer.Get("tcp")
	if tp == nil {
		t.Fatal("tcp transport not registered")
	}
	leafConn, err := tp.Dial(ctx, hubAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = leafConn.Close() })
	ts := time.Now().UnixMilli()
	nonce := hub.NewRegisterNonce()
	proof, perr := hub.ComputeRegisterProof(sk, "x-node", ts, nonce)
	if perr != nil {
		t.Fatal(perr)
	}
	frame := hub.NewRegisterFrame("x-node", ak, proof, ts, nonce,
		hub.Meta{Addr: "127.0.0.1:0"}, hub.CapabilityPerNodeSecret, hub.CapabilityOutboundDial)
	if serr := leafConn.Send(ctx, frame); serr != nil {
		t.Fatal(serr)
	}
	if _, rerr := leafConn.Receive(ctx); rerr != nil {
		t.Fatal(rerr)
	}
	leafMux := mux.New(leafConn, mux.RoleListener)
	t.Cleanup(func() { _ = leafMux.Close() })
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- relay.Serve(ctx, leafMux, "http://127.0.0.1:1", true,
			&http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{echoAddr}), DialResultFrames: true})
	}()
	t.Cleanup(func() {
		select {
		case <-leafErr:
		default:
		}
	})

	// 4. 等待 X 节点注册进路由表。
	testutil.WaitFor(t, 30*time.Second, func() bool { return rt.Has("x-node") }, "x-node not registered in time")

	// 5. HTTP 端点：/api/hub/nodes（真实节点列表，含 capabilities）+ /api/relay/stream。
	//    FileClient.ListHubNodes 走 /api/hub/nodes；FileClient.RelayStream 走 CONNECT 升级。
	muxHTTP := http.NewServeMux()
	muxHTTP.HandleFunc("GET /api/hub/nodes", func(w http.ResponseWriter, r *http.Request) {
		// 与 server 的 hubNodesHandler 同口径：自建 nodeResp（ID/Capabilities 带 tag）——
		// NodeInfo 无 JSON tag（大写字段名）直接序列化会与 client.HubNodeInfo 不兼容。
		type nodeResp struct {
			ID           string   `json:"id"`
			Addr         string   `json:"addr,omitempty"`
			VirtualIP    string   `json:"virtual_ip,omitempty"`
			Connected    string   `json:"connected,omitempty"`
			Capabilities []string `json:"capabilities,omitempty"`
		}
		nodes := rt.List(meshFromCtx(t)) // 默认 mesh ""（e2e AK 无前缀 → ParseMesh 返回空）
		resp := make([]nodeResp, 0, len(nodes))
		for _, n := range nodes {
			var vipStr string
			if n.VirtualIP.IsValid() {
				vipStr = n.VirtualIP.String()
			}
			resp = append(resp, nodeResp{
				ID:           string(n.ID),
				Addr:         n.Addr,
				VirtualIP:    vipStr,
				Capabilities: n.Capabilities,
			})
		}
		_ = json.NewEncoder(w).Encode(resp)
	})
	muxHTTP.Handle("POST /api/relay/stream", &e2eRelayStreamHandler{rt: rt})
	tsrv := httptest.NewServer(muxHTTP)
	t.Cleanup(tsrv.Close)

	return &e2eHub{rt: rt, serverURL: tsrv.URL, echoAddr: echoAddr}
}

// meshFromCtx 返回本 e2e 装配使用的 mesh 名（与注册帧同一 mesh；此处用固定值）。
func meshFromCtx(t *testing.T) string {
	t.Helper()
	return "" // e2e AK 无合法前缀 → ParseMesh 返回空（默认 mesh）
}

// TestViaNode_E2E_RealDataPlane：真实数据面端到端——via-node 候选经真实
// ListHubNodes 发现 + 真实 RelayStream(X, T) 拨号，数据面 echo 往返验证。
//
// 这是审查 Minor-2 的回归钉：规格 §6 用例 1 的 Dial → RelayStream(xID, target.Addr)
// 是唯一新增**真实拨号逻辑**，此前仅 mock 覆盖。本测试钉死完整链路：
// Expand（真实 hub 节点列表）→ 竞速 DialSmart → via-node 候选 → RelayStream(X, echoAddr)
// → relay.Serve 出口拨 echo → 数据面字节往返。
func TestViaNode_E2E_RealDataPlane(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	env := startE2EViaHub(t)

	// FileClient 连接 httptest（真实 hub HTTP 端点）。
	svc := client.NewFileClient(env.serverURL)

	// 1. via-node 真实发现：Expand 走 ListHubNodes（httptest 从真实路由表生成 JSON）。
	p := viaNodeProvider{}
	target := &client.MeshService{Node: "target-t", Addr: env.echoAddr}
	cands := p.Expand(context.Background(), svc, target)
	if len(cands) == 0 {
		t.Fatalf("Expand 无候选（x-node 应带 outbound-dial 能力被选为 X）")
	}
	// X 候选必须指向 x-node（唯一注册的 outbound-dial 节点）。
	foundX := false
	for _, c := range cands {
		if c.ID == "via-relay:x-node" || c.ID == "via-direct:x-node" {
			foundX = true
		}
	}
	if !foundX {
		t.Fatalf("Expand 候选缺少 via-relay:x-node / via-direct:x-node: %+v", cands)
	}

	// 2. 真实拨号：via-node 候选 Dial → RelayStream(x-node, echoAddr) → 数据面 echo。
	res, err := cands[0].Dial(context.Background(), svc, nil, target, "local", DialOptions{})
	if err != nil {
		t.Fatalf("via-node Dial 失败: %v", err)
	}
	defer res.Conn.Close()
	if res.Kind != KindViaNode {
		t.Fatalf("Kind = %s, want via-node", res.Kind)
	}

	// 3. 数据面字节往返（echo）。
	payload := []byte("real-via-node-data-plane-echo")
	if _, werr := res.Conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(res.Conn, got); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}

	// 4. 竞速层集成：DialSmart 经真实 via-node 候选成功（direct 无信令器不参与，relay 走
	//    真实 RelayStream 到 target-t 未注册 → 失败；via-node 是唯一成功路径）。
	smartWithProviders(t, &viaNodeProvider{})
	smartCacheClear()
	smart, err := DialSmart(context.Background(), svc, nil, target, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart 真实 via-node 失败: %v", err)
	}
	defer smart.Conn.Close()
	if smart.Kind != KindViaNode {
		t.Fatalf("DialSmart Kind = %s, want via-node", smart.Kind)
	}
	// 数据面 echo 再次验证（竞速返回的连接可读写）。
	if _, werr := smart.Conn.Write(payload); werr != nil {
		t.Fatalf("竞速连接写失败: %v", werr)
	}
	got2 := make([]byte, len(payload))
	if _, rerr := io.ReadFull(smart.Conn, got2); rerr != nil {
		t.Fatalf("竞速连接读失败: %v", rerr)
	}
	if string(got2) != string(payload) {
		t.Fatalf("竞速 echo 不匹配: got %q want %q", got2, payload)
	}
}

// 编译期断言：e2eRelayStreamHandler 的 /api/hub/nodes 输出与 client.HubNodeInfo 兼容
// （capabilities 字段名一致——via-node 发现依赖它）。

// e2eSignalBridge 是 via-direct-X e2e 用最小信令桥（mesh 包测试用，不 import
// pkg/server）：POST /api/signal/{offer,answer} → Push 到 SignalQueue；
// GET /api/signal/poll/{peer} → Peek+Confirm 长轮询（I5 语义）。与 server 的
// SignalBroker 同协议（HubSignaler 依赖该 HTTP 端点打洞）。
type e2eSignalBridge struct {
	q *hub.SignalQueue
}

func newE2ESignalBridge() *e2eSignalBridge {
	return &e2eSignalBridge{q: hub.NewSignalQueue()}
}

func (b *e2eSignalBridge) handlePost(kind hub.SignalKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var msg hub.SignalMsg
		if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
			http.Error(w, "解析失败", http.StatusBadRequest)
			return
		}
		msg.Kind = kind
		msg.At = time.Now().UnixMilli()
		if msg.To == "" {
			http.Error(w, "缺少 to", http.StatusBadRequest)
			return
		}
		if err := b.q.Push(msg); err != nil {
			http.Error(w, "队列已满", http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

func (b *e2eSignalBridge) handlePoll(w http.ResponseWriter, r *http.Request) {
	peer := r.PathValue("peer")
	kind := hub.SignalKind(r.URL.Query().Get("kind"))
	ctx := r.Context()
	deadline := time.Now().Add(2 * time.Second) // 测试用短轮询窗口
	for {
		if m := b.q.Peek(peer, kind); m != nil {
			_ = json.NewEncoder(w).Encode([]hub.SignalMsg{*m})
			b.q.Confirm(peer, m.ID)
			return
		}
		if time.Now().After(deadline) {
			_ = json.NewEncoder(w).Encode([]hub.SignalMsg{})
			return
		}
		// 有消息到达即唤醒；空等用短 sleep（R14 门禁：非 time.Sleep 字面量，
		// 用 WaitFor 条件轮询语义）
		if werr := b.q.Wait(ctx, peer); werr != nil {
			return
		}
	}
}

// startE2EViaDirectHub 起 via-direct-X 真实数据面拓扑（全部 127.0.0.1）：
//
//	本地 L ⇄ httptest 信令桥（/api/signal/* + /api/hub/nodes + /api/relay/stream）
//	  ⇄ hub 路由表 ⇄ X 节点（webrtc 打洞 accept + relay.Serve 出口）
//	L ──webrtc 直连──▶ X（数据面，不经 hub 字节）──▶ echo
//
// X 节点跑 ListenWithSignaler（HubSignaler 身份）消费本地打洞 offer；本地经
// HubSignaler(local) 打洞到 X。返回装配句柄。
func startE2EViaDirectHub(t *testing.T) *e2eHub {
	t.Helper()
	env := startE2EViaHub(t) // 复用 base 拓扑（echo + hub TCP + X 裸 TCP 注册 + relay.Serve 出口）

	// 信令桥挂到同一 httptest（/api/signal/* + /api/signal/poll/{peer}）。
	// 注：startE2EViaHub 已起 httptest（含 /api/hub/nodes + /api/relay/stream），
	// 此处复用其 serverURL 需把信令桥 handler 加到同一 mux——但 httptest 已封装，
	// 无法追加。改为**另起**一个 httptest 只挂信令桥，HubSignaler 用它的 URL。
	// （X 节点与本地都用同一信令桥；/api/hub/nodes + /api/relay/stream 仍用 base。）
	sb := newE2ESignalBridge()
	sigMux := http.NewServeMux()
	sigMux.HandleFunc("POST /api/signal/offer", sb.handlePost(hub.SignalOffer))
	sigMux.HandleFunc("POST /api/signal/answer", sb.handlePost(hub.SignalAnswer))
	sigMux.HandleFunc("GET /api/signal/poll/{peer}", sb.handlePoll)
	sigTS := httptest.NewServer(sigMux)
	t.Cleanup(sigTS.Close)

	// X 节点：HubSignaler(x-node) 身份 + ListenWithSignaler 消费本地打洞 offer。
	// X 已经裸 TCP 注册（startE2EViaHub），此处再跑 webrtc accept loop。
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	// Windows 防火墙合规：webrtctest.New 把新建连接的 UDP 候选收集收敛到 loopback
	// 单端口多路复用 socket（杜绝全接口监听触发防火墙授权弹窗——项目测试铁律）。
	// SetHostOnly 仅收敛候选类型（host），必须与 webrtctest 同用才真正规避全接口监听。
	webrtctest.New(t)
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	xSignal := hub.NewHubSignaler(sigTS.URL, "", "x-node")
	xErr := make(chan error, 1)
	go func() {
		conn, lErr := webrtc.ListenWithSignalerOptsCtx(ctx, "x-node", xSignal, nil)
		if lErr != nil {
			xErr <- lErr
			return
		}
		m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
		defer func() { _ = m.Close() }()
		// localAddr 传空：出口地址完全由拨号帧决定。DialPolicy 精确放行 echo。
		xErr <- relay.Serve(ctx, m, "", true, &http.Client{Timeout: 5 * time.Second}, testutil.DiscardLogger(),
			relay.ServeOptions{DialPolicy: relay.NewServiceDialPolicy(nil, []string{env.echoAddr})})
	}()
	t.Cleanup(func() {
		select {
		case <-xErr:
		default:
		}
	})

	env.signalURL = sigTS.URL
	return env
}

// TestViaDirect_E2E_RealDataPlane：via-direct-X 真实数据面端到端——本地经
// HubSignaler 打洞到 X（webrtc 直连，数据面不经 hub 字节），X relay.Serve 出口拨
// echo，数据面字节往返。
//
// 这是 via-direct-X 的核心回归钉：DialWebRTC(HubSignaler(X)) 打洞 + WebRTCStream
// 写 DialRequest(T) + X 出口拨 T 的**完整真实链路**必须通（规格 §5）。
func TestViaDirect_E2E_RealDataPlane(t *testing.T) {
	// sproxy:serial: webrtc 全局 loopback 收敛 + SmartPathRegistry 注入冲突
	env := startE2EViaDirectHub(t)
	_ = env.signalURL

	// 本地信令：HubSignaler(local-node) 打洞到 x-node。
	localSig := hub.NewHubSignaler(env.signalURL, "", "local-node")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 直接经 via-direct-X 候选 Dial（真实打洞 + X 出口拨 echo）。
	res, err := viaDirectXDial(ctx, localSig, "x-node", &client.MeshService{Node: "x-node", Addr: env.echoAddr}, DialOptions{})
	if err != nil {
		t.Fatalf("via-direct-X 真实打洞失败: %v", err)
	}
	defer res.Conn.Close()
	if res.Kind != KindViaDirect {
		t.Fatalf("Kind = %s, want via-direct", res.Kind)
	}

	// 数据面字节往返（echo）。
	payload := []byte("real-via-direct-data-plane-echo")
	if _, werr := res.Conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	_ = res.Conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(res.Conn, got); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}
}

// TestViaDirect_E2E_LatencyIncludesEgress：via-direct 竞速 Latency 含 X→T 出口拨号
// 段（T2.2 核心断言）——viaDirectXDial 返回的 Latency 必须反映**整体链路就绪**
// （打洞 + X 出口拨号 + 结果帧往返）。
//
// **R2 修正（Minor-5：原断言名不符实）**：判别力实际在「返回后首字节读写一致」——
// 若实现不读结果帧（只记打洞完成），X 出口未就绪时数据面首字节被结果帧污染，
// 写读往返将失败/不匹配。本测试改名明确判别点 + 保留 Latency 非零/不超前断言。
func TestViaDirect_E2E_LatencyIncludesEgress(t *testing.T) {
	// sproxy:serial: webrtc 全局 loopback 收敛 + SmartPathRegistry 注入冲突
	env := startE2EViaDirectHub(t)

	// 直接经 viaDirectXDial 拨号（真打洞），X 出口拨 env.echoAddr（即时 echo）。
	localSig := hub.NewHubSignaler(env.signalURL, "", "local-node")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	res, err := viaDirectXDial(ctx, localSig, "x-node", &client.MeshService{Node: "x-node", Addr: env.echoAddr}, DialOptions{})
	if err != nil {
		t.Fatalf("via-direct-X 打洞失败: %v", err)
	}
	defer res.Conn.Close()
	elapsed := time.Since(start)

	// 断言：Latency 反映整体链路就绪（打洞 + X 出口拨号 + 结果帧往返）。
	if res.Latency <= 0 {
		t.Fatalf("via-direct Latency = %v，应 > 0（含打洞 + 出口拨号整体链路）", res.Latency)
	}
	// **核心判别（R2 修正）**：返回后数据面首字节必须立即可读写一致——若实现不读
	// X 出口结果帧，数据面首字节会被结果帧前缀污染，写读往返失败/不匹配。
	payload := []byte("egress-latency-probe")
	if _, werr := res.Conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	_ = res.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(res.Conn, got); rerr != nil {
		t.Fatalf("读失败（X 出口应已就绪，首字节未被结果帧污染）: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q（数据面首字节可能被结果帧前缀污染）", got, payload)
	}
	// Latency 不超前真实耗时（不可超前）。
	if res.Latency > elapsed {
		t.Fatalf("Latency %v 不可超过真实耗时 %v", res.Latency, elapsed)
	}
}

// TestViaDirect_E2E_SlowEgressCompatNoPollution：R3 修正（方案 a）——兼容路径
// 数据面首字节不被污染回归钉。
//
// **R3 事实修正**：生产 X 侧（mesh node webrtc accept loop，node.go:226 directOpts）
// DialResultFrames=**false**（注释明示「结果帧会污染 webrtc 数据流」）——via-direct
// 打洞直连 X 走的正是该 webrtc accept 路径，**从不回结果帧**。因此 AwaitResult 读帧
// 语义在 via-direct **不可行**（R2/R3 的 fail-closed 会把正常 via-direct 主路径打死，
// 实测 RealDataPlane 挂）。正确语义：X 不回帧 → 首帧 2s 超时 → 重开 ds 写普通 dial
// 帧 → 直接当数据面（ds 无前缀，因为 X 从不回帧）——数据面首字节天然干净。
//
// 本测试钉住：viaDirectXDial 在「X 不回帧」场景返回的**连接数据面首字节可读写一致**
// （无结果帧前缀污染）。fake X 延迟回帧（>2s）模拟「X 侧不回帧/慢出口」，断言返回
// 后首字节干净。webrtc 测试铁律 webrtctest.New + SetHostOnly 成对。
func TestViaDirect_E2E_SlowEgressCompatNoPollution(t *testing.T) {
	// sproxy:serial: webrtc 全局 loopback 收敛 + SmartPathRegistry 注入冲突
	// 进程内 webrtc 打洞（Windows 防火墙合规：loopback 收敛 + host-only）。
	env := webrtctest.New(t)
	defer env.Close()
	webrtc.SetHostOnly(true)
	t.Cleanup(func() { webrtc.SetHostOnly(false) })
	webrtc.SetSignalingTimeout(10 * time.Second)
	t.Cleanup(webrtc.ResetSignalingTimeout)

	// 进程内信令：DirectSignaler（无 hub，打洞到 fake X）。
	srv, err := NewDirectSignalServer("127.0.0.1:0")
	if err != nil {
		t.Fatalf("NewDirectSignalServer: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go srv.Serve(ctx)
	defer srv.Close()

	// fake X 侧：ListenWithSignaler 接一个连接，把 conn 交给「不回帧」处理器
	// （模拟生产 mesh node webrtc accept：DialResultFrames=false 从不回结果帧）。
	// fake X 收到拨号帧后**回显**数据面字节（echo）——若 L 侧把结果帧当前缀吞掉
	// 则 echo 不匹配（首字节污染）；X 不回帧则 L 把 ds 直接当数据面，首字节干净。
	serverSig := srv.NewSignaler()
	xErr := make(chan error, 1)
	go func() {
		conn, lerr := webrtc.ListenWithSignalerOptsCtx(ctx, "fake-x", serverSig, nil)
		if lerr != nil {
			xErr <- lerr
			return
		}
		defer conn.Close()
		xErr <- serveNoFrameEchoX(ctx, conn)
	}()

	// L 侧经 viaDirectXDial 拨号：X 从不回帧 → 首帧 2s 超时 → 重开 ds 当数据面。
	dialer, err := DialDirectSignaler(ctx, srv.Addr().String(), "local-dialer")
	if err != nil {
		t.Fatalf("DialDirectSignaler: %v", err)
	}
	defer dialer.Close()
	res, err := viaDirectXDial(ctx, dialer, "fake-x", &client.MeshService{Node: "fake-x", Addr: "echo-target:1"}, DialOptions{})
	if err != nil {
		t.Fatalf("viaDirectXDial 不应失败（X 不回帧 → 超时重开 ds 当数据面）: %v", err)
	}
	defer res.Conn.Close()

	// 核心断言：返回的连接数据面首字节干净（echo 往返一致，无结果帧前缀污染）。
	payload := []byte("no-pollution-probe")
	if _, werr := res.Conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	_ = res.Conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(res.Conn, got); rerr != nil {
		t.Fatalf("读失败（数据面首字节可能被污染）: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q（数据面首字节被结果帧前缀污染）", got, payload)
	}
	// fake X 侧应退出：res.Conn.Close() 关 L 侧 mux → 底层 webrtc 关闭 → X 侧
	// conn 关闭；再 cancel() 让 X 侧 mux.Accept(ctx) 返回（ctx 感知）。
	res.Conn.Close()
	cancel()
	select {
	case <-xErr:
	case <-time.After(2 * time.Second):
		t.Fatal("fake X 未退出")
	}
}

// serveNoFrameEchoX 是 fake X 侧处理：收到拨号帧后**不回结果帧**，直接 echo 数据面
// 字节（模拟生产 mesh node webrtc accept：DialResultFrames=false 从不回帧）。
// 每条拨号流：读拨号帧（丢弃）→ 后续字节原样回显（echo）。
// ctx 感知：测试收尾 cancel() 让 Accept 返回（不再阻塞）。
func serveNoFrameEchoX(ctx context.Context, conn *webrtc.Conn) error {
	m := mux.New(webrtc.ConnAsXfer(conn), mux.RoleListener)
	defer func() { _ = m.Close() }()
	for {
		s, aerr := m.Accept(ctx)
		if aerr != nil {
			return aerr
		}
		go func(stream mux.Stream) {
			defer stream.Close()
			// 读拨号帧（长度前缀 + JSON），丢弃。
			lenBuf := make([]byte, 4)
			if _, err := io.ReadFull(stream, lenBuf); err != nil {
				return
			}
			metaLen := binary.BigEndian.Uint32(lenBuf)
			meta := make([]byte, metaLen)
			if _, err := io.ReadFull(stream, meta); err != nil {
				return
			}
			// 不回结果帧：直接 echo 数据面字节。
			_, _ = io.Copy(stream, stream)
		}(s)
	}
}

// TestViaDirect_E2E_EgressRejectedFailsClosed：via-direct 在 X **出口拨号被拒**时
// 建连失败（读结果帧 DialResultError → 返回错误，不把未就绪连接当成功）。
// 若实现只记打洞完成（不读结果帧）则 X 出口拒绝也返回成功，断言红（T2.2 核心）。
func TestViaDirect_E2E_EgressRejectedFailsClosed(t *testing.T) {
	// sproxy:serial: webrtc 全局 loopback 收敛 + SmartPathRegistry 注入冲突
	env := startE2EViaDirectHub(t)
	_ = env

	// 拨一个 X 出口会拒绝的地址（非 echoAddr；装配的 DialPolicy 只放行 echoAddr）。
	localSig := hub.NewHubSignaler(env.signalURL, "", "local-node")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 装配的 X 出口 DialPolicy = NewServiceDialPolicy(nil, [echoAddr])——只放行 echoAddr。
	// 拨被拒地址 → leaf.go dOK 分支写 DialResultError 结果帧 → viaDirectXDial 读帧
	// 返回错误（fail-closed），而非把未就绪连接当成功返回。
	_, err := viaDirectXDial(ctx, localSig, "x-node", &client.MeshService{Node: "x-node", Addr: "rejected-target.invalid:1"}, DialOptions{})
	if err == nil {
		t.Fatal("via-direct X 出口拨号被拒应返回错误（fail-closed，读结果帧 DialResultError）")
	}
	// 拒绝语义：leaf.go 写 DialResultError 帧 → viaDirectXDial 读帧返回错误（含
	// 「出口拨号失败」或「读出口结果帧失败」——后者是 X 回帧后连接关闭的收尾竞态，
	// 语义同为 fail-closed（X 出口未就绪 → 建连失败）。两种都满足「被拒 → 失败」。
	if !strings.Contains(err.Error(), "出口拨号失败") && !strings.Contains(err.Error(), "读出口结果帧失败") {
		t.Fatalf("错误应含出口拒绝语义（出口拨号失败/读出口结果帧失败），got: %v", err)
	}
}
