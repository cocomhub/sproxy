// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package contextcfg

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestConfig_LoadAndRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{
		CurrentContext: "sg",
		Environments:   []*Environment{{Name: "sg", ServerURL: "https://hub:18083", HubURL: "wss://hub:18083/ws", NodeID: "home"}},
		Users:          []*User{{Name: "alice", AccessKey: "ak-1", AccessKeySecret: "s1", AccessKeyID: "skey-1", Owner: "alice"}},
		Contexts:       []*Context{{Name: "sg", Environment: "sg", User: "alice"}},
	}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.CurrentContext != "sg" || len(got.Environments) != 1 || got.Environments[0].HubURL != "wss://hub:18083/ws" {
		t.Errorf("round-trip mismatch: %+v", got)
	}
	if got.Users[0].AccessKeySecret != "s1" {
		t.Errorf("secret round-trip: %q", got.Users[0].AccessKeySecret)
	}
	// Windows 上 Chmod 权限位语义不同（仅影响可写位，恒报 0666），跳过权限断言。
	// Linux/macOS 上严格要求 0600（凭据明文）。
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(path)
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("config 文件权限 = %o, want 600", fi.Mode().Perm())
		}
	}
}

func TestLoad_MissingFile_ReturnsDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg, err := Load(filepath.Join(dir, "nope.yaml"))
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if cfg == nil || len(cfg.Environments) != 0 || cfg.CurrentContext != "" {
		t.Errorf("缺省应为空模型: %+v", cfg)
	}
}

func TestConfig_SetCurrentContext(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := &Config{CurrentContext: "a", Environments: []*Environment{{Name: "a"}}, Users: []*User{{Name: "u"}}, Contexts: []*Context{{Name: "a", Environment: "a", User: "u"}}}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := SetCurrentContext(path, "a"); err != nil {
		t.Fatalf("SetCurrentContext: %v", err)
	}
	got, _ := Load(path)
	if got.CurrentContext != "a" {
		t.Errorf("current-context 未更新: %q", got.CurrentContext)
	}
}

func TestConfig_ContextValidation(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		Environments: []*Environment{{Name: "sg"}},
		Users:        []*User{{Name: "alice"}},
		Contexts:     []*Context{{Name: "c1", Environment: "sg", User: "alice"}, {Name: "c2", Environment: "missing"}},
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "c2") {
		t.Errorf("应报 context c2 引用缺失 environment: %v", err)
	}
}

func TestMigrateLegacyFlatConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "sclient.yaml")
	if err := os.WriteFile(legacy, []byte("server_url: https://hub:18083\naccess_key: ak-1\naccess_key_secret: s1\naccess_key_id: skey-1\nhub_url: wss://hub:18083/ws\nnode_id: home\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := MigrateLegacy(legacy, "")
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if len(cfg.Environments) != 1 || cfg.Environments[0].Name != "default" {
		t.Errorf("env 名应为 default: %+v", cfg.Environments)
	}
	if cfg.Environments[0].ServerURL != "https://hub:18083" || cfg.Environments[0].HubURL != "wss://hub:18083/ws" {
		t.Errorf("连接面字段映射错: %+v", cfg.Environments[0])
	}
	if cfg.Users[0].AccessKey != "ak-1" || cfg.Users[0].AccessKeySecret != "s1" {
		t.Errorf("凭据面字段映射错: %+v", cfg.Users[0])
	}
	if len(cfg.Contexts) != 1 || cfg.Contexts[0].Name != "default" || cfg.CurrentContext != "default" {
		t.Errorf("context/current 生成错: %+v", cfg)
	}
}

func TestMigrateLegacyWithEnvName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	legacy := filepath.Join(dir, "sclient.prod.yaml")
	if err := os.WriteFile(legacy, []byte("server_url: https://prod:18083\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := MigrateLegacy(legacy, "prod")
	if err != nil {
		t.Fatalf("MigrateLegacy: %v", err)
	}
	if cfg.Environments[0].Name != "prod" {
		t.Errorf("SCLIENT_ENV=prod 映射 env 名 prod: %+v", cfg.Environments)
	}
}

func TestResolve_Priority(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		CurrentContext: "cur",
		Environments:   []*Environment{{Name: "a"}, {Name: "b"}},
		Users:          []*User{{Name: "u1"}, {Name: "u2"}},
		Contexts: []*Context{
			{Name: "cur", Environment: "a", User: "u1"},
			{Name: "sel", Environment: "b", User: "u2"},
		},
	}
	// 1) --context 最高。
	r, err := Resolve(cfg, ResolveArgs{Context: "sel", Environment: "a", User: "u2"})
	if err != nil || r.Environment.Name != "b" || r.User.Name != "u2" {
		t.Errorf("--context 应覆盖 env/user: %+v, %v", r, err)
	}
	// 2) --env+--user 覆盖 current。
	r2, _ := Resolve(cfg, ResolveArgs{Environment: "b", User: "u2"})
	if r2.Environment.Name != "b" || r2.User.Name != "u2" {
		t.Errorf("--env/--user 应覆盖 current: %+v", r2)
	}
	// 3) 无 flag → current。
	r3, _ := Resolve(cfg, ResolveArgs{})
	if r3.Environment.Name != "a" || r3.User.Name != "u1" {
		t.Errorf("默认应走 current: %+v", r3)
	}
	// 4) current 缺失 → 报错指引 context use。
	cfg.CurrentContext = ""
	if _, err := Resolve(cfg, ResolveArgs{}); err == nil || !strings.Contains(err.Error(), "context use") {
		t.Errorf("无 current 且无 flag 应报错指引: %v", err)
	}
}

func TestResolve_SingleOverride(t *testing.T) {
	t.Parallel()
	cfg := &Config{
		CurrentContext: "cur",
		Environments:   []*Environment{{Name: "a"}, {Name: "b"}},
		Users:          []*User{{Name: "u1"}},
		Contexts:       []*Context{{Name: "cur", Environment: "a", User: "u1"}},
	}
	// 只覆盖 user → env 保持 current 的。
	r, _ := Resolve(cfg, ResolveArgs{User: "u1"})
	if r.Environment.Name != "a" {
		t.Errorf("仅 --user 时 env 应保持 current: %+v", r)
	}
}
