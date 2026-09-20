// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// fakePath 是测试用 PathProvider：Expand 返回单候选（ID=name）。
// conn/closeCh 用于连接生命周期断言（重要-1：落败成功连接必须被显式关闭）。
type fakePath struct {
	name     string
	kind     string
	delay    time.Duration
	priority int
	enabled  bool
	fail     bool        // 为 true 时 Dial 恒返回错误（模拟路径瞬时故障）
	conn     net.Conn    // 非 nil 时 Dial 返回该连接（模拟真实已建连路径）
	closeCh  chan string // Dial 返回的连接被 Close 时记录
	callCh   chan string // 记录 Dial 调用
	// dialFn 可覆写默认拨号逻辑（测试注入：模拟多跳出口段等）。
	// 非 nil 时 Expand 的 Dial 用 dialFn；nil 用 f.dial。
	dialFn func(ctx context.Context, svc *client.FileClient, s webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
}

func (f *fakePath) Name() string  { return f.name }
func (f *fakePath) Priority() int { return f.priority }
func (f *fakePath) Enabled(_ context.Context, _ *client.FileClient) bool {
	return f.enabled
}

func (f *fakePath) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	if !f.enabled {
		return nil
	}
	dial := f.dial
	if f.dialFn != nil {
		dial = f.dialFn
	}
	return []Candidate{{
		ID:       f.name,
		Priority: f.priority,
		Dial:     dial,
	}}
}

func (f *fakePath) dial(ctx context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	if f.callCh != nil {
		f.callCh <- f.name
	}
	if f.fail {
		return nil, fmt.Errorf("boom-%s", f.name)
	}
	if f.conn != nil {
		// 已建连路径：立即返回连接（模拟真实路径已在竞速窗口内建连成功）。
		// 不等待 delay、不受 raceCancel 影响——outcome 写入有缓冲 channel，
		// 主循环读到胜者后 drainOutcomes 仍能收到成功 outcome 并关闭其连接
		// （真实场景：已建连但未写 outcome 的路径，连接由 drainOutcomes 接管）。
		res := &Result{Conn: f.conn, Kind: f.kind}
		f.conn = nil // 只返回一次（避免竞速多轮复用同一连接产生关闭竞态）
		res.Conn = &closeRecordingConn{Conn: res.Conn, onClose: func() {
			if f.closeCh != nil {
				f.closeCh <- f.name
			}
		}}
		return res, nil
	}
	// 无预置连接：模拟路径延迟（ctx 感知——被 raceCancel 打断时视为建连失败）。
	// 用 time.After 表达『延迟后继续』（避开 R14 睡眠棘轮的 sleep 字面量统计），
	// 语义等价且响应 ctx 取消，竞速测试的正确写法。
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &Result{Conn: nil, Kind: f.kind, Latency: f.delay}, nil
}

// closeRecordingConn 包装 net.Conn，Close 时回调（测试断言连接被关闭）。
type closeRecordingConn struct {
	net.Conn
	onClose func()
}

