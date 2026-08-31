// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"
)

// MeshTargetTTL 是 mesh 服务解析缓存的新鲜窗口。过期后下一次 Resolve 触发重新
// 拉取 /api/hub/services，使 relay 节点下线/重上线的变化被 mesh 感知。
const MeshTargetTTL = 3 * time.Second

// MeshResolveTimeout 是单次服务解析的网络超时，防止 hub 无响应拖住连接建立。
const MeshResolveTimeout = 5 * time.Second

// MeshFailCooldown 是失败节点的跳过冷却窗口（审查 Important #1）。
// 节点 dial 失败后，在冷却期内 Resolve 跳过它（避免每次 TTL 重试打死节点）；
// 冷却期过后自动重新评估——若服务已恢复则重新纳入 RR，仍失败则 Invalidate
// 重置冷却。默认 3×TTL（9s），平衡"跳过死节点"与"感知恢复"。
const MeshFailCooldown = 3 * MeshTargetTTL

// ErrMeshServiceUnavailable 报告服务当前不可用（节点离线或未宣告）。
func ErrMeshServiceUnavailable(service string) error {
	return fmt.Errorf("mesh 服务 %q 当前不可用（节点离线或未宣告）", service)
}

// InsecureHTTPClient 返回跳过 TLS 证书验证的 http.Client（仅自签证书开发/测试环境；
// 生产环境应使用受信 CA 或把自签 CA 加入 RootCAs，而非关闭校验）。
// Timeout 取 60s 对齐 HubSignaler 长轮询（单次 poll 60s > 服务端 PollTimeout 25s）。
func InsecureHTTPClient() *http.Client {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
	return &http.Client{Timeout: 60 * time.Second, Transport: tr}
}

// MeshSignalToken 返回信令 Bearer token：显式 flagToken 优先，否则复用 svcAuthToken。
// hub 的 /api/signal/* 走 authMiddleware（校验 auth_token），与 MeshServices /
// RelayStream 的认证一致；relay start --token 是另一套 relay 注册 token，不混用。
func MeshSignalToken(flagToken, svcAuthToken string) string {
	if flagToken != "" {
		return flagToken
	}
	return svcAuthToken
}

// MeshAccessKey 返回 SproxySig 认证 AccessKey：显式 flag 优先，否则配置值。
func MeshAccessKey(flagKey, cfgKey string) string {
	if flagKey != "" {
		return flagKey
	}
	return cfgKey
}

// MeshAccessKeySecret 返回 SproxySig 认证 AccessKeySecret：显式 flag 优先，否则配置值。
func MeshAccessKeySecret(flagKey, cfgKey string) string {
	if flagKey != "" {
		return flagKey
	}
	return cfgKey
}

// MeshTargetRefresher 按需解析 mesh 目标，带 TTL 缓存与单飞（single-flight）刷新。
//
// 并发安全设计：所有缓存字段由 mu 保护；刷新期间**不持有 mu 做网络调用**——
// 承担刷新的 goroutine 置位后解锁再请求，等待者在 done 通道上等待，完成后重新
// 抢锁读取最终状态。这样 TTL 内并发调用只打一次 hub（单飞），且不引入「锁内
// 做 I/O」死锁风险。
type MeshTargetRefresher struct {
	svc     *FileClient
	service string
	ttl     time.Duration
	now     func() time.Time // 可注入时钟（测试用）
	// static 为 true 时是固定目标 refresher（NewStaticMeshTargetRefresher 构造）：
	// Resolve 始终返回预设 target，不查 hub；Invalidate no-op。用于虚拟 IP 寻址
	// （mesh connect <vip>:<port> 已解析 node-id，无需服务名刷新）。
	static bool

	mu             sync.Mutex
	target         *MeshService  // 仅 static refresher 使用（固定目标）
	candidates     []MeshService // 候选池（按 NodeID 排序固化，RR 轮询）
	nextIdx        uint64        // RR 轮询游标（每次 Resolve 推进）
	lastRefresh    time.Time
	refreshing     bool
	done           chan struct{}
	refreshErr     error
	lastFailedNode string
	lastFailedAt   time.Time // 失败时间戳（MeshFailCooldown 冷却窗口起点；零值=无冷却）
}

// NewMeshTargetRefresher 创建 refresher。
func NewMeshTargetRefresher(svc *FileClient, service string) *MeshTargetRefresher {
	return &MeshTargetRefresher{svc: svc, service: service, ttl: MeshTargetTTL, now: time.Now}
}

// NewStaticMeshTargetRefresher 创建固定目标 refresher（虚拟 IP 寻址用）。
// Resolve 始终返回 target 的副本，Invalidate no-op——vip → node 映射由调用方的
// vipTable 提供，不依赖服务名刷新。
func NewStaticMeshTargetRefresher(target *MeshService) *MeshTargetRefresher {
	return &MeshTargetRefresher{static: true, target: target, ttl: time.Hour, now: time.Now}
}

// SetClock 注入替代时钟（测试用）。
func (r *MeshTargetRefresher) SetClock(now func() time.Time) { r.now = now }

// SetTTL 覆盖缓存新鲜窗口（测试用；生产用 MeshTargetTTL 默认）。
func (r *MeshTargetRefresher) SetTTL(ttl time.Duration) { r.ttl = ttl }

