// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"log/slog"
	"math"
	"net"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/slogutil"
)

// RateLimiter implements a sliding-window rate limiter using only the stdlib.
// Thread-safe via sync.Mutex.
//
// 当前实现为全局限流（全局单实例）+ 每 IP 令牌桶限流。
// 每个客户端 IP 获得 limit/10 的令牌桶配额，先检查 per-IP 令牌桶，
// 配额耗尽后回退到全局滑动窗口。
//
// coordinator（可选）：多实例协调后端（见 ratelimit_coord.go）。装配后
// Middleware 的最终放行还须经 coordinator.Allow（key=归一化 IP），多实例
// 共享配额。nil = 不协调（既有单实例行为，零回归）。
//
// 限流维度扩展（roadmap 11.10-⑪ P1，设计文档 2026-09-24-ratelimit-dimensions.md）：
//   - endpointLimits/endpointDefaults：per-endpoint 限流（path → {limit, window}），
//     精确匹配优先 + "/" 段边界前缀最长匹配；无匹配规则透传（只限显式配置的端点）。
//   - sem：全局并发上限（非阻塞 semaphore，0/未装配 = 关闭）。Middleware 放行链：
//     并发闸 → per-IP 桶（含全局窗口回退，现状语义）→ per-endpoint 桶 → coordinator。
//
// 新维度默认关闭（endpoints 空 + max_concurrent=0）→ 放行链与现状逐字一致（零回归）。
type RateLimiter struct {
	mu         sync.Mutex
	enabled    bool
	limit      int
	window     time.Duration
	timestamps []time.Time
	logger     *slog.Logger

	// Per-IP token bucket
	ipBuckets   sync.Map
	ipQuota     float64
	lastCleanup time.Time

	// endpointLimits 是 per-endpoint 限流规则表（path → 规则；空 = 不启用）。
	endpointLimits map[string]*endpointRule
	// endpointDefaults 是未匹配任何端点规则时的兜底规则；nil = 无兜底（透传）。
	endpointDefaults *endpointRule
	// sem 是全局并发上限信号量；nil = 不启用。热更新重建后旧请求继续归还旧 sem
	// （Middleware 绑定获取时的 sem，defer 保证不泄漏，见 AcquireConcurrent 注释）。
	sem *semaphore

	coordinator Coordinator // 多实例协调后端；nil = 不协调（默认）

	// clientIPFn 是 per-IP 桶键解析函数（装配层注入；nil = normalizeRemoteIP 默认，零回归）。
	clientIPFn func(*http.Request) string
}

// EndpointLimit 是 per-endpoint 限流规则（rate_limit.endpoints 段：path → {limit, window}）。
// Limit<=0 / Window<=0 沿用 NewRateLimiter 的默认化（归 5 / 1s，与 UpdateConfig 一致）。
type EndpointLimit struct {
	Limit  int           `yaml:"limit" mapstructure:"limit"`
	Window time.Duration `yaml:"window" mapstructure:"window"`
}

// endpointRule 是单条端点规则的滑动窗口状态（含独立时间戳队列）。
type endpointRule struct {
	limit      int
	window     time.Duration
	timestamps []time.Time
}

// semaphore 是容量 = max_concurrent 的非阻塞信号量：acquire 满时立即失败（429），
// release 归还一个槽。只经 RateLimiter.mu 读取/替换（热更新），acquire/release
// 本身无锁（chan 并发安全）。
type semaphore struct {
	ch chan struct{}
}

func newSemaphore(n int) *semaphore {
	return &semaphore{ch: make(chan struct{}, n)}
}

// acquire 非阻塞占用一个槽（满则 false）。
func (s *semaphore) acquire() bool {
	select {
	case s.ch <- struct{}{}:
		return true
	default:
		return false
	}
}

// release 归还一个槽（只应与成功 acquire 配对调用）。
func (s *semaphore) release() {
	<-s.ch
}

// ipBucket 表示单个 IP 的令牌桶状态。
type ipBucket struct {
	tokens    float64
	lastCheck time.Time
}

// maxTimestampsCap 是时间戳队列的最大容量，防止内存泄漏。
const maxTimestampsCap = 100000

