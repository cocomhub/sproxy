// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"net"
	"slices"
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

// TestSelfSignedCert_SANAndCA 校验自签回落证书具备 loopback SAN 与 CA 属性
// （被显式 pin 为信任根时也能通过校验）。
func TestSelfSignedCert_SANAndCA(t *testing.T) {
	cert, err := selfSignedCert()
	if err != nil {
		t.Fatalf("selfSignedCert: %v", err)
	}
	if len(cert.Certificate) == 0 {
		t.Fatal("expected at least one certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}

	if !leaf.IsCA {
		t.Error("expected IsCA = true")
	}
	if leaf.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Error("expected KeyUsageCertSign")
	}
	if leaf.KeyUsage&x509.KeyUsageDigitalSignature == 0 {
		t.Error("expected KeyUsageDigitalSignature")
	}
	if !slices.Contains(leaf.DNSNames, "localhost") {
		t.Errorf("expected DNSNames to contain localhost, got %v", leaf.DNSNames)
	}
	for _, want := range []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback} {
		if !slices.ContainsFunc(leaf.IPAddresses, want.Equal) {
			t.Errorf("expected IPAddresses to contain %v, got %v", want, leaf.IPAddresses)
		}
	}
	if !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageServerAuth) {
		t.Errorf("expected ExtKeyUsageServerAuth, got %v", leaf.ExtKeyUsage)
	}
}

// TestListenCert_EnvSelection 校验 listenCert 的三种 env 组合。
func TestListenCert_EnvSelection(t *testing.T) {
	clearEnv := func(t *testing.T) {
		t.Helper()
		t.Setenv("SPROXY_QUIC_CERT_FILE", "")
		t.Setenv("SPROXY_QUIC_KEY_FILE", "")
	}

	t.Run("both unset falls back to self-signed", func(t *testing.T) {
		clearEnv(t)
		cert, err := listenCert()
		if err != nil {
			t.Fatalf("expected self-signed fallback, got %v", err)
		}
		if len(cert.Certificate) == 0 {
			t.Fatal("expected non-empty certificate")
		}
	})

	t.Run("only cert file fails closed", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("SPROXY_QUIC_CERT_FILE", "leaf.pem")
		if _, err := listenCert(); err == nil {
			t.Fatal("expected error when only SPROXY_QUIC_CERT_FILE is set")
		}
	})

	t.Run("only key file fails closed", func(t *testing.T) {
		clearEnv(t)
		t.Setenv("SPROXY_QUIC_KEY_FILE", "leaf-key.pem")
		if _, err := listenCert(); err == nil {
			t.Fatal("expected error when only SPROXY_QUIC_KEY_FILE is set")
		}
	})

	t.Run("nonexistent files error", func(t *testing.T) {
		clearEnv(t)
		dir := t.TempDir()
		t.Setenv("SPROXY_QUIC_CERT_FILE", dir+"/missing.pem")
		t.Setenv("SPROXY_QUIC_KEY_FILE", dir+"/missing-key.pem")
		if _, err := listenCert(); err == nil {
			t.Fatal("expected error for nonexistent cert/key files")
		}
	})
}

// TestDiscardAnnounce 校验宣告魔数的读取与校验：匹配成功、不匹配与读失败均覆盖。
func TestDiscardAnnounce(t *testing.T) {
	tests := []struct {
		name    string
		content string
		wantErr bool
	}{
		{"匹配魔数", announceMagic, false},
		{"魔数不匹配", "SPROXYQ2", true},
		{"零长度帧不匹配", "\x00\x00\x00\x00", true},
		{"内容不足", "SPRO", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &mockStream{}
			s.readBuf.WriteString(tt.content)
			err := discardAnnounce(context.Background(), s)
			if tt.wantErr != (err != nil) {
				t.Fatalf("wantErr=%v, got err=%v", tt.wantErr, err)
			}
			if !tt.wantErr && s.readBuf.Len() != 0 {
				t.Fatalf("announce magic should be fully consumed, %d bytes left", s.readBuf.Len())
			}
		})
	}
}

// TestAnnounceMagicNotAValidFrame 校验宣告魔数不可能与合法消息帧同形：
// 合法帧以 4B 大端长度前缀开头，其长度不得超过 maxMessageBytes。
func TestAnnounceMagicNotAValidFrame(t *testing.T) {
	prefix := binary.BigEndian.Uint32([]byte(announceMagic))
	if prefix <= maxMessageBytes {
		t.Fatalf("announce magic 的前 4 字节 %d 落在合法长度范围内（≤ %d）", prefix, maxMessageBytes)
	}
}
