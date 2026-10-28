// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// legacy_state_usage_gate_test.go 是「旧派生状态实现」棘轮（R46）。
//
// 背景（设计 2026-10-28-volume-state-maintenance §五）：本地卷的派生状态有两套——旧的
// 「写路径逐点维护」（`searchIndex.upsert/remove`、`checksums.json` 独立台账、`ExternalList`
// 薄转发）与新的「Root/FS 变异事件驱动」（`pkg/files/mutation_index.go` + `pkg/volumestate`）。
// 用户裁定允许**短期共存**，但共存不能演变成永久双轨 ⇒ 旧实现的独立使用点数由
// `legacy_state_usage.tsv` 登记，**只减不增**。
//
// 口径（三条正则的出现次数之和，逐文件）：
//   - `.index.<upsert|upsertDir|remove|removePrefix|invalidate>(`：写路径逐点维护索引；
//   - `ExternalList(`：外部卷列举薄转发；
//   - `checksums.json` / `checksumRel`：per-tenant 校验和台账。
// 排除 `pkg/files/mutation_index.go`（事件驱动的**新**机制入口，不是旧实现）。
//
// 规则：新增计数/上调 → 红（须在 reason 说明理由）；计数下降 → 红（须同步下调基线，锁定收益）；
// 条目对应文件已无计数 → 红（须删除该行）。变异：在任意未登记文件加一行 `.index.remove(` → 红。

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// legacyStateUsageFile 是登记表文件名。
const legacyStateUsageFile = "legacy_state_usage.tsv"

// legacyStateExcluded 是**新机制**入口（不计入旧实现口径）。
var legacyStateExcluded = map[string]bool{
	"pkg/files/mutation_index.go": true,
}

// legacyStatePatterns 是旧实现的三类使用点。
var legacyStatePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.index\.(upsert|upsertDir|remove|removePrefix|invalidate)\(`),
	regexp.MustCompile(`\bExternalList\(`),
	regexp.MustCompile(`checksums\.json|checksumRel`),
}

// scanLegacyStateUsage 统计非测试源码里旧实现使用点（file → 次数）。
func scanLegacyStateUsage(t *testing.T) map[string]int {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]int{}
	for _, dir := range []string{"pkg", "internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil //nolint:nilerr // 遍历容错（缺目录视为无）
			}
			if d.IsDir() {
				name := d.Name()
				if name == "testdata" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil //nolint:nilerr
			}
			rel = filepath.ToSlash(rel)
			if legacyStateExcluded[rel] || strings.HasPrefix(rel, "internal/archcheck/") {
				return nil
			}
			data, rdErr := os.ReadFile(p)
			if rdErr != nil {
				return nil //nolint:nilerr
			}
			n := 0
			for _, pat := range legacyStatePatterns {
				n += len(pat.FindAll(data, -1))
			}
			if n > 0 {
				out[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s: %v", dir, err)
		}
	}
	return out
}

// legacyBudget 是登记表条目（最大次数 + 理由）。
type legacyBudget struct {
	max    int
	reason string
}

// loadLegacyStateUsage 解析登记表。
func loadLegacyStateUsage(t *testing.T) map[string]legacyBudget {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "internal", "archcheck", legacyStateUsageFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s: %v", legacyStateUsageFile, err)
	}
	out := map[string]legacyBudget{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			t.Fatalf("%s 行格式应为 file<TAB>max<TAB>reason: %q", legacyStateUsageFile, line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(cols[1]))
		if cerr != nil {
			t.Fatalf("%s 次数非法 %q: %v", legacyStateUsageFile, cols[1], cerr)
		}
		reason := strings.TrimSpace(strings.Join(cols[2:], "\t"))
		if reason == "" {
			t.Fatalf("%s 条目缺少 reason: %q", legacyStateUsageFile, line)
		}
		out[strings.TrimSpace(cols[0])] = legacyBudget{max: n, reason: reason}
	}
	return out
}

// TestLegacyStateUsageRatchet 钉住旧派生状态实现的使用点数只减不增。
func TestLegacyStateUsageRatchet(t *testing.T) {
	t.Parallel()
	counts := scanLegacyStateUsage(t)
	budget := loadLegacyStateUsage(t)

	for file, b := range budget {
		cur, ok := counts[file]
		if !ok {
			t.Errorf("%s 已无旧实现使用点 ⇒ 请从 %s 删除该行（棘轮锁定清理成果）", file, legacyStateUsageFile)
			continue
		}
		switch {
		case cur > b.max:
			t.Errorf("%s 旧实现使用点 %d > 基线 %d（R46 只减不增）：请改用事件驱动机制，或显式上调基线并说明理由", file, cur, b.max)
		case cur < b.max:
			t.Errorf("%s 旧实现使用点已降到 %d < 基线 %d ⇒ 请同步下调 %s（锁定收益）", file, cur, b.max, legacyStateUsageFile)
		}
	}
	for file, cur := range counts {
		if _, ok := budget[file]; !ok {
			t.Errorf("%s 新增 %d 个旧实现使用点但未登记（R46）：请改用 pkg/files/mutation_index.go 的事件驱动机制，或在 %s 登记理由",
				file, cur, legacyStateUsageFile)
		}
	}
	// 覆盖探针：扫描面被改窄（例如目录白名单失效）时立刻红。
	if len(counts) < 10 {
		t.Fatalf("只扫描到 %d 个含旧实现使用点的文件（期望 ≥10）——扫描面被改窄", len(counts))
	}
	total := 0
	for _, n := range counts {
		total += n
	}
	if total < 30 {
		t.Fatalf("旧实现使用点总数 %d 低于探针下限 30——扫描面被改窄", total)
	}
}
