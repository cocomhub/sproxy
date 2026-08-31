// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"crypto/tls"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestXferListenerConfigFromConfig 验证从 server.Config 提取 xfer listener 配置：
// 证书缺省回落 cfg.TLS.*，access_key 派生参数取 access_keys 首对。
func TestXferListenerConfigFromConfig(t *testing.T) {
	cfg := Default()
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	cfg.AccessKeys = []AccessKeyConfig{
		{Key: "sk-mesh1-aaaaaaaaaaaaaaaa", Secret: strings.Repeat("a", 64), MeshID: "mesh1"},
		{Key: "sk-other-bbbbbbbbbbbbbbbb", Secret: strings.Repeat("b", 64), MeshID: "other"},
	}

	xc := XferListenerConfigFromConfig(cfg)
	if xc.CertFile != cfg.TLS.CertFile {
		t.Errorf("CertFile 应回落 cfg.TLS.CertFile %q，实际 %q", cfg.TLS.CertFile, xc.CertFile)
	}
	if xc.KeyFile != cfg.TLS.KeyFile {
		t.Errorf("KeyFile 应回落 cfg.TLS.KeyFile %q，实际 %q", cfg.TLS.KeyFile, xc.KeyFile)
	}
	if xc.AccessKey != cfg.AccessKeys[0].Key {
		t.Errorf("AccessKey 应取首对 %q，实际 %q", cfg.AccessKeys[0].Key, xc.AccessKey)
	}
	if xc.AccessKeySecret != cfg.AccessKeys[0].Secret {
		t.Errorf("AccessKeySecret 应取首对 %q，实际 %q", cfg.AccessKeys[0].Secret, xc.AccessKeySecret)
	}
	if xc.MeshID != cfg.AccessKeys[0].MeshID {
		t.Errorf("MeshID 应取首对 %q，实际 %q", cfg.AccessKeys[0].MeshID, xc.MeshID)
	}

	// nil 配置 → 零值（不 panic）。
	var zero XferListenerConfig
	if got := XferListenerConfigFromConfig(nil); got != zero {
		t.Errorf("nil 配置应返回零值，实际 %+v", got)
	}
}

// TestBuildXferTLSConfig_CertFiles 验证配置了 cert_file/key_file 时构造 listener 侧
// *tls.Config：非 nil、MinVersion=TLS1.2、Certificates 长度 1。
func TestBuildXferTLSConfig_CertFiles(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "cert.pem")
	keyFile := filepath.Join(dir, "key.pem")
	if err := certmgr.GenerateSelfSignedCert(certFile, keyFile); err != nil {
		t.Fatalf("GenerateSelfSignedCert: %v", err)
	}

	cfg := Default()
	cfg.TLS.CertFile = certFile
	cfg.TLS.KeyFile = keyFile
	cfg.TLS.AutoTLS = false

	tc, err := BuildXferTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildXferTLSConfig 应成功: %v", err)
	}
	if tc == nil {
		t.Fatal("BuildXferTLSConfig 返回 nil config")
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion 应为 TLS1.2 (0x%x)，实际 0x%x", tls.VersionTLS12, tc.MinVersion)
	}
	if len(tc.Certificates) != 1 {
		t.Errorf("Certificates 长度应为 1，实际 %d", len(tc.Certificates))
	}
}

// TestBuildXferTLSConfig_NoCertFails 验证无任何证书配置时返回 error（fail-closed）。
func TestBuildXferTLSConfig_NoCertFails(t *testing.T) {
	cfg := Default()
	cfg.TLS.CertFile = ""
	cfg.TLS.KeyFile = ""
	cfg.TLS.AutoTLS = false

	if _, err := BuildXferTLSConfig(cfg); err == nil {
		t.Fatal("无证书配置时 BuildXferTLSConfig 应返回 error（fail-closed）")
	}

	if _, err := BuildXferTLSConfig(nil); err == nil {
		t.Fatal("nil 配置时 BuildXferTLSConfig 应返回 error（fail-closed）")
	}
}

