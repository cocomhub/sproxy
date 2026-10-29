// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// list_source_gate_test.go 是「list/search 数据源收口」棘轮（R41，影子台账阶段）。
//
// 背景（设计 2026-10-28-volume-state-maintenance §15.2）：本地卷 list 曾由 `searchIndex`
// 提供（`index.list`）。迁移已完成：
//   - 步骤 2（#895）：list 优先经**卷状态视图**（`VolumeView.ListDir` + volumestate 缓存）；
//   - 步骤 3（2026-10-29）：**删除 `index.list`**，视图不可用时直接实时扫描（`listFallback`）——
//     `searchIndex` 只保留搜索语义。
//
// 本门禁现由两段组成：
//  1. **硬禁**：`.index.list(` 出现即失败（实现已删除，任何新增都是回归）；
//  2. **棘轮**：`.index.search(` 逐文件登记，只减不增（搜索是 searchIndex 的唯一职责）。
//
// 2026-10-29 追加收敛：外部卷列举接缝 `ExternalList` / `ExternalVolumeSource` 已删除——
// 本地卷与外部卷共用 `VolumeViewLister.ListDirView`（键空间由 `ResolveUserLocation` 统一）。
// 因此 `ExternalList(` 也转为**硬禁**。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// listSourceCallsFile 是登记表文件名。
const listSourceCallsFile = "list_source_calls.tsv"

// listIndexListPattern 是**已删除**的 list 索引入口（硬禁：任何出现都是回归）。
var listIndexListPattern = regexp.MustCompile(`\.index\.list\(`)

// externalListPattern 是**已删除**的外部卷列举接缝（2026-10-29 收敛进 VolumeViewLister；
// 硬禁：任何出现都是回归）。
var externalListPattern = regexp.MustCompile(`ExternalList\(`)

// listSourcePatterns 是 list/search 数据源的直调形态。
var listSourcePatterns = []*regexp.Regexp{
	regexp.MustCompile(`\.index\.search\(`),
}

// scanListSourceCalls 统计非测试源码里的直调点数（file → count）。
func scanListSourceCalls(t *testing.T) map[string]int {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]int{}
	for _, dir := range []string{"pkg", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil //nolint:nilerr // 遍历容错
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" {
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
			if strings.HasPrefix(rel, "internal/archcheck/") {
				return nil
			}
			data, rdErr := os.ReadFile(p)
			if rdErr != nil {
				return nil //nolint:nilerr
			}
			n := 0
			for _, pat := range listSourcePatterns {
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

// loadListSourceCalls 读取登记表（file → max）。
func loadListSourceCalls(t *testing.T) map[string]int {
	t.Helper()
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", listSourceCallsFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s: %v（先跑 ARCHCHECK_WRITE_BASELINE=1 生成基线）", listSourceCallsFile, err)
	}
	out := map[string]int{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			t.Fatalf("%s 行格式应为 file<TAB>max<TAB>reason: %q", listSourceCallsFile, line)
		}
		n, cerr := strconv.Atoi(strings.TrimSpace(cols[1]))
		if cerr != nil {
			t.Fatalf("%s 次数非法 %q: %v", listSourceCallsFile, cols[1], cerr)
		}
		if strings.TrimSpace(strings.Join(cols[2:], "\t")) == "" {
			t.Fatalf("%s 条目缺少 reason: %q", listSourceCallsFile, line)
		}
		out[strings.TrimSpace(cols[0])] = n
	}
	return out
}