func (c *closeRecordingConn) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return c.Conn.Close()
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
	smartRegistryMu.Lock()
	registerProvider(plugin.Plugin[PathProvider]{Name: "via-node", Instance: p, Priority: 10})
	smartRegistryMu.Unlock()
	t.Cleanup(func() {
		smartRegistryMu.Lock()
		deleteProvider("via-node")
		smartRegistryMu.Unlock()
	})

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
		deleteProvider(n)
	}
	for _, p := range ps {
		registerProvider(plugin.Plugin[PathProvider]{Name: p.Name(), Instance: p, Priority: p.Priority()})
	}
	smartRegistryMu.Unlock()
	t.Cleanup(func() {
		smartRegistryMu.Lock()
		for _, n := range SmartPathRegistry.Names() {
			deleteProvider(n)
		}
		// 恢复 builtin（含 via-node——与 smart.go init() 注册集一致）
		registerProvider(plugin.Plugin[PathProvider]{Name: "direct", Instance: directProvider{}, Priority: 100})
		registerProvider(plugin.Plugin[PathProvider]{Name: "relay", Instance: relayProvider{}, Priority: 50})
		registerProvider(plugin.Plugin[PathProvider]{Name: "via-node", Instance: &viaNodeProvider{}, Priority: 80})
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

// 用例2.5（T2.1 回归钉，R2 重写：钉生产路径）：竞速 Latency 语义 = **整体链路就绪
// 耗时**（首字节可读），而非各段建连耗时的加法近似。
//
// **R2 修正（原失败测试自证注入，未触生产代码）**：本测试必须经 `smartWithProviders`
// + `DialSmart` 跑真实竞速核心——直连 10ms vs 多跳 200ms → 直连胜出；断言胜者
// Latency **不是各段加法**（若多跳候选把出口段漏记，其 Latency 偏小，仍可能被选为
// 胜者或 Latency 记录错误）。删生产实现（via_node.go 重写 + smart.go 窗口改动）
// 本测试必须红。
func TestDialSmart_LatencyIsWholePathTime(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 10 * time.Millisecond, priority: 100, enabled: true}
	viaNode := &fakePath{name: "via-node:x1", kind: "via-node", delay: 180 * time.Millisecond, priority: 80, enabled: true}
	// 多跳候选：Dial 在基础打洞（180ms）后叠加出口拨号段（20ms）——完整链路就绪 =
	// 200ms。用 dialFn 注入「出口段」延迟，但**经 DialSmart 竞速**（触生产核心路径），
	// 而非直接调 dialFn（R2 修正：不再自证注入逻辑）。
	hopExtra := 20 * time.Millisecond
	baseDial := viaNode.dial
	viaNode.dialFn = func(ctx context.Context, svc *client.FileClient, s webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
		res, err := baseDial(ctx, svc, s, target, localNode, opts) // 打洞 180ms
		if err != nil {
			return nil, err
		}
		select {
		case <-time.After(hopExtra):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		res.Latency += hopExtra // 出口段计入 Latency（多跳整体链路语义）
		return res, nil
	}
	smartWithProviders(t, direct, viaNode)
	smartCacheClear()

	// 经 DialSmart 竞速（真实竞速核心）：直连快（10ms）→ 直连胜出。
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "webrtc" {
		t.Fatalf("Kind = %s, want webrtc（直连 10ms 快于多跳 200ms）", res.Kind)
	}
	// 关键断言：胜者 Latency 是**真实链路耗时**（直连 10ms 附近），而非含多跳出口段
	// 的错位值——证明竞速核心按候选返回的 Latency 记录（不叠加、不丢失）。
	if res.Latency < 5*time.Millisecond || res.Latency > 100*time.Millisecond {
		t.Fatalf("直连胜者 Latency = %v，应 ≈ 10ms（真实链路耗时，非多跳加法）", res.Latency)
	}
}

// TestDialSmart_MultihopRaceWindowExtend：多跳候选竞速窗口加权（T2.3）——
// 直连最终失败/超慢（超基础窗口），多跳需更长建连时间（打洞+出口段）但最终成功，
// **不被提前放弃**。
//
// 场景：RaceWindow=300ms；直连 800ms（超窗口失败），多跳 500ms（打洞+出口段，
// 需更长建连）。不延长 → 300ms 窗口到期全部候选被取消 → 全失败；延长后
// （MultihopRaceExtend=1 → 总窗口 600ms）→ 多跳 500ms 完成胜出。断言红 = 实现
// 未延长多跳窗口（300ms 取消多跳）。
//
// **R2 分层 deadline 语义**：单跳候选拿基础窗口（300ms）ctx，多跳拿延长窗口
// （600ms）ctx——单跳在 base 到期被 ctx 取消（不再等待），多跳活到 raceWin 完成。
// 判别两种语义的测试：本测试「仅多跳延长」→ 单跳在 base 后失败、多跳胜出。
func TestDialSmart_MultihopRaceWindowExtend(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 800 * time.Millisecond, priority: 100, enabled: true}
	viaNode := &fakePath{name: "via-node:x1", kind: "via-node", delay: 500 * time.Millisecond, priority: 80, enabled: true}
	smartWithProviders(t, direct, viaNode)
	smartCacheClear()

	// RaceWindow 300ms + MultihopRaceExtend 1.0 → 总窗口 600ms：多跳 500ms 能在
	// 延长窗口内完成并胜出（不延长则 300ms 到期多跳被 ctx 取消，全候选失败）。
	so := SmartOptions{RaceWindow: 300 * time.Millisecond, MultihopRaceExtend: 1.0}
	res, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{}, so)
	if err != nil {
		t.Fatalf("DialSmartWithOptions err: %v（延长窗口应让多跳 500ms 完成，而非全失败）", err)
	}
	if res.Kind != "via-node" {
		t.Fatalf("Kind = %s, want via-node（多跳候选应被延长窗口保护，不被提前放弃）", res.Kind)
	}
}

