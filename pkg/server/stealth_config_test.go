// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"testing"
)

// TestApplyTLSMasking_CipherOrderALPN 验证伪装参数注入：
//   - CipherOrder 非空 → CipherSuites 应用对应顺序
//   - ALPN 非空 → NextProtos 应用
//   - 两者为空 → 不修改（零回归）
func TestApplyTLSMasking_CipherOrderALPN(t *testing.T) {
	t.Parallel()
	base := &tls.Config{MinVersion: tls.VersionTLS12}

	// 零配置：不修改（零回归）。
	cfg := &TLSConfig{}
	changed, err := ApplyTLSMasking(base, cfg)
	if err != nil {
		t.Fatalf("空配置应无错误: %v", err)
	}
	if changed {
		t.Fatalf("空配置不应产生变更（零回归）")
	}
	if base.CipherSuites != nil || base.NextProtos != nil {
		t.Fatalf("空配置不应修改 tls.Config: %+v", base)
	}

	// 非空 CipherOrder + ALPN：注入。
	base2 := &tls.Config{MinVersion: tls.VersionTLS12}
	cfg2 := &TLSConfig{
		CipherOrder: []string{"TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384"},
		ALPN:        []string{"http/1.1", "h2"},
	}
	changed2, err := ApplyTLSMasking(base2, cfg2)
	if err != nil {
		t.Fatalf("合法配置应无错误: %v", err)
	}
	if !changed2 {
		t.Fatalf("非空配置应报告变更（可观测铁律）")
	}
	if len(base2.CipherSuites) != 2 {
		t.Fatalf("CipherSuites 应注入 2 个, got %d", len(base2.CipherSuites))
	}
	if base2.CipherSuites[0] != tls.TLS_AES_128_GCM_SHA256 {
		t.Fatalf("CipherSuites[0] = %d, want TLS_AES_128_GCM_SHA256", base2.CipherSuites[0])
	}
	if len(base2.NextProtos) != 2 || base2.NextProtos[0] != "http/1.1" {
		t.Fatalf("NextProtos 应注入 [http/1.1 h2], got %v", base2.NextProtos)
	}
}

// TestApplyTLSMasking_InvalidCipher 验证非法 cipher 名拒绝（fail-closed）。
func TestApplyTLSMasking_InvalidCipher(t *testing.T) {
	t.Parallel()
	base := &tls.Config{MinVersion: tls.VersionTLS12}
	cfg := &TLSConfig{CipherOrder: []string{"NOT_A_CIPHER"}}
	if _, err := ApplyTLSMasking(base, cfg); err == nil {
		t.Fatalf("非法 cipher 名应拒绝（fail-closed）")
	}
}

// TestWSConfig_PathDefaultAndCustom 验证 WS 路径默认 /ws 与自定义值可配：
//   - Path 空 = 默认 /ws（零回归）
//   - Path 非空 = 生效（不再被 S36 忽略）
func TestWSConfig_PathDefaultAndCustom(t *testing.T) {
	t.Parallel()
	// 默认（零配置）：Path 空，装配回落 /ws。
	cfg := Default()
	if cfg.Hub.Transports.WS.Path != "" {
		t.Fatalf("默认 WS Path 应为空（回落 /ws）")
	}
	// 自定义路径生效（配置解析后直接可读）。
	cfg2 := &Config{Hub: HubConfig{Transports: TransportConfigs{WS: WSTransportConfig{Path: "/api/v1/stream"}}}}
	cfg2.SetDefaults()
	if cfg2.Hub.Transports.WS.Path != "/api/v1/stream" {
		t.Fatalf("自定义 WS Path 应保留, got %q", cfg2.Hub.Transports.WS.Path)
	}
}

// TestIdlePadding_DefaultOff 验证空闲填充默认关（零回归）：Default() 的 IdlePadding=false。
func TestIdlePadding_DefaultOff(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.IdlePadding {
		t.Fatalf("空闲填充应默认关（零回归）")
	}
}
