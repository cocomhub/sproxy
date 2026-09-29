// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
// ID 是候选唯一标识（缓存 key，如 "via-relay:node-x" / "via-direct:node-x" / "direct" / "relay"）。
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
	conn, err := DialWebRTC(ctx, signaler, target, opts.ICE, opts.E2E)
	if err != nil {
		return nil, fmt.Errorf("direct: %w", err)
	}
	return &Result{Conn: conn, Kind: KindWebRTC, EndToEnd: opts.E2E != nil, Latency: time.Since(start)}, nil
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
	target *client.MeshService, _ string, opts DialOptions) (*Result, error) {
	// 复用 dialRelay（含 E2E 分支）——单一中继入口，避免多套实现漂移。
	return dialRelay(ctx, svc, target, opts)
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
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "via-node", Instance: &viaNodeProvider{}, Priority: 80})
}

// registerProvider 注册提供者并递增注册表代次（缓存快照一致性：Register/Delete
// 都改变候选集合，gen 变化使缓存 miss 强制重新竞速）。调用方须持 smartRegistryMu。
func registerProvider(p plugin.Plugin[PathProvider]) {
	SmartPathRegistry.Register(p)
	smartRegistryGen.Add(1)
}

// deleteProvider 删除提供者并递增注册表代次（同上）。调用方须持 smartRegistryMu。
func deleteProvider(name string) {
	SmartPathRegistry.Delete(name)
	smartRegistryGen.Add(1)
}

// winnerCacheEntry 是胜者缓存条目（key = 目标 node）。
// CandidateID 是胜出**候选 ID**（如 "via-relay:node-x" / "via-direct:node-x" / "direct" / "relay"，非提供者注册名）；
// Snapshot 是胜出时刻的候选快照（候选展开模型：一个提供者可展开多个候选，命中时**直接复用
// 快照拨号**——零 Expand、零 ListHubNodes 网络往返，TTL 内纯内存）。
// RegistryGen 是写入时注册表代次：命中时若代次未变（注册表无 Register/Delete）则快照仍有效；
// 代次变化（运行期插件注册/替换）→ 快照可能过期，删缓存重新竞速。
type winnerCacheEntry struct {
	CandidateID string
	Snapshot    *Candidate
	RegistryGen uint64
	Latency     time.Duration
	ExpireAt    time.Time
}

// smartRegistryGen 是注册表代次：每次 Register/Delete / 白名单变化递增。
// 缓存快照据此判定是否过期（gen 未变 = 注册表未动，快照仍有效，零 Expand 复用）。
//
// atomic 而非锁内读写：smartCacheGet（竞速入口，未持 smartRegistryMu）与
// SetTrustedNodes（smartRegistryMu 内写）分属两把锁，锁内互斥无法覆盖跨锁读写；
// atomic 保证无 data race（CI Test Sub-Modules 实证：via_node_test.go:284 读 vs
// via_node.go:54 写冲突）。
var smartRegistryGen atomic.Uint64

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

// smartRace 是一次竞速的共享拨号上下文：svc/signaler/target/localNode/opts/so 在缓存
// 命中、候选收集、并行竞速与胜者收尾各阶段原样透传，避免逐函数高参透传。
type smartRace struct {
	svc       *client.FileClient
	signaler  webrtc.Signaler
	target    *client.MeshService
	localNode string
	opts      DialOptions
	so        SmartOptions
}

// raceCtrl 是竞速循环的收尾控制句柄：胜者确定后取消其余候选、消费在途结果并写胜者缓存。
type raceCtrl struct {
	outerCancel, baseCancel context.CancelFunc
	cacheTTL                time.Duration
	started, errs           int
	outCh                   chan smartOutcome
}

// smartCacheTTL 是胜者缓存默认有效期（抖动链路自适应：过期自动重新竞速）。
// 可由 SmartOptions.CacheTTL 覆盖（外部库可配置）。
const smartCacheTTL = 30 * time.Second

