// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

// testSignalSecret 返回测试节点的 per-node secret（I1）。
// 与注册时写入 RouteTable 的 Secret 保持一致。
func testSignalSecret(id string) string { return "test-secret-" + id }

// signalReq 构造携带 node-id 与 per-node secret 头的信令请求（I1）。
// 所有成功路径用例都走此辅助，确保身份校验通过。
func signalReq(method, target, nodeID, body string) *http.Request {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(signalNodeHeader, nodeID)
	req.Header.Set(signalNodeSecretHeader, testSignalSecret(nodeID))
	return req
}

func signalTestMux(b *SignalBroker) *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/signal/offer", func(w http.ResponseWriter, r *http.Request) {
		b.handleSignalPost(w, r, hub.SignalOffer)
	})
	m.HandleFunc("POST /api/signal/answer", func(w http.ResponseWriter, r *http.Request) {
		b.handleSignalPost(w, r, hub.SignalAnswer)
	})
	m.HandleFunc("POST /api/signal/candidate", func(w http.ResponseWriter, r *http.Request) {
		b.handleSignalPost(w, r, hub.SignalCandidate)
	})
	m.HandleFunc("GET /api/signal/poll/{peer}", b.handleSignalPoll)
	return m
}

// newSignalTestBroker 构造带已注册节点（含 per-node Secret）的 SignalBroker。
// 用 AddWithInfoAndServices 预置 Secret（I1）——仅 Add 不写 info 表，
// LookupInfo 会判定节点未注册。
func newSignalTestBroker(t *testing.T) *SignalBroker {
	t.Helper()
	rt := hub.NewMeshRouteTable()
	for _, id := range []string{"peer-a", "peer-b"} {
		a, _ := xfertest.Pipe()
		m := mux.New(a, mux.RoleDialer)
		t.Cleanup(func() { _ = m.Close() })
		rt.Add("", hub.NodeInfo{ID: hub.NodeID(id), Mux: m, Secret: testSignalSecret(id)}, nil)
	}
	return NewSignalBroker(rt)
}

// TestSignalBroker_FlushSignalPreservesNodes：信令持久化必须同时保留节点注册与
// 收件箱——FlushSignal 不能只写 messages 而丢 nodes（否则任何一次信令往来都会
// 让已持久化的节点注册从文件里消失，重启后节点全丢）。
func TestSignalBroker_FlushSignalPreservesNodes(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.Add("", hub.NodeInfo{ID: "peer-a", Mux: m, Secret: "sec-a"}, nil)
	rt.Add("", hub.NodeInfo{ID: "peer-b", Secret: "sec-b"}, nil) // 离线节点（Mux nil）

	b := NewSignalBroker(rt)
	path := filepath.Join(t.TempDir(), "hub.json")
	p := hub.NewPersister(path)
	b.SetPersister(p)

	// 先落盘节点注册（模拟节点注册触发的持久化）。
	if err := p.FlushFn(func() *hub.Snapshot { return hub.SnapshotRouteTable(rt) }); err != nil {
		t.Fatalf("FlushFn 应落盘无错，got %v", err)
	}

	// 投递一条信令并 FlushSignal（模拟 handleSignalPost 的持久化路径）。
	if err := b.queue.Push(hub.SignalMsg{Kind: hub.SignalOffer, From: "peer-a", To: "peer-b", SDP: "v=0"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if err := b.FlushSignal(p); err != nil {
		t.Fatalf("FlushSignal 应落盘无错，got %v", err)
	}

	// 重新加载：节点注册与信令收件箱必须都保留。
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(snap.Nodes) != 2 {
		t.Fatalf("FlushSignal 后快照节点 = %d, want 2（信令持久化不能丢节点注册）", len(snap.Nodes))
	}
	if len(snap.Messages) != 1 || len(snap.Messages[0].Msgs) != 1 || snap.Messages[0].Msgs[0].To != "peer-b" {
		t.Fatalf("FlushSignal 后快照消息 = %+v, want 1 条 to peer-b", snap.Messages)
	}
}

func TestSignalBroker_PostAndPoll(t *testing.T) {
	b := newSignalTestBroker(t)
	// I63：空 poll 长轮询用短超时，避免默认 25s 阻塞拖慢测试（-race 下翻倍）。
	b.pollTimeout = 100 * time.Millisecond
	mux := signalTestMux(b)

	// POST offer 给 peer-b（调用方 peer-a）
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"from":"peer-a","to":"peer-b","sdp":"offer-sdp"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	// poll peer-b 应拿到 offer（调用方 peer-b）
	pollReq := signalReq(http.MethodGet, "/api/signal/poll/peer-b", "peer-b", "")
	pw := httptest.NewRecorder()
	mux.ServeHTTP(pw, pollReq)
	if pw.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", pw.Code)
	}
	var msgs []hub.SignalMsg
	if err := json.NewDecoder(pw.Body).Decode(&msgs); err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].Kind != hub.SignalOffer || msgs[0].SDP != "offer-sdp" {
		t.Fatalf("unexpected poll result: %+v", msgs)
	}

	// 再 poll 应为空（已被取走）
	pw2 := httptest.NewRecorder()
	pollReq2 := signalReq(http.MethodGet, "/api/signal/poll/peer-b", "peer-b", "")
	mux.ServeHTTP(pw2, pollReq2)
	var msgs2 []hub.SignalMsg
	if err := json.NewDecoder(pw2.Body).Decode(&msgs2); err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 0 {
		t.Fatalf("expected empty second poll, got %+v", msgs2)
	}
}