// NewRateLimiter creates a RateLimiter allowing up to `limit` requests
// per sliding `window` duration.
func NewRateLimiter(limit int, window time.Duration, logger *slog.Logger) *RateLimiter {
	log := slogutil.Default(logger)
	if limit <= 0 {
		log.Warn("rate limiter created with limit <= 0, defaulting to 5")
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	ipQuota := float64(limit) / 10.0
	if ipQuota < 1 {
		ipQuota = 1
	}
	return &RateLimiter{
		enabled:     true,
		limit:       limit,
		window:      window,
		logger:      log,
		ipQuota:     ipQuota,
		lastCleanup: time.Now(),
	}
}

// UpdateConfig 热更新限流参数（PUT /api/config 接线）。
// 复用现有 mu 与实例，不重建 handler 链（xfer LocalHandler 已持有构造期引用），
// 不清空 timestamps（旧窗口按新 window 自然过期）。enabled=false 时 Middleware 短路放行。
// 与 NewRateLimiter 相同的默认逻辑：limit<=0 归 5、window<=0 归 1s。
func (rl *RateLimiter) UpdateConfig(enabled bool, limit int, window time.Duration) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if limit <= 0 {
		rl.logger.Warn("rate limiter update: limit <= 0, defaulting to 5")
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	ipQuota := float64(limit) / 10.0
	if ipQuota < 1 {
		ipQuota = 1
	}
	rl.enabled = enabled
	rl.limit = limit
	rl.window = window
	rl.ipQuota = ipQuota
}

// SetCoordinator 装配多实例协调后端（config 接线）。nil 清除（= 不协调）。
// 调用点：RegisterRoutes 装配期与 UpdateConfig 热更新期；必须持 mu 或装配期
// 无并发（Middleware 尚未挂载）。
func (rl *RateLimiter) SetCoordinator(c Coordinator) {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.coordinator = c
}

// Allow reports whether the current request is within the global rate limit.
// 不使用 per-IP 限流。
func (rl *RateLimiter) Allow() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.allowGlobalLocked()
}

// allowGlobalLocked 执行全局滑动窗口检查（调用者必须已持有 rl.mu）。
func (rl *RateLimiter) allowGlobalLocked() bool {
	return slideWindowAllow(&rl.timestamps, rl.limit, rl.window)
}

// slideWindowAllow 是滑动窗口通用实现（全局窗口与 per-endpoint 规则共用）：
// 裁剪过期时间戳 → 未超限则追加当前时间戳并放行。limit<=0 时恒拒绝（防御）。
func slideWindowAllow(timestamps *[]time.Time, limit int, window time.Duration) bool {
	if limit <= 0 {
		return false
	}
	now := time.Now()
	cutoff := now.Add(-window)

	// Binary search for first non-expired entry
	idx := sort.Search(len(*timestamps), func(i int) bool {
		return (*timestamps)[i].After(cutoff)
	})
	*timestamps = (*timestamps)[idx:]

	// 限制切片容量上限，防止异常流量导致内存泄漏
	if cap(*timestamps) > maxTimestampsCap {
		trimmed := make([]time.Time, len(*timestamps))
		copy(trimmed, *timestamps)
		*timestamps = trimmed
	}

	if len(*timestamps) >= limit {
		return false
	}

	*timestamps = append(*timestamps, now)
	return true
}

// AllowIP 检查请求是否在限流范围内，优先使用 per-IP 令牌桶，
// 配额耗尽后回退到全局滑动窗口。
func (rl *RateLimiter) AllowIP(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.allowIPLocked(ip)
}

// cleanupIPBuckets 清理超过 2 个窗口未使用的 IP 桶条目。
func (rl *RateLimiter) cleanupIPBuckets() {
	cutoff := time.Now().Add(-rl.window * 2)
	rl.ipBuckets.Range(func(key, value any) bool {
		bucket := value.(*ipBucket) //nolint:errcheck // 类型断言安全：我们只存储 *ipBucket
		if bucket.lastCheck.Before(cutoff) {
			rl.ipBuckets.Delete(key)
		}
		return true
	})
}

