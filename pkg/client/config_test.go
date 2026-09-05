// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/internal/size"
	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/tunnel"
	"gopkg.in/yaml.v3"
)

// 测试共享常量：access-key 驱动的隧道密钥固定 AK/SK。
const (
	testTunnelAK = "sk-test-1234567890abcdef1234567890abcdef"
	testTunnelSK = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

// mapProvider 将 map[string]any 转换为 provider.Provider 用于测试。
type mapProvider struct {
	m map[string]any
}

func (p mapProvider) Unmarshal(obj any) error {
	// 使用 yaml 作为中介：map → yaml bytes → struct
	// Config 结构体使用 yaml tag，所以 yaml.Unmarshal 能正确匹配字段
	data, err := yaml.Marshal(p.m)
	if err != nil {
		return err
	}
	return yaml.Unmarshal(data, obj)
}

// compile-time interface check
var _ provider.Provider = mapProvider{}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	// valid config — all fields set, no tunnel_key → no error
	cfg := &Config{ServerURL: "http://127.0.0.1:8080", Timeout: 30, ChunkSize: size.DefaultChunkSize}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() on valid config: %v", err)
	}

	// empty ServerURL + SetDefaults → defaults to localhost
	cfg2 := &Config{Timeout: 30}
	cfg2.SetDefaults()
	if err := cfg2.Validate(); err != nil {
		t.Fatalf("Validate() on config with empty ServerURL: %v", err)
	}
	if cfg2.ServerURL != "https://127.0.0.1:18083" {
		t.Errorf("expected ServerURL to default, got %q", cfg2.ServerURL)
	}

	// zero Timeout + SetDefaults → defaults to 300
	cfg3 := &Config{ServerURL: "http://x", Timeout: 0}
	cfg3.SetDefaults()
	if err := cfg3.Validate(); err != nil {
		t.Fatalf("Validate() on config with zero Timeout: %v", err)
	}
	if cfg3.Timeout != 300 {
		t.Errorf("expected Timeout to default to 300, got %d", cfg3.Timeout)
	}

	// zero ChunkSize + SetDefaults → defaults to DefaultChunkSize
	cfg4 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: 0}
	cfg4.SetDefaults()
	if err := cfg4.Validate(); err != nil {
		t.Fatalf("Validate() on config with zero ChunkSize: %v", err)
	}
	if cfg4.ChunkSize != size.DefaultChunkSize {
		t.Errorf("expected ChunkSize to default to %d, got %d", size.DefaultChunkSize, cfg4.ChunkSize)
	}

	// auth_token 任意字符串都合法
	cfg7 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, AccessKey: "ak", AccessKeySecret: "my-token"}
	if err := cfg7.Validate(); err != nil {
		t.Fatalf("Validate() on config with AccessKeySecret: %v", err)
	}

	// auth_token 空字符串也合法
	cfg8 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, AccessKeySecret: ""}
	if err := cfg8.Validate(); err != nil {
		t.Fatalf("Validate() on config with empty AccessKeySecret: %v", err)
	}
}

// TestConfigValidate_PeerFingerprintsInvalid 验证 peer_fingerprints 在配置加载
// Validate 阶段响亮报错（m-4）：手改 YAML 笔误不被静默跳过（否则握手时全部对端被拒且报错费解）。
func TestConfigValidate_PeerFingerprintsInvalid(t *testing.T) {
	t.Parallel()

	id, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	validFP := id.Fingerprint()

	// 合法指纹：无错误。
	cfg := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, PeerFingerprints: []string{validFP}}
	if err = cfg.Validate(); err != nil {
		t.Fatalf("Validate() on valid fingerprint: %v", err)
	}

	// 非法指纹：须报错，并指出是 peer_fingerprints 配置问题。
	bad := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, PeerFingerprints: []string{"not-a-fingerprint"}}
	err = bad.Validate()
	if err == nil {
		t.Fatal("Validate() should reject invalid peer_fingerprints")
	}
	if !strings.Contains(err.Error(), "peer_fingerprints") {
		t.Fatalf("错误应指明 peer_fingerprints 配置项, 实际: %v", err)
	}
}

func TestLoadFromProvider(t *testing.T) {
	t.Parallel()

	p := mapProvider{m: map[string]any{"server_url": "http://test:8080", "timeout": 60, "access_key": "ak", "access_key_secret": "secret"}}

	cfg, err := LoadFromProvider(p)
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if cfg.ServerURL != "http://test:8080" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "http://test:8080")
	}
	if cfg.Timeout != 60 {
		t.Errorf("Timeout = %d, want %d", cfg.Timeout, 60)
	}
	if cfg.ChunkSize != size.DefaultChunkSize {
		t.Errorf("ChunkSize = %d, want %d", cfg.ChunkSize, size.DefaultChunkSize)
	}
	if cfg.AccessKeySecret != "secret" {
		t.Errorf("AccessKeySecret = %q, want %q", cfg.AccessKeySecret, "secret")
	}
}

