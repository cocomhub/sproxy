// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// quota_direct_release_gate_test.go 是门禁 **R44**：**直接调用账本原语必须登记理由**。
//
// 背景（评审 R14 C3）：`commitUp`/`releaseUp`/`releaseCommittedUp` 是**不加锁**的账本原语；
// 任何「绕过 `withWaitLock` 的直调」都可能**释放额度却不广播** ⇒ 等待者睡满超时才 507。
// R34 只检查「登记过的入口函数体内是否出现 withWaitLock」（函数级），挡不住**新增的直调点**
// （例如有人图省事在写路径里直接 `pool.releaseUp(...)`）。
//
// 本门禁做**调用点级**双向登记：每个直调点必须登记（含理由），登记项也必须仍存在（表不腐化）。
// 因此新增直调点必然红；删掉直调点却没删登记也红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// quotaPrimitives 是「不加锁的账本原语」方法名。
var quotaPrimitives = map[string]bool{
	"commitUp":           true,
	"releaseUp":          true,
	"releaseCommittedUp": true,
}

// quotaDirectRow 是登记表一行：file \t func \t callee \t reason。
type quotaDirectRow struct{ File, Func, Callee string }

// scanQuotaDirectCalls 返回 pkg/quota 非测试源码里对原语的直调点（file, 外层函数, 被调方法）。
func scanQuotaDirectCalls(t *testing.T, root string) map[quotaDirectRow]string {
	t.Helper()
	fset := token.NewFileSet()
	out := map[quotaDirectRow]string{} // row → file:line
	dir := filepath.Join(root, "pkg", "quota")
	for _, p := range goSourceFiles(t, dir, false) {
		af, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("解析 %s: %v", p, err)
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := fd.Name.Name
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, okCall := n.(*ast.CallExpr)
				if !okCall {
					return true
				}
				sel, okSel := call.Fun.(*ast.SelectorExpr)
				if !okSel || !quotaPrimitives[sel.Sel.Name] {
					return true
				}
				out[quotaDirectRow{File: rel, Func: fn, Callee: sel.Sel.Name}] = ""
				return true
			})
		}
	}
	return out
}

// readQuotaDirectTable 读取登记表（file \t func \t callee \t reason）。
func readQuotaDirectTable(t *testing.T, root string) map[quotaDirectRow]string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "internal", "archcheck", "quota_direct_release.tsv"))
	if err != nil {
		t.Fatalf("读取 quota_direct_release.tsv: %v", err)
	}
	out := map[quotaDirectRow]string{}
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 4 {
			t.Fatalf("登记表行格式非法（需 <file>\\t<func>\\t<callee>\\t<理由>）: %q", line)
		}
		row := quotaDirectRow{File: parts[0], Func: parts[1], Callee: parts[2]}
		if _, dup := out[row]; dup {
			t.Fatalf("重复登记: %q", line)
		}
		out[row] = parts[3]
	}
	if len(out) == 0 {
		t.Fatal("登记表为空（门禁失效）")
	}
	return out
}

// TestR44_QuotaDirectReleasesAreRegistered 断言直调点与登记表严格一一对应。
func TestR44_QuotaDirectReleasesAreRegistered(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	calls := scanQuotaDirectCalls(t, root)
	table := readQuotaDirectTable(t, root)
	if len(calls) == 0 {
		t.Fatal("未扫描到任何账本原语直调点 —— 解析器失效（门禁假绿）")
	}

	var unregistered, stale []string
	for row := range calls {
		if _, ok := table[row]; !ok {
			unregistered = append(unregistered, row.File+" "+row.Func+"() → "+row.Callee)
		}
	}
	for row, reason := range table {
		if _, ok := calls[row]; !ok {
			stale = append(stale, row.File+" "+row.Func+"() → "+row.Callee+"（理由："+reason+"）")
		}
	}
	sort.Strings(unregistered)
	sort.Strings(stale)

	if len(unregistered)+len(stale) > 0 {
		msg := "R44 违规：\n"
		if len(unregistered) > 0 {
			msg += "  未登记的直调点（账本原语不加锁：必须说明为何不丢唤醒）:\n    " +
				strings.Join(unregistered, "\n    ") + "\n"
		}
		if len(stale) > 0 {
			msg += "  登记表中已不存在的直调点（表腐化）:\n    " + strings.Join(stale, "\n    ") + "\n"
		}
		msg += "登记位置: internal/archcheck/quota_direct_release.tsv"
		t.Fatal(msg)
	}
}
