// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

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

// newSignalTestBroker 构造带已注册节点的 SignalBroker（身份绑定校验需要）。
func newSignalTestBroker(t *testing.T) *SignalBroker {
	t.Helper()
	rt := hub.NewRouteTable()
	for _, id := range []string{"peer-a", "peer-b"} {
		a, _ := xfertest.Pipe()
		m := mux.New(a, mux.RoleDialer)
		t.Cleanup(func() { _ = m.Close() })
		rt.Add(hub.NodeID(id), m)
	}
	return NewSignalBroker(rt)
}

func TestSignalBroker_PostAndPoll(t *testing.T) {
	b := newSignalTestBroker(t)
	mux := signalTestMux(b)

	// POST offer 给 peer-b（调用方 peer-a）
	req := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"peer-a","to":"peer-b","sdp":"offer-sdp"}`))
	req.Header.Set(signalNodeHeader, "peer-a")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	// poll peer-b 应拿到 offer（调用方 peer-b）
	pollReq := httptest.NewRequest(http.MethodGet, "/api/signal/poll/peer-b", nil)
	pollReq.Header.Set(signalNodeHeader, "peer-b")
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
	pollReq2 := httptest.NewRequest(http.MethodGet, "/api/signal/poll/peer-b", nil)
	pollReq2.Header.Set(signalNodeHeader, "peer-b")
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

	// 2. X-Node-ID 与 from 不一致 → 403（冒充被拒）
	req2 := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"peer-a","to":"peer-b","sdp":"x"}`))
	req2.Header.Set(signalNodeHeader, "peer-b") // 声称自己是 peer-b 却以 peer-a 发
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for from mismatch, got %d", w2.Code)
	}

	// 3. poll 非自己收件箱 → 403（窃听被拒）
	pollReq := httptest.NewRequest(http.MethodGet, "/api/signal/poll/peer-a", nil)
	pollReq.Header.Set(signalNodeHeader, "peer-b") // 声称自己是 peer-b 却轮询 peer-a
	pw := httptest.NewRecorder()
	mux.ServeHTTP(pw, pollReq)
	if pw.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for poll mismatch, got %d", pw.Code)
	}

	// 4. X-Node-ID 未注册 → 400
	req3 := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"ghost","to":"peer-b","sdp":"x"}`))
	req3.Header.Set(signalNodeHeader, "ghost")
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered node, got %d", w3.Code)
	}
}

func TestSignalBroker_BadInput(t *testing.T) {
	b := NewSignalBroker(hub.NewRouteTable()) // 空路由表
	mux := signalTestMux(b)
	// 缺 to/from
	req := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"sdp":"x"}`))
	req.Header.Set(signalNodeHeader, "n")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	// 坏 JSON
	req2 := httptest.NewRequest(http.MethodPost, "/api/signal/answer", strings.NewReader("{bad"))
	req2.Header.Set(signalNodeHeader, "n")
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w2.Code)
	}
}

func TestSignalBroker_UnregisteredPeer(t *testing.T) {
	b := newSignalTestBroker(t) // 只有 peer-a / peer-b 注册
	mux := signalTestMux(b)
	// poll 未注册 peer → 400（身份校验：ghost 未注册）
	req := httptest.NewRequest(http.MethodGet, "/api/signal/poll/ghost", nil)
	req.Header.Set(signalNodeHeader, "ghost")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered peer, got %d", w.Code)
	}
}
