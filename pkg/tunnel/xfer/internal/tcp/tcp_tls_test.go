// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	_ "github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin" // 注册内置 tcp（init）
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/internal/tcp"
)

// testTLSConfig 生成一套完整的自签 CA + 服务端证书，返回服务端/客户端 *tls.Config。
//
// 关键点（避免 "certificate signed by unknown authority / invalid signature"）：
//   - 先建**自签 root CA**（IsCA=true、KeyUsageCertSign），再用 CA 签发服务端证书；
//   - 客户端信任池只放 root CA 证书；
//   - 服务端 tls.Certificate 的 Leaf 显式解析（x509.ParseCertificate），避免
//     tls.X509KeyPair 不设 Leaf 导致校验链拿不到中间证书。
func testTLSConfig(t *testing.T) (*tls.Config, *tls.Config) {
	t.Helper()

	newKey := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatalf("生成 ECDSA 私钥: %v", err)
		}
		return k
	}

	// 1) 自签 root CA
	caKey := newKey()
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tcp+tls test root CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("创建 root CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("解析 root CA: %v", err)
	}

	// 2) 服务端证书（CA 签发）
	srvKey := newKey()
	srvTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	srvDER, err := x509.CreateCertificate(rand.Reader, srvTmpl, caCert, &srvKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("创建服务端证书: %v", err)
	}
	srvKeyPEM, err := x509.MarshalECPrivateKey(srvKey)
	if err != nil {
		t.Fatalf("marshal 服务端私钥: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srvDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: srvKeyPEM})
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair: %v", err)
	}
	leaf, err := x509.ParseCertificate(srvDER)
	if err != nil {
		t.Fatalf("解析服务端证书: %v", err)
	}
	cert.Leaf = leaf // 显式设 Leaf：避免校验时缺中间证书

	// 3) 客户端信任池 = root CA
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	serverCfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	clientCfg := &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
	return serverCfg, clientCfg
}

// TestTcpTLS_RoundTrip 验证 DialTLS ↔ ListenTLS 的 TLS 握手 + 载荷往返。
func TestTcpTLS_RoundTrip(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	serverCfg, clientCfg := testTLSConfig(t)

	ln, err := tcp.ListenTLS(ctx, "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	defer ln.Close()
	lna, _ := ln.(interface{ Addr() net.Addr })
	addr := lna.Addr()

	var serverConn xfer.Conn
	var acceptErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		serverConn, acceptErr = ln.Accept(ctx)
	})
	time.Sleep(50 * time.Millisecond)

	clientConn, err := tcp.DialTLS(ctx, addr.String(), clientCfg)
	if err != nil {
		t.Fatalf("DialTLS: %v", err)
	}
	defer clientConn.Close()

	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
	if serverConn == nil {
		t.Fatal("expected server conn")
	}
	defer serverConn.Close()

	msg := []byte("hello tcp+tls")
	if err := clientConn.Send(ctx, msg); err != nil {
		t.Fatalf("client Send: %v", err)
	}
	if err := serverConn.Send(ctx, []byte("reply")); err != nil {
		t.Fatalf("server Send: %v", err)
	}
	got := make([]byte, len(msg))
	if err := readFull(ctx, serverConn, got); err != nil {
		t.Fatalf("server Receive: %v", err)
	}
	if string(got) != string(msg) {
		t.Fatalf("server 收到 %q, want %q", got, msg)
	}
	// 双向往返（审查 M-3）：客户端也必须能解密读取服务端 Send 的消息。
	reply := make([]byte, len("reply"))
	if err := readFull(ctx, clientConn, reply); err != nil {
		t.Fatalf("client Receive: %v", err)
	}
	if string(reply) != "reply" {
		t.Fatalf("client 收到 %q, want %q", reply, "reply")
	}
}

// TestTcpTLS_WrongCARejected 验证客户端不信任服务端证书时握手失败（fail-closed）。
// 用带短超时的 ctx 断言握手**快速**失败（非阻塞到超时）。
func TestTcpTLS_WrongCARejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	serverCfg, _ := testTLSConfig(t)

	ln, err := tcp.ListenTLS(ctx, "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	defer ln.Close()
	lna, _ := ln.(interface{ Addr() net.Addr })
	addr := lna.Addr()

	// 服务端必须接受连接并参与 TLS 握手，客户端才会收到服务端证书后校验失败。
	// TlsListener 内部并发握手：accept 循环接受连接、后台 handshakeConn 握手；
	// wrong-CA 客户端握手失败 → 该连接被丢弃，accept 继续等待。goroutine 随
	// ctx 取消 / ln.Close 退出。
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			if _, aerr := ln.Accept(ctx); aerr != nil {
				return
			}
		}
	})
	// defer LIFO：先 cancel（accept goroutine 立即退出）再 wg.Wait，ln.Close 最后收尾，
	// 避免 3s 死等拖尾（审查 I-2）。
	defer func() { cancel(); wg.Wait() }()

	// 客户端信任一个无关的 self-signed CA（同函数生成、不同 CA）。
	_, otherCfg := testTLSConfig(t)

	// 客户端 VerifyingTimeout：握手时校验失败应立即返回（证书不受信任），
	// 不应阻塞到 ctx 截止（fail-closed 语义）。设 2s 检查避免把超时当通过。
	dialCtx, dcancel := context.WithTimeout(ctx, 2*time.Second)
	defer dcancel()
	start := time.Now()
	_, err = tcp.DialTLS(dialCtx, addr.String(), otherCfg)
	if err == nil {
		t.Fatal("不受信任 CA 的客户端握手应失败（fail-closed）")
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("握手失败应快速返回（fail-closed），耗时 %.2fs", elapsed.Seconds())
	}
}

