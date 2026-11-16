// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"net/http"
	"testing"
)

// tunnelBaseTransport 取出传统隧道外层客户端的底层 *http.Transport（穿透
// sigRoundTripper），断言装配形态并返回供 TLS 策略比较。
func tunnelBaseTransport(t *testing.T, c *FileClient) *http.Transport {
	t.Helper()
	if c.tunnelClient == nil {
		t.Fatal("tunnelClient 未装配（WithTunnel 未生效）")
	}
	srt, ok := c.tunnelClient.HTTPClient.Transport.(*sigRoundTripper)
	if !ok {
		t.Fatalf("隧道 Transport 应为 *sigRoundTripper，实际 %T", c.tunnelClient.HTTPClient.Transport)
	}
	base, ok := srt.base.(*http.Transport)
	if !ok {
		t.Fatalf("sigRoundTripper.base 应为 *http.Transport，实际 %T", srt.base)
	}
	return base
}

// TestWithTunnel_InheritsDirectTransportTLS 锁定缺陷修复：传统 /tunnel 隧道外层
// 必须与直连面复用同一 Transport（TLS 策略随 --insecure/--ca-file 一致生效），
// 且与选项顺序无关（WithTunnel 先/后于 TLS 选项均可）。
func TestWithTunnel_InheritsDirectTransportTLS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		opts []Option
	}{
		{"隧道先于 insecure", []Option{WithTunnel(testTunnelAK, testTunnelSK), WithInsecureTLS()}},
		{"insecure 先于隧道", []Option{WithInsecureTLS(), WithTunnel(testTunnelAK, testTunnelSK)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := NewFileClient("https://127.0.0.1:18083", tc.opts...)
			if err := c.InitError(); err != nil {
				t.Fatalf("InitError: %v", err)
			}
			direct, ok := c.httpClient.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("直连 Transport 应为 *http.Transport，实际 %T", c.httpClient.Transport)
			}
			base := tunnelBaseTransport(t, c)
			if base != direct {
				t.Fatal("隧道外层 Transport 必须与直连面同源（否则 TLS 策略不生效）")
			}
			if direct.TLSClientConfig == nil || !direct.TLSClientConfig.InsecureSkipVerify {
				t.Fatal("直连面 TLS 应为 InsecureSkipVerify=true")
			}
		})
	}
}

// TestWithTunnel_DefaultTransportIsolated 锁定：隧道外层 Transport 不是进程共享的
// http.DefaultTransport（硬规则 17——共享池会被外部 CloseIdleConnections 打断在途请求）。
func TestWithTunnel_DefaultTransportIsolated(t *testing.T) {
	t.Parallel()
	c := NewFileClient("https://127.0.0.1:18083", WithTunnel(testTunnelAK, testTunnelSK))
	if err := c.InitError(); err != nil {
		t.Fatalf("InitError: %v", err)
	}
	base := tunnelBaseTransport(t, c)
	if base == http.DefaultTransport {
		t.Fatal("隧道外层 Transport 不得为 http.DefaultTransport（需 per-instance 隔离池）")
	}
}
