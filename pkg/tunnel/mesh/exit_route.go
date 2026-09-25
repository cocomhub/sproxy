// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// exit_route.go 提供「本地直连优先 → 回退出口」与「自动选出口」的路由装配：
//   - NewLocalOrExitDial：本地 net.Dialer 直连目标（有界超时），失败/超时回退注入的 exit 拨号闭包；
//   - NewAutoExitDial：本地直连失败后从 hub 节点列表自动选出口（outbound-dial 能力优先 +
//     exclude 排除名单，候选 failover），目标由出口节点拨号策略把关。
//
// 签名与 pkg/httpproxy.DialFunc / pkg/socks5.DialFunc 兼容（func(ctx, addr) (net.Conn, error)），
// 本包不 import pkg/httpproxy（R1 分层），仅靠签名一致。二期竞速升级（并行双候选 + TTL 缓存）
// 签名不变，见设计文档 §5。
package mesh

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// DefaultLocalDialTimeout 是本地直连探测默认超时（被墙 TCP 黑洞可感知的合理上界）。
const DefaultLocalDialTimeout = 3 * time.Second

// localDialFunc 是本地直连拨号函数（包级可注入，测试替换为慢桩模拟黑洞；
// 对齐 webrtc.SetSTUN 的包级全局模式）。
var localDialFunc = func(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

// result 是一次拨号候选的结果（raceDial 内部 channel 传递；包级供 closeLoserConn 用）。
type result struct {
	conn net.Conn
	err  error
}

// raceDial 并行竞速两个拨号候选，先成功者胜（取消另一个并回收其可能已建立的连接）。
// 竞速窗口 = localTimeout；local 用 localTimeout 超时叠加在 raceCtx 上（黑洞时超时失败），
// exit 用 raceCtx（无额外超时，但随胜出/调用方取消立即终止——保证 loser 无连接泄漏）。
// 两者都失败 → 聚合错误（含两者信息）。
func raceDial(localTimeout time.Duration,
	local, exit func(ctx context.Context, addr string) (net.Conn, error),
	ctx context.Context, addr string,
) (net.Conn, error) {
	// raceCtx 是两候选共享的可取消 ctx：胜出返回前取消，使 loser 拨号立即终止
	// （否则 local 胜出时 exit 仍继续拨号，成功的中继连接无人 Close 而泄漏）。
	raceCtx, raceCancel := context.WithCancel(ctx)
	defer raceCancel()
	localCtx, localCancel := context.WithTimeout(raceCtx, localTimeout)
	defer localCancel()
	localCh := make(chan result, 1)
	exitCh := make(chan result, 1)
	go func() {
		conn, err := local(localCtx, addr)
		localCh <- result{conn, err}
	}()
	go func() {
		conn, err := exit(raceCtx, addr)
		exitCh <- result{conn, err}
	}()
	// 先成功者胜：取消另一个（raceCancel 使 loser 拨号立即终止——RelayStream/
	// net.Dialer 均尊重 ctx 取消，loser 随后退出并写入 buffered channel，无泄漏）；
	// 非阻塞回收另一 channel 中已成功的 conn 并 Close（不阻塞等 loser 完成——
	// 否则竞速收益被「等慢 loser」抵消）。
	var localErr, exitErr error
	pending := 2
	for pending > 0 {
		select {
		case r := <-localCh:
			pending--
			if r.err == nil {
				raceCancel()
				closeLoserConn(exitCh)
				return r.conn, nil
			}
			localErr = r.err
		case r := <-exitCh:
			pending--
			if r.err == nil {
				raceCancel()
				closeLoserConn(localCh)
				return r.conn, nil
			}
			exitErr = r.err
		}
	}
	return nil, fmt.Errorf("本地与出口均失败: local: %v; exit: %v", localErr, exitErr)
}

// closeLoserConn 非阻塞回收败者 channel 中已建立的连接并 Close（防泄漏）。
// cancel 已使败者拨号终止；此处只处理「败者恰在胜出前已成功」的竞态窗口。
func closeLoserConn(ch chan result) {
	select {
	case r := <-ch:
		if r.err == nil && r.conn != nil {
			_ = r.conn.Close()
		}
	default:
	}
}

// NewLocalOrExitDial 构造「本地直连优先 → 回退出口」拨号函数。
// localTimeout 是本地直连探测超时（0 = 不试本地，直接 exit）；exit 为 nil 时退化为纯本地直连
// （等价 Config.Dial=nil，本机出口语义）。
// 本地被墙（黑洞挂起直到超时）时**并行竞速**：exit 无需等待 localTimeout 满，先成功者胜
// （收益：每新连接省下最多 localTimeout 的等待）。本地快时 local 立即胜出（零额外开销）。
func NewLocalOrExitDial(localTimeout time.Duration, exit func(ctx context.Context, addr string) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error) {
	// 构造时捕获一次包级 localDialFunc（不可变快照）：raceDial 会在子 goroutine 中调用
	// local 闭包，若闭包内再读包级变量，会与测试 t.Cleanup 恢复（Write）形成 data race
	// （CI Test Sub-Modules 实证：TestLocalOrExitDial_Race_ExitWinsWhileLocalBlackholed）。
	// 捕获后闭包只引用本快照，不再读包级状态。
	local := localDialFunc
	if exit == nil {
		return func(ctx context.Context, addr string) (net.Conn, error) {
			return local(ctx, addr)
		}
	}
	return func(ctx context.Context, addr string) (net.Conn, error) {
		if localTimeout > 0 {
			// 并行竞速：local 与 exit 同时拨，先成功者胜（被墙时 exit 不等 localTimeout）。
			return raceDial(localTimeout, local, exit, ctx, addr)
		}
		return exit(ctx, addr)
	}
}

// NewAutoExitDial 构造「本地直连优先 → 回退自动选出口」拨号函数。
// nodeLister 注入候选源（生产 = svc.ListHubNodes，测试 = 桩）；
// exitDialFor(nodeID) 构造经该节点的出口拨号闭包；
// exclude 是出口候选排除名单（精确 node-id 匹配命中跳过——被排除节点仍可被 SmartDial
// via-node 选为中转中间节点，「能中转但不出站」）。
// 候选判据：Capabilities 含 outbound-dial 优先；无则回落全部在线节点减 exclude。
// 顺序尝试候选（失败跳过下一个）；全部不可达才报错。

// ExitGroupMode 是出口节点组负载均衡模式（roadmap 11.1-④ 多出口负载均衡）。
// 模式只影响「每连接的起点选择」；无论模式都保留组内 failover 兜底
// （选中节点失败 → 循环尝试组内其余节点，覆盖全组）。
type ExitGroupMode string

const (
	ExitGroupFailover   ExitGroupMode = "failover"    // 按序 failover：恒首节点（默认，零回归）
	ExitGroupRoundRobin ExitGroupMode = "round-robin" // 轮询：自增 counter 取模选起点
	ExitGroupWeighted   ExitGroupMode = "weighted"    // 加权：权重扇区轮转选起点
)

// NormalizeExitGroupMode 归一 exit 组模式字符串（大小写不敏感）；未知值 → 错误
// （fail-closed，CLI 校验用——非法配置在启动期拦截，不静默回落）。
func NormalizeExitGroupMode(s string) (ExitGroupMode, error) {
	switch ExitGroupMode(strings.ToLower(s)) {
	case ExitGroupFailover:
		return ExitGroupFailover, nil
	case ExitGroupRoundRobin:
		return ExitGroupRoundRobin, nil
	case ExitGroupWeighted:
		return ExitGroupWeighted, nil
	default:
		return "", fmt.Errorf("exit-group: 未知模式 %q（支持 failover/round-robin/weighted）", s)
	}
}

// PickExitGroup 选出口节点组的起始节点下标（纯函数，可单测 + 变异验证）：
//   - failover → 恒 0（原按序行为）；
//   - round-robin → int(atomic.AddUint64(counter, 1)-1) % len(nodes)；
//   - weighted → 按权重扇区轮转（counter % total，确定性可测，避免 rand 不可测）。
//
// 权重语义：weights[i] 是 nodes[i] 的权重扇区大小；weights 缺失/长度不匹配/全零 →
// 等权回落（文档化，等同 round-robin）。len(nodes) == 0 → error（fail-closed）。
func PickExitGroup(mode ExitGroupMode, weights []int, counter *uint64, nodes []string) (int, error) {
	if len(nodes) == 0 {
		return 0, fmt.Errorf("exit-group: 节点组为空")
	}
	if counter == nil {
		return 0, fmt.Errorf("exit-group: counter 未注入")
	}
	switch mode {
	case ExitGroupFailover:
		return 0, nil
	case ExitGroupRoundRobin:
		n := atomic.AddUint64(counter, 1) - 1
		return int(n % uint64(len(nodes))), nil
	case ExitGroupWeighted:
		// 权重语义：weights[i] 是 nodes[i] 的权重扇区大小；
		// 长度不匹配/全零（≤0）→ 等权回落（等同 round-robin 轮转，Warn 由调用方负责）。
		total := 0
		if len(weights) == len(nodes) {
			for _, w := range weights {
				if w > 0 {
					total += w
				}
			}
		}
		if total == 0 {
			n := atomic.AddUint64(counter, 1) - 1
			return int(n % uint64(len(nodes))), nil
		}
		// 权重扇区轮转：counter 自增定位到 total 权重内位置，按扇区归属映射到节点下标。
		pos := int(atomic.AddUint64(counter, 1)-1) % total
		acc := 0
		for i := range nodes {
			if weights[i] <= 0 {
				continue
			}
			acc += weights[i]
			if pos < acc {
				return i, nil
			}
		}
		// 防御：理论上不可达（pos < total 且扇区覆盖 total）；回落首节点。
		return 0, nil
	default:
		return 0, fmt.Errorf("exit-group: 未知模式 %q", mode)
	}
}

// NewExitGroupDial 构造出口节点组拨号（roadmap P1 出口策略管理）：
// 组内按序尝试出口节点，首节点失败自动切下一个（组内 failover）。
// 兼容委托：等价 NewExitGroupDialWithMode(..., ExitGroupFailover, nil)——零回归。
// 仍经 NewLocalOrExitDial 包装——本地直连在 localTimeout 内成功则不出组。
// exitDialFor(nodeID) 返回该节点的出口拨号；组内全失败 → 最后一个错误传播（fail-closed）。
func NewExitGroupDial(localTimeout time.Duration, nodes []string, exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error)) func(ctx context.Context, addr string) (net.Conn, error) {
	return NewExitGroupDialWithMode(localTimeout, nodes, exitDialFor, ExitGroupFailover, nil)
}

