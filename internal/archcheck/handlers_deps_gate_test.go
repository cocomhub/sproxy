// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// handlers_deps_gate_test.go 是门禁 **R7**：`Handlers` 必须是**分域依赖的组合**，
// 不得回退为扁平字段（server 治理 S4 的结构棘轮）。
//
// 背景：`Handlers` 曾是 100+ 扁平字段的 god object——任何新特性都往里加字段，
// 导致装配层与所有领域强耦合、无法按切片拆分。S4 把它收敛为 12 个**值嵌入**的
// 分域 deps（core/http/auth/files/volume/cloud/share/hub/cluster/audit/ai/ops），
// 每个分组就是未来切片的 Deps。
//
// 判据：
//  1. `type Handlers struct` 的字段数 ≤ handlersFieldBudget（当前 12）；
//  2. 每个字段必须是**嵌入**（无字段名）且类型名以 `Deps` 结尾；
//  3. 新增字段必须加到 deps.go 的某个分组，而不是 Handlers 本体。
//
// 为什么用 AST 而不是文本：文本扫描会把注释里的 `x *T` 当成字段（假红/假绿）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// handlersFieldBudget 是 Handlers 直接字段数的上限（当前 12 个分组，只减不增）。
const handlersFieldBudget = 12

// TestR7_HandlersIsComposedOfDeps 断言 Handlers 只由分域 deps 嵌入组成。
func TestR7_HandlersIsComposedOfDeps(t *testing.T) {
	t.Parallel()
	path := filepath.Join(moduleRoot(t), "pkg", "server", "handlers.go")
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("解析 %s: %v", path, err)
	}

	var st *ast.StructType
	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Handlers" {
			return true
		}
		st, _ = ts.Type.(*ast.StructType)
		return false
	})
	if st == nil {
		t.Fatal("handlers.go 中找不到 type Handlers struct（门禁判据失效）")
	}

	fields := st.Fields.List
	if len(fields) == 0 {
		t.Fatal("Handlers 无字段——解析失效或结构被清空（门禁假绿）")
	}
	if len(fields) > handlersFieldBudget {
		t.Errorf("R7：Handlers 有 %d 个字段，超过预算 %d。新依赖必须加到 deps.go 的某个分域分组，"+
			"不要直接加到 Handlers 本体（god object 回归）", len(fields), handlersFieldBudget)
	}
	for _, fl := range fields {
		if len(fl.Names) != 0 {
			t.Errorf("R7：Handlers 出现具名字段 %q（第 %d 行）——必须是**嵌入**的分域 deps，"+
				"否则该字段无法随切片搬走", fl.Names[0].Name, fset.Position(fl.Pos()).Line)
			continue
		}
		typ := ""
		switch x := fl.Type.(type) {
		case *ast.Ident:
			typ = x.Name
		case *ast.StarExpr:
			if id, ok := x.X.(*ast.Ident); ok {
				typ = id.Name
			}
		}
		if !strings.HasSuffix(typ, "Deps") {
			t.Errorf("R7：Handlers 嵌入类型 %q 不以 Deps 结尾（第 %d 行）——分域依赖统一命名 *Deps",
				typ, fset.Position(fl.Pos()).Line)
		}
	}
}