// TestListSourceCallsRatchet 钉住 list/search 数据源直调点只减不增（R41 影子台账）。
func TestListSourceCallsRatchet(t *testing.T) {
	t.Parallel()
	counts := scanListSourceCalls(t)
	budget := loadListSourceCalls(t)
	for file, b := range budget {
		cur, ok := counts[file]
		if !ok {
			t.Errorf("%s 已无直调点 ⇒ 请从 %s 删除该行（迁移成果要锁定）", file, listSourceCallsFile)
			continue
		}
		switch {
		case cur > b:
			t.Errorf("%s 直调点 %d > 基线 %d（R41 只减不增）：新代码请走卷状态视图（VolumeView/FS.ListDir），"+
				"或显式上调基线并说明理由", file, cur, b)
		case cur < b:
			t.Errorf("%s 直调点已降到 %d < 基线 %d ⇒ 请同步下调 %s（锁定收益）", file, cur, b, listSourceCallsFile)
		}
	}
	for file, cur := range counts {
		if _, ok := budget[file]; !ok {
			t.Errorf("%s 新增 %d 个 list/search 数据源直调点但未登记（R41）：请走卷状态视图，或在 %s 登记理由",
				file, cur, listSourceCallsFile)
		}
	}
	// 覆盖探针：扫描面被改窄时立刻红。
	total := 0
	for _, n := range counts {
		total += n
	}
	if len(counts) < 1 || total < 1 {
		t.Fatalf("扫描面疑似被改窄：文件 %d 处 / 直调点 %d（期望 ≥1 / ≥1）", len(counts), total)
	}
	// 硬禁：`index.list` 已随 §15.2 步骤 3 删除（list 走卷状态视图/实时扫描）。
	for file, hits := range scanPatternHits(t, listIndexListPattern) {
		t.Errorf("%s 出现 %d 处 `.index.list(`：该入口已删除（list 走卷状态视图/实时扫描），"+
			"请改用 VolumeView/FS.ListDir（或 listFallback）", file, hits)
	}
	// 硬禁：`ExternalList` 已随「外部卷列举统一进 VolumeViewLister」删除。
	for file, hits := range scanPatternHits(t, externalListPattern) {
		t.Errorf("%s 出现 %d 处 `ExternalList(`：该接缝已删除（本地卷与外部卷共用 "+
			"VolumeViewLister.ListDirView），请改用 VolumeView/FS.ListDir", file, hits)
	}
}

// scanPatternHits 统计非测试源码里某个模式的命中数（file → count），用于硬禁断言。
func scanPatternHits(t *testing.T, re *regexp.Regexp) map[string]int {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]int{}
	for _, dir := range []string{"pkg", "internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil //nolint:nilerr // 遍历容错
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" {
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
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil //nolint:nilerr
			}
			if n := len(re.FindAll(data, -1)); n > 0 {
				out[filepath.ToSlash(rel)] += n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", dir, err)
		}
	}
	return out
}

// TestListSourceCallsBaselineHelper 重写登记表（ARCHCHECK_WRITE_BASELINE=1 时）。
func TestListSourceCallsBaselineHelper(t *testing.T) {
	// sproxy:serial: 基线重写是手动一次性操作（ARCHCHECK_WRITE_BASELINE=1），会改写共享 tsv
	if os.Getenv("ARCHCHECK_WRITE_BASELINE") != "1" {
		t.Skip("设置 ARCHCHECK_WRITE_BASELINE=1 才会重写 list_source_calls.tsv 基线")
	}
	counts := scanListSourceCalls(t)
	var b strings.Builder
	b.WriteString("# list/search 数据源直调点登记（R41 影子台账：只减不增）\n")
	b.WriteString("# 口径：`.index.list(` / `.index.search(` / `ExternalList(` 的出现次数；\n")
	b.WriteString("# 目标形态见设计 §15.2（list 由卷状态视图投影，searchIndex 只保留搜索）。\n")
	b.WriteString("# 格式：file<TAB>max<TAB>reason\n")
	files := make([]string, 0, len(counts))
	for f := range counts {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, file := range files {
		b.WriteString(file + "\t" + strconv.Itoa(counts[file]) + "\t现状基线（迁移推进时同步下调）\n")
	}
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", listSourceCallsFile)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 %s: %v", listSourceCallsFile, err)
	}
	t.Logf("已写入 %d 个文件的直调点基线", len(counts))
}
