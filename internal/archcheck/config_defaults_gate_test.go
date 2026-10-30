// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// config_defaults_gate_test.go 是「配置默认值安全」门禁（R43）。
//
// 规则（AGENTS.md 7c + 设计 2026-10-28-volume-state-maintenance §六 R43）：
// **新增配置字段的默认值必须安全**——bool 默认 false（需要「默认开启」时把语义反转成
// `disable_xxx`），string 默认空串。默认 true 的 bool 让「显式关闭」与「未配置」不可区分；
// 非空 string 默认让「用户没配」与「用户配了这个值」不可区分。
//
// 判据：`pkg/server/config/defaults.go` 的 `Default()` 内，结构体字面量里**字面量**为
// `true` 或**非空字符串**的字段即「特化默认值」，必须登记在 `config_defaults.tsv`
// （每条写理由）。登记表与实际情况必须**完全一致**（新增须登记、移除须删行）——与 R46 同一
// 棘轮语义，避免「登记了但没人清理」。
//
// 只统计可直接判定的字面量（`false`/`""`/数字/常量标识符不统计）：常量无法静态判定取值，
// 强行统计会产生误报。
//
// 变异：在 Default() 里新增一个 `Foo: true`（未登记）→ 门禁红。

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

// configDefaultsFile 是登记表文件名。
const configDefaultsFile = "config_defaults.tsv"

// flaggedDefault 是一条「特化默认值」记录（字段路径 + 值形态）。
type flaggedDefault struct {
	Path  string // 形如 `TLS.Enabled` / `RateLimit.Requests`；同层嵌套用点号连接
	Value string // `true` 或 `"字面量"`（便于人工核对）
}

// scanConfigDefaults 扫描 Default() 的结构体字面量，返回特化默认值集合（按路径排序）。
func scanConfigDefaults(t *testing.T) []flaggedDefault {
	t.Helper()
	path := filepath.Join(moduleRoot(t), "pkg", "server", "config", "defaults.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("解析 config/defaults.go: %v", err)
	}
	var fn *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name != nil && fd.Name.Name == "Default" {
			fn = fd
			break
		}
	}
	if fn == nil {
		t.Fatal("config/defaults.go 中找不到 Default()（门禁判据失效，须更新本门禁）")
	}
	var out []flaggedDefault
	var walk func(n ast.Node, prefix []string)
	walk = func(n ast.Node, prefix []string) {
		ast.Inspect(n, func(node ast.Node) bool {
			cl, ok := node.(*ast.CompositeLit)
			if !ok {
				return true
			}
			for _, elt := range cl.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, ok := kv.Key.(*ast.Ident)
				if !ok {
					continue
				}
				path := append(append([]string{}, prefix...), key.Name)
				if nested, isLit := kv.Value.(*ast.CompositeLit); isLit {
					walk(nested, path)
					continue
				}
				if f := flaggedLiteral(kv.Value); f != "" {
					out = append(out, flaggedDefault{Path: strings.Join(path, "."), Value: f})
				}
			}
			return false // 已处理本层（避免重复遍历）
		})
	}
	walk(fn.Body, nil)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// flaggedLiteral 报告该表达式是否为「特化默认值」字面量（true / 非空字符串）。
func flaggedLiteral(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		if v.Name == "true" {
			return "true"
		}
	case *ast.BasicLit:
		if v.Kind == token.STRING && strings.Trim(v.Value, `"`) != "" {
			return v.Value
		}
	}
	return ""
}

// loadConfigDefaults 读取登记表（path → reason）。
func loadConfigDefaults(t *testing.T) map[string]string {
	t.Helper()
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", configDefaultsFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s: %v（先跑 ARCHCHECK_WRITE_BASELINE=1 生成基线）", configDefaultsFile, err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 2 {
			t.Fatalf("%s 行格式应为 path<TAB>reason: %q", configDefaultsFile, line)
		}
		reason := strings.TrimSpace(strings.Join(cols[1:], "\t"))
		if reason == "" {
			t.Fatalf("%s 条目缺少 reason: %q", configDefaultsFile, line)
		}
		out[strings.TrimSpace(cols[0])] = reason
	}
	return out
}

// TestConfigDefaultsAreSafe 钉住「特化默认值」全部登记（R43：新增默认值必须安全或显式登记）。
func TestConfigDefaultsAreSafe(t *testing.T) {
	t.Parallel()
	found := scanConfigDefaults(t)
	if len(found) == 0 {
		t.Fatal("未扫描到任何特化默认值——判据失效（Default() 结构变了？）")
	}
	allowed := loadConfigDefaults(t)
	seen := make(map[string]bool, len(found))
	for _, f := range found {
		seen[f.Path] = true
		if _, ok := allowed[f.Path]; !ok {
			t.Errorf("配置字段 %s 的默认值是 %s（特化默认值）但未登记：请改为安全默认"+
				"（bool=false / string=\"\"；需要默认开启时反转语义为 disable_xxx），"+
				"或在 %s 登记理由", f.Path, f.Value, configDefaultsFile)
		}
	}
	for path := range allowed {
		if !seen[path] {
			t.Errorf("%s 已无特化默认值（或字段已重命名）⇒ 请从 %s 删除该行", path, configDefaultsFile)
		}
	}
}

// TestConfigDefaultsBaselineHelper 重写登记表（ARCHCHECK_WRITE_BASELINE=1 时）。
func TestConfigDefaultsBaselineHelper(t *testing.T) {
	if os.Getenv("ARCHCHECK_WRITE_BASELINE") != "1" {
		t.Skip("设置 ARCHCHECK_WRITE_BASELINE=1 才会重写 config_defaults.tsv 基线")
	}
	found := scanConfigDefaults(t)
	var b strings.Builder
	b.WriteString("# 配置特化默认值登记（R43：新增默认值必须安全或在此登记理由）\n")
	b.WriteString("# 判据：pkg/server/config/defaults.go 的 Default() 里字面量 true / 非空字符串的字段。\n")
	b.WriteString("# 格式：字段路径<TAB>理由\n")
	for _, f := range found {
		b.WriteString(f.Path + "\t历史默认值（" + f.Value + "）；新增字段请改用安全默认或反转语义\n")
	}
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", configDefaultsFile)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 %s: %v", configDefaultsFile, err)
	}
	t.Logf("已写入 %d 条特化默认值", len(found))
}
