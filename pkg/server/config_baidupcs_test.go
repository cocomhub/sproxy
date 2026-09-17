// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestBaidupcsConfig_Defaults 钉住 baidupcs 配置段默认值：禁用 + Disks 空数组。
func TestBaidupcsConfig_Defaults(t *testing.T) {
	t.Parallel()
	cfg := Default()
	if cfg.Baidupcs.Enabled {
		t.Fatal("Baidupcs.Enabled 默认应为 false（关闭）")
	}
	if len(cfg.Baidupcs.Disks) != 0 {
		t.Fatalf("Baidupcs.Disks 默认应为空数组，got %d 个", len(cfg.Baidupcs.Disks))
	}
}

// TestBaidupcsConfig_Validate 钉住 baidupcs 配置段校验（fail-closed）：
//   - enabled=true 时 Disks 非空；
//   - 每盘 name 非空；
//   - 每盘 BDUSS 与 BinaryPath 至少一个非空（否则无法保证任何执行路径可用）；
//   - disabled（默认）不要求任何字段。
func TestBaidupcsConfig_Validate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // 空 = 期望通过
	}{
		{"默认禁用 → 通过", func(c *Config) {}, ""},
		{"启用+1 盘（name+BDUSS）→ 通过", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{{Name: "mydisk", BDUSS: "test-bduss"}}
		}, ""},
		{"启用+1 盘（name+BinaryPath）→ 通过", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{{Name: "mydisk", BinaryPath: "/usr/local/bin/BaiduPCS-Go"}}
		}, ""},
		{"启用+2 盘（不同 name/BDUSS）→ 通过", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{
				{Name: "disk1", BDUSS: "bduss-1"},
				{Name: "disk2", BDUSS: "bduss-2"},
			}
		}, ""},
		{"启用+空 Disks → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
		}, "baidupcs"},
		{"启用+盘缺 name → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{{BDUSS: "test-bduss"}}
		}, "baidupcs"},
		{"启用+盘无凭据 → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{{Name: "mydisk"}}
		}, "baidupcs"},
		{"启用+盘名重复 → 拒绝", func(c *Config) {
			c.Baidupcs.Enabled = true
			c.Baidupcs.Disks = []BaidupcsDiskConfig{
				{Name: "disk1", BDUSS: "bduss-1"},
				{Name: "disk1", BDUSS: "bduss-2"},
			}
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
