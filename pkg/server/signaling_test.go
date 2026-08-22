// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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
	rt := hub.NewRouteTable()
	for _, id := range []string{"peer-a", "peer-b"} {
		a, _ := xfertest.Pipe()
		m := mux.New(a, mux.RoleDialer)
		t.Cleanup(func() { _ = m.Close() })
		rt.AddWithInfoAndServices(hub.NodeInfo{ID: hub.NodeID(id), Mux: m, Secret: testSignalSecret(id)}, nil)
	}
	return NewSignalBroker(rt)
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
	rt := hub.NewRouteTable()
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })
	rt.AddWithInfoAndServices(hub.NodeInfo{ID: "nonsecret", Mux: m}, nil) // 不设 Secret
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
