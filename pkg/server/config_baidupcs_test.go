// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestBaidupcsConfig_Defaults 钉住 baidupcs 配置段默认值：禁用 + 空卷名。
func TestBaidupcsConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Baidupcs.Enabled {
		t.Fatal("Baidupcs.Enabled 默认应为 false（关闭）")
	}
	if cfg.Baidupcs.Name != "" {
		t.Fatalf("Baidupcs.Name 默认应为空，got %q", cfg.Baidupcs.Name)
	}
	if cfg.Baidupcs.LocalRoot != "" {
		t.Fatalf("Baidupcs.LocalRoot 默认应为空（装配时取默认），got %q", cfg.Baidupcs.LocalRoot)
	}
}

// TestBaidupcsConfig_Validate 钉住 baidupcs 配置段校验（fail-closed）：
//   - enabled=true 时 name 必须非空；
//   - enabled=true 时 BDUSS 与 BinaryPath 至少一个非空（否则无法保证任何执行路径可用）；
//   - disabled（默认）不要求任何字段。
func TestBaidupcsConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // 空 = 期望通过
	}{
		{"默认禁用 → 通过", func(c *Config) {}, ""},
		{"启用+name+BDUSS → 通过", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Name = "mydisk"
			c.Baidupcs.BDUSS = "test-bduss"
		}, ""},
		{"启用+name+BinaryPath → 通过", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Name = "mydisk"
			c.Baidupcs.BinaryPath = "/usr/local/bin/BaiduPCS-Go"
		}, ""},
		{"启用+缺 name → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.BDUSS = "test-bduss"
		}, "baidupcs"},
		{"启用+无凭据 → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Name = "mydisk"
		}, "baidupcs"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			tc.mutate(cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("应通过校验, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("应拒绝（%s），却通过校验", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("错误应包含 %q，got %v", tc.wantErr, err)
			}
		})
	}
}