func TestSignalBroker_IdentityBinding(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)

	// 1. 缺 X-Node-ID 头 → 400
	req := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"peer-a","to":"peer-b","sdp":"x"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for missing X-Node-ID, got %d", w.Code)
	}

	// 2. body 里伪造 From 无效：服务端从 X-Node-ID 派生 From（body 注入面被消除）。
	//    header=peer-b（声称自己是 peer-b），body 写 from=peer-a、to=peer-a
	//    → From 被覆盖为 peer-b（忽略 body 的 from=peer-a），投递到 peer-a 成功。
	req2 := signalReq(http.MethodPost, "/api/signal/offer", "peer-b", `{"from":"peer-a","to":"peer-a","sdp":"x"}`)
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (From derived from header, body ignored), got %d", w2.Code)
	}
	// 验证 poll peer-a 收到 From==peer-b（而非 body 里的 peer-a）
	pollVerify := signalReq(http.MethodGet, "/api/signal/poll/peer-a", "peer-a", "")
	pwv := httptest.NewRecorder()
	mux.ServeHTTP(pwv, pollVerify)
	var vmsgs []hub.SignalMsg
	_ = json.NewDecoder(pwv.Body).Decode(&vmsgs)
	if len(vmsgs) != 1 || vmsgs[0].From != "peer-b" {
		t.Fatalf("expected From derived as peer-b, got %+v", vmsgs)
	}

	// 3. poll 非自己收件箱 → 403（窃听被拒）
	pollReq := signalReq(http.MethodGet, "/api/signal/poll/peer-a", "peer-b", "") // 声称自己是 peer-b 却轮询 peer-a
	pw := httptest.NewRecorder()
	mux.ServeHTTP(pw, pollReq)
	if pw.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for poll mismatch, got %d", pw.Code)
	}

	// 4. X-Node-ID 未注册 → 400
	req3 := signalReq(http.MethodPost, "/api/signal/offer", "ghost", `{"from":"ghost","to":"peer-b","sdp":"x"}`)
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered node, got %d", w3.Code)
	}
}

