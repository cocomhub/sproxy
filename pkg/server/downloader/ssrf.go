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
)

// IsPrivateIP 检查 IP 是否属于私有/内部/环回/保留地址。
// 阻止：环回、私有、链路本地、多播、未指定、广播、CGNAT 及其他保留地址。
func IsPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	// 额外检查 IPv4 保留地址段
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
func ValidateURLHost(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("ssrf: invalid URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
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

	// DNS 解析 hostname 并检查
	ips, err := net.DefaultResolver.LookupIPAddr(context.Background(), host)
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

// safeCheckRedirect 返回一个 CheckRedirect 函数，验证重定向目标 URL 安全。
func safeCheckRedirect() func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		// 对外部 URL 做 host 验证，内部拼接的 URL 由调用方保证
		if strings.HasPrefix(req.URL.String(), "http") {
			return ValidateURLHost(req.URL.String())
		}
		return nil
	}
}