// NewExitGroupDialWithMode 构造带负载均衡模式的出口节点组拨号（roadmap 11.1-④）。
// mode 只影响每连接的起点选择：
//   - failover → 恒首节点（默认，零回归）；
//   - round-robin → 闭包持 atomic counter 自增取模选起点（每连接轮转分发）；
//   - weighted → 权重扇区轮转选起点（weights 缺失/全零等权回落）。
//
// 无论模式：选中起点失败 → 从该起点开始循环尝试组内其余节点（覆盖全组）——
// round-robin/weighted 分发 + failover 兜底双语义，模式切换不牺牲可用性。
// 组内全失败 → 最后一个错误传播（fail-closed）。仍经 NewLocalOrExitDial 包装
// （本地直连在 localTimeout 内成功则不出组）。
func NewExitGroupDialWithMode(localTimeout time.Duration, nodes []string, exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error), mode ExitGroupMode, weights []int) func(ctx context.Context, addr string) (net.Conn, error) {
	// 每连接共享的起点选择 counter（race 安全；weighted 等权回落时 counter 轮转语义不变）。
	var counter uint64
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		if len(nodes) == 0 {
			return nil, fmt.Errorf("exit-group: 节点组为空")
		}
		if exitDialFor == nil {
			return nil, fmt.Errorf("exit-group: exitDialFor 未注入")
		}
		start, err := PickExitGroup(mode, weights, &counter, nodes)
		if err != nil {
			return nil, err
		}
		// 从起点开始循环尝试组内其余节点（无论模式都保留 failover 兜底）。
		var lastErr error
		for k := range nodes {
			nodeID := nodes[(start+k)%len(nodes)]
			conn, derr := exitDialFor(nodeID)(ctx, addr)
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr != nil {
			return nil, fmt.Errorf("exit-group: 全部出口节点不可达: %w", lastErr)
		}
		return nil, fmt.Errorf("exit-group: 无可用出口节点")
	}
	return NewLocalOrExitDial(localTimeout, exit)
}