func TestLoadConfig_EmptyPath(t *testing.T) {
	t.Parallel()

	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\"): %v", err)
	}
	if cfg.ServerURL != "https://127.0.0.1:18083" {
		t.Errorf("expected default ServerURL, got %q", cfg.ServerURL)
	}
}

func TestLoadConfig_NonexistentPath(t *testing.T) {
	dir := t.TempDir()
	// 父目录存在但文件本身不存在，LoadConfig 应返回默认配置，不创建文件
	path := filepath.Join(dir, "sclient.yaml")

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on nonexistent path should not error, got: %v", err)
	}
	if cfg.ServerURL != "https://127.0.0.1:18083" {
		t.Errorf("expected default ServerURL, got %q", cfg.ServerURL)
	}
	// config file should NOT have been created
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("expected LoadConfig to NOT create config file at %s", path)
	}
}

func TestLoadConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")
	content := "server_url: https://example.com\ntimeout: 99\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.ServerURL != "https://example.com" {
		t.Errorf("ServerURL = %q, want %q", cfg.ServerURL, "https://example.com")
	}
	if cfg.Timeout != 99 {
		t.Errorf("Timeout = %d, want %d", cfg.Timeout, 99)
	}
}

func TestHandleConfigShow(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ServerURL = "https://example.com"
	cfg.Timeout = 120
	cfg.AccessKey = "ak"
	cfg.AccessKeySecret = "my-secret-token"
	cfg.ChunkSize = 8 << 20
	cfg.MaxChunkSize = 32 << 20
	cfg.AllowTransportFallback = true

	var buf bytes.Buffer
	HandleConfigShow(cfg, &buf)
	out := buf.String()

	if !strings.Contains(out, "ServerURL:     https://example.com") {
		t.Errorf("expected ServerURL in output, got: %s", out)
	}
	if !strings.Contains(out, "Timeout:       120") {
		t.Errorf("expected Timeout in output, got: %s", out)
	}
	if !strings.Contains(out, "AccessKeySecret: ****") {
		t.Errorf("expected fully-masked AccessKeySecret in output, got: %s", out)
	}
	// M2：show 输出不得泄露任何 secret hex 前缀字符。
	if strings.Contains(out, "my-s") {
		t.Errorf("secret 前缀泄漏到 show 输出: %s", out)
	}
	if strings.Contains(out, "my-secret-token") {
		t.Errorf("secret 明文泄漏到 show 输出: %s", out)
	}
	if !strings.Contains(out, "ChunkSize:     8388608") {
		t.Errorf("expected ChunkSize in output, got: %s", out)
	}
	if !strings.Contains(out, "MaxChunkSize:  33554432") {
		t.Errorf("expected MaxChunkSize in output, got: %s", out)
	}
	if !strings.Contains(out, "AllowTransportFallback: true") {
		t.Errorf("expected AllowTransportFallback in output, got: %s", out)
	}
}

func TestSaveConfig_Error(t *testing.T) {
	// 写入只读目录应触发错误
	cfg := DefaultConfig()
	err := SaveConfig(cfg, "/nonexistent/path/sclient.yaml")
	if err == nil {
		t.Fatal("expected error saving to nonexistent path, got nil")
	}
}

func TestLoadConfig_ReadError(t *testing.T) {
	// 指向目录而非文件应触发读取错误
	dir := t.TempDir()
	_, err := LoadConfig(dir)
	if err == nil {
		t.Fatal("expected error reading directory as config file, got nil")
	}
}

func TestLoadConfig_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte(": invalid yaml :: {{"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestLoadConfig_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.yaml")
	if err := os.WriteFile(path, nil, 0644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig on empty file: %v", err)
	}
	if cfg.ServerURL != "https://127.0.0.1:18083" {
		t.Errorf("expected defaults for empty file, got %q", cfg.ServerURL)
	}
}

func TestHandleConfigShow_MaskedShortKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AccessKeySecret = "short-sk"

	var buf bytes.Buffer
	HandleConfigShow(cfg, &buf)
	out := buf.String()

	if !strings.Contains(out, "AccessKeySecret: ****") {
		t.Errorf("expected fully-masked AccessKeySecret, got: %s", out)
	}
	// M2：短 secret 同样全掩，不泄露任何字符。
	if strings.Contains(out, "short") {
		t.Errorf("短 secret 前缀泄漏到 show 输出: %s", out)
	}
}

func TestHandleConfigShow_EmptySecret(t *testing.T) {
	cfg := DefaultConfig()
	cfg.AccessKeySecret = ""

	var buf bytes.Buffer
	HandleConfigShow(cfg, &buf)
	out := buf.String()

	// 未配置 secret 时输出空值（不打印 *）。既已缺省会显示 0 长度值。
	if !strings.Contains(out, "AccessKeySecret: ") {
		t.Errorf("expected AccessKeySecret line with empty value, got: %s", out)
	}
	if strings.Contains(out, "AccessKeySecret: ****") {
		t.Errorf("未配置 secret 不应打印掩码 ****, got: %s", out)
	}
}

