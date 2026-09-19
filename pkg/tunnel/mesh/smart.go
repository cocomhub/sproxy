// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// PathProvider 是 SmartDial 的一条路径类型（P1 直连 / P2 中继 / P4 经中间节点多跳）。
// 通过 Expand 展开为竞速候选——一个类型可有多个候选（via-node 的每个中间节点 X）。
// 未来新增路径只需实现本接口并 Register。
type PathProvider interface {
	// Name 是路径唯一名（"direct" / "relay" / "via-node"）。
	Name() string
	// Priority 竞速排序依据：高者优先（同优先级按注册顺序）。
	Priority() int
	// Expand 展开该路径类型的全部候选（direct=1, relay=1, via-node=N 个 X）。
	// 条件不可用时返回 nil（如 via-node 无候选中间节点）。
	Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate
}

// Candidate 是竞速核心的最小单位：一条具体路径实例。
// ID 是候选唯一标识（缓存 key，如 "via-node:node-x" / "direct" / "relay"）。
type Candidate struct {
	ID       string
	Priority int // 继承提供者 Priority（排序/截断用），自包含
	Dial     func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
}

// EnabledProvider 是可选接口：提供者实现它可条件启用（如 --gateway 存在时 gateway
// 才启用）。未实现 = 恒启用（Expand 已是条件展开入口，恒启用提供者无需 Enabled）。
type EnabledProvider interface {
	Enabled(ctx context.Context, svc *client.FileClient) bool
}

// directProvider 实现 P1 直连（webrtc 打洞，不回落）。
type directProvider struct{}

func (directProvider) Name() string  { return "direct" }
func (directProvider) Priority() int { return 100 }
func (directProvider) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{{
		ID:       "direct",
		Priority: 100,
		Dial:     directDial,
	}}
}

// directDial 是直连候选的拨号函数（原 directProvider.Dial 逻辑）。
func directDial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
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

func (relayProvider) Name() string  { return "relay" }
func (relayProvider) Priority() int { return 50 }
func (relayProvider) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{{
		ID:       "relay",
		Priority: 50,
		Dial:     relayDial,
	}}
}

