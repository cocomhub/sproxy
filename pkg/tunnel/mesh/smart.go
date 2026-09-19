// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// PathProvider 是 SmartDial 的一条候选路径实现（P1 直连 / P2 中继 / P3 网关 /
// P4..Pn 经中间节点多跳，各实现一个）。未来新增路径只需实现本接口并 Register。
type PathProvider interface {
	// Name 是路径唯一名（"direct" / "relay" / "gateway" / "via-node"）。
	Name() string
	// Dial 建立数据面连接并写好拨号帧（复用 mesh.Dial 的现有原语）。
	Dial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
	// Priority 竞速排序依据：高者优先（同优先级按注册顺序）。
	Priority() int
	// Enabled 条件启用：false 的提供者不参与竞速（如 P3 需 --gateway、P4 需候选中间节点）。
	Enabled(ctx context.Context, svc *client.FileClient) bool
}

// directProvider 实现 P1 直连（webrtc 打洞，不回落）。
type directProvider struct{}

func (directProvider) Name() string { return "direct" }
func (directProvider) Priority() int {
	return 100
}
func (directProvider) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (directProvider) Dial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, _ string, opts DialOptions) (*Result, error) {
	start := time.Now()
	if !SignalerUsable(signaler) || target.Node == "" {
		return nil, fmt.Errorf("direct: 无可用信令器或目标节点为空")
	}
	conn, err := DialWebRTC(ctx, signaler, target, opts.ICE)
	if err != nil {
		return nil, fmt.Errorf("direct: %w", err)
	}
	return &Result{Conn: conn, Kind: KindWebRTC, Latency: time.Since(start)}, nil
}

// relayProvider 实现 P2 中继（hub 中继流）。
type relayProvider struct{}

func (relayProvider) Name() string { return "relay" }
func (relayProvider) Priority() int {
	return 50
}
func (relayProvider) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (relayProvider) Dial(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
	target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	start := time.Now()
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	return &Result{Conn: conn, Kind: KindRelay, Latency: time.Since(start)}, nil
}

// SmartPathRegistry 是 SmartDial 的路径提供者注册表。
// builtin: direct(P1) + relay(P2)；外部插件可 Register 覆盖/新增（gateway(P3)、via-node(P4)）。
var SmartPathRegistry = plugin.New[PathProvider]("smart-path", builtinProviders())

// smartRegistryMu 串行化注册表修改（测试注入）与候选快照（DialSmart），
// 防止 t.Parallel 测试互改全局注册表造成竞态。
var smartRegistryMu sync.Mutex

// builtinProviders 返回内置兜底（direct + relay），对应现有 mesh.Dial 固定顺序。
func builtinProviders() PathProvider { return directProvider{} }

func init() {
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "direct", Instance: directProvider{}, Priority: 100})
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "relay", Instance: relayProvider{}, Priority: 50})
}

// winnerCacheEntry 是胜者缓存条目（key = 目标 node）。
// Provider 是胜出路径提供者的注册名（SmartPathRegistry.Get(Provider) 找回路径）。
type winnerCacheEntry struct {
	Provider string
	Latency  time.Duration
	ExpireAt time.Time
}

var smartCache = struct {
	mu sync.Mutex
	m  map[string]winnerCacheEntry
}{m: make(map[string]winnerCacheEntry)}

// smartOutcome 是单条候选路径的竞速结果（DialSmart 与 drainOutcomes 共享）。
type smartOutcome struct {
	name string
	res  *Result
	err  error
}

// smartCacheTTL 是胜者缓存有效期（抖动链路自适应：过期自动重新竞速）。
const smartCacheTTL = 30 * time.Second

// smartRaceWindow 是竞速窗口：超过此时长仍未胜出的候选不再等待。
const smartRaceWindow = 5 * time.Second

// smartCacheClear 仅测试用：清空胜者缓存。
func smartCacheClear() {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m = make(map[string]winnerCacheEntry)
}

// DialSmart 是 SmartDial 入口：缓存命中走缓存路径（单路），miss/过期并行竞速。
func DialSmart(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
	if target == nil || target.Node == "" {
		return nil, fmt.Errorf("smart dial: 目标节点为空")
	}
	// 1. 缓存命中 → 单路走缓存路径。
	if cached, ok := smartCacheGet(target.Node); ok {
		if p, ok := SmartPathRegistry.Get(cached.Provider); ok && p.Enabled(ctx, svc) {
			return p.Dial(ctx, svc, signaler, target, localNode, opts)
		}
		smartCacheDelete(target.Node) // 缓存路径失效 → 删缓存重新竞速
	}

	// 2. 收集候选：注册表中 Enabled 的提供者，按 Priority 降序，总候选 ≤ 4。
	// smartRegistryMu 串行化：避免并行测试的 smartWithProviders 在遍历 Names 中途改注册表。
	smartRegistryMu.Lock()
	type cand struct {
		p PathProvider
	}
	cands := make([]cand, 0, 4)
	for _, name := range SmartPathRegistry.Names() {
		if len(cands) >= 4 {
			break
		}
		p, ok := SmartPathRegistry.Get(name)
		if !ok || !p.Enabled(ctx, svc) {
			continue
		}
		cands = append(cands, cand{p: p})
	}
	smartRegistryMu.Unlock()
	if len(cands) == 0 {
		return nil, fmt.Errorf("smart dial: 无可用的路径提供者")
	}

	// 3. 并行竞速：每条候选 goroutine 独立 Dial，首胜者胜出，其余关闭。
	raceCtx, raceCancel := context.WithTimeout(ctx, smartRaceWindow)
	defer raceCancel()
	outCh := make(chan smartOutcome, len(cands))
	started := 0
	for _, c := range cands {
		p := c.p
		go func() {
			res, err := p.Dial(raceCtx, svc, signaler, target, localNode, opts)
			outCh <- smartOutcome{name: p.Name(), res: res, err: err}
		}()
		started++
	}

	var errs []error
	for range started {
		o := <-outCh
		if o.err != nil {
			// 聚合全部候选失败上下文（不丢任一候选名，便于排障）。
			errs = append(errs, fmt.Errorf("%s: %w", o.name, o.err))
			continue
		}
		// 首胜者：关闭其余（raceCancel 触发其余 goroutine 的 ctx 取消 + defer 关闭），
		// 写缓存（存提供者 Name，供缓存命中 Get 找回路径），返回。
		raceCancel()
		go drainOutcomes(outCh, started-1) // 收走其余结果，避免泄漏（goroutine 已由 raceCancel 结束）
		smartCacheSet(target.Node, o.name, o.res.Latency)
		return o.res, nil
	}
	return nil, fmt.Errorf("smart dial 全部候选失败: %w", errors.Join(errs...))
}

func smartCacheGet(node string) (winnerCacheEntry, bool) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	e, ok := smartCache.m[node]
	if !ok {
		return winnerCacheEntry{}, false
	}
	if time.Now().After(e.ExpireAt) {
		delete(smartCache.m, node)
		return winnerCacheEntry{}, false
	}
	return e, true
}

func smartCacheSet(node, provider string, latency time.Duration) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m[node] = winnerCacheEntry{Provider: provider, Latency: latency, ExpireAt: time.Now().Add(smartCacheTTL)}
}

func smartCacheDelete(node string) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	delete(smartCache.m, node)
}

// drainOutcomes 收走剩余竞速结果（goroutine 已因 raceCancel 结束，仅防 channel 泄漏）。
func drainOutcomes(ch chan smartOutcome, n int) {
	for range n {
		<-ch
	}
}
