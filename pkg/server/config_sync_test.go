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
	if len(cfg.SyncRemotes) != 0 {
		t.Fatalf("SyncRemotes 默认应为空，got %v", cfg.SyncRemotes)
	}
}

func TestSyncConfig_SetDefaults(t *testing.T) {
	cfg := Default()
	cfg.Sync.MaxConcurrent = 0
	cfg.Sync.TaskTTL = 0
	cfg.SetDefaults()
	if cfg.Sync.MaxConcurrent != 3 {
		t.Fatalf("SetDefaults 应补 MaxConcurrent=3，got %d", cfg.Sync.MaxConcurrent)
	}
	if cfg.Sync.TaskTTL != 24*time.Hour {
		t.Fatalf("SetDefaults 应补 TaskTTL=24h，got %v", cfg.Sync.TaskTTL)
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
