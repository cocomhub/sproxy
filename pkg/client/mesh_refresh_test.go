// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func servicesHandler(hits *atomic.Int32, get func() string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(get()))
	}
}

func TestMeshTargetRefresher_TTLCacheAndFailover(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")

	// 首次 resolve：node-a（字典序首）。
	t1, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t1.Node != "node-a" {
		t.Fatalf("首次应取 node-a, got %q", t1.Node)
	}
	// TTL 内缓存命中（不再打 HTTP）。
	if _, rerr := r.Resolve(context.Background()); rerr != nil {
		t.Fatal(rerr)
	}
	if hits.Load() != 1 {
		t.Fatalf("期望 1 次 hub 调用, got %d", hits.Load())
	}
	// invalidate(node-a) 后跳过死节点选 node-b。
	r.Invalidate("node-a")
	t2, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if t2.Node != "node-b" {
		t.Fatalf("invalidate 后应选 node-b, got %q", t2.Node)
	}
}

func TestMeshTargetRefresher_ServiceAbsent(t *testing.T) {
	ts := httptest.NewServer(servicesHandler(&atomic.Int32{}, func() string {
		return `[]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "missing")
	_, err := r.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "当前不可用") {
		t.Fatalf("期望 ErrMeshServiceUnavailable, got %v", err)
	}
}

func TestMeshTargetRefresher_SingleFlight(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-release:
		case <-time.After(5 * time.Second):
		}
		_, _ = w.Write([]byte(`[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"}]`))
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")

	firstDone := make(chan error, 1)
	go func() { _, err := r.Resolve(context.Background()); firstDone <- err }()
	deadline := time.Now().Add(2 * time.Second)
	for hits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() == 0 {
		t.Fatal("刷新未到达 handler")
	}

	const n = 5
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Resolve(context.Background())
		}(i)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("waiter[%d] error: %v", i, e)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("期望单飞 1 次 hub 调用, got %d", hits.Load())
	}
}

func TestMeshSignalToken(t *testing.T) {
	if got := MeshSignalToken("flag", "cfg"); got != "flag" {
		t.Fatalf("MeshSignalToken flag 优先, got %q", got)
	}
	if got := MeshSignalToken("", "cfg"); got != "cfg" {
		t.Fatalf("MeshSignalToken cfg 回落, got %q", got)
	}
	if got := MeshSignalToken("", ""); got != "" {
		t.Fatalf("MeshSignalToken 全空应空串, got %q", got)
	}
}

// TestMeshTargetRefresher_TTLExpiry（D2 回归）：时钟推进超过 TTL 后重新拉取，
// 节点上下线变化被感知（缓存命中测试只验证"永不过期"的路径，会掩盖过期重取缺失）。
func TestMeshTargetRefresher_TTLExpiry(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(3 * time.Second)
	cur := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)
	r.SetClock(func() time.Time { return cur })

	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("首次应 1 次 hub 调用, got %d", hits.Load())
	}
	// TTL 内缓存命中。
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("TTL 内应命中缓存, got %d 次调用", hits.Load())
	}
	// 时钟推进超过 TTL → 过期 → 重新拉取（否则节点上下线永不被感知）。
	cur = cur.Add(10 * time.Second)
	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 2 {
		t.Fatalf("TTL 过期应重新拉取, got %d 次调用", hits.Load())
	}
}

// TestMeshTargetRefresher_FetchError（D3 回归）：hub 拉取失败应返回明确错误，
// 而非"服务不可用"（否则网络故障被误报为服务离线）。
func TestMeshTargetRefresher_FetchError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	_, err := r.Resolve(context.Background())
	if err == nil || !strings.Contains(err.Error(), "查询 mesh 服务失败") {
		t.Fatalf("期望查询失败错误, got %v", err)
	}
}

// TestMeshTargetRefresher_ConcurrentCacheHit（D6）：TTL 内并发 resolve 全部命中缓存，
// 只打一次 hub（缓存命中在并发下不退化）。
func TestMeshTargetRefresher_ConcurrentCacheHit(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)

	if _, err := r.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = r.Resolve(context.Background())
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("resolve[%d] error: %v", i, e)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("TTL 内并发应全部命中缓存（1 次 HTTP）, got %d", hits.Load())
	}
}