// smartRaceWindow 是竞速窗口默认值：超过此时长仍未胜出的候选不再等待。
// 可由 SmartOptions.RaceWindow 覆盖。
const smartRaceWindow = 5 * time.Second

// smartMultihopRaceExtend 是多跳候选竞速窗口的默认额外延长：多跳（via-node /
// via-direct）需要比单跳候选更长的竞速窗口（打洞 + X 出口拨号 + 结果帧往返），
// 统一 5s RaceWindow 会让「慢但最终更快」的多跳被提前放弃（短路径系统性偏袒）。
// 默认在 RaceWindow 基础上加倍（多跳 = 基础窗口 × (1+MultihopRaceExtend)）。
// 可由 SmartOptions.MultihopRaceExtend 覆盖（零值 = 本默认；负值钳 0 = 不延长）。
const smartMultihopRaceExtend = 1.0

// SmartOptions 是 SmartDial 的可配置参数（外部库复用入口）。
// 零值字段使用默认值（CacheTTL=30s / RaceWindow=5s / MaxCandidates=5），
// 与 DialSmart 默认行为一致。
type SmartOptions struct {
	// CacheTTL 是胜者缓存有效期；0 = 默认 30s。
	CacheTTL time.Duration
	// RaceWindow 是竞速窗口；0 = 默认 5s。
	RaceWindow time.Duration
	// MaxCandidates 是竞速候选数上限；0 = 默认 8（direct+relay+最多 3 个 via-node X × 双候选）。
	MaxCandidates int
	// MultihopRaceExtend 是多跳候选竞速窗口额外延长倍数（**零值 = 默认 1.0 加倍**；
	// 显式 0 无法与未设置区分，负值钳 0 = 不延长）。
	// 多跳（via-node/via-direct）候选在基础 RaceWindow 内未胜出时，等待窗口延长
	// 至 RaceWindow × (1+MultihopRaceExtend)——避免「慢但最终更快」的多跳被提前放弃
	// （打洞 + X 出口拨号 + 结果帧往返需更长时间）。
	MultihopRaceExtend float64
	// FallbackDial 是竞速**全部候选失败 / 无可选路径**时的降级拨号（T3 --smart 优雅降级）。
	// 非 nil 时，竞速失败（含无可选路径）回退到该拨号函数并返回其结果——连接仍可用，
	// 而非向调用方报错；nil（默认）保持原行为（竞速失败返回聚合错误，零回归）。
	// CLI 装配传 mesh.Dial（固定顺序 webrtc→relay），使 --smart=true 在竞速失败时
	// 回退到固定顺序而非报错。
	FallbackDial func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
		target *client.MeshService, localNode string) (*Result, error)
	// TrustedNodes 是 via-node 中间节点白名单（--trust-x，T5 信任收敛）：非空时
	// via-node 竞速只选白名单内的 X（白名单外节点即使声明 outbound-dial 也不选——
	// X 是经手中转、可观测流量的节点，白名单 = 显式信任声明）；空 = 全部可信（兼容现状）。
	TrustedNodes []string
	// QualityRouting 是传输质量感知选路开关（roadmap 5.3 P1；默认 false 零回归）。
	// 开启后 SmartDial 竞速候选按历史质量（重传率）预排序：质量分高者先启动、
	// 同 RTT 时质量高者先胜；无历史候选中性（不歧视）。质量数据来自
	// RegisterQualitySource 注册的 mux 指标源（#430 计数器聚合）。
	// 显式开关（用户确认铁律）：默认关 = 纯延迟竞速行为不变。
	QualityRouting bool
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
		so.MaxCandidates = 8
	}
	// MultihopRaceExtend 零值 = 默认加倍（>0 覆盖；负数钳 0）。
	// 注意：0 与未设置无法区分（float64 零值）——语义定为「0 = 不延长」会丢失默认
	// 加倍。故零值统一填默认 smartMultihopRaceExtend（外部库显式传 0 也按默认加倍，
	// 避免意外关闭多跳保护；如需不延长可传负数（钳 0）。文档对齐此语义。
	if so.MultihopRaceExtend == 0 {
		so.MultihopRaceExtend = smartMultihopRaceExtend
	}
	if so.MultihopRaceExtend < 0 {
		so.MultihopRaceExtend = 0
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
	r := &smartRace{svc: svc, signaler: signaler, target: target, localNode: localNode, opts: opts, so: so}
	// 1. 缓存命中 → 单路走缓存路径（候选索引：快照直接复用，零 Expand）。
	if res, handled, derr := r.tryCache(ctx); handled {
		return res, derr
	}
	// 2. 收集候选：遍历提供者 → Expand 展开全部候选（direct=1, relay=1, via-node=N）。
	cands, err := collectSmartCandidates(ctx, svc, target, so)
	if err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		// T3 --smart 优雅降级：无可选路径（全部提供者未启用/Expand 空）→
		// 配置了 FallbackDial 则降级到固定顺序，而非向调用方报错。
		if so.FallbackDial != nil {
			slog.Debug("smart dial 无可选路径，降级到 FallbackDial", "target_node", target.Node)
			return so.FallbackDial(ctx, svc, signaler, target, localNode)
		}
		return nil, fmt.Errorf("smart dial: 无可用的路径候选")
	}
	// 3. 并行竞速：每条候选 goroutine 独立 Dial，首胜者胜出，其余关闭。
	return raceSmartCandidates(ctx, r, cands)
}

