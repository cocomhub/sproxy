// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/tls"
	"fmt"
	"slices"
)

// ApplyTLSMasking 把被动伪装层参数（roadmap §5.3 P1，形态对齐）注入 tls.Config：
//   - CipherOrder 非空 → CipherSuites 应用对应顺序（贴近主流 HTTP 栈）
//   - ALPN 非空 → NextProtos 应用
//   - 两者为空 → 不修改（默认保守零回归）
//
// 返回 changed=true 表示实际产生了变更（可观测铁律：装配层据此输出启动日志，
// 禁静默降级——用户配置了但没生效必须能发现）。
// 非法 cipher 名返回错误（fail-closed，不静默忽略）。
func ApplyTLSMasking(tc *tls.Config, cfg *TLSConfig) (bool, error) {
	if tc == nil || cfg == nil {
		return false, nil
	}
	changed := false
	if len(cfg.CipherOrder) > 0 {
		suites := make([]uint16, 0, len(cfg.CipherOrder))
		for _, name := range cfg.CipherOrder {
			id, ok := tlsCipherID(name)
			if !ok {
				return false, fmt.Errorf("tls.cipher_order 含未知 cipher %q（fail-closed，不静默忽略）", name)
			}
			suites = append(suites, id)
		}
		tc.CipherSuites = suites
		changed = true
	}
	if len(cfg.ALPN) > 0 {
		// ALPN 是字符串协议名（如 http/1.1、h2），直接应用；空串非法（fail-closed）。
		if slices.Contains(cfg.ALPN, "") {
			return false, fmt.Errorf("tls.alpn 含空协议名（fail-closed）")
		}
		tc.NextProtos = append([]string(nil), cfg.ALPN...)
		changed = true
	}
	return changed, nil
}

// tlsCipherID 把 cipher 名字符串映射到 crypto/tls 常量 ID。
// 覆盖主流 TLS 1.3（默认）与常见 TLS 1.2 套件；未知名返回 false（fail-closed）。
func tlsCipherID(name string) (uint16, bool) {
	m := map[string]uint16{
		// TLS 1.3（Go 默认仅这些）。
		"TLS_AES_128_GCM_SHA256":       tls.TLS_AES_128_GCM_SHA256,
		"TLS_AES_256_GCM_SHA384":       tls.TLS_AES_256_GCM_SHA384,
		"TLS_CHACHA20_POLY1305_SHA256": tls.TLS_CHACHA20_POLY1305_SHA256,
		// TLS 1.2 常见套件。
		"TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256":   tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
		"TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384":   tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		"TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256": tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		"TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384": tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
		"TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305":    tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
		"TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305":  tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
	}
	id, ok := m[name]
	return id, ok
}
