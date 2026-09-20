// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/testutil"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"  // 注册内置 tcp 传输
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic" // 注册 QUIC 传输层
)

// TestRelayStart_QUICTransport_NoWS_RelayDial 是 QUIC 装配的 CLI 级端到端验证：
// 经真实 runRelayOnce（--transport quic）注册叶子到**仅 QUIC** 的 hub，再经
// relay dial 语义（/api/relay/stream）拨号 echo 服务成功。
//
// 拓扑：echo server ⇄ leaf(relay.Serve, 出口模式, 服务宣告 echo) ⇄ hub(仅 QUIC)
// ⇄ RelayStreamHandler ⇄ caller(原始 TCP CONNECT 风格)。
//
// 与 TestRelayStart_TCPTransport_NoWS_RelayDial 完全同构，仅传输层从 tcp 换成
// quic（UDP）：hub 用 xfer.Get("quic").Listen + hubSrv.AcceptTCP（AcceptTCP 是
// 通用 xfer.Listener 抽象，传输无关），leaf 用 runRelayOnce(transport="quic")。
// QUIC 自带 TLS，需注入互相校验的证书 env（SPROXY_QUIC_CERT_FILE/KEY/CA_CERT）。
func TestRelayStart_QUICTransport_NoWS_RelayDial(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	setupQUICTestCertEnv(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	// 1. echo server
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echoLn.Close()
	go func() {
		for {
			c, aerr := echoLn.Accept()
			if aerr != nil {
				return
			}
			go func(cn net.Conn) {
				defer cn.Close()
				_, _ = io.Copy(cn, cn)
			}(c)
		}
	}()
	echoAddr := echoLn.Addr().String()

	// 2. hub：仅 QUIC（无 WS/无 TCP），SproxySig 准入
	const (
		ak = "ak-relay-quic-00000000000000000000"
		sk = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	)
	rt := hub.NewMeshRouteTable()
	hs := hub.NewHubServer(rt, hub.NewAuthenticator(accesskey.NewRingFromKeyPairs([]accesskey.KeyPair{{Key: ak, Secret: sk}})), testutil.DiscardLogger())
	qTP := xfer.Get("quic")
	if qTP == nil {
		t.Fatal("quic 传输层未注册（ext/quic import 应触发 init 注册）")
	}
	qln, err := qTP.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hub QUIC listen 失败: %v", err)
	}
	defer qln.Close()
	go func() { _ = hs.AcceptTCP(ctx, qln) }()
	hubAddr := qln.(interface{ Addr() string }).Addr()

	// 3. leaf：真实 runRelayOnce，transport=quic，声明 echo 服务 + 出口模式
	leafErr := make(chan error, 1)
	go func() {
		leafErr <- runRelayOnce(ctx, "quic", "leaf-cli-quic", hubAddr, "http://127.0.0.1:1",
			ak, sk, "", false, "", true, []string{"echo:" + echoAddr}, nil, hub.DefaultVirtualSubnet, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()

	// 4. 等待叶子注册进路由表
	testutil.WaitFor(t, 30*time.Second, func() bool { return rt.Has("leaf-cli-quic") },
		"leaf-cli-quic not registered in time")

	// 5. RelayStreamHandler + httptest（等价 relay dial 的 HTTP 面）
	h := server.NewRelayStreamHandler(rt, testutil.DiscardLogger())
	tsrv := httptest.NewServer(h)
	defer tsrv.Close()

	// 6. 原始 TCP CONNECT 风格拨号（等价 FileClient.RelayStream）
	srvAddr := strings.TrimPrefix(tsrv.URL, "http://")
	conn, err := net.Dial("tcp", srvAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	body, _ := json.Marshal(server.RelayStreamRequest{Target: "leaf-cli-quic", Type: "tcp", Addr: echoAddr})
	reqLine := fmt.Sprintf("POST /api/relay/stream HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", srvAddr, len(body))
	if _, werr := io.WriteString(conn, reqLine); werr != nil {
		t.Fatal(werr)
	}
	if _, werr := conn.Write(body); werr != nil {
		t.Fatal(werr)
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(statusLine, " 200 ") {
		rest, _ := io.ReadAll(io.LimitReader(br, 4<<10))
		t.Fatalf("hub 返回 %s%s", strings.TrimSpace(statusLine), rest)
	}
	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil {
			t.Fatal(rerr)
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}

	// 7. 双向字节流：写 payload 读回 echo
	payload := []byte("cli-quic-relay-dial-ok")
	if _, werr := conn.Write(payload); werr != nil {
		t.Fatalf("写失败: %v", werr)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(conn, got); rerr != nil {
		t.Fatalf("读失败: %v", rerr)
	}
	if string(got) != string(payload) {
		t.Fatalf("echo 不匹配: got %q want %q", got, payload)
	}

	cancel()
	select {
	case <-leafErr:
	case <-time.After(2 * time.Second):
		t.Fatal("runRelayOnce 未退出")
	}
}

// setupQUICTestCertEnv 生成一套可互相校验的 QUIC TLS 证书环境并注入 env：
// Listen 加载 leaf（SPROXY_QUIC_CERT_FILE/KEY_FILE），Dial 用 CA 池校验
// （SPROXY_QUIC_CA_CERT）。与 ext/quic 测试的 setupQUICTLS 同构（纯 stdlib）。
// 注意：t.Setenv 不能在 t.Parallel 后调用，本测试未并行化（quic 网络测试）。
func setupQUICTestCertEnv(t *testing.T) {
	t.Helper()
	certPath, keyPath, caPath := writeQUICTestCertFiles(t, t.TempDir())
	t.Setenv("SPROXY_QUIC_CERT_FILE", certPath)
	t.Setenv("SPROXY_QUIC_KEY_FILE", keyPath)
	t.Setenv("SPROXY_QUIC_CA_CERT", caPath)
}

// writeQUICTestCertFiles 生成 CA + leaf 证书对（与 ext/quic tls_test_helper 同构，
// 全部运行时 ecdsa 生成，无静态私钥）。返回 leaf 证书、leaf 私钥、CA 证书文件路径；
// leaf 带 127.0.0.1 / ::1 / localhost SAN 与 ServerAuth 用途。
func writeQUICTestCertFiles(t *testing.T, dir string) (certPath, keyPath, caPath string) {
	t.Helper()

	now := time.Now()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sproxy-quic-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA cert: %v", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf cert: %v", err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyDER, _ := x509.MarshalPKCS8PrivateKey(leafKey)
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	certFile := filepath.Join(dir, "leaf.pem")
	keyFile := filepath.Join(dir, "leaf-key.pem")
	caFile := filepath.Join(dir, "ca.pem")
	for path, data := range map[string][]byte{certFile: leafPEM, keyFile: leafKeyPEM, caFile: caPEM} {
		if werr := os.WriteFile(path, data, 0o600); werr != nil {
			t.Fatalf("write %s: %v", path, werr)
		}
	}
	return certFile, keyFile, caFile
}
