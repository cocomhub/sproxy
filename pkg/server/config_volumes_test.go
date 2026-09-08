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

// TestVolumesConfig_Validate_KeepsExplicitRootFromStorageRoot 锁定 M-4 红线（任务 1 复审遗留
// Minor）：storage_root 与显式 volumes[].root 并存时，归一（SetDefaults）与 Validate **绝不**
// 以 storage_root 覆写显式非空 root——即便显式 root 恰为缺省占位 defaultStorageRoot（与合成
// 单卷同值，最易被误判为「未配」而静默改根）。空 root（YAML 未写）作为对照：仅此形态才跟随
// storage_root。表驱动覆盖三种代表性形态。
func TestVolumesConfig_Validate_KeepsExplicitRootFromStorageRoot(t *testing.T) {
	cases := []struct {
		name     string
		volRoot  string // volumes[0].root；"" 表示 YAML 未写 root
		wantRoot string
	}{
		{"显式占位 root=defaultStorageRoot", defaultStorageRoot, defaultStorageRoot},
		{"显式自定义 root=/mnt/x", "/mnt/x", "/mnt/x"},
		{"空 root → 跟随 storage_root", "", "/data"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			vol := map[string]any{"name": "default"}
			if tc.volRoot != "" {
				vol["root"] = tc.volRoot
			}
			cfg, err := LoadFromProvider(mapProvider{m: map[string]any{
				"storage_root": "/data",
				"volumes":      []any{vol},
			}})
			if err != nil {
				t.Fatalf("LoadFromProvider: %v", err)
			}
			if len(cfg.Volumes) != 1 || cfg.Volumes[0].Root != tc.wantRoot {
				t.Fatalf("volumes[0].root=%q, want %q（全量 %+v）", cfg.Volumes[0].Root, tc.wantRoot, cfg.Volumes)
			}
			// Validate 幂等：二次校验也不得改写显式 root。
			if err := cfg.Validate(); err != nil {
				t.Fatalf("二次 Validate: %v", err)
			}
			if cfg.Volumes[0].Root != tc.wantRoot {
				t.Fatalf("二次 Validate 后 volumes[0].root 被改写为 %q, want %q", cfg.Volumes[0].Root, tc.wantRoot)
			}
		})
	}
}
