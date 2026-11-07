// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// task_lifecycle_gate_test.go 是门禁 **R43**：**云任务状态机的「重启恢复」必须覆盖全部非终态**。
//
// 背景（评审 R14 C1）：磁盘上的任务状态此前只有 `downloading` 会被重启续跑，`pending`
// 一律转 failed（批量 100 条里约 97 条受影响，1h 后连目录删除）—— 这类缺陷不会让编译或
// 既有测试失败，因为「新增/改写状态」与「恢复分支」之间没有任何机械约束。
//
// 本门禁把三者钉在一起：
//   - **代码写入的 status**（`task.Status = "x"` / `Status: "x"`）必须登记；
//   - 登记的 status 分为 terminal / non-terminal；
//   - **non-terminal 必须有非 keep 的恢复动作**（restart/fail），且该动作必须在恢复实现
//     （`manager_persist.go`）里出现该状态字面量 —— 否则「重启后原地不动」= 永久卡住。
//
// 新增状态而不更新表 ⇒ 红；把非终态标成 keep ⇒ 红；删恢复分支 ⇒ 红。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// taskStatusLiteralRe 只匹配**任务状态**写入（排除 `IntegrityStatus` 等同后缀字段）。
var taskStatusLiteralRe = regexp.MustCompile(`(?:^|[^A-Za-z])Status\s*(?:=|:)\s*"([a-z_]+)"`)

// taskStatusRow 是登记表一行。
type taskStatusRow struct {
	Status   string
	Kind     string // terminal | non-terminal
	Recovery string // restart | fail | keep
	Note     string
}

// readTaskStatusTable 读取 `task_status_recovery.tsv`。
func readTaskStatusTable(t *testing.T, root string) map[string]taskStatusRow {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "archcheck", "task_status_recovery.tsv"))
	if err != nil {
		t.Fatalf("读取 task_status_recovery.tsv: %v", err)
	}
	out := map[string]taskStatusRow{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			t.Fatalf("登记表行格式非法（需 <status>\\t<terminal|non-terminal>\\t<restart|fail|keep>\\t<备注>）: %q", line)
		}
		note := ""
		if len(parts) > 3 {
			note = parts[3]
		}
		row := taskStatusRow{Status: parts[0], Kind: parts[1], Recovery: parts[2], Note: note}
		switch row.Kind {
		case "terminal", "non-terminal":
		default:
			t.Fatalf("非法 kind %q（%s）", row.Kind, line)
		}
		switch row.Recovery {
		case "restart", "fail", "keep":
		default:
			t.Fatalf("非法 recovery %q（%s）", row.Recovery, line)
		}
		if _, dup := out[row.Status]; dup {
			t.Fatalf("重复状态: %q", row.Status)
		}
		out[row.Status] = row
	}
	if len(out) == 0 {
		t.Fatal("登记表为空（门禁失效）")
	}
	return out
}

// scanWrittenTaskStatuses 从 pkg/cloud 非测试源码提取**被写入**的任务状态字面量。
func scanWrittenTaskStatuses(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{} // status → file:line（首个出现处，便于修）
	dir := filepath.Join(root, "pkg", "cloud")
	for _, p := range goSourceFiles(t, dir, false) {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("读 %s: %v", p, err)
		}
		lines := strings.Split(string(b), "\n")
		for i, line := range lines {
			for _, m := range taskStatusLiteralRe.FindAllStringSubmatch(line, -1) {
				rel, _ := filepath.Rel(root, p)
				if _, ok := out[m[1]]; !ok {
					out[m[1]] = rel + ":" + itoaArch(i+1)
				}
			}
		}
	}
	return out
}

// itoaArch 是 archcheck 内部的小工具（避免为一行引入 strconv 依赖冲突）。
func itoaArch(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// restartDecisionExpr 提取 `restart := ...` 判定式（恢复是否续跑的唯一决策点）。
// 不用正则：避免转义噪声（此前 python 补丁把 \s 展开成真字符导致编译失败）。
func restartDecisionExpr(src string) string {
	for line := range strings.SplitSeq(src, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "restart :=") || strings.HasPrefix(t, "restart =") {
			return t
		}
	}
	return ""
}