// tryCache 尝试缓存路径（候选索引核心）：命中且快照有效时直接复用快照拨号，
// 零 Expand。返回 handled=true 表示调用方应返回 (res, derr)。
func (r *smartRace) tryCache(ctx context.Context) (*Result, bool, error) {
	cached, ok := smartCacheGet(r.target.Node)
	if !ok {
		return nil, false, nil
	}
	// 快照拨号前不重新 Expand（候选索引核心）：gen 未变 = 注册表未动，胜出时刻的
	// 快照仍有效——via-node 的 Expand 每次 ListHubNodes HTTP 往返，命中复用快照
	// 使「TTL 内纯内存复用」对 via-node 也成立（审查 Minor-3）。
	if cand := cached.Snapshot; cand != nil && cand.Dial != nil {
		res, derr := cand.Dial(ctx, r.svc, r.signaler, r.target, r.localNode, r.opts)
		if derr == nil {
			return res, true, nil
		}
		// 缓存路径瞬时故障：删缓存 → 落到下方重新竞速（其余候选参与故障转移，
		// 避免 TTL 窗口内持续失败）。derr 仅记录诊断——聚合上下文由重竞速中
		// 该提供者再次失败补回（下方竞速的 errs 聚合会带上本次失败）。
		smartCacheDelete(r.target.Node)
		slog.Debug("smart dial 缓存路径失败，删缓存重新竞速", "provider", cached.CandidateID, "error", derr, "target_node", r.target.Node)
	} else {
		smartCacheDelete(r.target.Node) // 快照缺失（不应发生，防御）→ 删缓存重新竞速
	}
	return nil, false, nil
}

