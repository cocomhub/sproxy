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

	coordinator Coordinator // 多实例协调后端；nil = 不协调（默认）

	// clientIPFn 是 per-IP 桶键解析函数（装配层注入；nil = normalizeRemoteIP 默认，零回归）。
	clientIPFn func(*http.Request) string
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
	now := time.Now()
	cutoff := now.Add(-rl.window)

	// Binary search for first non-expired entry
	idx := sort.Search(len(rl.timestamps), func(i int) bool {
		return rl.timestamps[i].After(cutoff)
	})
	rl.timestamps = rl.timestamps[idx:]

	// 限制切片容量上限，防止异常流量导致内存泄漏
	if cap(rl.timestamps) > maxTimestampsCap {
		trimmed := make([]time.Time, len(rl.timestamps))
		copy(trimmed, rl.timestamps)
		rl.timestamps = trimmed
	}

	if len(rl.timestamps) >= rl.limit {
		return false
	}

	rl.timestamps = append(rl.timestamps, now)
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
// 使用 per-IP 令牌桶 + 全局限流。
// 装配了 coordinator 时，per-IP 放行后还须经 coordinator.Allow(ip)（多实例共享配额）。
// When the limit is exceeded, it responds with 429 Too Many Requests (JSON).
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 热更新 enabled=false 时短路放行（不重建 handler 链）。
		// 读 enabled 与 AllowIP 共用 mu（AllowIP 持锁后重读），避免数据竞争。
		rl.mu.Lock()
		enabled := rl.enabled
		ip := rl.clientIP(r)
		allowed := enabled && rl.allowIPLocked(ip)
		coord := rl.coordinator
		rl.mu.Unlock()
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
