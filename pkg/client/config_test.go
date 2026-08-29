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
	"gopkg.in/yaml.v3"
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

	// invalid tunnel_key length → error
	cfg5 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, TunnelKey: "too-short"}
	if err := cfg5.Validate(); err == nil {
		t.Fatal("expected error for invalid tunnel_key length, got nil")
	}

	// valid tunnel_key length → no error
	cfg6 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, TunnelKey: strings.Repeat("a", 64)}
	if err := cfg6.Validate(); err != nil {
		t.Fatalf("Validate() on config with 64-char tunnel_key: %v", err)
	}

	// auth_token 任意字符串都合法
	cfg7 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, AuthToken: "my-token"}
	if err := cfg7.Validate(); err != nil {
		t.Fatalf("Validate() on config with AuthToken: %v", err)
	}

	// auth_token 空字符串也合法
	cfg8 := &Config{ServerURL: "http://x", Timeout: 30, ChunkSize: size.DefaultChunkSize, AuthToken: ""}
	if err := cfg8.Validate(); err != nil {
		t.Fatalf("Validate() on config with empty AuthToken: %v", err)
	}
}

func TestLoadFromProvider(t *testing.T) {
	t.Parallel()

	p := mapProvider{m: map[string]any{"server_url": "http://test:8080", "timeout": 60, "auth_token": "secret"}}

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
	if cfg.AuthToken != "secret" {
		t.Errorf("AuthToken = %q, want %q", cfg.AuthToken, "secret")
	}
}

func TestLoadFromProvider_InvalidTunnelKey(t *testing.T) {
	t.Parallel()

	p := mapProvider{m: map[string]any{"server_url": "http://test:8080", "tunnel_key": "bad-key"}}

	_, err := LoadFromProvider(p)
	if err == nil {
		t.Fatal("expected error for invalid tunnel_key, got nil")
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
	cfg.TunnelKey = strings.Repeat("d", 64)
	cfg.AuthToken = "my-secret-token"
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
	if !strings.Contains(out, "dddd****") {
		t.Errorf("expected masked TunnelKey in output, got: %s", out)
	}
	if !strings.Contains(out, "my-s****") {
		t.Errorf("expected masked AuthToken in output, got: %s", out)
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
	cfg.TunnelKey = "short"

	var buf bytes.Buffer
	HandleConfigShow(cfg, &buf)
	out := buf.String()

	if !strings.Contains(out, "shor****") {
		t.Errorf("expected masked short key (shor****) in output, got: %s", out)
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
// config set 支持 hub_url/relay_token/node_id 三个通用 mesh 参数；hub_url 校验
// URL 格式，node_id 拒绝空白字符。
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
	if err := ApplyConfigSet(cfg, "relay_token", "tok"); err != nil {
		t.Fatalf("relay_token set: %v", err)
	}
	if cfg.RelayToken != "tok" {
		t.Fatalf("RelayToken = %q, want tok", cfg.RelayToken)
	}
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

// TestLoadConfig_MeshParams（P2-配置1）：YAML 中 hub_url/relay_token/node_id 正确解码。
func TestLoadConfig_MeshParams(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sclient.yaml")
	content := "server_url: https://127.0.0.1:18083\nhub_url: wss://hub.example.com/ws\nrelay_token: rt\nnode_id: node-a\n"
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
	if cfg.RelayToken != "rt" {
		t.Fatalf("RelayToken = %q", cfg.RelayToken)
	}
	if cfg.NodeID != "node-a" {
		t.Fatalf("NodeID = %q", cfg.NodeID)
	}
}
