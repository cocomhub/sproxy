// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestValidate_Volumes_WebDAVType 钉住 webdav 系统盘并入 volumes[]（V2）：
// volumes[] type=webdav 的外部卷校验（extra.url http(s) 必填 + username/password 或
// token 认证至少一组，fail-closed）。
func TestValidate_Volumes_WebDAVType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // 空 = 期望通过
	}{
		{"volumes[] type=webdav + extra.url+username/password → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "wd", Type: "webdav",
				Extra: map[string]any{
					"url": "https://dav.example.com/webdav", "username": "u", "password": "p",
				},
			})
		}, ""},
		{"volumes[] type=webdav + extra.url+token → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "wd", Type: "webdav",
				Extra: map[string]any{"url": "http://127.0.0.1:8080/webdav", "token": "tok"},
			})
		}, ""},
		{"volumes[] type=webdav 缺 url → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "wd", Type: "webdav",
				Extra: map[string]any{"username": "u", "password": "p"},
			})
		}, "url"},
		{"volumes[] type=webdav url scheme 非法（ftp）→ 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "wd", Type: "webdav",
				Extra: map[string]any{"url": "ftp://host/webdav", "username": "u", "password": "p"},
			})
		}, "url"},
		{"volumes[] type=webdav 缺认证 → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "wd", Type: "webdav",
				Extra: map[string]any{"url": "https://dav.example.com/webdav"},
			})
		}, "认证"},
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