// collectSmartCandidates 收集竞速候选：遍历提供者 Expand 展开全部候选，注入
// --trust-x 白名单，按 Candidate.Priority 降序排序（质量感知选路二次预排序）并
// 截断到 MaxCandidates 上限。返回 (cands, err)——err 当前恒 nil，保留以利扩展。
func collectSmartCandidates(ctx context.Context, svc *client.FileClient, target *client.MeshService, so SmartOptions) ([]Candidate, error) {
	// smartRegistryMu 串行化：避免并行测试的 smartWithProviders 在遍历 Names 中途改注册表。
	smartRegistryMu.Lock()
	// T5 --trust-x：把 SmartOptions.TrustedNodes 注入 via-node 提供者（锁内，与注册表
	// 修改同锁防并行竞态）。白名单约束「竞速时选哪些中间节点 X」——空 = 全部可信（零回归）。
	if vp, ok := SmartPathRegistry.Get("via-node"); ok {
		if v, ok := vp.(*viaNodeProvider); ok {
			v.SetTrustedNodes(so.TrustedNodes)
		} else {
			// 类型断言失败（外部插件覆盖了 via-node 注册）：白名单无法注入。
			// 不 fail-closed——生产 init 注册保证类型正确（&viaNodeProvider{}），
			// Warn 足够定位；若外部替换则其 Expand 自行负责信任语义。
			slog.Warn("via-node 提供者类型异常，无法注入 --trust-x 白名单", "type", fmt.Sprintf("%T", vp))
		}
	}
	cands := make([]Candidate, 0, so.MaxCandidates)
	for _, name := range SmartPathRegistry.Names() {
		p, ok := SmartPathRegistry.Get(name)
		if !ok {
			continue
		}
		if ep, ok := p.(EnabledProvider); ok && !ep.Enabled(ctx, svc) {
			continue // 条件提供者未启用
		}
		for _, c := range p.Expand(ctx, svc, target) {
			if c.Dial == nil {
				continue // 外部插件可能构造 nil Dial（防 panic，fail-closed 跳过）
			}
			cands = append(cands, c)
		}
	}
	smartRegistryMu.Unlock()
	return sortAndTruncateCandidates(cands, so), nil
}

// sortAndTruncateCandidates 对竞速候选排序并截断：显式按 Candidate.Priority 降序
// （高优先候选先进入截断窗口、先参与竞速），质量感知选路（QualityRouting 开启时）
// 二次预排序（同 Priority 内健康者先启动），并按 MaxCandidates 截断。
func sortAndTruncateCandidates(cands []Candidate, so SmartOptions) []Candidate {
	// 显式按 Candidate.Priority 降序排序（与注释一致）：高优先候选先进入截断窗口，
	// 高优先者先参与竞速；候选数超上限时优先保留高优先候选。
	// 同优先级不区分先后——竞速结果由 RTT 决定，与收集顺序无关。
	slices.SortFunc(cands, func(a, b Candidate) int { return b.Priority - a.Priority })
	// 质量感知选路（roadmap 5.3 P1，QualityRouting 显式开关默认关）：
	// 开启后按候选历史质量分**二次预排序**（同 Priority 内健康者先启动）——
	// 质量差的候选延迟启动（竞速窗口内晚加入，健康候选先完成），同 RTT 时健康者先胜。
	// 无历史候选中性（Score=0.5）与健康同权，不歧视首次候选。
	if so.QualityRouting && len(cands) > 1 {
		logQualityWeighting(cands, true)
		slices.SortStableFunc(cands, func(a, b Candidate) int {
			// 先按 Priority 降序（保持主序），再按质量分降序（健康候选优先）。
			if a.Priority != b.Priority {
				return b.Priority - a.Priority
			}
			_, sa := qualitySortKey(a.ID)
			_, sb := qualitySortKey(b.ID)
			if sa != sb {
				if sa > sb {
					return -1
				}
				return 1
			}
			return 0
		})
	}
	if len(cands) > so.MaxCandidates {
		cands = cands[:so.MaxCandidates]
	}
	return cands
}

