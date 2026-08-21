// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSignalHub 实现 /api/signal/{offer,answer} 与 /api/signal/poll/{peer}
// 的最小语义，等价于 server.SignalBroker 的队列行为（按 To 入箱，poll 取走）。
func fakeSignalHub(t *testing.T) *httptest.Server {
	t.Helper()
	q := NewSignalQueue()
	mux := http.NewServeMux()
	for _, kind := range []SignalKind{SignalOffer, SignalAnswer, SignalCandidate} {
		mux.HandleFunc("POST /api/signal/"+string(kind), func(w http.ResponseWriter, r *http.Request) {
			var m SignalMsg
			if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
				http.Error(w, "bad", http.StatusBadRequest)
				return
			}
			m.Kind = kind
			q.Push(m)
			w.WriteHeader(http.StatusAccepted)
		})
	}
	mux.HandleFunc("GET /api/signal/poll/{peer}", func(w http.ResponseWriter, r *http.Request) {
		peer := r.PathValue("peer")
		msgs := []SignalMsg{}
		for {
			if m := q.Pop(peer); m != nil {
				msgs = append(msgs, *m)
				continue
			}
			break
		}
		_ = json.NewEncoder(w).Encode(msgs)
	})
	return httptest.NewServer(mux)
}

// TestHubSignaler_OfferAnswerRoundTrip 验证修复后的信令方向语义：
// A(拨号方) 发 offer 给 B，B(listen) 等到 offer 并回 answer 给 A，A 等到 answer。
// 这是 C1 死锁的回归测试——修复前 Wait* 用 From 过滤会永远等不到。
func TestHubSignaler_OfferAnswerRoundTrip(t *testing.T) {
	hub := fakeSignalHub(t)
	defer hub.Close()

	sigA := NewHubSignaler(hub.URL, "", "node-A")
	sigB := NewHubSignaler(hub.URL, "", "node-B")

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	// B 先开始等 offer（listener）
	type listenResult struct {
		from string
		sdp  string
		err  error
	}
	listenDone := make(chan listenResult, 1)
	go func() {
		from, sdp, err := sigB.WaitOffer(ctx)
		listenDone <- listenResult{from: from, sdp: sdp, err: err}
	}()

	// 稍后 A 发 offer
	time.Sleep(100 * time.Millisecond)
	if err := sigA.SendOffer("node-B", "offer-sdp-123"); err != nil {
		t.Fatalf("SendOffer: %v", err)
	}

	// B 应等到 offer，from == A
	select {
	case r := <-listenDone:
		if r.err != nil {
			t.Fatalf("WaitOffer: %v", r.err)
		}
		if r.from != "node-A" {
			t.Fatalf("WaitOffer from = %q, want node-A", r.from)
		}
		if r.sdp != "offer-sdp-123" {
			t.Fatalf("WaitOffer sdp = %q, want offer-sdp-123", r.sdp)
		}
	case <-ctx.Done():
		t.Fatal("WaitOffer 未在超时前返回（C1 死锁回归）")
	}

	// B 回 answer 给 A
	answerDone := make(chan struct{})
	go func() {
		defer close(answerDone)
		from, sdp, err := sigA.WaitAnswer(ctx)
		if err != nil {
			t.Errorf("WaitAnswer: %v", err)
			return
		}
		if from != "node-B" {
			t.Errorf("WaitAnswer from = %q, want node-B", from)
		}
		if sdp != "answer-sdp-456" {
			t.Errorf("WaitAnswer sdp = %q, want answer-sdp-456", sdp)
		}
	}()
	time.Sleep(100 * time.Millisecond)
	if err := sigB.SendAnswer("node-A", "answer-sdp-456"); err != nil {
		t.Fatalf("SendAnswer: %v", err)
	}
	select {
	case <-answerDone:
	case <-ctx.Done():
		t.Fatal("WaitAnswer 未在超时前返回")
	}
}

// TestHubSignaler_PollRejectsUnregistered 验证 poll 非 200 时返回错误而非挂起。
func TestHubSignaler_PollRejectsUnregistered(t *testing.T) {
	// 一个只对未注册 peer 返回 404 的 hub
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "peer 节点未注册", http.StatusNotFound)
	}))
	defer ts.Close()

	sig := NewHubSignaler(ts.URL, "", "node-A")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := sig.WaitOffer(ctx)
	if err == nil {
		t.Fatal("expected error for 404 poll")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected 404 in error, got: %v", err)
	}
}