func TestHandleConfigShow_NilReceiver(t *testing.T) {
	var buf bytes.Buffer
	HandleConfigShow(nil, &buf)
	if buf.Len() != 0 {
		t.Errorf("expected no output for nil config, got: %s", buf.String())
	}
}

// TestApplyConfigSet_MeshParams（P2-配置1）：
// config set 支持 hub_url/node_id 两个通用 mesh 参数；hub_url 校验 URL 格式，
// node_id 拒绝空白字符。已废除的旧配置键返回未知键错误（fail-closed）。
func TestApplyConfigSet_MeshParams(t *testing.T) {
	cfg := DefaultConfig()

	if err := ApplyConfigSet(cfg, "hub_url", "wss://hub.example.com/ws"); err != nil {
		t.Fatalf("hub_url set: %v", err)
	}
	if cfg.HubURL != "wss://hub.example.com/ws" {
		t.Fatalf("HubURL = %q, want wss://hub.example.com/ws", cfg.HubURL)
	}
	// 非法 hub_url 拒绝
	if err := ApplyConfigSet(cfg, "hub_url", "not a url"); err == nil {
		t.Fatal("非法 hub_url 应报错")
	}
	// 已废除的旧配置键应返回未知键错误（ApplyConfigSet 默认分支覆盖）。
	if err := ApplyConfigSet(cfg, "node_id", "node-a"); err != nil {
		t.Fatalf("node_id set: %v", err)
	}
	if cfg.NodeID != "node-a" {
		t.Fatalf("NodeID = %q, want node-a", cfg.NodeID)
	}
	if err := ApplyConfigSet(cfg, "node_id", "bad node"); err == nil {
		t.Fatal("含空白的 node_id 应报错")
	}
}

// TestApplyConfigSet_XferTLSParams（阶段5 PR-4）：config set 支持 xfer_ca_file /
// xfer_insecure 两个 xfer tcp+tls 传输配置键；xfer_ca_file 校验非空，
// xfer_insecure 校验布尔。
func TestApplyConfigSet_XferTLSParams(t *testing.T) {
	cfg := DefaultConfig()

	if err := ApplyConfigSet(cfg, "xfer_ca_file", "/path/ca.pem"); err != nil {
		t.Fatalf("xfer_ca_file set: %v", err)
	}
	if cfg.XferCAFile != "/path/ca.pem" {
		t.Fatalf("XferCAFile = %q, want /path/ca.pem", cfg.XferCAFile)
	}
	if err := ApplyConfigSet(cfg, "xfer_ca_file", ""); err != nil {
		t.Fatalf("xfer_ca_file 清空应成功: %v", err)
	}
	if cfg.XferCAFile != "" {
		t.Fatalf("XferCAFile 清空后应为空, 实际 %q", cfg.XferCAFile)
	}

	if err := ApplyConfigSet(cfg, "xfer_insecure", "true"); err != nil {
		t.Fatalf("xfer_insecure set: %v", err)
	}
	if !cfg.XferInsecure {
		t.Fatal("XferInsecure = false, want true")
	}
	if err := ApplyConfigSet(cfg, "xfer_insecure", "false"); err != nil {
		t.Fatalf("xfer_insecure 复位: %v", err)
	}
	if cfg.XferInsecure {
		t.Fatal("XferInsecure = true, want false")
	}
	if err := ApplyConfigSet(cfg, "xfer_insecure", "not-bool"); err == nil {
		t.Fatal("非法 xfer_insecure 应报错")
	}
}

// TestLoadConfig_XferTLSParams（阶段5 PR-4）：YAML 中 xfer_ca_file / xfer_insecure
// 正确解码。
func TestLoadConfig_XferTLSParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")
	content := "server_url: https://127.0.0.1:18083\nxfer_ca_file: /etc/sproxy/xfer-ca.pem\nxfer_insecure: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.XferCAFile != "/etc/sproxy/xfer-ca.pem" {
		t.Fatalf("XferCAFile = %q, want /etc/sproxy/xfer-ca.pem", cfg.XferCAFile)
	}
	if !cfg.XferInsecure {
		t.Fatal("XferInsecure = false, want true")
	}
}

// TestLoadConfig_MeshParams（P2-配置1）：YAML 中 hub_url/node_id 正确解码
// （已废除的旧配置键不再识别）。
func TestLoadConfig_MeshParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")
	content := "server_url: https://127.0.0.1:18083\nhub_url: wss://hub.example.com/ws\nnode_id: node-a\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HubURL != "wss://hub.example.com/ws" {
		t.Fatalf("HubURL = %q", cfg.HubURL)
	}
	if cfg.NodeID != "node-a" {
		t.Fatalf("NodeID = %q", cfg.NodeID)
	}
}