func TestSignalBroker_NodeSecret(t *testing.T) {
	b := newSignalTestBroker(t)
	sm := signalTestMux(b) // 命名 sm 避免遮蔽 mux 包（case 4 需用 mux.New）

	// 1. 正确 secret → 202
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-b","sdp":"x"}`)
	w := httptest.NewRecorder()
	sm.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 with correct secret, got %d", w.Code)
	}

	// 2. 错误 secret → 403
	reqErr := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"to":"peer-b","sdp":"x"}`))
	reqErr.Header.Set(signalNodeHeader, "peer-a")
	reqErr.Header.Set(signalNodeSecretHeader, "wrong-secret")
	wErr := httptest.NewRecorder()
	sm.ServeHTTP(wErr, reqErr)
	if wErr.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for wrong secret, got %d", wErr.Code)
	}

	// 3. 缺 secret 头 → 403
	reqMiss := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"to":"peer-b","sdp":"x"}`))
	reqMiss.Header.Set(signalNodeHeader, "peer-a")
	wMiss := httptest.NewRecorder()
	sm.ServeHTTP(wMiss, reqMiss)
	if wMiss.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for missing secret, got %d", wMiss.Code)
	}

	// 4. 已注册但 Secret==""（未声明 per-node-secret 能力）→ 403 fail-closed
	rt := hub.NewMeshRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.Add("", hub.NodeInfo{ID: "nonsecret", Mux: m}, nil) // 不设 Secret
	b2 := NewSignalBroker(rt)
	mux2 := signalTestMux(b2)
	reqEmpty := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"to":"peer-b","sdp":"x"}`))
	reqEmpty.Header.Set(signalNodeHeader, "nonsecret")
	wEmpty := httptest.NewRecorder()
	mux2.ServeHTTP(wEmpty, reqEmpty)
	if wEmpty.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for empty-secret node, got %d", wEmpty.Code)
	}
}

func TestSignalBroker_BadInput(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)
	// I64：用已注册节点 + 正确 secret，确保「缺少 to」分支真正被命中
	// （之前用空路由表，400 来自「节点未注册」而非被测分支）。
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"sdp":"x"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	// 坏 JSON：同样用已注册节点 + 正确 secret，命中「JSON 解析失败」分支
	req2 := signalReq(http.MethodPost, "/api/signal/answer", "peer-a", "{bad")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w2.Code)
	}
}

func TestSignalBroker_BodyTooLarge(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)
	// 超大 body（超过 maxSignalBodyBytes 8KB）→ 413（S41：MaxBytesError 分类）
	big := `{"from":"peer-a","to":"peer-b","sdp":"` + strings.Repeat("x", maxSignalBodyBytes+1) + `"}`
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", big)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for oversized body, got %d", w.Code)
	}
}

func TestSignalBroker_SelfSendRejected(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)
	// 给自己发信令（from == to）→ 400
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-a","sdp":"x"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for self-send, got %d", w.Code)
	}
}

func TestSignalBroker_UnregisteredPeer(t *testing.T) {
	b := newSignalTestBroker(t) // 只有 peer-a / peer-b 注册
	mux := signalTestMux(b)
	// poll 未注册 peer → 400（身份校验：ghost 未注册）
	req := signalReq(http.MethodGet, "/api/signal/poll/ghost", "ghost", "")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered peer, got %d", w.Code)
	}
}

func TestSignalBroker_QueueFull(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)

	// 填满 per-sender 配额：同一 sender（peer-a）到 peer-b 的未消费消息达到上限
	// （hub.maxSignalPerSender = 32）后，Push 返回 ErrSignalPerSenderCap → POST 回 429（I12）。
	const perSenderCap = 32
	for i := range perSenderCap {
		req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-b","sdp":"x"}`)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("expected 202 for message %d, got %d: %s", i, w.Code, w.Body.String())
		}
	}

	// 下一次 POST 应 429
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-b","sdp":"x"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 for overflow, got %d: %s", w.Code, w.Body.String())
	}
}

