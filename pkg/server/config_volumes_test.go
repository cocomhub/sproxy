// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

func TestVolumesConfig_DefaultsAndParse(t *testing.T) {
	// 未配 volumes → 单卷合成（name=default, root=StorageRoot）
	c := Default()
	if got := len(c.Volumes); got != 1 {
		t.Fatalf("未配 volumes 应有合成单卷, got %d", got)
	}
	if c.Volumes[0].Name != "default" || c.Volumes[0].Root != c.StorageRoot {
		t.Fatalf("合成默认卷不符: %+v", c.Volumes[0])
	}
	if c.Placement != "prefer-default" {
		t.Fatalf("缺省 placement 应为 prefer-default, got %q", c.Placement)
	}
}

// TestVolumesConfig_SetDefaults_KeepsExplicitFirstVolumeRoot 锁定 M-4 红线：SetDefaults 只
// 填充**空** root，绝不覆写用户显式写的非空 volumes[0].root——即便 storage_root 另配了
// /data，显式 root /mnt/x 也必须保留（防归一阶段静默吞掉用户意图）。
func TestVolumesConfig_SetDefaults_KeepsExplicitFirstVolumeRoot(t *testing.T) {
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
		"storage_root": "/data",
		"volumes": []any{
			map[string]any{"name": "default", "root": "/mnt/x"},
		},
	}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if len(cfg.Volumes) != 1 || cfg.Volumes[0].Root != "/mnt/x" {
		t.Fatalf("显式 volumes[0].root 不应被 storage_root 覆写, got %+v", cfg.Volumes)
	}
	if cfg.Volumes[0].Name != "default" {
		t.Fatalf("卷名应保留显式 default, got %q", cfg.Volumes[0].Name)
	}
	// 归一后显式 root 仍有效 → Validate 应通过（不因覆写而丢根）。
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestVolumesConfig_Validate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"合法多卷", func(c *Config) {
			c.Volumes = []VolumeConfig{
				{Name: "main", Root: "./storage", ACL: &VolumeACLConfig{Mode: "allow", Owners: []string{"alice"}}},
				{Name: "disk2", Root: "/mnt/disk2", VolCapacity: 1 << 30},
			}
		}, ""},
		{"非法 mode", func(c *Config) { c.Volumes[0].ACL = &VolumeACLConfig{Mode: "bogus"} }, "acl mode"},
		{"重复卷名", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "default", Root: "/x"})
		}, "重复"},
		{"非法卷名", func(c *Config) { c.Volumes[0].Name = ".." }, "非法"},
		{"负容量", func(c *Config) { c.Volumes[0].VolCapacity = -1 }, "不能为负"},
		{"非法 placement", func(c *Config) { c.Placement = "round-robin" }, "placement"},
		{"非法 owner", func(c *Config) {
			c.Volumes[0].ACL = &VolumeACLConfig{Mode: VolumeACLDeny, Owners: []string{"a/b"}}
		}, "acl owners"},
		{"非首卷空 root", func(c *Config) {
			c.Volumes = append(c.Volumes, VolumeConfig{Name: "disk2"})
		}, "root"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := Default() // 每 case 独立实例，避免浅拷贝共享底层数组污染相邻 case
			tc.mutate(c)
			err := c.Validate()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("期望通过, got %v", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("期望含 %q, got %v", tc.wantErr, err)
			}
		})
	}
}
