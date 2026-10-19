// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// recovery_gate_test.go 是门禁 **R37**（设计 docs/designs/2026-10-19-audit-service.md §8.2 +
// program M3「统一恢复协调器」）：
//
//	① **唯一编排入口**：非测试源码中构造恢复协调器（`recovery.New(`）的文件必须登记为
//	   `orchestration` 行 —— 子系统**不得自行编排**恢复（否则「统一协调器」失效，恢复点重新散开）。
//	② **恢复清单棘轮**：`recovery_handlers.tsv` 记录全部恢复实现；`legacy-pending`（尚未接入协调器
//	   的既有自恢复）**只减不增** ⇒ 新增长任务的恢复必须走「作用域注册」或「legacy 注册」，
//	   而不是再写一套自扫目录的恢复。
//	③ **清单不得腐化**：每行的 `file` 与 `symbol` 必须仍存在；form 为 `legacy`/`scope` 的行还必须
//	   能在源码里找到对应注册字面量（`recovery.Legacy{…Kind: "<kind>"` / `recovery.Registration{…`）。
//
// 扫描口径 = **根 module**（下探遇到自带 go.mod 的嵌套 module 即停）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// maxLegacyPending 是 R37 的棘轮基线：尚未接入协调器的既有自恢复实现数量**只减不增**。
// 收紧（接入协调器）后必须**同步下调**本常量（与 TSV 一起改）。
const maxLegacyPending = 0

// orchestratorFile 是**唯一**允许构造恢复协调器的装配文件。
const orchestratorFile = "pkg/server/recovery_wiring.go"

var recoveryFormWhitelist = map[string]bool{
	"orchestration":  true, // 编排入口（协调器构造）
	"legacy":         true, // 已接入协调器（legacy 兜底）
	"scope":          true, // 已接入协调器（WAL 作用域恢复）
	"legacy-pending": true, // 尚未接入（棘轮约束）
}

// TestRecoveryGate_R37 R37 三重校验。
func TestRecoveryGate_R37(t *testing.T) {
	t.Parallel()
	rows := readTSVRows(t, "recovery_handlers.tsv", "|")
	// 跳过表头注释（# 开头）与空行。
	type row struct{ file, symbol, form, kind, note string }
	var parsed []row
	seen := map[string]bool{}
	for _, r := range rows {
		if len(r) < 4 {
			continue
		}
		f := strings.TrimSpace(r[0])
		if f == "" || strings.HasPrefix(f, "#") {
			continue
		}
		rw := row{file: f, symbol: strings.TrimSpace(r[1]), form: strings.TrimSpace(r[2]),
			kind: strings.TrimSpace(r[3])}
		if len(r) > 4 {
			rw.note = strings.TrimSpace(r[4])
		}
		if !recoveryFormWhitelist[rw.form] {
			t.Fatalf("R37: form 非法 %q（file=%s）；允许 = orchestration/legacy/scope/legacy-pending", rw.form, rw.file)
		}
		key := rw.file + "|" + rw.symbol
		if seen[key] {
			t.Fatalf("R37: 重复登记 %s", key)
		}
		seen[key] = true
		parsed = append(parsed, rw)
	}
	if len(parsed) == 0 {
		t.Fatal("R37: recovery_handlers.tsv 必须至少登记编排入口（不得为空表 ⇒ 假绿）")
	}

	// ③ 防腐化：file/symbol 必须仍在源码中；legacy/scope 行必须有注册字面量。
	pending := 0
	orchestrators := map[string]bool{}
	for _, rw := range parsed {
		src, ok := readRepoFile(t, rw.file)
		if !ok {
			t.Fatalf("R37: 登记的文件不存在 %s", rw.file)
		}
		if rw.symbol != "" && rw.symbol != "-" && !strings.Contains(src, rw.symbol) {
			t.Fatalf("R37: 登记的符号 %q 在 %s 中已不存在（清单腐化，需同步更新）", rw.symbol, rw.file)
		}
		switch rw.form {
		case "orchestration":
			orchestrators[rw.file] = true
			if rw.file != orchestratorFile {
				t.Fatalf("R37: 编排入口只允许 %s，登记了 %s", orchestratorFile, rw.file)
			}
			if !strings.Contains(src, "recovery.New(") || !strings.Contains(src, "c.Run(") {
				t.Fatalf("R37: %s 应同时包含 recovery.New( 与 c.Run(（否则清单与实现不符）", rw.file)
			}
		case "legacy", "scope":
			if rw.kind == "" || rw.kind == "-" {
				t.Fatalf("R37: %s（%s）必须登记 kind", rw.symbol, rw.form)
			}
			lit := `recovery.Legacy{`
			if rw.form == "scope" {
				lit = `recovery.Registration{`
			}
			if !strings.Contains(src, lit) || !strings.Contains(src, `"`+rw.kind+`"`) {
				t.Fatalf("R37: %s 声明 form=%s（kind=%s），但源码里找不到 %s 与 %q",
					rw.symbol, rw.form, rw.kind, lit, rw.kind)
			}
		case "legacy-pending":
			pending++
		}
	}

	// ② 棘轮：legacy-pending 不得增加。
	if pending > maxLegacyPending {
		t.Fatalf("R37: legacy-pending 增加（%d > 基线 %d）⇒ 新增长任务的恢复必须走协调器注册，不得再写自恢复",
			pending, maxLegacyPending)
	}

	// ① 唯一编排入口：非测试源码中构造协调器的文件必须（且只能）是登记为 orchestration 的那一个。
	for _, f := range waitqGoFiles(t, moduleRoot(t)) {
		src, ok := readRepoFile(t, f)
		if !ok || !strings.Contains(src, "recovery.New(") {
			continue
		}
		if !orchestrators[f] {
			if f == orchestratorFile {
				t.Fatalf("R37: %s 构造了恢复协调器，但**未在 recovery_handlers.tsv 登记为 orchestration**（清单必须与实现一致）", f)
			}
			t.Fatalf("R37: %s 自行构造恢复协调器（recovery.New）—— 恢复编排只允许在 %s（子系统必须注册任务族，不得自行编排）",
				f, orchestratorFile)
		}
	}
}

// readRepoFile 读仓库内相对路径文件（失败返回 false）。
func readRepoFile(t *testing.T, rel string) (string, bool) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(moduleRoot(t), filepath.FromSlash(rel)))
	if err != nil {
		return "", false
	}
	return string(b), true
}
