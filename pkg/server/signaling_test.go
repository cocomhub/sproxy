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

// newSignalTestBroker 构造带已注册节点的 SignalBroker（from/to 校验需要）。
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

	// POST offer 给 peer-b
	req := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"peer-a","to":"peer-b","sdp":"offer-sdp"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d", w.Code)
	}

	// poll peer-b 应拿到 offer
	pollReq := httptest.NewRequest(http.MethodGet, "/api/signal/poll/peer-b", nil)
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
	mux.ServeHTTP(pw2, httptest.NewRequest(http.MethodGet, "/api/signal/poll/peer-b", nil))
	var msgs2 []hub.SignalMsg
	if err := json.NewDecoder(pw2.Body).Decode(&msgs2); err != nil {
		t.Fatal(err)
	}
	if len(msgs2) != 0 {
		t.Fatalf("expected empty second poll, got %+v", msgs2)
	}
}

func TestSignalBroker_BadInput(t *testing.T) {
	b := NewSignalBroker(hub.NewRouteTable()) // 空路由表
	mux := signalTestMux(b)
	// 缺 to/from
	req := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"sdp":"x"}`))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
	// 坏 JSON
	req2 := httptest.NewRequest(http.MethodPost, "/api/signal/answer", strings.NewReader("{bad"))
	w2 := httptest.NewRecorder()
	mux.ServeHTTP(w2, req2)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w2.Code)
	}
	// from 未注册：拒绝
	req3 := httptest.NewRequest(http.MethodPost, "/api/signal/offer", strings.NewReader(`{"from":"ghost","to":"peer-b","sdp":"x"}`))
	w3 := httptest.NewRecorder()
	mux.ServeHTTP(w3, req3)
	if w3.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unregistered from, got %d", w3.Code)
	}
}

func TestSignalBroker_UnregisteredPeer(t *testing.T) {
	b := newSignalTestBroker(t) // 只有 peer-a / peer-b 注册
	mux := signalTestMux(b)
	// poll 未注册 peer → 404
	req := httptest.NewRequest(http.MethodGet, "/api/signal/poll/ghost", nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unregistered peer, got %d", w.Code)
	}
}