// raceSmartCandidates 并行竞速全部候选：每条候选 goroutine 独立 Dial，首胜者胜出，
// 其余关闭。多跳候选（候选 ID 前缀 "via-"）竞速窗口按 MultihopRaceExtend 延长——
// 单跳在基础窗口到期被 ctx 取消自然失败（不再等待），多跳活到延长窗口完成。
// 全部候选失败 → 回落 FallbackDial（配置了时）或聚合错误（零回归）。
func raceSmartCandidates(ctx context.Context, r *smartRace, cands []Candidate) (*Result, error) {
	// 多跳候选（via-node/via-direct）竞速窗口延长语义：基础窗口内多跳未胜出时
	// **不立即放弃**，等待延长窗口（单跳候选在基础窗口到期即被 ctx 取消——不再等待）。
	// 实现：**分层 deadline**——单跳候选拿 baseRaceCtx（RaceWindow），多跳候选
	// （候选 ID 前缀 "via-"）拿 extendedRaceCtx（RaceWindow × (1+Extend)）。
	raceExtend := r.so.MultihopRaceExtend
	if raceExtend < 0 {
		raceExtend = 0
	}
	raceWin := r.so.RaceWindow + time.Duration(float64(r.so.RaceWindow)*raceExtend)
	// 总窗口 ctx（兜底：所有候选最终都受它约束，防单跳 ctx 泄漏到总窗口外）。
	outerCtx, outerCancel := context.WithTimeout(ctx, raceWin)
	defer outerCancel()
	// 单跳基础窗口 ctx（多跳候选不用它——多跳拿外层 raceWin ctx）。
	baseCtx, baseCancel := context.WithTimeout(ctx, r.so.RaceWindow)
	defer baseCancel()
	outCh := make(chan smartOutcome, len(cands))
	// 每条候选 goroutine 独立 Dial；质量感知选路（QualityRouting 开启时）对劣化候选
	// 延迟启动（健康候选先跑先胜出）。返回实际启动的候选数。
	started := spawnRaceCandidates(r, cands, baseCtx, outerCtx, outCh)

	// 收尾控制句柄：胜者确定后取消其余候选、消费在途结果并写胜者缓存。
	ctrl := &raceCtrl{outerCancel: outerCancel, baseCancel: baseCancel,
		cacheTTL: r.so.CacheTTL, started: started, outCh: outCh}
	var errs []error
	for range started {
		o := <-outCh
		if o.err != nil || o.res == nil {
			// 失败或返回空结果（外部插件可能返回 (nil, nil)——nil 解引用会 panic）
			// 均按失败聚合上下文，不丢候选名。
			errs = append(errs, raceOutcomeError(o))
			continue
		}
		ctrl.errs = len(errs)
		ctrl.finish(r, cands, o)
		return o.res, nil
	}
	res, rerr := fallbackOrErr(r.so, ctx, r.svc, r.signaler, r.target, r.localNode,
		fmt.Errorf("smart dial 全部候选失败: %w", errors.Join(errs...)))
	return res, rerr
}

// raceOutcomeError 把一条失败/空结果竞速结果转成带候选名的聚合错误。
func raceOutcomeError(o smartOutcome) error {
	if o.err != nil {
		return fmt.Errorf("%s: %w", o.name, o.err)
	}
	return fmt.Errorf("%s: 返回空结果", o.name)
}

// finish 处理胜者（原 endRaceWithWinner）：回填质量、取消其余候选、消费在途结果并写
// 胜者缓存。drain 只收胜者之后仍在途的发送：胜者已消费 1 个，错误 outcome 已消费 errs 个，
// 剩余在途 = started-1-errs。若按 started-1 读会在空 channel 上永久阻塞泄漏
// goroutine（错误 outcome 先于胜者到达是故障转移的常态路径）。
func (c *raceCtrl) finish(r *smartRace, cands []Candidate, o smartOutcome) {
	// 质量回填：胜者候选 ID + 其 Result 提取 mux 注册质量源（下次竞速带历史加权）。
	registerWinnerQuality(o.name, o.res)
	c.outerCancel()
	c.baseCancel()
	if n := c.started - 1 - c.errs; n > 0 {
		go drainOutcomes(c.outCh, n)
	}
	// 胜者 o.name 是候选 ID（cands 中找快照）；快照供缓存命中直接复用（零 Expand）。
	var snapshot *Candidate
	for i := range cands {
		if cands[i].ID == o.name {
			snapshot = &cands[i]
			break
		}
	}
	smartCacheSet(r.target.Node, o.name, snapshot, o.res.Latency, c.cacheTTL)
}

