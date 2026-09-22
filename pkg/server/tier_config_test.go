// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"strings"
	"testing"
)

// TestTierConfig_Validate 校验 tier 取值合法性（仅 hot|warm|cold，缺省 hot）。
func TestTierConfig_Validate(t *testing.T) {
	t.Parallel()

	// 合法取值：显式 hot/warm/cold 与缺省（空 = hot）。
	for _, tc := range []struct {
		tier string
		ok   bool
	}{
		{"", true}, // 缺省 = hot
		{"hot", true},
		{"warm", true},
		{"cold", true},
		{"frozen", false},
		{"HOT", false},
		{"h", false},
	} {
		cfg := Default()
		cfg.Volumes = []VolumeConfig{
			{Name: "default", Root: t.TempDir(), Tier: tc.tier},
			{Name: "cold1", Root: t.TempDir(), Tier: "cold"},
		}
		err := cfg.Validate()
		if tc.ok && err != nil {
			t.Fatalf("tier=%q 应合法，got err: %v", tc.tier, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "tier")) {
			t.Fatalf("tier=%q 应非法（含 tier 错误），got err: %v", tc.tier, err)
		}
	}
}

// TestTierDefault_Hot 默认卷 tier 缺省为 hot（装配后 Volume.Tier=hot，零回归）。
func TestTierDefault_Hot(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: t.TempDir()}}
	if got := cfg.Volumes[0].Tier; got != "" {
		t.Fatalf("缺省 VolumeConfig.Tier 应为空串（=hot），got %q", got)
	}
}

// TestTierPolicyValidate 校验 tier_policy 配置（interval>0 才启用；负值拒绝）。
func TestTierPolicyValidate(t *testing.T) {
	t.Parallel()

	cfg := Default()
	if cfg.TierPolicy.Interval != 0 {
		t.Fatalf("缺省 tier_policy.interval 应为 0（默认关零回归），got %v", cfg.TierPolicy.Interval)
	}
	cfg.TierPolicy.Interval = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "tier_policy") {
		t.Fatalf("tier_policy.interval 负值应拒绝，got err: %v", err)
	}
}
