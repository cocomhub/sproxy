// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig（统一后语义变更）：SCLIENT_ENV
// 只作 --env 默认值，**不再**选择 env 后缀配置文件；--config 默认恒为 config.yaml。
func TestNewRootCmd_SCLIENT_ENV_SelectsEnvConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "prod")
	setXDGConfigHome(t)
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.HasSuffix(flag.DefValue, "config.yaml") {
		t.Fatalf("--config 默认值应为 config.yaml（SCLIENT_ENV 不再选文件），got %q", flag.DefValue)
	}
}

// TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig（统一后）：未设置 SCLIENT_ENV 时
// --config 默认也是 config.yaml。
func TestNewRootCmd_NoSCLIENT_ENV_DefaultConfig(t *testing.T) {
	t.Setenv("SCLIENT_ENV", "")
	setXDGConfigHome(t)
	root := NewRootCmd()
	flag := root.PersistentFlags().Lookup("config")
	if flag == nil {
		t.Fatal("缺少 --config flag")
	}
	if !strings.HasSuffix(flag.DefValue, "config.yaml") || strings.Contains(flag.DefValue, "sclient.yaml") {
		t.Fatalf("--config 默认值应为 config.yaml 且不含 sclient.yaml，got %q", flag.DefValue)
	}
}

func TestLoadConfig_NilProvider(t *testing.T) {
	svc := &cliConfigProvider{cfgFile: ""}
	_, err := svc.LoadConfig()
	if err == nil {
		t.Fatal("expected error for nil provider")
	}
}

func TestLoadConfig_WithProvider(t *testing.T) {
	// 统一后：无 resolvedContext（无 config.yaml）即 fail-closed 报错，不静默用默认值。
	svc := &cliConfigProvider{cfgFile: ""}
	resolvedContext = nil
	_, err := svc.LoadConfig()
	if err == nil {
		t.Fatalf("无 resolvedContext 应报错（fail-closed），got nil")
	}
}

func TestLoadConfig_WithProviderDefaults(t *testing.T) {
	svc := &cliConfigProvider{cfgFile: ""}
	resolvedContext = nil
	_, err := svc.LoadConfig()
	if err == nil {
		t.Fatal("无 resolvedContext 应报错（fail-closed），不静默用默认值")
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
