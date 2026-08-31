// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
	"time"
)

func TestSyncConfig_Defaults(t *testing.T) {
	cfg := Default()
	if cfg.Sync.MaxConcurrent != 3 {
		t.Fatalf("Sync.MaxConcurrent 默认应为 3，got %d", cfg.Sync.MaxConcurrent)
	}
	if cfg.Sync.TaskTTL != 24*time.Hour {
		t.Fatalf("Sync.TaskTTL 默认应为 24h，got %v", cfg.Sync.TaskTTL)
	}
	if cfg.Sync.MaxRetries != 10 {
		t.Fatalf("Sync.MaxRetries 默认应为 10，got %d", cfg.Sync.MaxRetries)
	}
	if cfg.Sync.RetryDelay != 10*time.Second {
		t.Fatalf("Sync.RetryDelay 默认应为 10s，got %v", cfg.Sync.RetryDelay)
	}
	if cfg.Sync.RetryBackoff != 2 {
		t.Fatalf("Sync.RetryBackoff 默认应为 2，got %v", cfg.Sync.RetryBackoff)
	}
	if len(cfg.SyncRemotes) != 0 {
		t.Fatalf("SyncRemotes 默认应为空，got %v", cfg.SyncRemotes)
	}
}

func TestSyncConfig_SetDefaults(t *testing.T) {
	cfg := Default()
	cfg.Sync.MaxConcurrent = 0
	cfg.Sync.TaskTTL = 0
	cfg.Sync.MaxRetries = 0
	cfg.Sync.RetryDelay = 0
	cfg.Sync.RetryBackoff = 0
	cfg.SetDefaults()
	if cfg.Sync.MaxConcurrent != 3 {
		t.Fatalf("SetDefaults 应补 MaxConcurrent=3，got %d", cfg.Sync.MaxConcurrent)
	}
	if cfg.Sync.TaskTTL != 24*time.Hour {
		t.Fatalf("SetDefaults 应补 TaskTTL=24h，got %v", cfg.Sync.TaskTTL)
	}
	if cfg.Sync.MaxRetries != 10 {
		t.Fatalf("SetDefaults 应补 MaxRetries=10，got %d", cfg.Sync.MaxRetries)
	}
	if cfg.Sync.RetryDelay != 10*time.Second {
		t.Fatalf("SetDefaults 应补 RetryDelay=10s，got %v", cfg.Sync.RetryDelay)
	}
	if cfg.Sync.RetryBackoff != 2 {
		t.Fatalf("SetDefaults 应补 RetryBackoff=2，got %v", cfg.Sync.RetryBackoff)
	}
}

func TestSyncConfig_Validate_RemoteURLAndName(t *testing.T) {
	base := Default()

	// 空 name → 拒绝
	cfg := *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "", URL: "http://127.0.0.1:18083"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("sync_remotes name 为空应拒绝")
	}

	// 重复 name → 拒绝
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{
		{Name: "dup", URL: "http://127.0.0.1:1"},
		{Name: "dup", URL: "http://127.0.0.1:2"},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("sync_remotes name 重复应拒绝")
	}

	// 非法 URL → 拒绝
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "not-a-url"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("sync_remotes url 非法应拒绝")
	}

	// 非 http/https scheme → 拒绝
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "ftp://127.0.0.1:21"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("sync_remotes url scheme 非 http/https 应拒绝")
	}

	// 合法配置（无凭据）→ Validate 通过（凭据 fail-closed 在 CreateTask 层）
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "http://127.0.0.1:18083"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("合法 sync_remotes 不应被 Validate 拒绝: %v", err)
	}
}

func TestSyncConfig_Validate_RemoteErrorContainsName(t *testing.T) {
	base := Default()
	cfg := *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "", URL: "http://127.0.0.1:18083"}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "sync_remotes") {
		t.Fatalf("错误应包含 sync_remotes 字段上下文: %v", err)
	}
}

// TestSyncConfig_Validate_PlainHTTPRemoteRejected 验证 sync_remotes 明文 http 仅限
// loopback（本机调试）；远程 http（SproxySig AK/SK 明文上线）被 Validate 拒绝
// （安全审查 MEDIUM：对齐联邦 peering 的 TLS 安全边界）。
func TestSyncConfig_Validate_PlainHTTPRemoteRejected(t *testing.T) {
	base := Default()

	// loopback http → 允许（本机调试）
	cfg := *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "http://127.0.0.1:18083"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("loopback http 应允许: %v", err)
	}

	// 远程 http（非 loopback）→ 拒绝（AK/SK 明文上线）
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "http://example.com:18083"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("远程 http 明文应拒绝（AK/SK 明文上线）")
	}

	// https 远程 → 允许（TLS 加密）
	cfg = *base
	cfg.SyncRemotes = []SyncRemoteConfig{{Name: "r", URL: "https://example.com:18083"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("https 远程应允许: %v", err)
	}
}
