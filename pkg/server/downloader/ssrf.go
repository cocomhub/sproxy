// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxRedirects 是 HTTP 重定向最大跟踪次数。
const maxRedirects = 10

// IsPrivateIP 检查 IP 是否属于私有/内部/环回/保留地址。
// 阻止：环回、私有、链路本地、多播、未指定、广播、CGNAT 及其他保留地址。
//
// IPv6 覆盖说明：
//   - IsLoopback()      覆盖 ::1
//   - IsLinkLocalUnicast()  覆盖 fe80::/10
//   - IsLinkLocalMulticast() 覆盖 ff02::/16
//   - IsUnspecified()   覆盖 ::
//   - IsMulticast()     覆盖 ff00::/8
//   - IsPrivate()       覆盖 fc00::/7 (ULA)
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 额外检查 IPv4 保留地址段（IPv6 地址不会进入此分支）
	if ip4 := ip.To4(); ip4 != nil {
		// 0.0.0.0/8 "this network"
		if ip4[0] == 0 {
			return true
		}
		// CGNAT 100.64.0.0/10
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 {
			return true
		}
		// 198.18.0.0/15 benchmark
		if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) {
			return true
		}
		// 广播 255.255.255.255
		if ip4.Equal(net.IPv4bcast) {
			return true
		}
		// 240.0.0.0/4 reserved
		if ip4[0] >= 240 {
			return true
		}
	}
	return false
}

// ValidateURLHost 校验 URL 的 host 是否安全（非内部地址）。
// 解析 hostname 并检查所有解析出的 IP 是否安全。
// 内部创建带 10s 超时的 context 以防 DNS 解析长时间阻塞。
func ValidateURLHost(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ssrf: invalid URL: %w", err)
	}
	return validateURLParsed(parsed)
}

// validateURLParsed 对已解析的 URL 做 SSRF 校验。
func validateURLParsed(parsed *url.URL) error {
	// 大小写不敏感检查 scheme
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("ssrf: unsupported scheme %q", parsed.Scheme)
	}

	host := parsed.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: empty host")
	}

	// 直接检查 IP 格式的 host（如 http://127.0.0.1/）
	if ip := net.ParseIP(host); ip != nil {
		if IsPrivateIP(ip) {
			return fmt.Errorf("ssrf: connection to private/internal IP %s is blocked", ip)
		}
		return nil
	}

	// DNS 解析 hostname 并检查。
	// 使用带超时的 context 防止长时间阻塞（平台默认无超时的 DNS 解析）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("ssrf: hostname resolution failed for %q: %w", host, err)
	}
	for _, ipAddr := range ips {
		if IsPrivateIP(ipAddr.IP) {
			return fmt.Errorf("ssrf: hostname %q resolves to private/internal IP %s", host, ipAddr.IP)
		}
	}
	return nil
}

// validateURLHostAfterDo 在 HTTP 请求完成后二次验证最终 URL 的 IP 是否安全。
// 用于防御 DNS 重绑定攻击：仅对 hostname 格式的 URL 执行二次 DNS 解析，
// 因为直接 IP 地址已在预检 ValidateURLHost 中验证过，且不可能被重绑定。
func validateURLHostAfterDo(u *url.URL) error {
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("ssrf: empty host after request")
	}

	// 直接 IP 格式：已在预检中验证，无需二次检查
	if net.ParseIP(host) != nil {
		return nil
	}

	// DNS 二次解析：检查 hostname 是否在请求期间被重绑定到私有 IP
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return fmt.Errorf("ssrf: post-request hostname resolution failed for %q: %w", host, err)
	}
	for _, ipAddr := range ips {
		if IsPrivateIP(ipAddr.IP) {
			return fmt.Errorf("ssrf: post-request hostname %q resolves to private/internal IP %s", host, ipAddr.IP)
		}
	}
	return nil
}

// safeCheckRedirect 返回一个 CheckRedirect 函数，验证重定向目标 URL 安全。
func safeCheckRedirect() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		// 对外部 URL 做 host 验证，内部拼接的 URL 由调用方保证。
		// 使用 req.URL.Scheme 进行 scheme 检查而非字符串前缀，
		// 避免路径中含 "http" 字样的误判。
		scheme := strings.ToLower(req.URL.Scheme)
		if scheme == "http" || scheme == "https" {
			return validateURLParsed(req.URL)
		}
		return nil
	}
}