// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// tierWarmVolsCfg 构造 hot+warm+cold 三卷配置（default=hot，warm1=warm，cold1=cold）。
func tierWarmVolsCfg(tmpDir string) *Config {
	cfg := Default()
	cfg.StorageRoot = tmpDir
	cfg.Volumes = []VolumeConfig{
		{Name: "default", Root: filepath.Join(tmpDir, "hot"), Tier: "hot"},
		{Name: "warm1", Root: filepath.Join(tmpDir, "warm"), Tier: "warm"},
		{Name: "cold1", Root: filepath.Join(tmpDir, "cold"), Tier: "cold"},
	}
	return cfg
}

// TestTierWarm_TwoStageDowngrade 两级降级：hot 超 hot 阈值 → warm；warm 超 warm 阈值 → cold。
func TestTierWarm_TwoStageDowngrade(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierWarmVolsCfg(t.TempDir())
		cfg.TierPolicy = TierPolicyConfig{
			Interval:    time.Hour,
			MaxAgeHot:   time.Minute, // hot 档：超 1 分钟且 ≥10B → 降 warm
			MinSizeHot:  10,
			MaxAgeWarm:  time.Hour, // warm 档：超 1 小时且 ≥10B → 降 cold
			MinSizeWarm: 10,
		}
	})

	uploadTierFile(t, baseURL, "default", "old.txt", strings.Repeat("x", 100))

	h := registeredHandlersFrom(t, cfgPtr)
	// 回拨 mtime 超过 hot 阈值（2 分钟 > 1 分钟）但未超 warm 阈值（2 分钟 < 1 小时）。
	rt := h.tenantFor("")
	abs, _ := rt.Root().Abs("user/old.txt")
	if err := os.Chtimes(abs, time.Now().Add(-2*time.Minute), time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	tm := newTierManager(h, time.Hour)
	tm.scanOnce()

	// 第一级：hot → warm（old.txt 应在 warm1 卷）。
	warmTnt := h.volumeTenant("warm1", "")
	if warmTnt == nil || warmTnt.Root() == nil {
		t.Fatalf("warm 卷租户不可用")
	}
	if _, err := warmTnt.Root().Stat("user/old.txt"); err != nil {
		t.Fatalf("第一级降级后 warm 卷应存在 old.txt，stat err: %v", err)
	}
	// hot 卷不再有。
	hotTnt := h.volumeTenant("default", "")
	if _, err := hotTnt.Root().Stat("user/old.txt"); err == nil {
		t.Fatalf("第一级降级后 hot 卷不应再有 old.txt")
	}

	// 第二级：warm 超 warm 阈值（回拨 mtime 超过 1 小时）→ 降 cold。
	// 文件已在 warm 卷：对 warm 卷租户路径回拨。
	warmRoot := warmTnt.Root()
	warmAbs, _ := warmRoot.Abs("user/old.txt")
	if err := os.Chtimes(warmAbs, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("Chtimes2: %v", err)
	}
	tm.scanOnce()

	coldTnt := h.volumeTenant("cold1", "")
	if coldTnt == nil || coldTnt.Root() == nil {
		t.Fatalf("cold 卷租户不可用")
	}
	if _, err := coldTnt.Root().Stat("user/old.txt"); err != nil {
		t.Fatalf("第二级降级后 cold 卷应存在 old.txt，stat err: %v", err)
	}
	if _, err := warmTnt.Root().Stat("user/old.txt"); err == nil {
		t.Fatalf("第二级降级后 warm 卷不应再有 old.txt")
	}
}

