// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// client_ip.go 是客户端真实 IP 解析（信任代理语义）与认证前 IP 门（auth.allow_ips）。
// 两者都依赖 Handlers 的配置快照与 logger（装配态），故留在装配根，不随限流切片下沉；
// RemoteAddr 归一化复用 ratelimit.NormalizeRemoteIP（per-IP 桶键的单一实现）。
package server

import (
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/cocomhub/sproxy/pkg/netutil"
	"github.com/cocomhub/sproxy/pkg/server/ratelimit"
)

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
	remoteHost := ratelimit.NormalizeRemoteIP(remoteAddr)
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
		return ratelimit.NormalizeRemoteIP(r.RemoteAddr)
	}
	return resolveClientIP(r.RemoteAddr, r.Header.Get("X-Forwarded-For"), netutil.ParseIPNets(cfg.Auth.TrustedProxies))
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
		if !inAnyNet(net.ParseIP(ip), netutil.ParseIPNets(cfg.Auth.AllowIPs)) {
			h.log().WarnContext(r.Context(), "auth: ip not allowed",
				"remote", r.RemoteAddr, "ip", ip, "method", r.Method, "path", r.URL.Path)
			http.Error(w, "forbidden: ip not allowed", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}
