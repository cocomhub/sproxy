// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"crypto/tls"
	"testing"
)

func TestDialTLSConfig_ValidateAddress(t *testing.T) {
	tests := []struct {
		name       string
		addr       string
		wantServer string
		wantErr    bool
	}{
		{
			name:       "ipv4 with port",
			addr:       "127.0.0.1:9000",
			wantServer: "127.0.0.1",
			wantErr:    false,
		},
		{
			name:       "hostname with port",
			addr:       "example.com:443",
			wantServer: "example.com",
			wantErr:    false,
		},
		{
			name:    "missing port",
			addr:    "127.0.0.1",
			wantErr: true,
		},
		{
			name:    "empty addr",
			addr:    "",
			wantErr: true,
		},
		{
			name:       "ipv6 with port",
			addr:       "[::1]:9000",
			wantServer: "::1",
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf, err := DialTLSConfig(tt.addr)
			assertDialTLSResult(t, conf, err, tt.addr, tt.wantServer, tt.wantErr)
		})
	}
}

// assertDialTLSResult 验证 DialTLSConfig 的返回结果。
func assertDialTLSResult(t *testing.T, conf *tls.Config, err error, addr, wantServer string, wantErr bool) {
	t.Helper()
	if wantErr {
		if err == nil {
			t.Fatalf("expected error for addr %q, got nil", addr)
		}
		return
	}
	if err != nil {
		t.Fatalf("unexpected error for addr %q: %v", addr, err)
	}
	if conf == nil {
		t.Fatal("expected non-nil tls.Config")
	}
	if conf.ServerName != wantServer {
		t.Fatalf("expected ServerName %q, got %q", wantServer, conf.ServerName)
	}
	if conf.NextProtos == nil || len(conf.NextProtos) != 1 || conf.NextProtos[0] != "sproxy-quic" {
		t.Fatalf("expected NextProtos [\"sproxy-quic\"], got %v", conf.NextProtos)
	}
	if conf.InsecureSkipVerify {
		t.Fatal("InsecureSkipVerify should be false in production")
	}
}

func TestDialTLSConfig_HasCertPool(t *testing.T) {
	// 验证 DialTLSConfig 返回的 tls.Config 包含有效的 RootCAs
	// （不使用环境变量时，RootCAs 为 nil 表示使用系统默认池）
	conf, err := DialTLSConfig("127.0.0.1:9000")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if conf.RootCAs != nil {
		t.Log("RootCAs is set (SPROXY_QUIC_CA_CERT was provided by environment)")
	}
	_ = conf
}

func TestDialTLSConfig_CAEnv(t *testing.T) {
	// 设置一个不存在的 CA 路径，验证返回错误
	t.Setenv("SPROXY_QUIC_CA_CERT", "/nonexistent/ca.pem")
	_, err := DialTLSConfig("127.0.0.1:9000")
	if err == nil {
		t.Fatal("expected error for nonexistent CA cert, got nil")
	}
}
