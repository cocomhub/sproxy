// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// fakePath 是测试用 PathProvider：固定 Kind、固定延迟、固定 Enabled。
type fakePath struct {
	name     string
	kind     string
	delay    time.Duration
	priority int
	enabled  bool
	fail     bool        // 为 true 时 Dial 恒返回错误（模拟路径瞬时故障）
	callCh   chan string // 记录 Dial 调用
}

func (f *fakePath) Name() string  { return f.name }
func (f *fakePath) Priority() int { return f.priority }
func (f *fakePath) Enabled(_ context.Context, _ *client.FileClient) bool {
	return f.enabled
}

func (f *fakePath) Dial(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	if f.callCh != nil {
		f.callCh <- f.name
	}
	if f.fail {
		return nil, fmt.Errorf("boom-%s", f.name)
	}
	time.Sleep(f.delay)
	return &Result{Conn: nil, Kind: f.kind, Latency: f.delay}, nil
}

// TestSmartPathRegistry_BuiltinProviders：注册表内置 direct + relay。
// 注：不断言 len==2（全局注册表可能被并行测试的临时注入项污染——R18 门禁下所有
// 测试 t.Parallel，Register/Delete 窗口会造成 flaky）。只断言 builtin 存在即可。
func TestSmartPathRegistry_BuiltinProviders(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	names := SmartPathRegistry.Names()
	has := func(want string) bool {
		return slices.Contains(names, want)
	}
	if !has("direct") || !has("relay") {
		t.Fatalf("builtin providers missing direct/relay: %v", names)
	}
	// Name 与注册名一致性（缓存命中依赖 p.Name() == 注册名）：防真实提供者缓存永不命中。
	d, r := directProvider{}, relayProvider{}
	if got := d.Name(); got != "direct" {
		t.Fatalf("directProvider.Name() = %q, want direct", got)
	}
	if got := r.Name(); got != "relay" {
		t.Fatalf("relayProvider.Name() = %q, want relay", got)
	}
}

// TestSmartPathRegistry_RegisterNewPath：Register 新提供者可被 Names 发现（可扩展性）。
func TestSmartPathRegistry_RegisterNewPath(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	p := &fakePath{name: "via-node", kind: "via-node", delay: time.Millisecond, enabled: true}
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "via-node", Instance: p, Priority: 10})
	t.Cleanup(func() { SmartPathRegistry.Delete("via-node") })

	if _, ok := SmartPathRegistry.Get("via-node"); !ok {
		t.Fatal("Register 后 Get(via-node) 应命中")
	}
}

// 注入测试提供者集合：清注册表 + 注册 fake（T.Cleanup 恢复 builtin）。
// ps 是 PathProvider 接口实现（*fakePath / *failingPath 均满足）。
// 持 smartRegistryMu 串行化，防并行测试互改全局注册表（R18 t.Parallel）。
func smartWithProviders(t *testing.T, ps ...PathProvider) {
	t.Helper()
	smartRegistryMu.Lock()
	for _, n := range SmartPathRegistry.Names() {
		SmartPathRegistry.Delete(n)
	}
	for _, p := range ps {
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: p.Name(), Instance: p, Priority: p.Priority()})
	}
	smartRegistryMu.Unlock()
	t.Cleanup(func() {
		smartRegistryMu.Lock()
		for _, n := range SmartPathRegistry.Names() {
			SmartPathRegistry.Delete(n)
		}
		// 恢复 builtin
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "direct", Instance: directProvider{}, Priority: 100})
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "relay", Instance: relayProvider{}, Priority: 50})
		smartRegistryMu.Unlock()
	})
}

// 用例1：直连慢/中继快 → 选中继
func TestDialSmart_PicksFastestPath(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	slow := &fakePath{name: "direct", kind: "webrtc", delay: 500 * time.Millisecond, priority: 100, enabled: true}
	fast := &fakePath{name: "relay", kind: "relay", delay: 50 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, slow, fast)

	smartCacheClear() // 清缓存避免跨用例污染
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "relay" {
		t.Fatalf("Kind = %s, want relay（快者胜出）", res.Kind)
	}
}

// 用例2：直连快 → 选直连
func TestDialSmart_PicksDirectWhenFast(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	fast := &fakePath{name: "direct", kind: "webrtc", delay: 10 * time.Millisecond, priority: 100, enabled: true}
	slow := &fakePath{name: "relay", kind: "relay", delay: 300 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, fast, slow)
	smartCacheClear()
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "webrtc" {
		t.Fatalf("Kind = %s, want webrtc", res.Kind)
	}
}

// 用例3：多跳 home→office(快)→B vs home→B(慢) → 选多跳（端到端 RTT 最短路）
func TestDialSmart_PicksMultihopWhenFastest(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	slowDirect := &fakePath{name: "direct", kind: "webrtc", delay: 400 * time.Millisecond, priority: 100, enabled: true}
	fastViaNode := &fakePath{name: "via-node", kind: "via-node", delay: 40 * time.Millisecond, priority: 10, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 500 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, slowDirect, fastViaNode, relay)
	smartCacheClear()
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "B", Addr: "b:1"}, "home", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "via-node" {
		t.Fatalf("Kind = %s, want via-node（多跳端到端 RTT 最短胜出）", res.Kind)
	}
}

