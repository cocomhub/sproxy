// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory_test

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adrg/xdg"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/builtin"
	"github.com/spf13/cobra"
)

// genIntegrationTestCerts 生成自签服务端证书/密钥文件，并构造服务端 *tls.Config。
// 自签证书文件同时用作客户端 CA 文件（auto_tls 生产场景一致）。
func genIntegrationTestCerts(t *testing.T) (serverCfg *tls.Config, caFile string) {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "test-cert.pem")
	keyFile := filepath.Join(dir, "test-key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("加载服务端证书失败: %v", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, certFile
}

// startTestTLSTransport 经 registry 启动 tcp+tls listener（builtin 设默认 → Listen 捕获
// 配置），后台 accept 循环完成 TLS 握手后关闭连接（防 listener 未 accept 导致 Dial
// 无法完成握手）。返回监听地址。调用方负责清理全局默认 TLS 配置（t.Cleanup）。
func startTestTLSTransport(t *testing.T, ctx context.Context, srvCfg *tls.Config) string {
	t.Helper()
	tp := xfer.Get("tcp+tls")
	if tp == nil {
		t.Fatal(`xfer.Get("tcp+tls") 应返回已注册的 TLS 变体`)
	}
	builtin.SetDefaultTLSConfig(srvCfg)
	t.Cleanup(func() { builtin.SetDefaultTLSConfig(nil) })
	ln, err := tp.Listen(ctx, "127.0.0.1:0")
	if err != nil {
		t.Fatalf("tcp+tls Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	addrLn, ok := ln.(interface{ Addr() net.Addr })
	if !ok {
		t.Fatal("tcp+tls listener 未暴露 Addr()")
	}
	addr := addrLn.Addr().String()
	go func() {
		for {
			c, aErr := ln.Accept(ctx)
			if aErr != nil {
				return // ctx 取消 / listener 关闭
			}
			_ = c.Close()
		}
	}()
	return addr
}

// newXferTLSTestClient 用真实 factory 构造 sclient tunnel --xfer tcp+tls 客户端。
// flags 追加 --ca-file / --insecure 等 flag 覆盖；hub 指向自签 TLS listener。
func newXferTLSTestClient(t *testing.T, hubAddr string, flags map[string]string) (*client.FileClient, error) {
	t.Helper()
	dir := t.TempDir()
	oldHome := xdg.ConfigHome
	xdg.ConfigHome = dir
	t.Cleanup(func() { xdg.ConfigHome = oldHome })

	cfgBody := "server_url: https://127.0.0.1:1\nhub_url: " + hubAddr + "\n"
	cfgFile := filepath.Join(dir, "sclient.yaml")
	if err := os.WriteFile(cfgFile, []byte(cfgBody), 0600); err != nil {
		t.Fatal(err)
	}
	provider := sclientcfg.New(cfgFile)
	factory := clientfactory.New(cfgFile, func() clientfactory.CfgBinder { return provider })

	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	cmd.Flags().String("ca-file", "", "")
	cmd.Flags().Bool("insecure", false, "")
	if err := cmd.Flags().Set("xfer", "tcp+tls"); err != nil {
		t.Fatal(err)
	}
	if hubAddr != "" {
		if err := cmd.Flags().Set("hub", hubAddr); err != nil {
			t.Fatal(err)
		}
	}
	for k, v := range flags {
		if err := cmd.Flags().Set(k, v); err != nil {
			t.Fatal(err)
		}
	}
	return factory.NewClient(cmd)
}

// TestFactory_NewClient_XferTLS_CAWired 验证：--xfer tcp+tls + --ca-file <自签CA> 经
// clientfactory.NewClient 装配客户端 TLS 配置后，xfer tcp+tls 传输可连到自签 TLS 服务端
// （builtin.SetDefaultTLSConfig 已调用且 RootCAs 含该自签 CA）。
func TestFactory_NewClient_XferTLS_CAWired(t *testing.T) {
	srvCfg, caFile := genIntegrationTestCerts(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addr := startTestTLSTransport(t, ctx, srvCfg)

	svc, err := newXferTLSTestClient(t, addr, map[string]string{"ca-file": caFile})
	if err != nil {
		t.Fatalf("NewClient（--xfer tcp+tls --ca-file）应成功: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}

	// 直接经 tcp+tls 传输拨号：证明默认 TLS 配置（RootCAs=自签CA）已装配。
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	conn, err := xfer.Get("tcp+tls").Dial(dctx, addr)
	if err != nil {
		t.Fatalf("tcp+tls Dial（NewClient 装配后）应成功: %v", err)
	}
	_ = conn.Close()
}

// TestFactory_NewClient_XferTLS_InsecureLoopbackWired 验证：--insecure + loopback hub
// 装配跳过证书校验后同样可连到自签 TLS 服务端。
func TestFactory_NewClient_XferTLS_InsecureLoopbackWired(t *testing.T) {
	srvCfg, _ := genIntegrationTestCerts(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addr := startTestTLSTransport(t, ctx, srvCfg)

	svc, err := newXferTLSTestClient(t, addr, map[string]string{"insecure": "true"})
	if err != nil {
		t.Fatalf("NewClient（--xfer tcp+tls --insecure loopback）应成功: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	conn, err := xfer.Get("tcp+tls").Dial(dctx, addr)
	if err != nil {
		t.Fatalf("tcp+tls Dial（--insecure 装配后）应成功: %v", err)
	}
	_ = conn.Close()
}

// TestFactory_NewClient_XferTLS_NoTLSConfigFailsClosed 验证：--xfer tcp+tls 未配
// --ca-file 也未 --insecure 时，NewClient 装配系统根池严格校验配置（不报错），但
// 连自签服务端握手失败（x509 unknown-authority）——设计决策：默认系统根池尝试，
// 服务端自签必须显式 --ca-file / --insecure，fail-closed 不静默降级。
func TestFactory_NewClient_XferTLS_NoTLSConfigFailsClosed(t *testing.T) {
	srvCfg, _ := genIntegrationTestCerts(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addr := startTestTLSTransport(t, ctx, srvCfg)

	svc, err := newXferTLSTestClient(t, addr, nil)
	if err != nil {
		t.Fatalf("NewClient（无 ca-file 无 insecure）应成功（装配系统根池）: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	dctx, dcancel := context.WithTimeout(ctx, 5*time.Second)
	defer dcancel()
	conn, err := xfer.Get("tcp+tls").Dial(dctx, addr)
	if err == nil {
		_ = conn.Close()
		t.Fatal("系统根池连自签服务端应握手失败（fail-closed）")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("错误应指明证书校验失败, 实际: %v", err)
	}
}

// TestFactory_NewClient_XferTLS_CAAndInsecureMutuallyExclusive 验证：--ca-file 与
// --insecure 同时指定时 NewClient fail-closed 报错。
func TestFactory_NewClient_XferTLS_CAAndInsecureMutuallyExclusive(t *testing.T) {
	srvCfg, caFile := genIntegrationTestCerts(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	addr := startTestTLSTransport(t, ctx, srvCfg)

	_, err := newXferTLSTestClient(t, addr, map[string]string{"ca-file": caFile, "insecure": "true"})
	if err == nil {
		t.Fatal("ca-file 与 insecure 互斥应报错")
	}
}

// TestFactory_NewClient_XferTLS_InsecureNonLoopbackRejected 验证：--insecure + 非
// loopback hub 时 NewClient fail-closed 拒绝（对齐 federation Config.Validate）。
func TestFactory_NewClient_XferTLS_InsecureNonLoopbackRejected(t *testing.T) {
	_, err := newXferTLSTestClient(t, "example.com:9999", map[string]string{"insecure": "true"})
	if err == nil {
		t.Fatal("非 loopback + insecure 应 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("错误应指明 loopback 限制, 实际: %v", err)
	}
}

// TestFactory_NewClient_XferTLS_NonTLSTransportNotAffected 验证：非 TLS 传输（tcp）时
// 不装配 TLS 默认配置——即使传入 --ca-file 也不报错（防非 TLS 传输被 TLS 装配干扰）。
func TestFactory_NewClient_XferTLS_NonTLSTransportNotAffected(t *testing.T) {
	dir := t.TempDir()
	oldHome := xdg.ConfigHome
	xdg.ConfigHome = dir
	t.Cleanup(func() { xdg.ConfigHome = oldHome })

	cfgBody := "server_url: https://127.0.0.1:1\nhub_url: 127.0.0.1:9999\n"
	cfgFile := filepath.Join(dir, "sclient.yaml")
	if err := os.WriteFile(cfgFile, []byte(cfgBody), 0600); err != nil {
		t.Fatal(err)
	}
	provider := sclientcfg.New(cfgFile)
	factory := clientfactory.New(cfgFile, func() clientfactory.CfgBinder { return provider })

	cmd := &cobra.Command{}
	cmd.Flags().String("server", "", "")
	cmd.Flags().String("xfer", "", "")
	cmd.Flags().String("hub", "", "")
	cmd.Flags().String("ca-file", "", "")
	cmd.Flags().Bool("insecure", false, "")
	if err := cmd.Flags().Set("xfer", "tcp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("ca-file", "whatever.pem"); err != nil {
		t.Fatal(err)
	}
	svc, err := factory.NewClient(cmd)
	if err != nil {
		t.Fatalf("非 TLS 传输（tcp）装配 TLS 配置不应报错: %v", err)
	}
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
}
