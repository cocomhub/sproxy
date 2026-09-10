// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestQUIC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	// 必须在 TestHarness 之前注入证书 env：其内部子测试会 t.Parallel()，
	// 而 t.Setenv 不允许在并行测试中调用（父测试未并行化，此处安全）。
	setupQUICTLS(t)
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "quic",
		Dial:   quic.Dial,
		Listen: quic.Listen,
	})
}

// TestListen_CertEnvFailClosed 验证证书 env 只设置其一时返回明确错误（不静默回落）。
// 该用例在错误返回前不会绑定 UDP 端口，故 Windows 上同样运行。
func TestListen_CertEnvFailClosed(t *testing.T) {
	tests := []struct {
		name    string
		certEnv string
		keyEnv  string
	}{
		{"only cert file", "leaf.pem", ""},
		{"only key file", "", "leaf-key.pem"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SPROXY_QUIC_CERT_FILE", tt.certEnv)
			t.Setenv("SPROXY_QUIC_KEY_FILE", tt.keyEnv)

			ln, err := quic.Listen(context.Background(), "127.0.0.1:0")
			if err == nil {
				ln.Close()
				t.Fatal("expected error when only one of cert/key is set")
			}
		})
	}
}

// TestListen_CertEnvInvalidFile 验证证书 env 指向不存在文件时返回错误。
func TestListen_CertEnvInvalidFile(t *testing.T) {
	t.Setenv("SPROXY_QUIC_CERT_FILE", filepath.Join(t.TempDir(), "missing.pem"))
	t.Setenv("SPROXY_QUIC_KEY_FILE", filepath.Join(t.TempDir(), "missing-key.pem"))

	ln, err := quic.Listen(context.Background(), "127.0.0.1:0")
	if err == nil {
		ln.Close()
		t.Fatal("expected error for nonexistent cert/key files")
	}
}
