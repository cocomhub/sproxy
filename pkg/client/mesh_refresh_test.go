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

func TestMeshSignalAndRelayToken(t *testing.T) {
	if got := MeshSignalToken("flag", "cfg"); got != "flag" {
		t.Fatalf("MeshSignalToken flag 优先, got %q", got)
	}
	if got := MeshSignalToken("", "cfg"); got != "cfg" {
		t.Fatalf("MeshSignalToken cfg 回落, got %q", got)
	}
	if got := MeshRelayToken("", "", "t", "a"); got != "t" {
		t.Fatalf("MeshRelayToken flag token 回落, got %q", got)
	}
	if got := MeshRelayToken("", "c", "t", "a"); got != "c" {
		t.Fatalf("MeshRelayToken 配置 relay 优先, got %q", got)
	}
}