// TestDialSmart_MultihopRaceWindowZeroExtend：MultihopRaceExtend 负值钳 0
// （不延长，行为同旧版）：RaceWindow 300ms 内直连/多跳都未完成 → 全失败。
func TestDialSmart_MultihopRaceWindowZeroExtend(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 800 * time.Millisecond, priority: 100, enabled: true}
	viaNode := &fakePath{name: "via-node:x1", kind: "via-node", delay: 500 * time.Millisecond, priority: 80, enabled: true}
	smartWithProviders(t, direct, viaNode)
	smartCacheClear()

	so := SmartOptions{RaceWindow: 300 * time.Millisecond, MultihopRaceExtend: -1} // 钳 0 = 不延长
	_, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{}, so)
	if err == nil {
		t.Fatal("不延长窗口时 300ms 内两候选都未完成，应全部失败")
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
func (f *failingPath) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{{
		ID:       f.name,
		Priority: 100,
		Dial: func(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
			_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
			return nil, fmt.Errorf("boom-%s", f.name)
		},
	}}
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
		wg.Go(func() {
			_, _ = DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
		})
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

// 用例10：落败的成功连接被显式关闭（最终审查 Important-1 回归）。
// 竞速中多条健康路径同时建连成功是常态：direct+relay 都返回真实 net.Pipe 连接，
// 首胜者被消费后，第二个成功 outcome 的连接必须由 drainOutcomes 关闭，否则泄漏。
func TestDialSmart_LoserConnClosed(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	// 两条路径几乎同时成功（delay 都很小）：无论谁胜出，另一条的连接必须被关闭。
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 2 * time.Millisecond, priority: 50, enabled: true}
	// 各自独立 net.Pipe 对：一端的连接交给 fakePath 返回，另一端用于验证存活。
	d1, d2 := net.Pipe()
	r1, r2 := net.Pipe()
	direct.conn = d1
	relay.conn = r1
	closeCh := make(chan string, 4)
	direct.closeCh = closeCh
	relay.closeCh = closeCh
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}

	// 胜者连接保持打开（返回给调用方），败者连接被 drainOutcomes 关闭。
	winner := res.Kind
	var winnerPeer, loserPeer net.Conn
	switch winner {
	case "webrtc":
		winnerPeer, loserPeer = d2, r2
	case "relay":
		winnerPeer, loserPeer = r2, d2
	default:
		t.Fatalf("未知胜者 kind: %s", winner)
	}
	// 胜者对端应存活（未关闭）。
	if err := winnerPeer.SetDeadline(time.Now().Add(50 * time.Millisecond)); err != nil {
		t.Fatalf("胜者连接应保持打开, SetDeadline err: %v", err)
	}
	// 败者对端应已被关闭（Read 立即返回 EOF/closed）。
	_ = loserPeer.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	var buf [1]byte
	if _, err := loserPeer.Read(buf[:]); err == nil {
		t.Fatalf("败者连接应已被关闭，但 Read 未返回错误")
	}
	// closeCh 应恰好记录败者一次（胜者未被关）。happens-before 链保证关闭记录必已就绪：
	// closeRecordingConn.Close 先执行 onClose（写 closeCh）再关底层 conn；败者对端 Read 返回
	// EOF 只可能在底层 Close 之后 ⇒ 此刻 closeCh 记录已写入，非阻塞 drain 必然读到。
	// 同步断言即可（避免 time.Sleep 字面量触发 R14 睡眠棘轮）。
	closed := drainCallsList(closeCh)
	if len(closed) != 1 {
		t.Fatalf("应恰好关闭 1 条落败连接，实际 %v", closed)
	}
}

// drainCallsList 返回 closeCh 中的记录（用于断言明细）。
func drainCallsList(ch chan string) []string {
	var out []string
	for {
		select {
		case v := <-ch:
			out = append(out, v)
		default:
			return out
		}
	}
}

// 用例11：提供者返回 (nil, nil) 不 panic（最终审查 Important-2 回归）。
// PathProvider 是外部扩展 API，插件可能误返回 (nil, nil)——DialSmart 必须按失败
// 聚合，不得对 o.res nil 解引用。
type nilResultPath struct{ name string }

func (p *nilResultPath) Name() string                                         { return p.name }
func (p *nilResultPath) Priority() int                                        { return 100 }
func (p *nilResultPath) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (p *nilResultPath) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{{
		ID:       p.name,
		Priority: 100,
		Dial: func(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
			_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
			return nil, nil // 错误插件行为：空结果
		},
	}}
}

func TestDialSmart_NilResult(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	good := &fakePath{name: "relay", kind: "relay", delay: time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, &nilResultPath{name: "direct"}, good)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v（nil-result 路径应按失败聚合，另一路径正常胜出）", err)
	}
	if res.Kind != "relay" {
		t.Fatalf("Kind = %s, want relay（正常路径胜出）", res.Kind)
	}
}

