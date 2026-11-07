// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// mutating_route_audit_gate_test.go 是门禁 **R35-b**：**变更型路由 → 业务语义审计**。
//
// 设计 docs/designs/2026-10-19-audit-service.md §2.5 / R35-b 要求：每个变更型路由
// （POST/PUT/PATCH/DELETE）都必须留下**业务语义**审计（action/object/result）；
// 传输层兜底事件（`http.*`，`fidelity=transport`）**不计入**达标。
//
// 本门禁此前**只有注释宣称、没有实现**（评审 R13 列为「R35-b/R35-e 门禁缺失但注释宣称」）。
// 现在以 `internal/archcheck/mutating_route_audit.tsv` 为唯一机器可读事实源，做三条 join：
//
//	① 路由表（R38 的 route_classes.tsv）中的变更型 pattern ↔ 本表**双向**一一对应
//	   （新增变更型路由不登记 ⇒ 红；删路由不删表 ⇒ 红）；
//	② `action=` 的值必须在**非测试源码**中真实存在（字符串字面量，或同名 `Action*` 常量被引用）
//	   ⇒ 堵住「登记了一个谁也不会发出的动作名」（拼写错误/意造）；
//	③ 该 action 必须出现在某个 `_test.go` 的**测试函数体内**（场景证据）
//	   ⇒ 堵住「登记了但从不发出」；也避免「随便一个测试文件里提一句常量」就转绿（那是假绿）。
//
// `pending` 是显式 backlog（尚未接业务审计/尚无场景证据），由 `pending_budget` **棘轮**约束
// 「只减不增」：新增变更型路由不得登记为 pending，必须直接给 `action=`。

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// mutatingMethods 是「变更型」方法集合（只读方法不要求业务语义审计）。
var mutatingMethods = map[string]bool{
	"POST": true, "PUT": true, "PATCH": true, "DELETE": true,
}

// routeIsMutating 判定 R38 表里的 `<METHOD> <pattern>` 是否为变更型。
func routeIsMutating(pattern string) bool {
	m, _, ok := strings.Cut(pattern, " ")
	return ok && mutatingMethods[m]
}

// mutatingAuditRow 是本表一行。
type mutatingAuditRow struct {
	Pattern string
	Action  string // 空 = pending
	Note    string
}

// readMutatingRouteAudit 解析登记表，返回 (rows, pendingBudget)。
func readMutatingRouteAudit(t *testing.T, root string) (map[string]mutatingAuditRow, int) {
	t.Helper()
	f, err := os.Open(filepath.Join(root, "internal", "archcheck", "mutating_route_audit.tsv"))
	if err != nil {
		t.Fatalf("读取 mutating_route_audit.tsv: %v", err)
	}
	defer f.Close()

	rows := map[string]mutatingAuditRow{}
	budget := -1
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#") {
			if v, ok := strings.CutPrefix(line, "# pending_budget="); ok {
				n, cerr := strconv.Atoi(strings.TrimSpace(v))
				if cerr != nil {
					t.Fatalf("pending_budget 非法: %q", line)
				}
				budget = n
			}
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			t.Fatalf("mutating_route_audit.tsv 行格式非法（需 <pattern>\\t<action=|pending>\\t<备注>）: %q", line)
		}
		pat, cell := parts[0], parts[1]
		note := ""
		if len(parts) > 2 {
			note = parts[2]
		}
		if _, dup := rows[pat]; dup {
			t.Fatalf("mutating_route_audit.tsv 重复条目: %q", pat)
		}
		row := mutatingAuditRow{Pattern: pat, Note: note}
		if cell != "pending" {
			a, ok := strings.CutPrefix(cell, "action=")
			if !ok || strings.TrimSpace(a) == "" {
				t.Fatalf("mutating_route_audit.tsv 单元值非法（需 action=<值> 或 pending）: %q", line)
			}
			row.Action = strings.TrimSpace(a)
		}
		rows[pat] = row
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("读取 mutating_route_audit.tsv: %v", err)
	}
	if budget < 0 {
		t.Fatal("mutating_route_audit.tsv 缺少 `# pending_budget=<n>` 棘轮基线（防 backlog 无声增长）")
	}
	if len(rows) == 0 {
		t.Fatal("登记表为空 —— 解析器或文件失效（门禁假绿）")
	}
	return rows, budget
}

// goSourceFiles 返回 dir 下（递归）满足 pred 的 .go 文件路径。
func goSourceFiles(t *testing.T, dir string, includeTests bool) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if !includeTests && strings.HasSuffix(p, "_test.go") {
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// actionConsts 收集 `Action* = "<value>"` 常量（value → 常量名；同名多值时取首个）。
func actionConsts(t *testing.T, root string) map[string]string {
	t.Helper()
	re := regexp.MustCompile(`\b(Action[A-Za-z0-9_]*)\s+(?:Action|string)\s*=\s*"([^"]+)"`)
	out := map[string]string{}
	for _, dir := range []string{filepath.Join(root, "pkg", "server"), filepath.Join(root, "pkg", "audit"), filepath.Join(root, "pkg", "files")} {
		for _, p := range goSourceFiles(t, dir, false) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读 %s: %v", p, err)
			}
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				if _, ok := out[m[2]]; !ok {
					out[m[2]] = m[1]
				}
			}
		}
	}
	return out
}

