// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// set_root_accessor_gate_test.go 是「`Set.Root(...)` 卷根访问器」棘轮门禁（R28）。
//
// 背景：`registry.Set.Root(name) *storage.Root` 让调用方**直接拿裸卷根**，绕过统一存储卷视图
// （`StorageVolume`/`FS` + 能力接口）。它是「装配层伸手进卷集拿 root」的点，也是统一存储卷模型
// M3 要收缩的面（设计 `docs/designs/2026-10-16-quota-volume-layer.md` §11）。
//
// 与 R26/R27 的分工：R26 管「`NewLocalFS` 直连路径」（根本不进 Root）；R27 管「`be.FS()` 直连」
// （外部卷封印绕过）；**R28 管「经 `Set.Root` 拿裸 root」**（进了 Root 但绕过统一视图）。
//
// 口径（与 R27 一致：**文本计数，偏严不偏松**）：统计非测试源码中
// `(volSet|vs|set|volumes|vols).Root(` 的**文本出现次数**（注释里的同名文本也计入）。
// 预算表见 `set_root_calls.tsv`（`file<TAB>max_calls<TAB>reason`），**只降不升**。
//
// 为什么是棘轮而不是清零：`storage.Root` 有 **32 个导出方法**，而 `FS`（7 方法）与
// `StorageVolume`（Name/Kind/FS/Close）**都不含**调用方所需的 `Abs`/`Stat`/`SealAt`/
// `ListOwners`/`OpenDecrypted`/`OpenFileEncrypted`/`LogicalSize` ⇒ 全量迁移需要 32 方法巨型接口
// 或重写写内核，**均非迁移**（M3 复核结论 ✓）。故：能显式化的探针已迁移（21 → 14 ✓，见 TSV 头），
// 余下**棘轮 + 逐条理由**。
//
// 收敛方式：`Root(name) != nil` / `== nil` 这类**间接探针** ⇒ `Set.HasLocalRoot(name)` ✓。

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// setRootAccessorRe 匹配「Set 类接收者」的 Root 访问器（R28 口径；偏严不偏松）。
var setRootAccessorRe = regexp.MustCompile(`(volSet|vs|set|volumes|vols)\.Root\(`)

// loadSetRootBudgets 读取 `set_root_calls.tsv`（file→max_calls）。
func loadSetRootBudgets(t *testing.T) map[string]int {
	t.Helper()
	f, err := os.Open("set_root_calls.tsv")
	if err != nil {
		t.Fatalf("读取 set_root_calls.tsv: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]int{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 3)
		if len(parts) < 2 {
			t.Fatalf("set_root_calls.tsv 行格式应为 file<TAB>max<TAB>reason: %q", line)
		}
		n, perr := strconv.Atoi(strings.TrimSpace(parts[1]))
		if perr != nil {
			t.Fatalf("set_root_calls.tsv 预算非整数: %q", line)
		}
		out[filepath.ToSlash(strings.TrimSpace(parts[0]))] = n
	}
	if serr := sc.Err(); serr != nil {
		t.Fatalf("扫描 set_root_calls.tsv: %v", serr)
	}
	return out
}

// TestSetRootAccessorRegistered 校验：① 未登记文件不得出现访问器；② 已登记文件不得超过预算。
func TestSetRootAccessorRegistered(t *testing.T) {
	t.Parallel()
	budgets := loadSetRootBudgets(t)
	counts := map[string]int{}
	for _, dir := range []string{"../../pkg", "../../cmd"} {
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, werr error) error {
			if werr != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil //nolint:nilerr // 遍历容错：只关心 .go 非测试文件
			}
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			if n := len(setRootAccessorRe.FindAll(b, -1)); n > 0 {
				rel := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(p), filepath.Clean("../..")+string(filepath.Separator)))
				counts[rel] = n
			}
			return nil
		})
		if err != nil {
			t.Fatalf("遍历 %s: %v", dir, err)
		}
	}
	for file, n := range counts {
		max, ok := budgets[file]
		if !ok {
			t.Errorf("%s 出现 Set.Root(...) 访问器 %d 次但未在 set_root_calls.tsv 登记；"+
				"先确认能否改用 HasLocalRoot / FS / 能力接口，不能才登记理由", file, n)
			continue
		}
		if n > max {
			t.Errorf("%s 的 Set.Root(...) 出现 %d 次，超过预算 %d（R28 只降不升）；"+
				"新增调用点必须显式登记并说明「为什么必需裸 root 而非 FS/接口」", file, n, max)
		}
	}
	// 预算未用满 ⇒ 提示收紧（棘轮才会越来越紧）。
	for file, max := range budgets {
		if n := counts[file]; n < max {
			t.Logf("提示：%s 可收紧预算 %d → %d（棘轮应收紧）", file, max, n)
		}
	}
}