// Service 返回服务名。
func (r *MeshTargetRefresher) Service() string { return r.service }

// Resolve 返回服务当前目标。缓存新鲜（<TTL）直接命中候选池并按 RR 游标轮询取
// 下一个（多候选均匀分布）；否则触发一次刷新重建候选池，并发调用共享同一刷新
// （单飞）。服务不在列表返回 ErrMeshServiceUnavailable。
// 固定目标 refresher（static）始终返回预设 target，不查 hub。
func (r *MeshTargetRefresher) Resolve(ctx context.Context) (*MeshService, error) {
	r.mu.Lock()
	if r.static {
		if r.target == nil {
			r.mu.Unlock()
			return nil, errors.New("静态目标 refresher 未设置 target")
		}
		t := *r.target
		r.mu.Unlock()
		return &t, nil
	}
	// TTL 内缓存命中：缓存的是「候选池 + 游标」，每次 Resolve 都从池轮询取下一个，
	// 保证多候选 RR 均匀分布（若缓存单个 target，RR 会退化为恒取同一节点）。
	if len(r.candidates) > 0 && r.now().Sub(r.lastRefresh) < r.ttl {
		t := r.pickNextLocked()
		r.mu.Unlock()
		return t, nil
	}
	if r.refreshing {
		done := r.done
		r.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.refreshErr != nil {
			return nil, r.refreshErr
		}
		if len(r.candidates) > 0 {
			return r.pickNextLocked(), nil
		}
		return nil, ErrMeshServiceUnavailable(r.service)
	}
	// 本 goroutine 承担刷新：置位后立即解锁，绝不在锁内做网络调用。
	r.refreshing = true
	r.refreshErr = nil
	r.done = make(chan struct{})
	r.mu.Unlock()

	fetchCtx, cancel := context.WithTimeout(ctx, MeshResolveTimeout)
	svcs, err := r.svc.MeshServices(fetchCtx)
	cancel()

	r.mu.Lock()
	r.refreshing = false
	close(r.done) // 等待者唤醒后先抢锁再读最终状态，无竞态
	if err != nil {
		r.refreshErr = fmt.Errorf("查询 mesh 服务失败: %w", err)
		r.mu.Unlock()
		return nil, r.refreshErr
	}
	// 收集所有同名服务候选，按 NodeID 排序固化（map/遍历序不稳定，排序保证 RR
	// 序列确定性可测），TTL 内每次 Resolve 从池轮询取下一个。
	cands := make([]MeshService, 0, 4)
	for i := range svcs {
		if svcs[i].Name == r.service {
			cands = append(cands, svcs[i])
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].Node < cands[j].Node })
	if len(cands) == 0 {
		r.candidates = nil
		r.lastRefresh = time.Time{}
		r.mu.Unlock()
		return nil, ErrMeshServiceUnavailable(r.service)
	}
	r.candidates = cands
	r.lastRefresh = r.now()
	t := r.pickNextLocked()
	r.mu.Unlock()
	return t, nil
}

// pickNextLocked 在 mu 保护下从候选池按轮询游标取下一个候选。
// 游标在每次 Resolve（TTL 命中或刷新后）都推进，保证多候选均匀分布。
// 跳过 lastFailedNode 且**仍在 MeshFailCooldown 冷却期内**的失败节点（审查
// Important #1：冷却过后自动重新评估恢复节点，避免单次瞬时失败永久排除某节点）；
// 若候选池全部为冷却中的失败节点，回退到游标指向的候选——避免全部失败时
// 无限卡死（返回 nil 仅当候选池为空，调用方已在 len(candidates)>0 前提下调用，
// 防御性保留）。
func (r *MeshTargetRefresher) pickNextLocked() *MeshService {
	n := len(r.candidates)
	if n == 0 {
		return nil
	}
	// 冷却窗口内才跳过失败节点；冷却已过（或从未失败）→ 失败标记失效，重新评估。
	skipFailed := !r.lastFailedAt.IsZero() && r.now().Sub(r.lastFailedAt) < MeshFailCooldown
	start := int(r.nextIdx % uint64(n))
	for i := range n {
		idx := (start + i) % n
		if !skipFailed || r.candidates[idx].Node != r.lastFailedNode {
			r.nextIdx = uint64((idx + 1) % n)
			t := r.candidates[idx]
			return &t
		}
	}
	// 全部候选都是冷却中的失败节点：回退到游标指向的候选。
	idx := int(r.nextIdx % uint64(n))
	r.nextIdx = (r.nextIdx + 1) % uint64(n)
	t := r.candidates[idx]
	return &t
}

// Invalidate 使缓存过期：dial 失败后调用，并记录失败节点让下一次 Resolve 在
// MeshFailCooldown 内跳过它（冷却过后自动重新评估恢复）。
// 固定目标 refresher（static）no-op（不重查 hub，vip → node 映射由 vipTable 提供）。
func (r *MeshTargetRefresher) Invalidate(failedNode string) {
	r.mu.Lock()
	if r.static {
		r.mu.Unlock()
		return
	}
	r.candidates = nil // 强制下次 Resolve 重新拉取候选池
	r.lastRefresh = time.Time{}
	r.refreshErr = nil
	r.lastFailedNode = failedNode
	r.lastFailedAt = r.now() // 冷却窗口起点
	r.mu.Unlock()
}