// TestSignalBroker_CrossMeshSignalingRejected（M-9 集成验收）：跨 mesh 信令拒绝。
// 场景 1：mesh-a 节点发信令给 mesh-b 节点 → 403（from/to 不同 mesh）。
// 场景 2：mesh-a 调用方用 mesh-b 节点的 X-Node-ID 声称自己 → 403（callerNode mesh 不一致）。
func TestSignalBroker_CrossMeshSignalingRejected(t *testing.T) {
	const (
		meshA = "mesh-a"
		meshB = "mesh-b"
	)
	rt := hub.NewMeshRouteTable()
	reg := func(t *testing.T, mesh, id string) {
		t.Helper()
		a, _ := xfertest.Pipe()
		m := mux.New(a, mux.RoleDialer)
		t.Cleanup(func() { _ = m.Close() })
		rt.Add(mesh, hub.NodeInfo{ID: hub.NodeID(id), Mux: m, Secret: testSignalSecret(id)}, nil)
	}
	reg(t, meshA, "peer-a")
	reg(t, meshB, "peer-b")
	b := NewSignalBroker(rt)
	m := signalTestMux(b)

	// 场景 1：from=peer-a（mesh-a）→ to=peer-b（mesh-b）→ 403。
	req1 := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-b","sdp":"x"}`)
	req1 = req1.WithContext(withMesh(req1.Context(), meshA))
	w1 := httptest.NewRecorder()
	m.ServeHTTP(w1, req1)
	if w1.Code != http.StatusForbidden {
		t.Fatalf("跨 mesh 信令（mesh-a → mesh-b）应 403, got %d: %s", w1.Code, w1.Body.String())
	}

	// 场景 2：mesh-a 调用方冒用 mesh-b 的 peer-b 身份 → 403（callerNode mesh 不一致）。
	req2 := signalReq(http.MethodPost, "/api/signal/offer", "peer-b", `{"to":"peer-a","sdp":"x"}`)
	req2 = req2.WithContext(withMesh(req2.Context(), meshA))
	w2 := httptest.NewRecorder()
	m.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("mesh 不一致的 callerNode 应 403, got %d: %s", w2.Code, w2.Body.String())
	}

	// 对照：同 mesh 信令（peer-a → peer-a 不行，需 peer-a→同 mesh 另一节点）。
	// 注册一个 mesh-a 的第二节点，验证同 mesh 投递仍 202。
	reg(t, meshA, "peer-a2")
	req3 := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-a2","sdp":"x"}`)
	req3 = req3.WithContext(withMesh(req3.Context(), meshA))
	w3 := httptest.NewRecorder()
	m.ServeHTTP(w3, req3)
	if w3.Code != http.StatusAccepted {
		t.Fatalf("同 mesh 信令应 202, got %d: %s", w3.Code, w3.Body.String())
	}
}

// TestSignalBroker_PurgeOnNodeRemove 验证 I6 联动：节点从 RouteTable 真正移除时
// （连接断开 RemoveIfOwned / 手动踢除 Remove），SignalBroker 收到下线回调并清空
// 其信令收件箱——离线 peer 的消息不再常驻内存占用 maxSignalTotal 全局配额
// （否则反复上下线的短命节点会耗尽配额 → 新信令 429）。
func TestSignalBroker_PurgeOnNodeRemove(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)

	// peer-a 发 offer 给 peer-b（入队）
	req := signalReq(http.MethodPost, "/api/signal/offer", "peer-a", `{"to":"peer-b","sdp":"purge-me"}`)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}
	if m := b.queue.Peek("peer-b", ""); m == nil {
		t.Fatal("expected message in peer-b inbox before removal")
	}

	// 节点下线（Remove 触发 RemoveHook → PurgeNode → queue.Purge）
	if !b.rt.Remove("peer-b") {
		t.Fatal("peer-b should be removed")
	}

	// 收件箱应已被清空（消息不再残留占配额）
	if m := b.queue.Peek("peer-b", ""); m != nil {
		t.Fatalf("expected empty inbox after node removal, got %+v", m)
	}

	// 全局积压计数应归零
	if b.queue.Total() != 0 {
		t.Fatalf("expected total backlog 0 after purge, got %d", b.queue.Total())
	}
}

