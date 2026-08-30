// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFederationConfig_Defaults：联邦配置默认值（Interval 30s / Timeout 10s）。
func TestFederationConfig_Defaults(t *testing.T) {
	cfg := Default()
	cfg.SetDefaults()
	if cfg.Hub.Federation.Interval != 30*time.Second {
		t.Errorf("federation.interval 默认应 30s, got %s", cfg.Hub.Federation.Interval)
	}
	if cfg.Hub.Federation.Timeout != 10*time.Second {
		t.Errorf("federation.timeout 默认应 10s, got %s", cfg.Hub.Federation.Timeout)
	}
}

// TestFederationConfig_Validate_LoopbackPeerNoCreds：loopback peer 无凭据合法
// （默认 loopback 安全面——仅本地调试 peering 无需强制凭据）。
func TestFederationConfig_Validate_LoopbackPeerNoCreds(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{ID: "peer-local", URL: "http://127.0.0.1:18083"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback peer 无凭据应通过校验: %v", err)
	}
}

// TestFederationConfig_Validate_RemotePeerRequiresCreds：远程 peer 必须显式配置
// 凭据（fail-closed）——无凭据直连远程 hub 属暴露面，拒绝启动。
func TestFederationConfig_Validate_RemotePeerRequiresCreds(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{ID: "peer-remote", URL: "http://192.168.1.100:18083"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatalf("远程 peer 无凭据应拒绝")
	}
	if !strings.Contains(err.Error(), "远程 peering 必须同时配置 access_key 与 access_key_secret") {
		t.Fatalf("错误信息应说明远程 peering 需成对凭据, got: %v", err)
	}
}

// TestFederationConfig_Validate_RemotePeerMissingAK：远程 peer 只配 SK 缺 AK 应拒绝
// （凭据必须成对，缺任一即无有效签名）。
func TestFederationConfig_Validate_RemotePeerMissingAK(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:              "peer-remote",
			URL:             "http://192.168.1.100:18083",
			AccessKeySecret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "同时配置 access_key 与 access_key_secret") {
		t.Fatalf("远程 peer 缺 AK 应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_BadPeerSKHex：peer 配置的 access_key_secret 非 64 hex
// 应拒绝（与顶层 access_keys 校验一致）。
func TestFederationConfig_Validate_BadPeerSKHex(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:              "peer-local",
			URL:             "http://127.0.0.1:18083",
			AccessKey:       "sk-0123456789abcdef",
			AccessKeySecret: "not-hex-not-64",
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "64 个十六进制字符") {
		t.Fatalf("peer SK 非 64 hex 应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_RemotePeerWithCreds：远程 peer 配置凭据 + 受信任证书
// （无 insecure，默认严格校验系统根池）通过。
func TestFederationConfig_Validate_RemotePeerWithCreds(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:              "peer-remote",
			URL:             "https://hub.example.com:18083",
			AccessKey:       "sk-0123456789abcdef",
			AccessKeySecret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("远程 peer 配置凭据应通过: %v", err)
	}
}

// TestFederationConfig_Validate_BadScheme：非法 scheme 拒绝。
func TestFederationConfig_Validate_BadScheme(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{ID: "peer-bad", URL: "ftp://127.0.0.1:18083", AccessKeySecret: "x"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Fatalf("非法 scheme 应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_DupPeerID：重复 peer ID 拒绝。
func TestFederationConfig_Validate_DupPeerID(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{ID: "peer-x", URL: "http://127.0.0.1:18083"},
		{ID: "peer-x", URL: "http://127.0.0.1:19000"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("重复 peer ID 应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_DupEmptyURLPeer：两个空 URL 空 ID 的 peer 都回落
// 默认 loopback 且归一化后 ID 同为默认 URL（缓存 key 冲突，运行时后写覆盖），
// 启动时拦截（fail-fast）。
func TestFederationConfig_Validate_DupEmptyURLPeer(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{}, // 空 URL 空 ID → 回落默认 127.0.0.1:18083，ID 归一为默认 URL
		{}, // 同上 → key 冲突
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("两个空 URL 空 ID peer 应判重复拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_RemoteInsecureRejected：远程 peer 配置
// insecure_skip_verify 应拒绝（跳过 TLS 校验 = MITM 风险，fail-closed）。
func TestFederationConfig_Validate_RemoteInsecureRejected(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:                 "peer-remote",
			URL:                "https://192.168.1.100:18083",
			AccessKey:          "sk-0123456789abcdef",
			AccessKeySecret:    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			InsecureSkipVerify: true,
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "insecure_skip_verify 仅允许用于 loopback peer") {
		t.Fatalf("远程 peer + insecure_skip_verify 应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_LoopbackInsecureAllowed：loopback peer 配置
// insecure_skip_verify 允许（本机自签开发/测试）。
func TestFederationConfig_Validate_LoopbackInsecureAllowed(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:                 "peer-local",
			URL:                "https://127.0.0.1:18083",
			InsecureSkipVerify: true,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback peer + insecure_skip_verify 应通过: %v", err)
	}
}

// TestFederationConfig_Validate_CAFileAndInsecureMutuallyExclusive：ca_file 与
// insecure_skip_verify 互斥（ca_file 是严格校验，跳过校验与其冲突）。
func TestFederationConfig_Validate_CAFileAndInsecureMutuallyExclusive(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:                 "peer-local",
			URL:                "https://127.0.0.1:18083",
			CAFile:             filepath.Join(t.TempDir(), "ca.pem"),
			InsecureSkipVerify: true,
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "互斥") {
		t.Fatalf("ca_file 与 insecure_skip_verify 应互斥拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_CAFileNotExist：ca_file 不存在应拒绝（fail-fast）。
func TestFederationConfig_Validate_CAFileNotExist(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{
			ID:              "peer-local",
			URL:             "https://127.0.0.1:18083",
			CAFile:          filepath.Join(t.TempDir(), "does-not-exist.pem"),
			AccessKey:       "sk-0123456789abcdef",
			AccessKeySecret: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "不可读") {
		t.Fatalf("ca_file 不存在应拒绝, got: %v", err)
	}
}

// TestFederationConfig_Validate_EnabledNoPeers：联邦启用但无 peers 合法
// （本 hub 仅作为被 peer，不主动拉取）。
func TestFederationConfig_Validate_EnabledNoPeers(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("联邦启用但无 peers 应通过: %v", err)
	}
}

// TestFederationConfig_Validate_DisabledIgnoresPeers：联邦未启用时 peers 遗留配置
// 不阻断启动（门控在 enabled，与 ws/dht 校验一致）。
func TestFederationConfig_Validate_DisabledIgnoresPeers(t *testing.T) {
	cfg := Default()
	cfg.Hub.Enabled = false
	cfg.Hub.Federation.Enabled = false
	cfg.Hub.Federation.Peers = []FederationPeerConfig{
		{ID: "peer-remote", URL: "http://192.168.1.100:18083"}, // 无凭据但联邦关闭
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("联邦关闭时 peers 遗留配置不应阻断: %v", err)
	}
}