// TestHubXferKey_FromFirstAccessKey 验证从 access_keys 首对派生隧道密钥，
// 且与直接 DeriveTunnelKey(secret, AccessKeyMesh(ak)) 等价（AD-3 一致性）。
func TestHubXferKey_FromFirstAccessKey(t *testing.T) {
	sk := strings.Repeat("a", 64) // 合法 64 hex（32B）
	ak := "sk-mesh1-" + strings.Repeat("b", 16)
	mesh := tunnel.AccessKeyMesh(ak)
	if mesh != "mesh1" {
		t.Fatalf("AccessKeyMesh(%q) 应为 mesh1，实际 %q", ak, mesh)
	}

	cfg := Default()
	cfg.AccessKeys = []AccessKeyConfig{
		{Key: ak, Secret: sk, MeshID: "mesh1"},
		{Key: "sk-other-" + strings.Repeat("c", 16), Secret: strings.Repeat("d", 64), MeshID: "other"},
	}

	key, err := HubXferKey(cfg)
	if err != nil {
		t.Fatalf("HubXferKey 应成功: %v", err)
	}
	if len(key) != 32 {
		t.Fatalf("派生密钥应为 32 字节，实际 %d", len(key))
	}
	want, err := tunnel.DeriveTunnelKey(sk, mesh)
	if err != nil {
		t.Fatalf("DeriveTunnelKey 参考值失败: %v", err)
	}
	if !bytes.Equal(key, want) {
		t.Errorf("HubXferKey 与直接 DeriveTunnelKey 不一致:\n got %x\nwant %x", key, want)
	}
}

// TestHubXferKey_NoAccessKeysFails 验证无 access_keys 时返回 error（fail-closed，
// 与规格 DoD 4 一致：无 access_keys → xfer listener 拒启）。
func TestHubXferKey_NoAccessKeysFails(t *testing.T) {
	cfg := Default() // 无 AccessKeys

	if _, err := HubXferKey(cfg); err == nil {
		t.Fatal("无 access_keys 时 HubXferKey 应返回 error（fail-closed）")
	}
	if _, err := HubXferKey(nil); err == nil {
		t.Fatal("nil 配置时 HubXferKey 应返回 error（fail-closed）")
	}
}

// TestXferIdentityPath 验证服务端身份文件路径默认 <uploads-dir>/sproxy/server-identity.json。
func TestXferIdentityPath(t *testing.T) {
	uploadsDir := filepath.Join("tmp", "data", "uploads")
	cfg := Default()
	cfg.UploadsDir = uploadsDir

	want := filepath.Join(uploadsDir, "sproxy", "server-identity.json")
	if got := XferIdentityPath(cfg); got != want {
		t.Errorf("XferIdentityPath = %q, want %q", got, want)
	}

	// uploadsDir 为空时仍返回相对路径（不 panic）。
	if got := XferIdentityPath(&Config{}); !strings.Contains(got, "server-identity.json") {
		t.Errorf("空 uploadsDir 时应返回含文件名的相对路径，实际 %q", got)
	}
}

// TestLoadXferIdentity 验证 LoadOrCreateIdentity 语义：首次生成、再次加载指纹一致。
func TestLoadXferIdentity(t *testing.T) {
	cfg := Default()
	cfg.UploadsDir = t.TempDir()

	id1, err := LoadXferIdentity(cfg)
	if err != nil {
		t.Fatalf("首次 LoadXferIdentity 应成功生成: %v", err)
	}
	if id1 == nil {
		t.Fatal("LoadXferIdentity 返回 nil 身份")
	}

	// 指纹格式 "sha256:<64 hex>"。
	fp1 := id1.Fingerprint()
	rest, ok := strings.CutPrefix(fp1, "sha256:")
	if !ok {
		t.Fatalf("指纹应以 sha256: 前缀，实际 %q", fp1)
	}
	if len(rest) != 64 {
		t.Fatalf("指纹 hex 部分应为 64 字符，实际 %d", len(rest))
	}
	if _, decErr := hex.DecodeString(rest); decErr != nil {
		t.Fatalf("指纹含非十六进制字符: %v", decErr)
	}

	// 再次加载 → 同一身份（指纹一致）。
	id2, err := LoadXferIdentity(cfg)
	if err != nil {
		t.Fatalf("再次 LoadXferIdentity 应加载既有身份: %v", err)
	}
	if id2.Fingerprint() != fp1 {
		t.Errorf("再次加载指纹不一致：%q != %q", id2.Fingerprint(), fp1)
	}
}

// TestLoadXferIdentity_CorruptFails 验证身份文件损坏时 fail-closed 返回错误，
// 不静默重建覆盖用户文件（LoadOrCreateIdentity 语义）。
func TestLoadXferIdentity_CorruptFails(t *testing.T) {
	cfg := Default()
	cfg.UploadsDir = t.TempDir()

	path := XferIdentityPath(cfg)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte("not a json identity"), 0600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, err := LoadXferIdentity(cfg); err == nil {
		t.Fatal("身份文件损坏时 LoadXferIdentity 应返回 error（fail-closed）")
	}
}
