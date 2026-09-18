// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"crypto/x509"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/certmgr"
)

// ---- 直连面 TLS CA（--ca-file 补全）----

// writeSelfSignedCA 生成一套自签服务端证书（certmgr.GenerateSelfSignedCert 同 auto_tls
// 生产形态：ECDSA P-256 / SAN localhost+127.0.0.1），并把证书文件同时作为 CA 文件返回
// （自签证书即自签名 CA，与 sclient --ca-file 分发场景一致）。
func writeSelfSignedCA(t *testing.T) (certFile, keyFile, caFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "srv-cert.pem")
	keyFile = filepath.Join(dir, "srv-key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	return certFile, keyFile, certFile
}

// TestWithCAFile_SetsRootCAs 验证 WithCAFile 把 PEM CA 装配为直连 Transport 的 RootCAs：
// 证书池含该 CA、不跳过证书校验（严格模式）、Timeout 保留。
func TestWithCAFile_SetsRootCAs(t *testing.T) {
	t.Parallel()
	_, _, caFile := writeSelfSignedCA(t)

	c := NewFileClient("https://sproxy.local:18083", WithCAFile(caFile))
	if c.httpClient == nil {
		t.Fatal("httpClient should not be nil")
	}
	if c.InitError() != nil {
		t.Fatalf("WithCAFile 有效 CA 不应有 InitError: %v", c.InitError())
	}
	transport, ok := c.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("expected *http.Transport")
	}
	if transport.TLSClientConfig == nil {
		t.Fatal("expected TLSClientConfig to be set")
	}
	if transport.TLSClientConfig.RootCAs == nil {
		t.Fatal("expected RootCAs to be set from CA file")
	}
	if transport.TLSClientConfig.InsecureSkipVerify {
		t.Error("WithCAFile 不得跳过证书校验（必须严格校验）")
	}
	if len(transport.TLSClientConfig.Certificates) != 0 {
		t.Error("WithCAFile 不应装配客户端证书")
	}
	// Timeout 保留（与 WithInsecureTLS 同语义）。
	if c.httpClient.Timeout != 300*time.Second {
		t.Errorf("expected timeout 300s, got %v", c.httpClient.Timeout)
	}
}

// TestWithCAFile_NonexistentFile 验证 CA 文件不存在/无有效 PEM 时 fail-closed 报错
// （不静默用系统根池）。
func TestWithCAFile_NonexistentFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	c := NewFileClient("https://127.0.0.1:18083", WithCAFile(filepath.Join(dir, "missing.pem")))
	if c.InitError() == nil {
		t.Fatal("WithCAFile 指向不存在文件应记录 InitError（fail-closed）")
	}
}

// TestWithCAFile_EmptyPEM 验证空文件同样 fail-closed。
func TestWithCAFile_EmptyPEM(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a pem"), 0644); err != nil {
		t.Fatal(err)
	}
	c := NewFileClient("https://127.0.0.1:18083", WithCAFile(empty))
	if c.InitError() == nil {
		t.Fatal("WithCAFile 指向无有效 PEM 文件应记录 InitError（fail-closed）")
	}
}

// TestWithCAFile_DirectTLSRoundTrip 验证直连面 CA 生效的端到端行为：自签 HTTPS 服务端
// （httptest.NewTLSServer）用 WithCAFile 信任其自签证书后，普通文件 API 请求成功；
// 不配 CA（系统根池）则握手失败（证明 CA 是生效开关，不是假绿）。
func TestWithCAFile_DirectTLSRoundTrip(t *testing.T) {
	t.Parallel()
	ts := newSelfSignedTLSServer(t)
	// 自签服务端证书导出为 CA 文件。
	caFile := writeServerCertAsCA(t, ts)

	c := NewFileClient(ts.URL, WithCAFile(caFile))
	resp, err := c.doRequest(t.Context(), http.MethodGet, "/healthz", nil, nil)
	if err != nil {
		t.Fatalf("WithCAFile 直连自签服务端应成功, got: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

// TestDirectTLS_WithoutCA_Fails 验证对照：同一自签服务端不配 CA（系统根池严格校验）
// 时握手失败——证明 WithCAFile 是让测试从红变绿的唯一开关（非假绿）。
func TestDirectTLS_WithoutCA_Fails(t *testing.T) {
	t.Parallel()
	ts := newSelfSignedTLSServer(t)

	c := NewFileClient(ts.URL)
	_, err := c.doRequest(t.Context(), http.MethodGet, "/healthz", nil, nil)
	if err == nil {
		t.Fatal("系统根池连自签服务端应握手失败（fail-closed）")
	}
	if !strings.Contains(err.Error(), "certificate") && !strings.Contains(err.Error(), "x509") {
		t.Fatalf("错误应指明证书校验失败, got: %v", err)
	}
}

// TestWithCAFile_RelayTLSConfigInherits 验证 relayTLSConfig 从 Transport 继承 WithCAFile
// 装配的 RootCAs（I34 语义延续：私有 CA 场景中继拨号可用）。
func TestWithCAFile_RelayTLSConfigInherits(t *testing.T) {
	t.Parallel()
	_, _, caFile := writeSelfSignedCA(t)

	c := NewFileClient("https://hub.example.com:18083", WithCAFile(caFile))
	cfg := c.relayTLSConfig()
	if cfg.RootCAs == nil {
		t.Fatal("relayTLSConfig 应继承 WithCAFile 的 RootCAs")
	}
	if cfg.InsecureSkipVerify {
		t.Error("relayTLSConfig 不应跳过证书校验（WithCAFile 是严格模式）")
	}
}

// newSelfSignedTLSServer 启动自签 HTTPS 服务端（httptest.NewTLSServer 返回的自签
// 证书即本测试的 CA 源）。
func newSelfSignedTLSServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("OK"))
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// writeServerCertAsCA 把 httptest TLS 服务端的自签证书写为 CA 文件并返回路径。
func writeServerCertAsCA(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}))) {
		t.Fatal("导出自签服务端证书失败")
	}
	caFile := filepath.Join(t.TempDir(), "srv-ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ts.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	return caFile
}