// TestTierWarm_IndependentThreshold warm 独立阈值：hot 未超 hot 阈值（年轻）但 warm 已超
// warm 阈值（不可能——年轻不会超 warm 长阈值）→ 本用例验证 warm 档只按 warm 阈值判定：
// hot 降 warm 需 hot 阈值；warm 降 cold 需 warm 阈值（warm 阈值长于 hot 时不误降）。
func TestTierWarm_IndependentThreshold(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierWarmVolsCfg(t.TempDir())
		cfg.TierPolicy = TierPolicyConfig{
			Interval:    time.Hour,
			MaxAgeHot:   time.Minute, // hot → warm：超 1 分钟
			MinSizeHot:  10,
			MaxAgeWarm:  time.Hour, // warm → cold：超 1 小时（远长于 hot）
			MinSizeWarm: 10,
		}
	})
	uploadTierFile(t, baseURL, "default", "young.txt", strings.Repeat("y", 100))

	h := registeredHandlersFrom(t, cfgPtr)
	// 回拨 2 分钟：超 hot 阈值（1 分钟）→ 降 warm；但未超 warm 阈值（1 小时）→ 不再降 cold。
	rt := h.tenantFor("")
	abs, _ := rt.Root().Abs("user/young.txt")
	if err := os.Chtimes(abs, time.Now().Add(-2*time.Minute), time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	tm := newTierManager(h, time.Hour)
	tm.scanOnce()

	// 应落在 warm（未超 warm 阈值 → 不降 cold）。
	warmTnt := h.volumeTenant("warm1", "")
	if _, err := warmTnt.Root().Stat("user/young.txt"); err != nil {
		t.Fatalf("应降级到 warm，stat err: %v", err)
	}
	coldTnt := h.volumeTenant("cold1", "")
	if _, err := coldTnt.Root().Stat("user/young.txt"); err == nil {
		t.Fatalf("未超 warm 阈值不应降 cold")
	}
}

// TestTierWarm_NoWarmFallbackToCold 无 warm 卷：hot → cold 直降（#452 行为零回归）。
func TestTierWarm_NoWarmFallbackToCold(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierVolsCfg(t.TempDir()) // 只有 hot+cold，无 warm
		cfg.TierPolicy = TierPolicyConfig{
			Interval:    time.Hour,
			MaxAgeHot:   time.Minute,
			MinSizeHot:  10,
			MaxAgeWarm:  time.Hour, // warm 阈值配置存在但无 warm 卷 → 直降 cold
			MinSizeWarm: 10,
		}
	})
	uploadTierFile(t, baseURL, "default", "big.txt", strings.Repeat("b", 100))

	h := registeredHandlersFrom(t, cfgPtr)
	rt := h.tenantFor("")
	abs, _ := rt.Root().Abs("user/big.txt")
	if err := os.Chtimes(abs, time.Now().Add(-2*time.Minute), time.Now().Add(-2*time.Minute)); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}

	tm := newTierManager(h, time.Hour)
	tm.scanOnce()

	coldTnt := h.volumeTenant("cold1", "")
	if _, err := coldTnt.Root().Stat("user/big.txt"); err != nil {
		t.Fatalf("无 warm 卷应直降 cold，stat err: %v", err)
	}
}

// TestTierWarm_PromoteWarmOnRead warm 读时回迁：访问 warm 卷文件 → 自动移回 hot 卷。
func TestTierWarm_PromoteWarmOnRead(t *testing.T) {
	t.Parallel()
	baseURL, cfgPtr := newTestServerWithAllRoutes(t, func(cfg *Config) {
		*cfg = *tierWarmVolsCfg(t.TempDir())
	})
	// 直接写文件到 warm 卷（模拟已降级到 warm）。
	uploadTierFile(t, baseURL, "warm1", "warm.txt", "warm-content")

	// 下载（无 ?volume=，跨卷定位命中 warm1）→ 触发回迁到 hot。
	resp, err := http.DefaultClient.Get(baseURL + "/download?filename=warm.txt")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download 应 200，got %d", resp.StatusCode)
	}

	h := registeredHandlersFrom(t, cfgPtr)
	hotTnt := h.volumeTenant("default", "")
	if _, err := hotTnt.Root().Stat("user/warm.txt"); err != nil {
		t.Fatalf("warm 回迁后 hot 卷应存在 warm.txt，stat err: %v", err)
	}
	warmTnt := h.volumeTenant("warm1", "")
	if _, err := warmTnt.Root().Stat("user/warm.txt"); err == nil {
		t.Fatalf("warm 回迁后 warm 卷不应再有 warm.txt")
	}
}
