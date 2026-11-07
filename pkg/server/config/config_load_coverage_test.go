// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// config_load_coverage_test.go 补**加载/保存边界**：LoadFromProvider 的解码/校验失败、
// LoadConfig 的路径为空/不存在/空文件/非法 YAML/校验失败/读失败，以及 SaveConfig 的
// 成功落盘与写入失败。临时目录统一走 testutil.MkdirTemp（R53）。
package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/provider"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// failingProvider 模拟 provider 解码失败（viper/YAML 层错误）。
type failingProvider struct{}

func (failingProvider) Unmarshal(any) error {
	return errors.New("boom")
}

var _ provider.Provider = failingProvider{}

func TestLoadFromProvider_DecodeError(t *testing.T) {
	t.Parallel()
	if _, err := LoadFromProvider(failingProvider{}); err == nil || !strings.Contains(err.Error(), "配置解码失败") {
		t.Fatalf("解码失败应返回包装错误, got %v", err)
	}
}

func TestLoadFromProvider_ValidationError(t *testing.T) {
	t.Parallel()
	if _, err := LoadFromProvider(mapProvider{m: map[string]any{
		"addr":         ":18083",
		"storage_root": "/tmp/sproxy-test",
		"volumes":      []map[string]any{{"name": "main", "root": "/tmp/sproxy-test", "tier": "bogus"}},
	}}); err == nil || !strings.Contains(err.Error(), "tier") {
		t.Fatalf("校验失败应返回错误, got %v", err)
	}
}

// TestLoadFromProvider_AllowIPsWarnPath 覆盖「allow_ips 未含回环」告警分支（行为不变，仅
// 确保该分支不 panic 且配置仍然加载成功）。
func TestLoadFromProvider_AllowIPsWarnPath(t *testing.T) {
	t.Parallel()
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"storage_root": "/tmp/sproxy-test",
		"auth":         map[string]any{"allow_ips": []string{"10.0.0.0/8"}},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if len(cfg.Auth.AllowIPs) != 1 {
		t.Fatalf("allow_ips 未保留: %v", cfg.Auth.AllowIPs)
	}
}

func TestLoadConfig_PathBoundaries(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "config-load-")
	cases := []struct {
		name    string
		path    func() string
		wantErr string
	}{
		{"空路径返回默认", func() string { return "" }, ""},
		{"文件不存在返回默认", func() string { return filepath.Join(dir, "missing.yaml") }, ""},
		{"目录路径读失败", func() string { return dir }, "读取配置文件失败"},
		{"空文件返回默认", func() string {
			p := filepath.Join(dir, "empty.yaml")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return p
		}, ""},
		{"非法 YAML 解析失败", func() string {
			p := filepath.Join(dir, "bad.yaml")
			if err := os.WriteFile(p, []byte("addr: [unclosed\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return p
		}, "解析配置文件失败"},
		{"校验失败", func() string {
			p := filepath.Join(dir, "invalid.yaml")
			if err := os.WriteFile(p, []byte("volumes:\n  - name: main\n    root: /tmp/x\n    tier: bogus\n"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			return p
		}, "配置校验失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg, err := LoadConfig(tc.path())
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("期望成功, got %v", err)
				}
				if cfg == nil || cfg.Addr == "" {
					t.Fatalf("默认配置不完整: %+v", cfg)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestLoadConfig_AllowIPsWarnPath(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "config-load-warn-")
	path := filepath.Join(dir, "warn.yaml")
	if err := os.WriteFile(path, []byte("auth:\n  allow_ips: [\"10.0.0.0/8\"]\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.Auth.AllowIPs) != 1 {
		t.Fatalf("allow_ips 未保留: %v", cfg.Auth.AllowIPs)
	}
}

func TestSaveConfig_RoundTripAndWriteError(t *testing.T) {
	t.Parallel()
	dir := testutil.MkdirTemp(t, "config-save-")
	path := filepath.Join(dir, "sproxy.yaml")
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Auth.AllowIPs = []string{"127.0.0.1"}
	if err := SaveConfig(cfg, path); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig 回读: %v", err)
	}
	if loaded.StorageRoot != cfg.StorageRoot {
		t.Fatalf("round-trip storage_root 不符: %q != %q", loaded.StorageRoot, cfg.StorageRoot)
	}
	// 父目录不存在 → os.WriteFile 失败（错误包装分支）。
	badPath := filepath.Join(dir, "missing-subdir", "sproxy.yaml")
	if err := SaveConfig(cfg, badPath); err == nil || !strings.Contains(err.Error(), "写入配置文件失败") {
		t.Fatalf("写入失败应返回包装错误, got %v", err)
	}
}