// TestTcpTLS_RegistryVariant 验证 "tcp+tls" 注册变体：SetDefaultTLSConfig 后就绪，
// 缺省（未设置）时 Dial 报错（fail-closed，防无凭据明文）。
func TestTcpTLS_RegistryVariant(t *testing.T) {
	tp := xfer.Get("tcp+tls")
	if tp == nil {
		t.Fatal(`xfer.Get("tcp+tls") 应返回已注册的 TLS 变体`)
	}

	// 未设置默认配置 → Dial / Listen 明确报错（fail-closed，审查 M-4）。
	ctx := t.Context()
	tcp.SetDefaultTLSConfig(nil)
	if _, err := tp.Dial(ctx, "127.0.0.1:1"); err == nil {
		t.Fatal("未设置默认 TLS 配置时 tcp+tls 变体 Dial 应报错")
	}
	if _, err := tp.Listen(ctx, "127.0.0.1:0"); err == nil {
		t.Fatal("未设置默认 TLS 配置时 tcp+tls 变体 Listen 应报错")
	}

	// 设置后即可用：用一个**自包含**配置（同时含服务端证书 + 客户端信任池 +
	// ServerName）设为默认，使 tp.Listen（服务端用）+ tp.Dial（客户端用）都能成功。
	serverCfg, clientCfg := testTLSConfig(t)
	combined := &tls.Config{
		Certificates: serverCfg.Certificates,
		RootCAs:      clientCfg.RootCAs,
		ServerName:   "127.0.0.1",
		MinVersion:   tls.VersionTLS12,
	}
	t.Cleanup(func() { tcp.SetDefaultTLSConfig(nil) })
	tcp.SetDefaultTLSConfig(combined)

	lctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ln, err := tp.Listen(lctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp+tls Listen（默认配置）: %v", err)
	}
	defer ln.Close()
	lna, _ := ln.(interface{ Addr() net.Addr })
	addr := lna.Addr()

	var wg sync.WaitGroup
	var acceptErr error
	wg.Go(func() {
		_, acceptErr = ln.Accept(lctx)
	})
	time.Sleep(50 * time.Millisecond)
	c, err := tp.Dial(lctx, addr.String())
	if err != nil {
		t.Fatalf("tcp+tls Dial（默认配置）: %v", err)
	}
	defer c.Close()
	wg.Wait()
	if acceptErr != nil {
		t.Fatal(acceptErr)
	}
}

// TestTcpTLS_HandshakeFailureSkips 验证：对端发垃圾/TLS 握手失败时，Accept 跳过该
// 连接继续接受（不返回错误、Listener 不停摆）。
func TestTcpTLS_HandshakeFailureSkips(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	serverCfg, goodClient := testTLSConfig(t)
	ln, err := tcp.ListenTLS(ctx, "127.0.0.1:0", serverCfg)
	if err != nil {
		t.Fatalf("ListenTLS: %v", err)
	}
	defer ln.Close()
	lna, _ := ln.(interface{ Addr() net.Addr })
	addr := lna.Addr()

	// 1) 先开一个"假 TLS"连接（明文垃圾）→ 服务端握手失败跳过。
	raw, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial raw: %v", err)
	}
	_, _ = raw.Write([]byte("not a tls handshake"))
	_ = raw.Close()

	// 2) 再开真实 TLS 连接 → 必须仍能被接受（证明 ListenTLS 没被垃圾打死）。
	// 直接用 first testTLSConfig 生成的 goodClient（与其 serverCfg 同一 CA 体系；
	// testTLSConfig 每次调用生成新 CA，二次调用无法信任第一个服务端证书）。
	accepted := make(chan xfer.Conn, 1)
	go func() {
		c, aerr := ln.Accept(ctx)
		accepted <- c
		if aerr != nil {
			t.Errorf("Accept 错误: %v", aerr)
		}
	}()
	time.Sleep(100 * time.Millisecond)

	cc, err := tcp.DialTLS(ctx, addr.String(), goodClient)
	if err != nil {
		t.Fatalf("good client DialTLS: %v", err)
	}
	defer cc.Close()

	select {
	case sc := <-accepted:
		if sc == nil {
			t.Fatal("真实 TLS 连接应被接受")
		}
		defer sc.Close()
	case <-time.After(5 * time.Second):
		t.Fatal("真实 TLS 连接未在 5s 内被接受（垃圾连接可能打死了 listener）")
	}
}

// readFull 从 xfer.Conn 读取恰好 len(buf) 字节（Receive 消息边界语义：解析长度前缀）。
func readFull(ctx context.Context, c xfer.Conn, buf []byte) error {
	n := 0
	for n < len(buf) {
		msg, err := c.Receive(ctx)
		if err != nil {
			return err
		}
		copy(buf[n:], msg)
		n += len(msg)
	}
	return nil
}