// TestSignalBroker_FlushSignalFiltersOrphanInbox（M4）：FlushSignal 生成的快照
// 必须过滤「收件箱归属节点已不在路由表」的孤儿 peer——避免把死信写入持久化文件
// （重启后既无人投递也无人消费，白白占配额）。
func TestSignalBroker_FlushSignalFiltersOrphanInbox(t *testing.T) {
	rt := hub.NewMeshRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.Add("", hub.NodeInfo{ID: "peer-a", Mux: m, Secret: "sec-a"}, nil)
	rt.Add("", hub.NodeInfo{ID: "peer-b", Secret: "sec-b"}, nil) // 离线节点（Mux nil）

	b := NewSignalBroker(rt)
	path := filepath.Join(t.TempDir(), "hub.json")
	p := hub.NewPersister(path)
	b.SetPersister(p)

	// 给 peer-b 投递一条信令（peer-b 在线注册，收件箱合法）。
	if err := b.queue.Push(hub.SignalMsg{Kind: hub.SignalOffer, From: "peer-a", To: "peer-b", SDP: "v=0"}); err != nil {
		t.Fatalf("Push: %v", err)
	}

	// peer-b 下线移除 → RemoveHook 应 PurgeNode 清空其收件箱。
	if !rt.Remove("peer-b") {
		t.Fatal("peer-b should be removed")
	}

	// FlushSignal：孤儿 peer-b 的收件箱不应出现在快照中。
	if err := b.FlushSignal(p); err != nil {
		t.Fatalf("FlushSignal: %v", err)
	}
	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, ms := range snap.Messages {
		if ms.Peer == "peer-b" {
			t.Fatalf("快照含孤儿收件箱 peer-b，M4 应过滤（节点已移除）: %+v", ms)
		}
	}
}

// TestHubPersist_OnChangeFiltersOrphanInbox（M4 onChange 路径）：节点注册/移除触发
// 的 onChange 持久化回调必须与 FlushSignal 一致，过滤「收件箱归属节点已不在路由表」
// 的孤儿 peer——否则节点下线（PurgeNode 清空收件箱）与下一次 onChange 快照之间
// 推入的孤儿消息会被持久化为死信。本测试直接构造 RegisterRoutes 全链路，验证
// onChange 落盘镜像不含孤儿收件箱（修复前 onChange 用原始 SnapshotSignalQueue，
// 孤儿消息会漏进文件）。
func TestHubPersist_OnChangeFiltersOrphanInbox(t *testing.T) {
	cfgPtr := &atomic.Pointer[Config]{}
	cfg := Default()
	cfg.Hub.Enabled = true
	cfg.UploadsDir = filepath.Join(t.TempDir(), "uploads")
	cfgPtr.Store(cfg)

	rt := hub.NewMeshRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.Add("", hub.NodeInfo{ID: "peer-a", Mux: m, Secret: "sec-a"}, nil)

	path := filepath.Join(t.TempDir(), "hub.json")
	p := hub.NewPersister(path)

	srvMux := http.NewServeMux()
	h := RegisterRoutes(t.Context(), RegisterRoutesOpts{
		Mux:        srvMux,
		CfgPtr:     cfgPtr,
		Version:    "test",
		BuildAt:    "now",
		Logger:     testLogger(),
		RouteTable: rt,
		HubPersist: p,
	})
	defer h.Close()

	// 直接向 peer-b 收件箱投递一条消息，但 peer-b 从未注册 → 孤儿收件箱。
	// （绕过 handleSignalPost 的 to 节点校验，模拟"节点已下线但消息残留"竞态。）
	if err := h.signalBroker.queue.Push(hub.SignalMsg{Kind: hub.SignalOffer, From: "peer-a", To: "peer-b", SDP: "v=0"}); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if got := h.signalBroker.queue.Total(); got != 1 {
		t.Fatalf("push 后 total = %d, want 1", got)
	}

	// 触发 onChange（节点注册成功路径）：再注册一个节点，持久化回调排队。
	rt.Add("", hub.NodeInfo{ID: "peer-c", Secret: "sec-c"}, nil)
	// onChange 是异步去抖 Schedule——同步 Flush 执行 pending 闭包（M4 过滤后落盘）。
	if err := p.Flush(nil); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	snap, err := p.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, ms := range snap.Messages {
		if ms.Peer == "peer-b" {
			t.Fatalf("onChange 快照含孤儿收件箱 peer-b，M4 应过滤: %+v", ms)
		}
	}
}
