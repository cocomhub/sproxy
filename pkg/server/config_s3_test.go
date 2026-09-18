// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestValidate_Volumes_S3Type 钉住 s3 系统盘并入 volumes[]（S2）：
// volumes[] type=s3 的外部卷校验（extra.endpoint/bucket/access_key/secret_key 必填，
// fail-closed）。
func TestValidate_Volumes_S3Type(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mutate  func(c *Config)
		wantErr string // 空 = 期望通过
	}{
		{"volumes[] type=s3 + extra 完整 → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{
					"endpoint": "127.0.0.1:9000", "bucket": "mybucket",
					"access_key": "ak", "secret_key": "sk",
				},
			})
		}, ""},
		{"volumes[] type=s3 endpoint 带 scheme → 通过", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{
					"endpoint": "https://s3.example.com", "bucket": "b",
					"access_key": "ak", "secret_key": "sk",
				},
			})
		}, ""},
		{"volumes[] type=s3 缺 endpoint → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{"bucket": "b", "access_key": "ak", "secret_key": "sk"},
			})
		}, "endpoint"},
		{"volumes[] type=s3 endpoint scheme 无 host → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{"endpoint": "https://", "bucket": "b", "access_key": "ak", "secret_key": "sk"},
			})
		}, "endpoint"},
		{"volumes[] type=s3 缺 bucket → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{"endpoint": "127.0.0.1:9000", "access_key": "ak", "secret_key": "sk"},
			})
		}, "bucket"},
		{"volumes[] type=s3 缺凭据 → 拒绝", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{
				Name: "s3d", Type: "s3",
				Extra: map[string]any{"endpoint": "127.0.0.1:9000", "bucket": "b"},
			})
		}, "access_key"},
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