// 用例4：缓存命中走缓存路径（单路，不竞速）
func TestDialSmart_CacheHitUsesCachedPath(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true, callCh: make(chan string, 8)}
	relay := &fakePath{name: "relay", kind: "relay", delay: time.Millisecond, priority: 50, enabled: true, callCh: make(chan string, 8)}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次：竞速（两路都拨号），胜者写缓存。
	first, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	// 断言首次竞速两路都被调用（先 drain 掉首次的调用记录）。
	calls1 := drainCalls(direct.callCh) + drainCalls(relay.callCh)
	if calls1 < 2 {
		t.Fatalf("首次应竞速两路，实际 %d 次调用", calls1)
	}
	// 清空 callCh 残留（胜者路径在竞速内已调用一次，缓存命中后再次调用——
	// 这里先清空以便精确断言「缓存命中只调胜者一路」）。
	_ = drainCalls(direct.callCh) + drainCalls(relay.callCh)

	// 缓存命中：只走缓存路径（单路），且返回的 Kind 与首次胜者一致。
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("cached dial: %v", err)
	}
	if res.Kind != first.Kind {
		t.Fatalf("cached Kind = %s, want %s（首次胜者）", res.Kind, first.Kind)
	}
	// 断言缓存命中只调了胜者路径一次（另一路 0 次）。
	winner, loser := direct, relay
	if res.Kind == relay.kind {
		winner, loser = relay, direct
	}
	if got := drainCalls(winner.callCh); got != 1 {
		t.Fatalf("缓存命中应只调胜者 1 次，实际 %d", got)
	}
	if got := drainCalls(loser.callCh); got != 0 {
		t.Fatalf("缓存命中不应调败者路径，实际 %d", got)
	}
}

// failingPath 恒失败提供者（测试全失败聚合错误）。
type failingPath struct{ name string }

func (f *failingPath) Name() string                                         { return f.name }
func (f *failingPath) Priority() int                                        { return 100 }
func (f *failingPath) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (f *failingPath) Dial(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	return nil, fmt.Errorf("boom-%s", f.name)
}

// 用例6：全失败 → 聚合错误上下文
func TestDialSmart_AllFail(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	smartWithProviders(t, &failingPath{name: "direct"}, &failingPath{name: "relay"})
	smartCacheClear()
	_, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err == nil {
		t.Fatal("全失败应返回错误")
	}
	if !strings.Contains(err.Error(), "direct") || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("错误应含各候选上下文: %v", err)
	}
}

// 用例7：-race 并发安全（并发 DialSmart 读写缓存）
func TestDialSmart_ConcurrentCacheRace(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	p := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, p)
	smartCacheClear()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
		}()
	}
	wg.Wait()
}

// drainCalls 收走 callCh 中已记录的调用名，返回数量。
func drainCalls(ch chan string) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}

// 用例8：缓存路径瞬时故障 → 删缓存重新竞速（Important-1 回归）
func TestDialSmart_CacheFailFallback(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 5 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次竞速：direct（1ms）快于 relay（5ms）→ direct 胜出写缓存。
	first, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if first.Kind != "webrtc" {
		t.Fatalf("首次胜者 = %s, want webrtc（direct 更快）", first.Kind)
	}

	// direct 瞬时故障：缓存命中 direct → Dial 失败 → 应删缓存重新竞速 → relay 胜出。
	direct.fail = true
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("fallback dial: %v", err)
	}
	if res.Kind != "relay" {
		t.Fatalf("故障转移 Kind = %s, want relay（缓存失败应删缓存重新竞速）", res.Kind)
	}
}

// 用例9：候选按 Priority 降序排序后截断 ≤4（Important-2 回归）
func TestDialSmart_PriorityOrdering(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	// 注册 5 个提供者：低优先在前（Names 注册顺序），最高优先 "ultra" 在最后。
	// 若收集后不排序，遍历 break 到第 4 个即丢 ultra；修复后高优先必进候选。
	low := &fakePath{name: "low", kind: "low", delay: 50 * time.Millisecond, priority: 1, enabled: true}
	mid1 := &fakePath{name: "mid1", kind: "mid1", delay: 40 * time.Millisecond, priority: 30, enabled: true}
	mid2 := &fakePath{name: "mid2", kind: "mid2", delay: 30 * time.Millisecond, priority: 40, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 20 * time.Millisecond, priority: 50, enabled: true}
	ultra := &fakePath{name: "ultra", kind: "ultra", delay: time.Millisecond, priority: 200, enabled: true}
	smartWithProviders(t, low, mid1, mid2, relay, ultra)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	// ultra 是最高优先且最快（1ms）→ 若排序正确必参与竞速并胜出。
	if res.Kind != "ultra" {
		t.Fatalf("Kind = %s, want ultra（高优先候选应进入竞速）", res.Kind)
	}
}
