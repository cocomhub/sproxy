// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

// bench_test.go 把 QUIC 传输登记进跨传输吞吐基准（roadmap P2 端到端带宽基准）。

import (
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func init() {
	xfertest.RegisterBench(xfertest.Harness{Name: "quic", Dial: quic.Dial, Listen: quic.Listen})
}

// BenchmarkXferThroughput 运行 quic 传输的全链路吞吐基准（bench-baseline 自动纳入）。
// QUIC 需要 TLS 证书环境（复用 setupQUICTLS 生成 CA + leaf 并 Setenv）。
func BenchmarkXferThroughput(b *testing.B) {
	// b 不是 *testing.T——setupQUICTLS 需 t.Setenv——**bench 不支持 Setenv**：
	// 用环境变量注入（CI 配 SPROXY_QUIC_CERT_FILE/KEY/CA 或回落自签证书）
	// 实际 Listen 回落自签 cert，Dial 校验失败（无 CA）——**基准需显式 CA**。
	// 方案：bench 内生成临时证书并 Setenv（os.Setenv 后清理）。
	dir, _ := os.MkdirTemp("", "quic-bench-*")
	defer os.RemoveAll(dir)
	// 复用 writeTestCertFiles（同包测试 helper）。
	cert, key, ca := writeTestCertFiles(&testing.T{}, dir)
	_ = os.Setenv("SPROXY_QUIC_CERT_FILE", cert)
	_ = os.Setenv("SPROXY_QUIC_KEY_FILE", key)
	_ = os.Setenv("SPROXY_QUIC_CA_CERT", ca)
	defer os.Unsetenv("SPROXY_QUIC_CERT_FILE")
	defer os.Unsetenv("SPROXY_QUIC_KEY_FILE")
	defer os.Unsetenv("SPROXY_QUIC_CA_CERT")
	xfertest.BenchmarkXferThroughput(b)
}