// Middleware wraps an http.Handler with rate limiting.
// 放行链（roadmap 11.10-⑪ P1）：并发闸 → per-IP 桶（含全局窗口回退，现状语义）→
// per-endpoint 桶 → coordinator（装配时）。全过才放行；任一拒绝 → 429 JSON（与现状一致）。
// 新维度默认关闭（sem nil + endpoints 空）→ 与现状逐字一致（零回归）。
//
// 并发闸获取成功者必须配对归还：defer 绑定**获取时的 sem**（热更新重建后旧请求
// 继续归还旧 sem，不泄漏——设计文档「热更新仅生效于新请求」）。
// When the limit is exceeded, it responds with 429 Too Many Requests (JSON).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 热更新 enabled=false 时短路放行（不重建 handler 链）。
		// 读 enabled/sem/coordinator 与放行链共用 mu，避免数据竞争。
		rl.mu.Lock()
		enabled := rl.enabled
		sem := rl.sem
		ip := rl.clientIP(r)
		var releaseSem func()
		allowed := true
		if enabled {
			// 并发闸最先：sem 满立即 429（不消耗 per-IP/全局/endpoint 配额）。
			// acquire 非阻塞（select+default），持锁调用安全；成功才绑定 release。
			if sem != nil {
				if !sem.acquire() {
					rl.mu.Unlock()
					rl.logger.Warn("rate limit exceeded", "remote_addr", ip, "path", r.URL.Path, "scope", "concurrent")
					sendJSONResponse(w, map[string]string{"error": "rate limit exceeded"}, http.StatusTooManyRequests)
					return
				}
				releaseSem = sem.release
			}
			// per-IP 令牌桶 + 全局窗口回退（allowIPLocked 内部完成，现状语义不变）。
			allowed = rl.allowIPLocked(ip)
			// per-endpoint 桶：无匹配规则透传（只限显式配置的端点）。
			if allowed {
				allowed = rl.allowEndpointLocked(r.URL.Path)
			}
		}
		coord := rl.coordinator
		rl.mu.Unlock()
		if releaseSem != nil {
			defer releaseSem()
		}
		if allowed && coord != nil {
			// 多实例协调：per-IP 放行后还须共享配额放行（key = 归一化 IP）。
			// coordinator.Allow 自带跨实例互斥，可安全在锁外调用。
			allowed = coord.Allow(ip, 1)
		}
		if !allowed {
			if enabled {
				rl.logger.Warn("rate limit exceeded", "remote_addr", ip, "path", r.URL.Path)
				sendJSONResponse(w, map[string]string{"error": "rate limit exceeded"}, http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// UpdateDimensions 热更新新维度（per-endpoint 规则 + 全局并发上限；PUT /api/config
// 与装配期共用）。沿用现有 mu 语义：endpoints 全量替换、endpoint_default 零值
// （limit<=0 且 window<=0）= 无兜底、max_concurrent<=0 = 关闭并发闸。
// 规则非法值（limit<=0 / window<=0）沿用 NewRateLimiter 默认化（归 5 / 1s）。
//
// sem 热更新：重建为容量 max_concurrent 的新 channel。在途持有者归还的是**获取时**
// 的旧 sem（Middleware 绑定 + defer），新请求用新 sem——旧请求不会污染新配额
// （设计文档「热更新仅生效于新请求，旧请求继续归还旧 sem，靠 defer 保证不泄漏」）。
func (rl *RateLimiter) UpdateDimensions(endpoints map[string]EndpointLimit, def EndpointLimit, maxConcurrent int) {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	rl.endpointLimits = make(map[string]*endpointRule, len(endpoints))
	for path, lim := range endpoints {
		l, w := rl.normalizeEndpointLimit(lim.Limit, lim.Window)
		rl.endpointLimits[path] = &endpointRule{limit: l, window: w}
	}
	if def.Limit > 0 && def.Window > 0 {
		l, w := rl.normalizeEndpointLimit(def.Limit, def.Window)
		rl.endpointDefaults = &endpointRule{limit: l, window: w}
	} else {
		rl.endpointDefaults = nil
	}
	if maxConcurrent > 0 {
		rl.sem = newSemaphore(maxConcurrent)
	} else {
		rl.sem = nil
	}
}

// normalizeEndpointLimit 沿用 NewRateLimiter 的默认化：limit<=0 归 5、window<=0 归 1s。
func (rl *RateLimiter) normalizeEndpointLimit(limit int, window time.Duration) (int, time.Duration) {
	if limit <= 0 {
		rl.logger.Warn("rate limiter endpoint rule: limit <= 0, defaulting to 5")
		limit = 5
	}
	if window <= 0 {
		window = time.Second
	}
	return limit, window
}

// AllowEndpoint 检查请求路径是否在 per-endpoint 限流范围内（无匹配规则 → true 透传）。
func (rl *RateLimiter) AllowEndpoint(path string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.allowEndpointLocked(path)
}

// allowEndpointLocked 执行 per-endpoint 滑动窗口检查（调用者必须已持有 rl.mu）。
// 匹配语义：精确匹配优先；否则按 "/" 段边界前缀最长匹配（规则 "/download" 命中
// "/download" 与 "/download/deep"，不命中 "/downloadx"）；无匹配规则时若有
// 兜底规则用兜底，否则 true 透传（只限显式配置的端点，不误伤 hub/mux 长连）。
func (rl *RateLimiter) allowEndpointLocked(path string) bool {
	rule := rl.matchEndpointRule(path)
	if rule == nil {
		if rl.endpointDefaults == nil {
			return true
		}
		rule = rl.endpointDefaults
	}
	return slideWindowAllow(&rule.timestamps, rule.limit, rule.window)
}

// matchEndpointRule 返回 path 命中的端点规则（nil = 无精确/前缀匹配）。
// 精确匹配优先于前缀；前缀要求完整段边界（path == p 或 path 以 p+"/" 开头），
// 多个前缀命中时取最长。
func (rl *RateLimiter) matchEndpointRule(path string) *endpointRule {
	if rule, ok := rl.endpointLimits[path]; ok {
		return rule
	}
	best := ""
	for p := range rl.endpointLimits {
		if len(p) <= len(best) {
			continue
		}
		if strings.HasPrefix(path, p+"/") {
			best = p
		}
	}
	if best == "" {
		return nil
	}
	return rl.endpointLimits[best]
}

// AcquireConcurrent 非阻塞获取一个全局并发槽（max_concurrent 未启用时恒 true）。
// 成功者必须配对 ReleaseConcurrent（保证 defer 配对；Middleware 放行链已保证）。
// 注意：直接配对使用（非 Middleware）时热更新重建 sem 会让本方法归还**新** sem——
// 生产路径（Middleware）绑定获取时的 sem，热更新安全；本方法供无热更新的直接使用。
func (rl *RateLimiter) AcquireConcurrent() bool {
	rl.mu.Lock()
	sem := rl.sem
	rl.mu.Unlock()
	if sem == nil {
		return true
	}
	return sem.acquire()
}

// ReleaseConcurrent 归还一个全局并发槽（与成功 AcquireConcurrent 配对）。
func (rl *RateLimiter) ReleaseConcurrent() {
	rl.mu.Lock()
	sem := rl.sem
	rl.mu.Unlock()
	if sem != nil {
		sem.release()
	}
}

// parseIPNets 把配置的 IP/CIDR 列表解析为 *net.IPNet 列表（供运行时匹配）。
// 非法条目（Validate 已拒绝）返回 nil。纯 IP 按 /32、/128 归一（与 validateIPList 一致）。
func parseIPNets(list []string) []*net.IPNet {
	nets := make([]*net.IPNet, 0, len(list))
	for _, s := range list {
		if _, ipnet, err := net.ParseCIDR(s); err == nil {
			nets = append(nets, ipnet)
			continue
		}
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		bits := 32
		if ip.To4() == nil {
			bits = 128
		}
		nets = append(nets, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
	}
	return nets
}

// inAnyNet 判定 ip 是否命中任一网段（网段为空 → 恒 false，由调用方先判空短路）。
func inAnyNet(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// maxXFFEntries 是 X-Forwarded-For 链解析的最大段数（超出即视为畸形，忽略整条回退
// RemoteAddr——fail-closed，防超长链 DoS；标准语义只取首个非信任项，天然 O(1)）。
const maxXFFEntries = 64

// resolveClientIP 解析请求的真实客户端 IP（信任代理语义）：
//   - remoteHost（RemoteAddr 去端口后的纯 IP）命中 trusted（信任代理列表）时，
//     从 X-Forwarded-For 链**右向左**取第一个非信任项（标准语义）；
//   - 链为空 / 全信任 / 畸形（非 IP、超长 >64 段）→ 回退 RemoteAddr；
//   - trusted 为空（未配置 trust_proxies）→ **一律忽略 XFF**（防伪造）。
func resolveClientIP(remoteAddr, xff string, trusted []*net.IPNet) string {
	remoteHost := normalizeRemoteIP(remoteAddr)
	if len(trusted) == 0 {
		return remoteHost
	}
	rip := net.ParseIP(remoteHost)
	if rip == nil || !inAnyNet(rip, trusted) {
		return remoteHost
	}
	if xff == "" {
		return remoteHost
	}
	entries := strings.Split(xff, ",")
	if len(entries) > maxXFFEntries {
		return remoteHost
	}
	// 右向左：最近一跳在最右。跳过信任项，取第一个非信任条目作为真实客户端。
	for _, entrie := range slices.Backward(entries) {
		token := strings.TrimSpace(entrie)
		if token == "" {
			continue
		}
		ip := net.ParseIP(token)
		if ip == nil {
			// 畸形条目：忽略整条 XFF（fail-closed），回退 RemoteAddr。
			return remoteHost
		}
		if !inAnyNet(ip, trusted) {
			return token
		}
	}
	return remoteHost
}

// clientIPFromRequest 是 resolveClientIP 的请求面封装（读取 RemoteAddr 与 XFF 头）。
func (h *Handlers) clientIPFromRequest(r *http.Request) string {
	cfg := h.cfgPtr.Load()
	if cfg == nil {
		return normalizeRemoteIP(r.RemoteAddr)
	}
	return resolveClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), parseIPNets(cfg.Auth.TrustedProxies))
}

// AllowIPsCoverLoopback 判定 allow_ips 是否同时覆盖 IPv4 与 IPv6 回环（启动告警辅助：
// 设计文档风险 2——回环也须在白名单，配漏 127.0.0.1/::1 时打印告警防运维自锁）。
func AllowIPsCoverLoopback(allowIPs []string) bool {
	hasV4, hasV6 := false, false
	for _, n := range parseIPNets(allowIPs) {
		if n.Contains(net.ParseIP("127.0.0.1")) {
			hasV4 = true
		}
		if n.Contains(net.ParseIP("::1")) {
			hasV6 = true
		}
	}
	return hasV4 && hasV6
}

// ipGate 是认证前 IP 门（auth.allow_ips）：
//   - AllowIPs 空 → 直通（零回归）；
//   - 非空 → 按 resolveClientIP 解析真实 IP，不在任何网段 → Warn + 403
//     （统一文案 "forbidden: ip not allowed"；不落审计——非业务事件防日志洪泛）。
//
// 挂在 authMiddleware 最前与公开凭据端点（register/nonce/login）——未授权来源在
// 认证前直接拒绝，不泄露认证面。
func (h *Handlers) ipGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := h.cfgPtr.Load()
		if cfg == nil || len(cfg.Auth.AllowIPs) == 0 {
			next.ServeHTTP(w, r)
			return
		}
		ip := h.clientIPFromRequest(r)
		if !inAnyNet(net.ParseIP(ip), parseIPNets(cfg.Auth.AllowIPs)) {
			h.log().WarnContext(r.Context(), "auth: ip not allowed",
				"remote", r.RemoteAddr, "ip", ip, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "forbidden: ip not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP 返回 per-IP 桶键：装配了 trusted_proxies 时按真实客户端 IP 计量（代理后
// 限流不合并到代理 IP）；未配置时与 normalizeRemoteIP 等价（零回归）。
func (rl *RateLimiter) clientIP(r *http.Request) string {
	if rl.clientIPFn != nil {
		return rl.clientIPFn(r)
	}
	return normalizeRemoteIP(r.RemoteAddr)
}

// SetClientIPFn 注入 per-IP 桶键解析函数（装配层在 cfg.Auth.TrustedProxies 非空时
// 设置；nil = normalizeRemoteIP 默认，零回归）。
func (rl *RateLimiter) SetClientIPFn(fn func(*http.Request) string) {
	rl.clientIPFn = fn
}

// normalizeRemoteIP 把 http.Request.RemoteAddr（"IP:port" 或裸 IP，可能是 IPv6
// "[::1]:1234"）归一到纯 IP，作为 per-IP 令牌桶的键。
// 去除端口是必须的：若按 "IP:port" 作键，每请求新 TCP 连接会有新端口，
// 触达全新桶（tokens=1）→ 永远放行，全局限流永不生效（限流被静默绕过）。
// 解析失败（罕见畸形输入）时回退原值——per-IP 桶"陌生键"仍受全局窗口兜底。
func normalizeRemoteIP(remoteAddr string) string {
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// allowIPLocked 实现 AllowIP 的加锁体（调用者必须已持有 rl.mu）。
// 每请求首查 per-IP 令牌桶，配额不足回退全局滑动窗口。
func (rl *RateLimiter) allowIPLocked(ip string) bool {
	if rl.limit <= 0 {
		return false
	}

	// 定期清理过期 IP 桶条目
	if time.Since(rl.lastCleanup) > rl.window*2 {
		rl.cleanupIPBuckets()
		rl.lastCleanup = time.Now()
	}

	// Per-IP 令牌桶检查
	if ip != "" && rl.ipQuota > 0 {
		val, _ := rl.ipBuckets.LoadOrStore(ip, &ipBucket{
			tokens:    rl.ipQuota,
			lastCheck: time.Now(),
		})
		bucket := val.(*ipBucket) //nolint:errcheck // 类型断言安全：我们只存储 *ipBucket

		now := time.Now()
		elapsed := now.Sub(bucket.lastCheck).Seconds()
		rate := rl.ipQuota / rl.window.Seconds()
		bucket.tokens = math.Min(rl.ipQuota, bucket.tokens+elapsed*rate)
		bucket.lastCheck = now

		if bucket.tokens >= 1 {
			bucket.tokens--
			// 记录到全局窗口，确保总配额准确
			rl.timestamps = append(rl.timestamps, now)
			return true
		}
	}

	// 回退到全局滑动窗口
	return rl.allowGlobalLocked()
}
