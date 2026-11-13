// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_cache_quota_test.go 钉住 L1a（精简 meta 缓存）的**磁盘配额淘汰**：
// 实测该缓存曾无上限增长到 5.29 GB / 9,489 文件 ⇒ 必须设限并按最旧优先淘汰。

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func mkFiles(specs ...metaCacheFile) []metaCacheFile { return specs }

// TestPlanMetaCacheEviction_KeepsWithinQuota 锁定核心语义：**超配额才删，且删最旧**，
// 直到总量 ≤ 配额。变异：改成"删最新"或"不删" ⇒ 本用例红。
func TestPlanMetaCacheEviction_KeepsWithinQuota(t *testing.T) {
	t.Parallel()
	files := mkFiles(
		metaCacheFile{Path: "c", Size: 300, ModTime: 3}, // 最新
		metaCacheFile{Path: "a", Size: 300, ModTime: 1}, // 最旧
		metaCacheFile{Path: "b", Size: 300, ModTime: 2},
	)
	// 总量 900，配额 600 ⇒ 必须删 1 个（最旧的 a）。
	ev := planMetaCacheEviction(files, 600)
	if len(ev) != 1 || ev[0].Path != "a" {
		t.Fatalf("应只删最旧的 a，得到 %v", ev)
	}
	// 不超配额 ⇒ 不删。
	if ev := planMetaCacheEviction(mkFiles(files...), 900); len(ev) != 0 {
		t.Fatalf("等于配额不应删，得到 %v", ev)
	}
	// 配额小于单文件 ⇒ 持续删到空（宁可全清也不超）。
	if ev := planMetaCacheEviction(mkFiles(files...), 1); len(ev) != 3 {
		t.Fatalf("配额极小应删光，得到 %d 条", len(ev))
	}
}

// TestPlanMetaCacheEviction_NegativeQuotaMeansUnlimited 负配额 = 显式不限制。
func TestPlanMetaCacheEviction_NegativeQuotaMeansUnlimited(t *testing.T) {
	t.Parallel()
	files := mkFiles(metaCacheFile{Path: "a", Size: 1 << 30, ModTime: 1})
	if ev := planMetaCacheEviction(files, -1); ev != nil {
		t.Fatalf("负配额应不限制，得到 %v", ev)
	}
}

// TestPlanMetaCacheEviction_DeterministicOnSameMTime 同 mtime 时按路径确定（便于复现）。
func TestPlanMetaCacheEviction_DeterministicOnSameMTime(t *testing.T) {
	t.Parallel()
	files := mkFiles(
		metaCacheFile{Path: "z", Size: 100, ModTime: 1},
		metaCacheFile{Path: "a", Size: 100, ModTime: 1},
	)
	ev := planMetaCacheEviction(files, 100)
	if len(ev) != 1 || ev[0].Path != "a" {
		t.Fatalf("同 mtime 应按路径升序确定性淘汰，得到 %v", ev)
	}
}

// TestEnforceMetaCacheQuota_OnDisk 端到端：真实目录 + 配额 ⇒ 超限文件被删、总大小落到配额内。
func TestEnforceMetaCacheQuota_OnDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	// 造 3 个 1 KiB 文件，mtime 递增（用 os.Chtimes 保证顺序确定）。
	names := []string{"a", "b", "c"}
	for i, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, make([]byte, 1024), 0o600); err != nil {
			t.Fatal(err)
		}
		ts := time.Unix(int64(1000+i), 0)
		if err := os.Chtimes(p, ts, ts); err != nil {
			t.Fatal(err)
		}
	}
	s := &SecretdataFS{opts: Options{MetaCacheDir: dir, MetaCacheQuotaBytes: 2048}} // 配额 2 KiB
	res := s.enforceMetaCacheQuota()
	if res.RemovedFiles != 1 {
		t.Fatalf("应删 1 个最旧文件，得到 %d", res.RemovedFiles)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 2 {
		t.Fatalf("应剩 2 个文件，得到 %d", len(ents))
	}
	left := make([]string, 0, len(ents))
	for _, e := range ents {
		left = append(left, e.Name())
	}
	sort.Strings(left)
	if left[0] != "b" || left[1] != "c" {
		t.Fatalf("应保留较新的 b/c，实际剩 %v", left)
	}
}
