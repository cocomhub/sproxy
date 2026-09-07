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

// TestVolumesConfig_LoadProvider_StorageRootOnlySynthesizesFollowingRoot 覆盖既有单卷
// 配置的升级路径：YAML 仅改 storage_root、不写 volumes 时，归一后的合成默认卷 root 必须
// 跟随 storage_root（而不是停在 Default() 的占位旧根 ./storage），保证写文件落在新根。
func TestVolumesConfig_LoadProvider_StorageRootOnlySynthesizesFollowingRoot(t *testing.T) {
	cfg, err := LoadFromProvider(mapProvider{m: map[string]any{"storage_root": "/data/root"}})
	if err != nil {
		t.Fatalf("LoadFromProvider: %v", err)
	}
	if got := len(cfg.Volumes); got != 1 {
		t.Fatalf("未配 volumes 应有合成单卷, got %d", got)
	}
	if cfg.Volumes[0].Name != "default" || cfg.Volumes[0].Root != "/data/root" {
		t.Fatalf("合成默认卷应跟随 storage_root, got %+v", cfg.Volumes[0])
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