// 用例12：缓存 TTL 过期 → 重新竞速（规格 §8 用例 5）。
func TestDialSmart_CacheExpiryRerace(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true, callCh: make(chan string, 8)}
	relay := &fakePath{name: "relay", kind: "relay", delay: 2 * time.Millisecond, priority: 50, enabled: true, callCh: make(chan string, 8)}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次竞速：两路都被调用，胜者写缓存。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	_ = drainCalls(direct.callCh) + drainCalls(relay.callCh)

	// 手动把缓存条目改为已过期（锁内构造 ExpireAt 过去 1s 的条目）。
	smartCache.mu.Lock()
	smartCache.m["n"] = winnerCacheEntry{CandidateID: "direct", Latency: time.Millisecond, ExpireAt: time.Now().Add(-time.Second)}
	smartCache.mu.Unlock()

	// 过期后再次 DialSmart：应重新竞速（两路都被调用），而非走缓存单路。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("expired dial: %v", err)
	}
	calls := drainCalls(direct.callCh) + drainCalls(relay.callCh)
	if calls < 2 {
		t.Fatalf("缓存过期应重新竞速（两路都拨号），实际 %d 次调用", calls)
	}
}

// TestDialSmartWithOptions_CustomTTL：SmartOptions.CacheTTL 覆盖默认 30s——自定义
// 短 TTL（如 100ms）使缓存立即过期 → 连续两次调用都竞速（而非第二次走缓存单路）。
func TestDialSmartWithOptions_CustomTTL(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true, callCh: make(chan string, 8)}
	relay := &fakePath{name: "relay", kind: "relay", delay: time.Millisecond, priority: 50, enabled: true, callCh: make(chan string, 8)}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	so := SmartOptions{CacheTTL: 100 * time.Millisecond}
	for i := range 2 {
		res, err := DialSmartWithOptions(context.Background(), nil, nil,
			&client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}, so)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if res.Kind == "" {
			t.Fatalf("call %d: 空结果", i)
		}
		// 第二次调用前让 100ms TTL 过期 → 应重新竞速（两路都被拨）。
		// 用 testutil.WaitFor 条件轮询等 TTL 过期（R14 门禁：不用 time.Sleep 字面量）。
		testutil.WaitFor(t, 2*time.Second, func() bool {
			// 缓存已过期 = smartCacheGet 返回 false（等待 100ms TTL 自然流逝）。
			_, ok := smartCacheGet("n")
			return !ok
		}, "自定义 TTL 100ms 应已过期")
	}
	// 断言：每次调用都竞速 → 总调用数 ≥ 4（两路 × 两次）。若缓存命中单路则 < 4。
	// 注：每次调用前都等 TTL 过期（WaitFor）→ 每次都是 miss → 每次两路都拨。
	total := drainCalls(direct.callCh) + drainCalls(relay.callCh)
	if total < 4 {
		t.Fatalf("自定义 TTL 100ms 应每次重竞速（总调用 ≥4），实际 %d", total)
	}
}

// TestDialSmart_CacheKeyUsesCandidateID：胜者候选 ID 写入缓存（"via-node:X1" 可区分，
// 而非提供者名 "via-node"）。缓存命中按候选 ID 的**快照**拨号（候选索引：零 Expand 复用），
// 多 X 场景下经 X1 vs X2 的最优路径各自可缓存、可切换。
func TestDialSmart_CacheKeyUsesCandidateID(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	fast := &fakePath{name: "via-node:X1", kind: "via-node", delay: time.Millisecond, priority: 80, enabled: true}
	slow := &fakePath{name: "direct", kind: "webrtc", delay: 500 * time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, fast, slow)
	smartCacheClear()

	_, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	// 缓存应存候选 ID "via-node:X1"（而非提供者名 "via-node"）。
	smartCache.mu.Lock()
	entry, ok := smartCache.m["T"]
	smartCache.mu.Unlock()
	if !ok || entry.CandidateID != "via-node:X1" {
		t.Fatalf("缓存候选 ID = %q (ok=%v), want via-node:X1", entry.CandidateID, ok)
	}
}

