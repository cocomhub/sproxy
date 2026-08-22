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
// poll 支持 ?kind= 过滤（与客户端 Wait* 传 kind 对齐，I9）。
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
			_ = q.Push(m)
			w.WriteHeader(http.StatusAccepted)
		})
	}
	mux.HandleFunc("GET /api/signal/poll/{peer}", func(w http.ResponseWriter, r *http.Request) {
		peer := r.PathValue("peer")
		msgs := []SignalMsg{}
		kind := SignalKind(r.URL.Query().Get("kind"))
		var m *SignalMsg
		if kind != "" {
			m = q.PopKind(peer, kind)
		} else {
			m = q.Pop(peer)
		}
		if m != nil {
			msgs = append(msgs, *m)
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

	// B 回 answer 给 A；A 侧 goroutine 通过 channel 回传断言（I50：
	// 避免 goroutine 内 t.Errorf 与主 goroutine 提前 t.Fatal 竞态）。
	type answerResult struct {
		from string
		sdp  string
		err  error
	}
	answerDone := make(chan answerResult, 1)
	go func() {
		from, sdp, err := sigA.WaitAnswer(ctx)
		answerDone <- answerResult{from: from, sdp: sdp, err: err}
	}()
	time.Sleep(100 * time.Millisecond)
	if err := sigB.SendAnswer("node-A", "answer-sdp-456"); err != nil {
		t.Fatalf("SendAnswer: %v", err)
	}
	select {
	case r := <-answerDone:
		if r.err != nil {
			t.Fatalf("WaitAnswer: %v", r.err)
		}
		if r.from != "node-B" {
			t.Fatalf("WaitAnswer from = %q, want node-B", r.from)
		}
		if r.sdp != "answer-sdp-456" {
			t.Fatalf("WaitAnswer sdp = %q, want answer-sdp-456", r.sdp)
		}
	case <-ctx.Done():
		t.Fatal("WaitAnswer 未在超时前返回")
	}
}

// TestHubSignaler_PollRejectsUnregistered 验证 poll 非 200 时返回错误而非挂起。
// 真实服务端未注册 poll 返回 400（errSignalNodeNotRegistered），mock 与之一致（I51）。
func TestHubSignaler_PollRejectsUnregistered(t *testing.T) {
	// 一个只对未注册 peer 返回 400 的 hub
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "peer 节点未注册", http.StatusBadRequest)
	}))
	defer ts.Close()

	sig := NewHubSignaler(ts.URL, "", "node-A")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	_, _, err := sig.WaitOffer(ctx)
	if err == nil {
		t.Fatal("expected error for 400 poll")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Fatalf("expected 400 in error, got: %v", err)
	}
}

// TestHubSignaler_SetContextCancel 验证注入 base context 后 Send* 受取消控制（I7）。
func TestHubSignaler_SetContextCancel(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sig := NewHubSignaler(ts.URL, "", "node-A")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sig.SetContext(ctx)
	if err := sig.SendOffer("node-B", "sdp"); err == nil {
		t.Fatal("expected error when base context cancelled")
	}
}

// TestHubSignaler_DedupSeenMessage 验证 seen-set 去重（I10）：
// 同一 ID 消息被重复 poll 到（模拟服务端重投/超时兜底）时，第二次 Wait 跳过。
func TestHubSignaler_DedupSeenMessage(t *testing.T) {
	// 一个每次 poll 都返回同一条固定消息的 hub（模拟服务端重投）
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/api/signal/poll/") {
			_ = json.NewEncoder(w).Encode([]SignalMsg{{
				ID: "fixed-id", Kind: SignalOffer, From: "node-B", To: "node-A",
				SDP: "sdp-1", At: time.Now().UnixMilli(),
			}})
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	sig := NewHubSignaler(ts.URL, "", "node-A")
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	from, sdp, err := sig.WaitOffer(ctx)
	if err != nil {
		t.Fatalf("first WaitOffer: %v", err)
	}
	if from != "node-B" || sdp != "sdp-1" {
		t.Fatalf("unexpected first message: from=%q sdp=%q", from, sdp)
	}

	// 第二次 WaitOffer：同一 ID 消息被 seen-set 跳过，最终超时返回（I10 去重）
	ctx2, cancel2 := context.WithTimeout(t.Context(), 700*time.Millisecond)
	defer cancel2()
	_, _, err2 := sig.WaitOffer(ctx2)
	if err2 == nil {
		t.Fatal("expected dedup to skip duplicate and time out")
	}
}

// TestHubSignaler_PollCarriesSecretHeader 验证携带 secret 时 post/poll 带
// X-Node-Secret 头（I1）。变参构造下不传 secret 的调用不受影响。
func TestHubSignaler_PollCarriesSecretHeader(t *testing.T) {
	var gotPost, gotPoll bool
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Node-Secret") != "topsecret" {
			http.Error(w, "missing secret", http.StatusForbidden)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/signal/poll/") {
			gotPoll = true
			_ = json.NewEncoder(w).Encode([]SignalMsg{})
			return
		}
		gotPost = true
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()

	sig := NewHubSignaler(ts.URL, "", "node-A", "topsecret")
	if err := sig.SendOffer("node-B", "sdp"); err != nil {
		t.Fatalf("SendOffer with secret: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := sig.poll(ctx, "node-A", SignalAnswer); err != nil {
		t.Fatalf("poll with secret: %v", err)
	}
	if !gotPost || !gotPoll {
		t.Fatalf("expected both post and poll to carry secret (post=%v poll=%v)", gotPost, gotPoll)
	}
}
