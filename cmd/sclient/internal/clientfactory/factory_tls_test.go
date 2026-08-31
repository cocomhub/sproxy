// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory

import (
	"crypto/tls"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/certmgr"
)

// genTestCertFiles 生成一套自签服务端证书/密钥文件（auto_tls 生产场景一致：
// ECDSA P-256、IP SAN 127.0.0.1/::1、DNS localhost）。自签证书本身作为 CA 文件，
// 客户端 RootCAs 信任它即可校验自签服务端证书。
func genTestCertFiles(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	certFile := filepath.Join(dir, "test-cert.pem")
	keyFile := filepath.Join(dir, "test-key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("生成自签证书失败: %v", err)
	}
	return certFile, keyFile
}

// TestBuildXferClientTLSConfig_DefaultSystemPool 验证：未指定 ca-file / insecure 时
// 返回系统根池严格校验配置（RootCAs=nil、InsecureSkipVerify=false、MinVersion=TLS1.2）。
// 对齐 hub/federation 的 peer TLS 前例（fail-closed，不静默降级）。
func TestBuildXferClientTLSConfig_DefaultSystemPool(t *testing.T) {
	cfg, err := buildXferClientTLSConfig("", false, "flag --insecure", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("默认（无 ca-file 无 insecure）应成功: %v", err)
	}
	if cfg == nil {
		t.Fatal("返回 nil config")
	}
	if cfg.InsecureSkipVerify {
		t.Error("默认配置不得跳过证书校验")
	}
	if cfg.RootCAs != nil {
		t.Error("默认配置 RootCAs 应为 nil（走系统根池）")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want TLS1.2", cfg.MinVersion)
	}
}

// TestBuildXferClientTLSConfig_CASetsRootCAs 验证：ca-file 指向有效 PEM 证书时
// RootCAs 非空且严格校验（不跳过）。
func TestBuildXferClientTLSConfig_CASetsRootCAs(t *testing.T) {
	certFile, _ := genTestCertFiles(t)
	cfg, err := buildXferClientTLSConfig(certFile, false, "flag --insecure", "127.0.0.1:9999")
	if err != nil {
		t.Fatalf("ca-file 装配应成功: %v", err)
	}
	if cfg == nil || cfg.RootCAs == nil {
		t.Fatal("ca-file 装配后 RootCAs 应为非 nil")
	}
	if cfg.InsecureSkipVerify {
		t.Error("ca-file 装配不得跳过证书校验")
	}
	if cfg.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion = %v, want TLS1.2", cfg.MinVersion)
	}
}

// TestBuildXferClientTLSConfig_InsecureLoopback 验证：insecure + loopback hub 允许，
// 跳过证书校验（InsecureSkipVerify=true）。
func TestBuildXferClientTLSConfig_InsecureLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9999", "localhost:9999", "[::1]:9999"} {
		cfg, err := buildXferClientTLSConfig("", true, "flag --insecure", addr)
		if err != nil {
			t.Fatalf("loopback(%s) + insecure 应成功: %v", addr, err)
		}
		if cfg == nil || !cfg.InsecureSkipVerify {
			t.Fatalf("loopback(%s) + insecure 应跳过证书校验", addr)
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("MinVersion = %v, want TLS1.2", cfg.MinVersion)
		}
	}
}

// TestBuildXferClientTLSConfig_InsecureNonLoopbackRejected 验证：insecure + 非 loopback
// hub 时 fail-closed 拒绝（对齐 federation Config.Validate：远程 + insecure 禁止）。
func TestBuildXferClientTLSConfig_InsecureNonLoopbackRejected(t *testing.T) {
	_, err := buildXferClientTLSConfig("", true, "flag --insecure", "example.com:9999")
	if err == nil {
		t.Fatal("非 loopback + insecure 应 fail-closed 拒绝")
	}
	if !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("错误应指明 loopback 限制, 实际: %v", err)
	}
}

// TestBuildXferClientTLSConfig_CAAndInsecureMutuallyExclusive 验证：ca-file 与 insecure
// 互斥（同时指定报错）。
func TestBuildXferClientTLSConfig_CAAndInsecureMutuallyExclusive(t *testing.T) {
	certFile, _ := genTestCertFiles(t)
	_, err := buildXferClientTLSConfig(certFile, true, "flag --insecure", "127.0.0.1:9999")
	if err == nil {
		t.Fatal("ca-file 与 insecure 互斥应报错")
	}
}

// TestBuildXferClientTLSConfig_CABadFile 验证：ca-file 指向不存在/无有效证书的文件时
// 报错（fail-closed，不静默用系统根池）。
func TestBuildXferClientTLSConfig_CABadFile(t *testing.T) {
	if _, err := buildXferClientTLSConfig(filepath.Join(t.TempDir(), "missing.pem"), false, "flag --insecure", "127.0.0.1:9999"); err == nil {
		t.Fatal("不存在的 ca 文件应报错")
	}
	// 文件存在但无有效 PEM 证书（垃圾内容）→ 报错。
	bad := filepath.Join(t.TempDir(), "bad.pem")
	if err := os.WriteFile(bad, []byte("garbage-not-pem"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := buildXferClientTLSConfig(bad, false, "flag --insecure", "127.0.0.1:9999"); err == nil {
		t.Fatal("无有效 PEM 证书的 ca 文件应报错")
	}
}

// TestBuildXferClientTLSConfig_InsecureBadAddr 验证：insecure 时 hub 地址非法
// （无 host:port）报错。
func TestBuildXferClientTLSConfig_InsecureBadAddr(t *testing.T) {
	if _, err := buildXferClientTLSConfig("", true, "flag --insecure", "not-an-addr"); err == nil {
		t.Fatal("insecure 时非法 hub 地址应报错")
	}
}
