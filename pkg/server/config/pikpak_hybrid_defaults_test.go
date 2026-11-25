// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package config

import "testing"

// TestPikpakHybridDefaults_MatchPikget 锁定「sproxy 与 pikget 关键下载参数一致」（防漂移门禁）。
//
// 用户 2026-11-26 明确：sproxy 应是 pikget 的长期运行版本，不允许出现有差异的独立实现。
// 任一侧改默认值而未同步 ⇒ 本用例红灯。
func TestPikpakHybridDefaults_MatchPikget(t *testing.T) {
	t.Parallel()
	d := Default()
	h := d.Pikpak.Hybrid
	if h.ChunkSize != 32<<20 {
		t.Fatalf("ChunkSize 默认 = %d，应与 pikget 一致（32MiB）", h.ChunkSize)
	}
	if h.Concurrency != 0 {
		t.Fatalf("Concurrency 默认 = %d，应留 0 由内核取默认 4（与 pikget 一致）", h.Concurrency)
	}
	if h.ShareRatio != 0 {
		t.Fatalf("ShareRatio 默认 = %v，应为 0=auto（与 pikget 一致）", h.ShareRatio)
	}
	if !h.AutoDelete {
		t.Fatal("AutoDelete 默认应为 true（与 pikget 一致，避免转存副本占空间）")
	}
	if h.DisableProgressJournal {
		t.Fatal("进度 WAL 默认应启用（与 pikget 一致）")
	}
	if h.DisableResetOnGCIDMismatch {
		t.Fatal("GCID 失配自愈默认应启用（sproxy 长期运行语义）")
	}
}