// actionEvidence 扫描**非测试**源码与**测试函数体**，返回 (生产存在, 测试函数体内存在)。
//
// 测试侧用 AST 判定「出现在某个 Test* 函数体内」——文件级 grep 会把「随便提一句」也当证据
// （那正是评审指出的假绿形态）。
func actionEvidence(t *testing.T, root string, action string, consts map[string]string) (prod, inTest bool) {
	t.Helper()
	lit := `"` + action + `"`
	constName := consts[action]

	// 生产侧：字面量存在，或同名常量被引用（常量定义行本身不算引用）。
	for _, dir := range []string{"pkg/server", "pkg/audit", "pkg/files"} {
		for _, p := range goSourceFiles(t, filepath.Join(root, dir), false) {
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("读 %s: %v", p, err)
			}
			s := string(b)
			if strings.Contains(s, lit) {
				prod = true
			} else if constName != "" {
				cnt := strings.Count(s, constName)
				defined := strings.Count(s, constName+` Action = `) + strings.Count(s, constName+` = `)
				if cnt > defined {
					prod = true
				}
			}
			if prod {
				break
			}
		}
		if prod {
			break
		}
	}
	if !prod {
		return false, false
	}

	fset := token.NewFileSet()
	for _, dir := range []string{"pkg/server", "pkg/audit", "pkg/files"} {
		for _, p := range goSourceFiles(t, filepath.Join(root, dir), true) {
			if !strings.HasSuffix(p, "_test.go") {
				continue
			}
			af, perr := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
			if perr != nil {
				t.Fatalf("解析 %s: %v", p, perr)
			}
			for _, decl := range af.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok || fd.Body == nil || !strings.HasPrefix(fd.Name.Name, "Test") {
					continue
				}
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if inTest {
						return false
					}
					switch v := n.(type) {
					case *ast.BasicLit:
						if v.Kind == token.STRING && v.Value == lit {
							inTest = true
						}
					case *ast.Ident:
						if constName != "" && v.Name == constName {
							inTest = true
						}
					}
					return true
				})
				if inTest {
					return prod, true
				}
			}
		}
	}
	return prod, false
}

// TestR35b_MutatingRoutesHaveBusinessAudit 断言变更型路由与业务审计登记表严格对应。
func TestR35b_MutatingRoutesHaveBusinessAudit(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// ① 路由表 → 变更型 pattern 集合（按 pattern 去重：同 pattern 同 handler ⇒ 同动作）。
	mutating := map[string]struct{}{}
	for key := range classifiedRoutes(t) {
		_, pattern, ok := strings.Cut(key, "\t")
		if !ok {
			t.Fatalf("route_classes.tsv key 非法: %q", key)
		}
		if routeIsMutating(pattern) {
			mutating[pattern] = struct{}{}
		}
	}
	if len(mutating) == 0 {
		t.Fatal("未解析到任何变更型路由 —— 解析器失效（门禁假绿）")
	}

	rows, budget := readMutatingRouteAudit(t, root)

	// ② 双向一一对应。
	var unregistered, stale []string
	for p := range mutating {
		if _, ok := rows[p]; !ok {
			unregistered = append(unregistered, p)
		}
	}
	for p := range rows {
		if _, ok := mutating[p]; !ok {
			stale = append(stale, p)
		}
	}
	sort.Strings(unregistered)
	sort.Strings(stale)

	// ③ action 证据 + pending 备注/棘轮。
	consts := actionConsts(t, root)
	var badAction, noProd, noTest, noNote []string
	pending := 0
	for _, row := range rows {
		if row.Action == "" {
			pending++
			if strings.TrimSpace(row.Note) == "" {
				noNote = append(noNote, row.Pattern)
			}
			continue
		}
		prod, inTest := actionEvidence(t, root, row.Action, consts)
		if !prod {
			noProd = append(noProd, row.Pattern+" -> "+row.Action)
			continue
		}
		if !inTest {
			noTest = append(noTest, row.Pattern+" -> "+row.Action)
			continue
		}
		if strings.HasPrefix(row.Action, "http.") {
			badAction = append(badAction, row.Pattern+" -> "+row.Action)
		}
	}
	sort.Strings(noProd)
	sort.Strings(noTest)
	sort.Strings(noNote)
	sort.Strings(badAction)

	var problems []string
	if len(unregistered) > 0 {
		problems = append(problems, "变更型路由未登记业务审计（deny-by-default）:\n    "+strings.Join(unregistered, "\n    "))
	}
	if len(stale) > 0 {
		problems = append(problems, "登记表含已不存在/非变更型的路由（表腐化）:\n    "+strings.Join(stale, "\n    "))
	}
	if len(badAction) > 0 {
		problems = append(problems, "传输层兜底动作（http.*）不计入业务审计达标:\n    "+strings.Join(badAction, "\n    "))
	}
	if len(noProd) > 0 {
		problems = append(problems, "动作名在非测试源码中不存在（拼写/意造）:\n    "+strings.Join(noProd, "\n    "))
	}
	if len(noTest) > 0 {
		problems = append(problems, "动作缺少场景证据（未出现在任何 Test* 函数体内）:\n    "+strings.Join(noTest, "\n    "))
	}
	if len(noNote) > 0 {
		problems = append(problems, "pending 行必须写备注（说明为何尚未达标）:\n    "+strings.Join(noNote, "\n    "))
	}
	if pending > budget {
		problems = append(problems, "pending 超预算（棘轮只减不增）：pending="+strconv.Itoa(pending)+" budget="+strconv.Itoa(budget))
	}
	if len(problems) > 0 {
		t.Fatalf("R35-b 违规（%d 条）:\n\n%s\n\n登记位置: internal/archcheck/mutating_route_audit.tsv\n"+
			"新增变更型路由必须登记 `action=<业务动作>`（且该动作需有场景用例）；确属未接入的既有路由登记 pending 并写备注。",
			len(problems), strings.Join(problems, "\n\n"))
	}
}
