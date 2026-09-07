// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package totp

import (
	"bytes"
	"errors"
	"testing"

	accesskey "github.com/cocomhub/sproxy/pkg/accesskey"
)

// stubProvider 是测试探针 Provider：返回固定图片字节（验证注册表按名装配）。
type stubProvider struct {
	out []byte
}

func (p stubProvider) Render(_ string) ([]byte, error) { return p.out, nil }

// TestServerSideQR_UnconfiguredSentinel 验证骨架默认态：未接入三方 TOTP/QR 依赖时
// ServerSideQR 返回哨兵错误 ErrNotConfigured（fail-closed，非占位/panic），且不产生输出。
func TestServerSideQR_UnconfiguredSentinel(t *testing.T) {
	out, err := ServerSideQR("otpauth://totp/example?secret=JBSWY3DPEHPK3PXP&issuer=example")
	if err == nil {
		t.Fatalf("骨架 ServerSideQR 应返回错误（未配置三方依赖）")
	}
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("应返回哨兵错误 ErrNotConfigured, got %v", err)
	}
	if out != nil {
		t.Fatalf("错误时应无输出, got %d 字节", len(out))
	}
}

// TestTotpRegistry_DefaultRegisteredAndUnconfigured 验证 init() 注册的默认项：
// GetStorer[Provider]("totp-qr") 命中，且 Render 返回 ErrNotConfigured（未配置态）。
func TestTotpRegistry_DefaultRegisteredAndUnconfigured(t *testing.T) {
	p, ok := accesskey.GetStorer[Provider]("totp-qr")
	if !ok {
		t.Fatalf("init 注册的 %q 应命中 Provider", "totp-qr")
	}
	if _, err := p.Render("otpauth://totp/example"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("默认注册项 Render 应返回 ErrNotConfigured, got %v", err)
	}
}

// TestTotpRegistry_RegisterUnregisterRoundtrip 验证注册表接入：
// 自定义 Provider 注册 → 按名命中并可用；UnregisterStorer 后不再命中。
func TestTotpRegistry_RegisterUnregisterRoundtrip(t *testing.T) {
	const name = "totp-qr-test-4c3"
	want := []byte("png-bytes")
	if err := accesskey.RegisterStorer(name, stubProvider{out: want}); err != nil {
		t.Fatalf("RegisterStorer: %v", err)
	}
	p, ok := accesskey.GetStorer[Provider](name)
	if !ok {
		t.Fatalf("GetStorer[Provider](%q) 应命中", name)
	}
	out, err := p.Render("x")
	if err != nil {
		t.Fatalf("命中项 Render: %v", err)
	}
	if !bytes.Equal(out, want) {
		t.Fatalf("Render 输出不一致: got %q, want %q", out, want)
	}

	accesskey.UnregisterStorer(name)
	if _, ok := accesskey.GetStorer[Provider](name); ok {
		t.Fatalf("UnregisterStorer 后不应命中")
	}
}