// spawnRaceCandidates 为每条候选启动竞速 goroutine，返回实际启动数。
// 单跳候选拿 baseCtx（RaceWindow）、多跳候选（ID 前缀 "via-"）拿 outerCtx
// （RaceWindow × (1+Extend)，活到延长窗口完成）；质量感知选路（QualityRouting 开启时）
// 对劣化候选（质量分 < 0.9）**延迟启动**（等健康候选先跑——健康者先胜出，劣化者不抢先；
// 无历史（中性）不延迟，不歧视首次候选）。
func spawnRaceCandidates(r *smartRace, cands []Candidate, baseCtx, outerCtx context.Context, outCh chan smartOutcome) int {
	started := 0
	for _, c := range cands {
		// 单跳 vs 多跳：多跳候选 ID 前缀 "via-"（via-node / via-direct）。
		candCtx := baseCtx
		if strings.HasPrefix(c.ID, "via-") {
			candCtx = outerCtx
		}
		// 质量感知选路（QualityRouting 开启时）：同 Priority 候选按质量分降序排列后，
		// 劣化候选**延迟启动**（等健康候选先跑——健康者先胜出，劣化者不抢先）。
		// 实现：对分数低于健康的候选加启动延迟（如 50ms，小于 RaceWindow 不误伤多跳），
		// 使健康候选先完成胜出。无历史（中性）不延迟，不歧视首次候选。
		if r.so.QualityRouting {
			_, sc := qualitySortKey(c.ID)
			if sc < 0.9 { // 劣化（重传率高）候选延迟启动
				cc := candCtx
				c := c
				go func() {
					select {
					case <-time.After(qualityStaggerDelay):
						res, err := c.Dial(cc, r.svc, r.signaler, r.target, r.localNode, r.opts)
						outCh <- smartOutcome{name: c.ID, res: res, err: err}
					case <-cc.Done():
						outCh <- smartOutcome{name: c.ID, err: cc.Err()}
					}
				}()
				started++
				continue
			}
		}
		go func() {
			res, err := c.Dial(candCtx, r.svc, r.signaler, r.target, r.localNode, r.opts)
			outCh <- smartOutcome{name: c.ID, res: res, err: err}
		}()
		started++
	}
	return started
}

// fallbackOrErr 是竞速失败返回点的统一降级出口（T3 --smart 优雅降级）：
// 配置了 FallbackDial（非 nil）→ 记录 Debug 日志并调用其返回结果（连接仍可用）；
// 未配置 → 原样返回传入的聚合错误（零回归，默认 DialSmart 不降级）。
func fallbackOrErr(so SmartOptions, ctx context.Context, svc *client.FileClient,
	signaler webrtc.Signaler, target *client.MeshService, localNode string,
	raceErr error) (*Result, error) {
	if so.FallbackDial == nil {
		return nil, raceErr
	}
	slog.Debug("smart dial 竞速全部候选失败，降级到 FallbackDial",
		"target_node", target.Node, "error", raceErr)
	return so.FallbackDial(ctx, svc, signaler, target, localNode)
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
	// 注册表代次变化（Register/Delete 递增 gen）→ 快照可能过期（提供者集合已变），
	// 删缓存视为 miss，迫使调用方重新竞速（候选索引的一致性闸门）。
	if e.RegistryGen != smartRegistryGen.Load() {
		delete(smartCache.m, node)
		return winnerCacheEntry{}, false
	}
	return e, true
}

// smartCacheSet 写胜者缓存（存候选快照 + 注册表代次；命中时零 Expand 复用快照）。
func smartCacheSet(node, candidateID string, snapshot *Candidate, latency, ttl time.Duration) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m[node] = winnerCacheEntry{
		CandidateID: candidateID,
		Snapshot:    snapshot,
		RegistryGen: smartRegistryGen.Load(), // atomic 读（缓存快照代次一致性）
		Latency:     latency,
		ExpireAt:    time.Now().Add(ttl),
	}
}

func smartCacheDelete(node string) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	delete(smartCache.m, node)
}

// drainOutcomes 收走剩余竞速结果（goroutine 已因 outerCancel/baseCancel 结束，仅防
// channel 泄漏），并**显式关闭落败的成功连接**（规格 §7：不泄漏 webrtc
// PeerConnection / relay 流——竞速中多条健康路径同时建连成功是常态，首胜者被消费后
// 其余连接必须释放）。
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
