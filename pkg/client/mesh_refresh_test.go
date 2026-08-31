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

// TestMeshTargetRefresher_Static 校验固定目标 refresher（虚拟 IP 寻址）：
// Resolve 始终返回预设 target（不查 hub），Invalidate no-op。
func TestMeshTargetRefresher_Static(t *testing.T) {
	target := &MeshService{Node: "node-b", Addr: "100.64.0.5:22"}
	r := NewStaticMeshTargetRefresher(target)

	got, err := r.Resolve(t.Context())
	if err != nil {
		t.Fatalf("static Resolve: %v", err)
	}
	if got.Node != "node-b" || got.Addr != "100.64.0.5:22" {
		t.Fatalf("static Resolve = %+v, want Node=node-b Addr=100.64.0.5:22", got)
	}
	// Invalidate no-op：再次 Resolve 仍返回固定 target。
	r.Invalidate("node-b")
	got2, err := r.Resolve(t.Context())
	if err != nil {
		t.Fatalf("static Resolve after Invalidate: %v", err)
	}
	if got2.Node != "node-b" {
		t.Fatalf("Invalidate 后 static Resolve 仍应返回固定 target, got %+v", got2)
	}
}

// --- 阶段 5 工作项 2 / PR-1：RR 轮询化新增测试 ---

// TestMeshTargetRefresher_RoundRobin 验证三候选 round-robin 均匀分布：
// SetTTL(time.Hour) 强制一次刷新填充候选池后，TTL 内连调 30 次应各命中 ~10 次
// （游标 mod 轮询，允许 ±1 偏差）。同时验证 TTL 内只打一次 hub
// （缓存=候选池+游标，而非单个 target）。
func TestMeshTargetRefresher_RoundRobin(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		// 乱序返回（排序后 node-a/b/c），同时覆盖排序确定性。
		return `[{"name":"svc","node":"node-b","addr":"10.0.0.2:22"},{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-c","addr":"10.0.0.3:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)

	counts := map[string]int{}
	const n = 30
	for range n {
		got, err := r.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		counts[got.Node]++
	}
	if hits.Load() != 1 {
		t.Fatalf("RR TTL 内应只打 1 次 hub（缓存=候选池+游标）, got %d", hits.Load())
	}
	for _, node := range []string{"node-a", "node-b", "node-c"} {
		if c := counts[node]; c < n/3-1 || c > n/3+1 {
			t.Fatalf("RR 分布不均: node=%s hits=%d, want ~%d (±1)", node, c, n/3)
		}
	}
}

// TestMeshTargetRefresher_RoundRobin_TTLHit 验证 TTL 内（未过期）每次 Resolve 也轮询
// 不同候选——缓存从「单值 target」改为「候选池 + 游标」，否则 RR 失效。
func TestMeshTargetRefresher_RoundRobin_TTLHit(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"},{"name":"svc","node":"node-c","addr":"10.0.0.3:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)

	want := []string{"node-a", "node-b", "node-c", "node-a"} // 排序后 [A,B,C] 轮询
	for i, wantNode := range want {
		got, err := r.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Node != wantNode {
			t.Fatalf("TTL 内第 %d 次 Resolve = %q, want %q", i+1, got.Node, wantNode)
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("TTL 内轮询应只打 1 次 hub, got %d", hits.Load())
	}
}

// TestMeshTargetRefresher_SkipFailedNode 验证 Invalidate(failedNode) 后 Resolve 在
// 冷却期内跳过该节点（在其余候选中轮询），冷却过后自动重新评估（恢复节点重新入池，
// 审查 Important #1）。
func TestMeshTargetRefresher_SkipFailedNode(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"},{"name":"svc","node":"node-c","addr":"10.0.0.3:22"}]`
	}))
	defer ts.Close()

	// 注入可推进的假时钟：控制 TTL 过期与冷却窗口。
	cur := time.Now()
	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetClock(func() time.Time { return cur })
	r.SetTTL(10 * time.Second)

	// 阶段 1：冷却期内跳过失败节点（首次 Resolve 刷新，lastFailedAt=cur，冷却 9s 内）。
	r.Invalidate("node-a")
	const n = 6
	for i := range n {
		got, err := r.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Node == "node-a" {
			t.Fatalf("第 %d 次 Resolve（冷却期）不应返回失败节点 node-a, got %q", i+1, got.Node)
		}
		// b/c 均应被选中（RR 在其余候选间轮询，非恒取同一节点）。
		if got.Node != "node-b" && got.Node != "node-c" {
			t.Fatalf("冷却期 Resolve 应返回 node-b 或 node-c, got %q", got.Node)
		}
	}

	// 阶段 2：推进时钟越过冷却窗口（>9s），且 TTL（10s）未过 → Resolve 命中缓存池，
	// 失败标记失效 → node-a 重新入池被选中。
	cur = cur.Add(MeshFailCooldown + time.Second)
	gotB, errB := r.Resolve(context.Background())
	if errB != nil {
		t.Fatal(errB)
	}
	if gotB.Node != "node-a" {
		t.Fatalf("冷却期过后 node-a 应重新纳入 RR（审查 Important #1），got %q", gotB.Node)
	}
}

// TestMeshTargetRefresher_AllFailedFallback 验证候选池全部为失败节点时回退到游标
// 指向的候选（不返回 ErrMeshServiceUnavailable，避免无限卡死）。
func TestMeshTargetRefresher_AllFailedFallback(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"svc","node":"node-a","addr":"10.0.0.1:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)
	r.Invalidate("node-a") // 唯一候选也是失败节点

	got, err := r.Resolve(context.Background())
	if err != nil {
		t.Fatalf("全部候选失败应回退而非报错, got %v", err)
	}
	if got.Node != "node-a" {
		t.Fatalf("回退应返回游标指向候选 node-a, got %q", got.Node)
	}
}

// TestMeshTargetRefresher_SortedCandidates 验证候选池按 NodeID 排序固化
// （map/遍历序不稳定，排序保证 RR 序列确定可测）。
func TestMeshTargetRefresher_SortedCandidates(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		// 乱序返回，排序后应为 node-a, node-b, node-c。
		return `[{"name":"svc","node":"node-c","addr":"10.0.0.3:22"},{"name":"svc","node":"node-a","addr":"10.0.0.1:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)

	want := []string{"node-a", "node-b", "node-c"}
	for i, wantNode := range want {
		got, err := r.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Node != wantNode {
			t.Fatalf("排序后第 %d 次 Resolve = %q, want %q", i+1, got.Node, wantNode)
		}
	}
}

// TestMeshTargetRefresher_FiltersByName 验证只收集同名服务候选（列表含其他服务名
// 不影响候选池；过滤后仍按 NodeID 排序轮询）。
func TestMeshTargetRefresher_FiltersByName(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(servicesHandler(&hits, func() string {
		return `[{"name":"other","node":"node-z","addr":"10.9.9.9:22"},{"name":"svc","node":"node-b","addr":"10.0.0.2:22"},{"name":"svc","node":"node-a","addr":"10.0.0.1:22"}]`
	}))
	defer ts.Close()

	svc := NewFileClient(ts.URL)
	r := NewMeshTargetRefresher(svc, "svc")
	r.SetTTL(time.Hour)

	want := []string{"node-a", "node-b", "node-a"} // 过滤后 [A,B] 轮询
	for i, wantNode := range want {
		got, err := r.Resolve(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got.Node != wantNode {
			t.Fatalf("过滤后第 %d 次 Resolve = %q, want %q", i+1, got.Node, wantNode)
		}
	}
}
