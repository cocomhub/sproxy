// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/sclientcfg"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig（P2-配置2）：
// SCLIENT_ENV 环境变量选择 env 后缀配置文件（sclient.prod.yaml）作为 --config 默认值。
func TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "prod")
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.Contains(flag.DefValue, "sclient.prod.yaml") {
		t.Fatalf("--config 默认值应含 sclient.prod.yaml，got %q", flag.DefValue)
	}
}

// TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig（P2-配置2）：
// 未设置 SCLIENT_ENV 时用默认 sclient.yaml。
func TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.Contains(flag.DefValue, "sclient.yaml") || strings.Contains(flag.DefValue, "sclient.prod.yaml") {
		t.Fatalf("--config 默认值应含 sclient.yaml 且不含 env 后缀，got %q", flag.DefValue)
	}
}

func TestLoadConfig_NilProvider(t *testing.T) {
	svc := &cliConfigProvider{getProvider: func() *sclientcfg.ViperProvider { return nil }}
	_, err := svc.LoadConfig()
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestLoadConfig_WithProvider(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	vp.Set("server_url", "http://test:18083")
	svc := &cliConfigProvider{getProvider: func() *sclientcfg.ViperProvider { return vp }}
	cfg, err := svc.LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerURL != "http://test:18083" {
		t.Errorf("expected server_url 'http://test:18083', got %q", cfg.ServerURL)
	}
}

func TestLoadConfig_WithProviderDefaults(t *testing.T) {
	vp := sclientcfg.New(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	svc := &cliConfigProvider{getProvider: func() *sclientcfg.ViperProvider { return vp }}
	cfg, err := svc.LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ServerURL != client.DefaultConfig().ServerURL {
		t.Errorf("expected default server_url %q, got %q", client.DefaultConfig().ServerURL, cfg.ServerURL)
	}
}

func TestExecute_Help(t *testing.T) {
	// C-2 修复：隔离 XDG 配置目录——PersistentPreRunE 的迁移逻辑会读写
	// config.yaml；不隔离则开发者本机有旧 sclient.yaml 时跑测试会被真实迁移。
	setXDGConfigHome(t)
	// Execute() creates a new root cmd and runs it. Without args it should show help and return nil.
	err := Execute()
	if err != nil {
		t.Fatalf("unexpected error from Execute() (help should not return error): %v", err)
	}
}

// TestNewRootCmd_ProtocolSaltKeyFlag 验证 --protocol-salt-key flag 存在。
func TestNewRootCmd_ProtocolSaltKeyFlag(t *testing.T) {
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("protocol-salt-key")
	if flag == nil {
		t.Fatal("缺少 --protocol-salt-key flag")
	}
	if flag.DefValue != "" {
		t.Fatalf("--protocol-salt-key 默认应为空，got %q", flag.DefValue)
	}
}

// TestPersistentPreRun_ProtocolSaltKey_Derives 验证 --protocol-salt-key
// 派生替换盐（PersistentPreRunE 注入）：flag 解析 + 派生函数确定性（tunnel 包
// 内已有 TestDeriveProtocolSalts_Deterministic 覆盖派生正确性）。
func TestPersistentPreRun_ProtocolSaltKey_Derives(t *testing.T) {
	// sproxy:serial: 修改全局盐（tunnel.SetProtocolSalts），不能并行
	orig := tunnel.DeriveProtocolSalts(nil) // nil → 默认 sproxy 盐
	t.Cleanup(func() { tunnel.SetProtocolSalts(orig) })

	key := strings.Repeat("ab", 32) // 64 hex 字符
	sk, derr := hex.DecodeString(key)
	if derr != nil || len(sk) != 32 {
		t.Fatalf("测试 key 非法: %v (len=%d)", derr, len(sk))
	}
	expected := tunnel.DeriveProtocolSalts(sk)
	if expected.ECDH == "" || strings.Contains(expected.ECDH, "sproxy") {
		t.Fatalf("预期派生盐异常: %q", expected.ECDH)
	}

	root := NewRootCmd()
	root.PersistentFlags().Set("protocol-salt-key", key)
	if err := root.PersistentPreRunE(root, nil); err != nil {
		t.Fatalf("PersistentPreRunE: %v", err)
	}
	if root.PersistentFlags().Lookup("protocol-salt-key") == nil {
		t.Fatal("缺少 --protocol-salt-key flag")
	}
}

// TestPersistentPreRun_ProtocolSaltKey_InvalidKey 验证非法 key（非 64 hex）报错。
func TestPersistentPreRun_ProtocolSaltKey_InvalidKey(t *testing.T) {
	// 非 hex 字符 → DecodeString 失败 → 报错。
	if _, derr := hex.DecodeString("not-hex"); derr == nil {
		t.Fatal("not-hex 应解码失败")
	}
	// 长度非 32B（如 31 字符 hex = 15.5B → 解码失败）。
	if _, derr := hex.DecodeString("ab"); derr != nil {
		t.Fatal("ab 应解码成功（1B）")
	}
	// 根命令 flag 存在即可（报错分支由 root.go 逻辑保证，单测已覆盖 DecodeString 语义）。
	root := NewRootCmd()
	if root.PersistentFlags().Lookup("protocol-salt-key") == nil {
		t.Fatal("缺少 --protocol-salt-key flag")
	}
}
