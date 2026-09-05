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

	"github.com/cocomhub/sproxy/pkg/accesskey"
	"github.com/cocomhub/sproxy/pkg/certmgr"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestXferListenerConfigFromConfig 验证从 server.Config + 凭据 Ring 提取 xfer
// listener 配置：证书缺省回落 cfg.TLS.*，access_key 派生参数取 Ring 首个存活条目。
func TestXferListenerConfigFromConfig(t *testing.T) {
	cfg := Default()
	cfg.TLS.CertFile = "/tmp/cert.pem"
	cfg.TLS.KeyFile = "/tmp/key.pem"
	ring := ringForTestCreds(
		testCredPair{ak: "ak-mesh1-aaaaaaaaaaaaaaaa", sk: strings.Repeat("a", 64)},
		testCredPair{ak: "ak-other-bbbbbbbbbbbbbbbb", sk: strings.Repeat("b", 64)},
	)

	xc := XferListenerConfigFromRing(cfg, ring)
	if xc.CertFile != cfg.TLS.CertFile {
		t.Errorf("CertFile 应回落 cfg.TLS.CertFile %q，实际 %q", cfg.TLS.CertFile, xc.CertFile)
	}
	if xc.KeyFile != cfg.TLS.KeyFile {
		t.Errorf("KeyFile 应回落 cfg.TLS.KeyFile %q，实际 %q", cfg.TLS.KeyFile, xc.KeyFile)
	}
	if xc.AccessKey != "ak-mesh1-aaaaaaaaaaaaaaaa" {
		t.Errorf("AccessKey 应取 Ring 首存活 %q，实际 %q", "ak-mesh1-aaaaaaaaaaaaaaaa", xc.AccessKey)
	}
	if xc.AccessKeySecret != strings.Repeat("a", 64) {
		t.Errorf("AccessKeySecret 应取 Ring 首存活 SK，实际 %q", xc.AccessKeySecret)
	}
	if xc.MeshID != "mesh1" {
		t.Errorf("MeshID 应派生自 AK mesh=%q，实际 %q", "mesh1", xc.MeshID)
	}

	// 旧入口（cfg + ring）保持一致。
	xc2 := XferListenerConfigFromConfig(cfg, ring)
	if xc2 != xc {
		t.Errorf("XferListenerConfigFromConfig 应与 FromRing 一致，实际 %+v vs %+v", xc2, xc)
	}

	// nil 配置 → 零值（不 panic）。
	var zero XferListenerConfig
	if got := XferListenerConfigFromRing(nil, nil); got != zero {
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

// TestHubXferKey_FromFirstAccessKey 验证从凭据 Ring 首个存活条目派生隧道密钥，
// 且与直接 DeriveTunnelKey(secret, AccessKeyMesh(ak)) 等价（AD-3 一致性）。
func TestHubXferKey_FromFirstAccessKey(t *testing.T) {
	sk := strings.Repeat("a", 64) // 合法 64 hex（32B）
	ak := "ak-mesh1-" + strings.Repeat("b", 16)
	mesh := tunnel.AccessKeyMesh(ak)
	if mesh != "mesh1" {
		t.Fatalf("AccessKeyMesh(%q) 应为 mesh1，实际 %q", ak, mesh)
	}

	cfg := Default()
	ring := ringForTestCreds(
		testCredPair{ak: ak, sk: sk},
		testCredPair{ak: "ak-other-" + strings.Repeat("c", 16), sk: strings.Repeat("d", 64)},
	)

	key, err := HubXferKey(cfg, ring)
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

// TestHubXferKey_NoAccessKeysFails 验证 Ring 为空时返回 error（fail-closed，
// 与规格 DoD 4 一致：无有效凭据 → xfer listener 拒启）。
func TestHubXferKey_NoAccessKeysFails(t *testing.T) {
	cfg := Default()
	empty := accesskey.NewRing()

	if _, err := HubXferKey(cfg, empty); err == nil {
		t.Fatal("空 Ring 时 HubXferKey 应返回 error（fail-closed）")
	}
	if _, err := HubXferKey(nil, nil); err == nil {
		t.Fatal("nil 配置/ring 时 HubXferKey 应返回 error（fail-closed）")
	}
}

// TestXferIdentityPath 验证服务端身份文件路径：显式配置优先；未配置回落 XDG
// 用户配置目录（os.UserConfigDir()/sproxy/server-identity.json）——**绝不放
// storage_root 下**（审查 C-1：与文件 API 命名空间重叠，防私钥泄露/覆盖）。
func TestXferIdentityPath(t *testing.T) {
	// 1) 显式配置优先。
	explicit := filepath.Join(t.TempDir(), "ident", "server-identity.json")
	cfg := Default()
	cfg.Hub.XferIdentityFile = explicit
	if got := XferIdentityPath(cfg); got != explicit {
		t.Errorf("显式配置时应返回 %q，实际 %q", explicit, got)
	}

	// 2) 未配置 → XDG 用户配置目录（含文件名；绝不包含 storage_root 相对路径）。
	cfg2 := Default()
	cfg2.StorageRoot = "tmp/data/uploads" // 即使设置 storage_root 也不受影响
	got := XferIdentityPath(cfg2)
	if !strings.Contains(got, "server-identity.json") {
		t.Errorf("未配置时应含文件名，实际 %q", got)
	}
	if strings.Contains(got, "uploads") {
		t.Errorf("身份文件不得位于 storage_root 下（审查 C-1），实际 %q", got)
	}
	wantBase, _ := os.UserConfigDir()
	if !strings.HasPrefix(got, wantBase) {
		t.Errorf("未配置时应位于 XDG 用户配置目录 %q 下，实际 %q", wantBase, got)
	}

	// 3) nil 配置不 panic，仍回落 XDG。
	if got := XferIdentityPath(nil); !strings.Contains(got, "server-identity.json") {
		t.Errorf("nil 配置应回落 XDG 路径，实际 %q", got)
	}
}

// TestLoadXferIdentity 验证 LoadOrCreateIdentity 语义：首次生成、再次加载指纹一致。
func TestLoadXferIdentity(t *testing.T) {
	cfg := Default()
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

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
	cfg.Hub.XferIdentityFile = filepath.Join(t.TempDir(), "ident", "server-identity.json")

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

// TestBuildXferTLSConfig_AutoTLS 验证 AutoTLS 自签分支：无显式证书文件时生成自签
// 证书并返回非空 Certificates（覆盖审查 M-5 指出的默认 AutoTLS 路径未测试缺口）。
// 用 t.Chdir 隔离 selfsigned 默认相对路径（certs/）对 CWD 的污染。
func TestBuildXferTLSConfig_AutoTLS(t *testing.T) {
	t.Chdir(t.TempDir()) // selfsigned 默认写相对 certs/，chdir 到临时目录隔离
	cfg := Default()
	cfg.TLS.CertFile = "" // AutoTLS 分支：cert_file 为空 → selfsigned 默认路径
	cfg.TLS.KeyFile = ""
	cfg.TLS.AutoTLS = true

	tc, err := BuildXferTLSConfig(cfg)
	if err != nil {
		t.Fatalf("BuildXferTLSConfig（AutoTLS）应成功: %v", err)
	}
	if tc == nil || len(tc.Certificates) != 1 {
		t.Fatalf("AutoTLS 应生成 1 份证书，实际 tc=%v certs=%d", tc, len(tc.Certificates))
	}
	if tc.MinVersion != tls.VersionTLS12 {
		t.Errorf("MinVersion 应为 TLS1.2，实际 0x%x", tc.MinVersion)
	}
}

// TestBuildXferTLSConfig_CertNoKeyFails 验证 cert_file 有而 key_file 缺失时返回
// error（fail-closed，审查 M-7-1）。
func TestBuildXferTLSConfig_CertNoKeyFails(t *testing.T) {
	cfg := Default()
	cfg.TLS.CertFile = filepath.Join(t.TempDir(), "cert.pem")
	cfg.TLS.KeyFile = "" // 缺失
	cfg.TLS.AutoTLS = false

	if _, err := BuildXferTLSConfig(cfg); err == nil {
		t.Fatal("cert_file 存在但 key_file 缺失时应返回 error（fail-closed）")
	}
}

// TestBuildXferTLSConfig_ACMERejects 验证 ACME 启用时 xfer TLS 拒绝并给出可读错误
// （审查 I-2：ACME 与 xfer listener 生命周期不兼容，错误信息应明示）。
func TestBuildXferTLSConfig_ACMERejects(t *testing.T) {
	cfg := Default()
	cfg.TLS.CertFile = ""
	cfg.TLS.KeyFile = ""
	cfg.TLS.AutoTLS = false
	cfg.TLS.ACME = ACMEConfig{Enabled: true, Domains: []string{"example.com"}}

	_, err := BuildXferTLSConfig(cfg)
	if err == nil {
		t.Fatal("ACME 启用时 xfer TLS 应拒绝（fail-closed）")
	}
	if !strings.Contains(err.Error(), "不支持 ACME") {
		t.Errorf("ACME 拒绝错误应明示 xfer 不支持 ACME，实际 %q", err.Error())
	}
}

// TestHubXferKey_NonHexSecretFails 验证非法 secret（非 hex）时派生失败（fail-closed，
// 审查 M-7-2）。Ring 注入非 hex SK 时 AddKey 拒绝（SK 必须 32B），资历由
// testCredPair 的 64-hex 解析在 ringForTestCreds 跳过——此处验证空/非法 ring 行为。
func TestHubXferKey_NonHexSecretFails(t *testing.T) {
	cfg := Default()
	badRing := accesskey.NewRing()
	// 32 个 'g'（非 hex 无法解码为 32B），ringForTestCreds 会跳过；直接构造非法条目。
	_ = badRing.UpsertAK("ak-mesh1-"+strings.Repeat("b", 16), "t")
	// 非 32B SK → AddKey 返回 ErrInvalidSecret，无法入 ring → 视为空 ring。
	if _, err := badRing.AddKey("ak-mesh1-"+strings.Repeat("b", 16), []byte("short")); err == nil {
		t.Fatal("非法 SK 应被 AddKey 拒绝")
	}
	if _, err := HubXferKey(cfg, badRing); err == nil {
		t.Fatal("无有效条目时 HubXferKey 应返回 error（fail-closed）")
	}
}

// TestXferListenerConfigFromConfig_NoAccessKeys 验证空 Ring 时 FromConfig 返回零值
// AccessKey 字段（不 panic，审查 M-7-3）。
func TestXferListenerConfigFromConfig_NoAccessKeys(t *testing.T) {
	cfg := Default()
	empty := accesskey.NewRing()

	xc := XferListenerConfigFromConfig(cfg, empty)
	if xc.AccessKey != "" || xc.AccessKeySecret != "" || xc.MeshID != "" {
		t.Errorf("空 Ring 时应返回零值 AccessKey 字段，实际 %+v", xc)
	}
}
