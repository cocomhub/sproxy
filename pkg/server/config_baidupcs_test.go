// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestValidate_Volumes_BaidupcsType 钉住 baidupcs 系统盘并入 volumes[]（V3 接入 T2）：
// volumes[] type=baidupcs 的外部卷校验（extra.bduss/binary_path 至少一个，fail-closed）。
func TestValidate_Volumes_BaidupcsType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // 空 = 期望通过
	}{
		{"volumes[] type=baidupcs + extra.bduss → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "mydisk", Type: "baidupcs",
				Extra: map[string]any{"bduss": "test-bduss"},
			})
		}, ""},
		{"volumes[] type=baidupcs + extra.binary_path → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "mydisk", Type: "baidupcs",
				Extra: map[string]any{"binary_path": "/usr/local/bin/BaiduPCS-Go"},
			})
		}, ""},
		{"volumes[] type=baidupcs 缺凭据 → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "mydisk", Type: "baidupcs",
				Extra: map[string]any{"baidu_root": "/disk1"},
			})
		}, "bduss"},
		{"volumes[] type=baidupcs extra 非字符串值 → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "mydisk", Type: "baidupcs",
				Extra: map[string]any{"bduss": 12345},
			})
		}, "bduss"},
		{"本地卷（Type 缺省）零回归 → 通过", func(c *Config) {
			// 不追加任何卷，Default() 的默认本地卷不受影响。
		}, ""},
		{"本地卷（Type=local）零回归 → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "extra-local", Type: "local", Root: "/tmp/extra-local",
			})
		}, ""},
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