func NewAutoExitDial(
	localTimeout time.Duration,
	nodeLister func(ctx context.Context) ([]client.HubNodeInfo, error),
	exitDialFor func(nodeID string) func(ctx context.Context, addr string) (net.Conn, error),
	exclude []string,
) func(ctx context.Context, addr string) (net.Conn, error) {
	exit := func(ctx context.Context, addr string) (net.Conn, error) {
		if nodeLister == nil {
			return nil, fmt.Errorf("auto-exit: 无候选源")
		}
		nodes, err := nodeLister(ctx)
		if err != nil {
			return nil, fmt.Errorf("auto-exit: 拉取节点列表失败: %w", err)
		}
		// 候选 = outbound-dial 能力优先，无则全部在线节点；再减排除名单。
		var candidates []client.HubNodeInfo
		for _, n := range nodes {
			if slices.Contains(exclude, n.ID) {
				continue
			}
			if slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
				candidates = append(candidates, n)
			}
		}
		if len(candidates) == 0 {
			for _, n := range nodes {
				if !slices.Contains(exclude, n.ID) {
					candidates = append(candidates, n)
				}
			}
		}
		var lastErr error
		for _, n := range candidates {
			if exitDialFor == nil {
				return nil, fmt.Errorf("auto-exit: exitDialFor 未注入")
			}
			conn, derr := exitDialFor(n.ID)(ctx, addr)
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr != nil {
			return nil, fmt.Errorf("auto-exit: 全部候选不可达: %w", lastErr)
		}
		return nil, fmt.Errorf("auto-exit: 无可用出口节点")
	}
	return NewLocalOrExitDial(localTimeout, exit)
}
