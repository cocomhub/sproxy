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