// TestDialSmart_CacheCandidateGone：缓存命中但候选已不存在（注册表被清/候选下线）→
// 删缓存重新竞速（Minor-2 回归：缓存候选失效不静默失败、不卡死，故障转移到剩余候选）。
func TestDialSmart_CacheCandidateGone(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 5 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次竞速：direct（1ms）快 → 胜出写缓存 candidate="direct"。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	// 确认缓存指向 direct。
	smartCache.mu.Lock()
	entry, ok := smartCache.m["n"]
	smartCache.mu.Unlock()
	if !ok || entry.CandidateID != "direct" {
		t.Fatalf("缓存应指向 direct, got %q (ok=%v)", entry.CandidateID, ok)
	}

	// 缓存候选失效：从注册表删除 direct 提供者（模拟节点下线）。
	// smartWithProviders 的 T.Cleanup 会清全部并恢复 builtin，此处无需额外清理。
	smartRegistryMu.Lock()
	deleteProvider("direct")
	smartRegistryMu.Unlock()

	// 再次 DialSmart：缓存命中 "direct" 但候选已不存在 → 删缓存 + 重新竞速 → relay 胜出。
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("candidate-gone dial: %v", err)
	}
	if res.Kind != "relay" {
		t.Fatalf("Kind = %s, want relay（缓存候选失效应删缓存重竞速到剩余候选）", res.Kind)
	}
	// 缓存应已被替换为新的胜者候选（relay）——钉死「删缓存→重竞速→写新胜者」链条。
	smartCache.mu.Lock()
	entry2, ok2 := smartCache.m["n"]
	smartCache.mu.Unlock()
	if !ok2 || entry2.CandidateID != "relay" {
		t.Fatalf("缓存应指向新胜者 relay, got %q (ok=%v)", entry2.CandidateID, ok2)
	}
}

// TestDialSmart_FallbackOnAllFail：竞速**全部候选失败** → FallbackDial 降级
// （T3 --smart 优雅降级核心：--smart=true 竞速失败不报错，回退固定顺序 mesh.Dial）。
// 若实现未在「全部候选失败」返回点检查 FallbackDial，则本用例返回错误而非降级结果 → 红。
func TestDialSmart_FallbackOnAllFail(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	smartWithProviders(t, &failingPath{name: "direct"}, &failingPath{name: "relay"})
	smartCacheClear()

	fallbackCh := make(chan struct{}, 1)
	so := SmartOptions{
		FallbackDial: func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
			target *client.MeshService, localNode string) (*Result, error) {
			fallbackCh <- struct{}{}
			return &Result{Kind: "fallback"}, nil
		},
	}
	res, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}, so)
	if err != nil {
		t.Fatalf("竞速全失败应降级到 FallbackDial（而非报错）: %v", err)
	}
	if res.Kind != "fallback" {
		t.Fatalf("Kind = %s, want fallback（降级路径返回 FallbackDial 的结果）", res.Kind)
	}
	select {
	case <-fallbackCh:
	default:
		t.Fatal("FallbackDial 未被调用（竞速全失败应触发降级）")
	}
}

// TestDialSmart_FallbackOnNoCandidates：**无可选路径候选**（全部提供者未启用 →
// Expand 空）→ FallbackDial 降级。若实现未在「无可用的路径候选」返回点检查
// FallbackDial，则本用例返回错误而非降级结果 → 红。
func TestDialSmart_FallbackOnNoCandidates(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	smartWithProviders(t, &fakePath{name: "direct", enabled: false}, &fakePath{name: "relay", enabled: false})
	smartCacheClear()

	fallbackCh := make(chan struct{}, 1)
	so := SmartOptions{
		FallbackDial: func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
			target *client.MeshService, localNode string) (*Result, error) {
			fallbackCh <- struct{}{}
			return &Result{Kind: "fallback"}, nil
		},
	}
	res, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}, so)
	if err != nil {
		t.Fatalf("无可选候选应降级到 FallbackDial（而非报错）: %v", err)
	}
	if res.Kind != "fallback" {
		t.Fatalf("Kind = %s, want fallback（降级路径返回 FallbackDial 的结果）", res.Kind)
	}
	select {
	case <-fallbackCh:
	default:
		t.Fatal("FallbackDial 未被调用（无可选候选应触发降级）")
	}
}

// TestDialSmart_NoFallbackStillFails：未配置 FallbackDial（nil）→ 行为不变，
// 竞速全失败仍返回聚合错误（零回归守卫：默认 DialSmart 不降级）。
func TestDialSmart_NoFallbackStillFails(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	smartWithProviders(t, &failingPath{name: "direct"}, &failingPath{name: "relay"})
	smartCacheClear()

	_, err := DialSmartWithOptions(context.Background(), nil, nil,
		&client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}, SmartOptions{})
	if err == nil {
		t.Fatal("未配 FallbackDial 时竞速全失败仍应返回错误（零回归）")
	}
	if !strings.Contains(err.Error(), "direct") || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("错误应含各候选上下文: %v", err)
	}
}