// funcBodyByName 返回 `func ... name(` 的函数体（到下一个顶层 func 为止）。
func funcBodyByName(src, name string) (string, bool) {
	marker := ") " + name + "("
	idx := strings.Index(src, marker)
	if idx < 0 {
		marker = "func " + name + "("
		idx = strings.Index(src, marker)
	}
	if idx < 0 {
		return "", false
	}
	rest := src[idx:]
	if end := strings.Index(rest, "\nfunc "); end > 0 {
		return rest[:end], true
	}
	return rest, true
}

// TestR43_TaskStatusesCoverRecovery 断言状态写入面 ↔ 登记表 ↔ 恢复实现三者一致。
func TestR43_TaskStatusesCoverRecovery(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	table := readTaskStatusTable(t, root)
	written := scanWrittenTaskStatuses(t, root)
	if len(written) == 0 {
		t.Fatal("未在 pkg/cloud 解析到任何任务状态写入 —— 解析器失效（门禁假绿）")
	}

	// ① 双向：写入的必须登记；登记的必须仍被写入（表不腐化）。
	var unregistered, stale []string
	for s, loc := range written {
		if _, ok := table[s]; !ok {
			unregistered = append(unregistered, s+" ("+loc+")")
		}
	}
	for s := range table {
		if _, ok := written[s]; !ok {
			stale = append(stale, s)
		}
	}
	sort.Strings(unregistered)
	sort.Strings(stale)

	// ② 非终态必须有恢复动作，且恢复实现里出现该状态（覆盖到）。
	persistSrc, err := os.ReadFile(filepath.Join(root, "pkg", "cloud", "manager_persist.go"))
	if err != nil {
		t.Fatalf("读 manager_persist.go: %v", err)
	}
	// 只认**恢复决策处**的状态字面量：
	//   restart ⇒ 必须出现在 `restart := ...` 这条判定式里（否则「重启」是空话）；
	//   fail    ⇒ 必须出现在 activateRecoveredTasks 的处置分支里。
	// 此前只查「文件里出现过」⇒ 删掉 restart 条件仍绿（变异验证抓到）。
	restartExpr := restartDecisionExpr(string(persistSrc))
	activateBody, okAct := funcBodyByName(string(persistSrc), "activateRecoveredTasks")
	if !okAct {
		t.Fatal("未找到 activateRecoveredTasks（恢复实现被改名/删除？）")
	}
	var uncovered []string
	for s, row := range table {
		switch {
		case row.Kind == "non-terminal" && row.Recovery == "keep":
			uncovered = append(uncovered, s+"（非终态却 keep ⇒ 重启后永久卡住）")
		case row.Recovery == "restart" && !strings.Contains(restartExpr, `"`+s+`"`):
			uncovered = append(uncovered, s+"（recovery=restart 但 restart 判定式未包含该状态）")
		case row.Recovery == "fail" && !strings.Contains(activateBody, `"`+s+`"`):
			uncovered = append(uncovered, s+"（recovery=fail 但恢复处置分支未包含该状态）")
		}
		if strings.TrimSpace(row.Note) == "" {
			uncovered = append(uncovered, s+"（缺少备注）")
		}
	}
	sort.Strings(uncovered)

	if len(unregistered)+len(stale)+len(uncovered) > 0 {
		var b strings.Builder
		b.WriteString("R43 违规：\n")
		if len(unregistered) > 0 {
			b.WriteString("  未登记的写入状态（新增状态必须登记并给出恢复动作）:\n    " + strings.Join(unregistered, "\n    ") + "\n")
		}
		if len(stale) > 0 {
			b.WriteString("  登记表中已不再写入的状态（表腐化）: " + strings.Join(stale, ", ") + "\n")
		}
		if len(uncovered) > 0 {
			b.WriteString("  恢复覆盖缺口:\n    " + strings.Join(uncovered, "\n    ") + "\n")
		}
		b.WriteString("登记位置: internal/archcheck/task_status_recovery.tsv")
		t.Fatal(b.String())
	}
}