// relayDial 是中继候选的拨号函数（原 relayProvider.Dial 逻辑）。
func relayDial(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
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

// builtinProviders 是注册表清空时 plugin.New 的 Active() 兜底实现；
// direct/relay 的真实注册由 init() 完成（见下方 init）。
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

// smartCacheTTL 是胜者缓存默认有效期（抖动链路自适应：过期自动重新竞速）。
// 可由 SmartOptions.CacheTTL 覆盖（外部库可配置）。
const smartCacheTTL = 30 * time.Second

// smartRaceWindow 是竞速窗口默认值：超过此时长仍未胜出的候选不再等待。
// 可由 SmartOptions.RaceWindow 覆盖。
const smartRaceWindow = 5 * time.Second

// SmartOptions 是 SmartDial 的可配置参数（外部库复用入口）。
// 零值字段使用默认值（CacheTTL=30s / RaceWindow=5s / MaxCandidates=5），
// 与 DialSmart 默认行为一致。
type SmartOptions struct {
	// CacheTTL 是胜者缓存有效期；0 = 默认 30s。
	CacheTTL time.Duration
	// RaceWindow 是竞速窗口；0 = 默认 5s。
	RaceWindow time.Duration
	// MaxCandidates 是竞速候选数上限；0 = 默认 5（direct+relay+最多 3 个 via-node X）。
	MaxCandidates int
}

// smartOptionsOrDefault 把 SmartOptions 零值字段填默认值（与 DialSmart 一致）。
func smartOptionsOrDefault(so SmartOptions) SmartOptions {
	if so.CacheTTL == 0 {
		so.CacheTTL = smartCacheTTL
	}
	if so.RaceWindow == 0 {
		so.RaceWindow = smartRaceWindow
	}
	if so.MaxCandidates == 0 {
		so.MaxCandidates = 5
	}
	return so
}

// smartCacheClear 仅测试用：清空胜者缓存。
func smartCacheClear() {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m = make(map[string]winnerCacheEntry)
}

// DialSmart 是 SmartDial 入口（默认参数）：缓存命中走缓存路径（单路），miss/过期并行竞速。
// 等价于 DialSmartWithOptions(ctx, svc, signaler, target, localNode, opts, SmartOptions{})。
func DialSmart(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
	return DialSmartWithOptions(ctx, svc, signaler, target, localNode, opts, SmartOptions{})
}

// DialSmartWithOptions 是 DialSmart 的可配置版本：SmartOptions 控制缓存 TTL / 竞速
// 窗口 / 候选上限（零值=默认）。外部库复用入口——需要调参时用它，默认行为走 DialSmart。
func DialSmartWithOptions(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, localNode string, opts DialOptions, so SmartOptions) (*Result, error) {
	so = smartOptionsOrDefault(so)
	if target == nil || target.Node == "" {
		return nil, fmt.Errorf("smart dial: 目标节点为空")
	}
	// 1. 缓存命中 → 单路走缓存路径（按候选 ID 找回）。
	if cached, ok := smartCacheGet(target.Node); ok {
		if cand := smartCandidateByID(ctx, svc, target, cached.Provider); cand != nil {
			res, derr := cand.Dial(ctx, svc, signaler, target, localNode, opts)
			if derr == nil {
				return res, nil
			}
			// 缓存路径瞬时故障：删缓存 → 落到下方重新竞速（其余候选参与故障转移，
			// 避免 TTL 窗口内持续失败）。derr 仅记录诊断——聚合上下文由重竞速中
			// 该提供者再次失败补回（下方竞速的 errs 聚合会带上本次失败）。
			smartCacheDelete(target.Node)
			slog.Debug("smart dial 缓存路径失败，删缓存重新竞速", "provider", cached.Provider, "error", derr, "target_node", target.Node)
		} else {
			smartCacheDelete(target.Node) // 缓存路径失效 → 删缓存重新竞速
		}
	}

	// 2. 收集候选：遍历提供者 → Expand 展开全部候选（direct=1, relay=1, via-node=N）。
	// smartRegistryMu 串行化：避免并行测试的 smartWithProviders 在遍历 Names 中途改注册表。
	smartRegistryMu.Lock()
	cands := make([]Candidate, 0, so.MaxCandidates)
	for _, name := range SmartPathRegistry.Names() {
		p, ok := SmartPathRegistry.Get(name)
		if !ok {
			continue
		}
		if ep, ok := p.(EnabledProvider); ok && !ep.Enabled(ctx, svc) {
			continue // 条件提供者未启用
		}
		cands = append(cands, p.Expand(ctx, svc, target)...)
	}
	smartRegistryMu.Unlock()
	// 显式按 Candidate.Priority 降序排序（与注释一致）：高优先候选先进入截断窗口，
	// 高优先者先参与竞速；候选数超上限时优先保留高优先候选。
	// 同优先级不区分先后——竞速结果由 RTT 决定，与收集顺序无关。
	slices.SortFunc(cands, func(a, b Candidate) int { return b.Priority - a.Priority })
	if len(cands) > so.MaxCandidates {
		cands = cands[:so.MaxCandidates]
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("smart dial: 无可用的路径候选")
	}

	// 3. 并行竞速：每条候选 goroutine 独立 Dial，首胜者胜出，其余关闭。
	raceCtx, raceCancel := context.WithTimeout(ctx, so.RaceWindow)
	defer raceCancel()
	outCh := make(chan smartOutcome, len(cands))
	started := 0
	for _, c := range cands {
		go func() {
			res, err := c.Dial(raceCtx, svc, signaler, target, localNode, opts)
			outCh <- smartOutcome{name: c.ID, res: res, err: err}
		}()
		started++
	}

	var errs []error
	for range started {
		o := <-outCh
		if o.err != nil || o.res == nil {
			// 失败或返回空结果（外部插件可能返回 (nil, nil)——nil 解引用会 panic）
			// 均按失败聚合上下文，不丢候选名。
			if o.err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", o.name, o.err))
			} else {
				errs = append(errs, fmt.Errorf("%s: 返回空结果", o.name))
			}
			continue
		}
		// 首胜者：关闭其余（raceCancel 触发其余 goroutine 的 ctx 取消 + defer 关闭），
		// 写缓存（存候选 ID，供缓存命中按 ID 找回路径），返回。
		raceCancel()
		// drain 只收胜者之后仍在途的发送：胜者已消费 1 个，错误 outcome 已消费
		// len(errs) 个，剩余在途 = started-1-len(errs)。若按 started-1 读会在空
		// channel 上永久阻塞泄漏 goroutine（错误 outcome 先于胜者到达是故障转移
		// 的常态路径）。
		if n := started - 1 - len(errs); n > 0 {
			go drainOutcomes(outCh, n)
		}
		smartCacheSet(target.Node, o.name, o.res.Latency, so.CacheTTL) // o.name = 候选 ID
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

// smartCandidateByID 按候选 ID 从注册表所有提供者的 Expand 结果中找回候选
// （缓存命中路径：存的是候选 ID 如 "via-node:node-x"，需重新展开才能找回）。
// 返回 nil 表示候选不存在（缓存失效，调用方删缓存重新竞速）。
func smartCandidateByID(ctx context.Context, svc *client.FileClient, target *client.MeshService, id string) *Candidate {
	smartRegistryMu.Lock()
	defer smartRegistryMu.Unlock()
	for _, name := range SmartPathRegistry.Names() {
		p, ok := SmartPathRegistry.Get(name)
		if !ok {
			continue
		}
		if ep, ok := p.(EnabledProvider); ok && !ep.Enabled(ctx, svc) {
			continue
		}
		for _, c := range p.Expand(ctx, svc, target) {
			if c.ID == id {
				return &c
			}
		}
	}
	return nil
}

func smartCacheSet(node, provider string, latency, ttl time.Duration) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m[node] = winnerCacheEntry{Provider: provider, Latency: latency, ExpireAt: time.Now().Add(ttl)}
}

func smartCacheDelete(node string) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	delete(smartCache.m, node)
}

// drainOutcomes 收走剩余竞速结果（goroutine 已因 raceCancel 结束，仅防 channel 泄漏），
// 并**显式关闭落败的成功连接**（规格 §7：不泄漏 webrtc PeerConnection / relay 流——
// 竞速中多条健康路径同时建连成功是常态，首胜者被消费后其余连接必须释放）。
func drainOutcomes(ch chan smartOutcome, n int) {
	for range n {
		o := <-ch
		if o.err == nil && o.res != nil && o.res.Conn != nil {
			_ = o.res.Conn.Close()
		}
	}
}

// DialSmartDefault 是 DialSmart 的 5 参便捷包装（无 opts/SmartOptions），供 CLI 等
// 调用点直接作 meshDialFunc 使用（免包适配闭包）。等价于 DialSmart(ctx, svc, signaler,
// target, localNode, DialOptions{})。
func DialSmartDefault(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, localNode string) (*Result, error) {
	return DialSmart(ctx, svc, signaler, target, localNode, DialOptions{})
}
