// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// audit_readface_gate_test.go 是门禁 **R35-e**：审计**读面**必须
//
//	① 有管理员门（`auditReadAllowed`）；
//	② 服务端强制过滤（非管理员**不得**通过请求参数放宽 —— 当前语义是「非管理员一律 403」，
//	   比 owner 过滤更强；将来开放用户维度视图时必须在同一裁决函数内收紧）；
//	③ `export` 与 `list` **同一门、同一读管线**（不得出现绕过门的第二条读路径）。
//
// 设计 docs/designs/2026-10-19-audit-service.md §5.7 + M1-11（revC P0：读面曾无 ACL，
// `reader` 可读全量）。此前只有行为用例、没有机械门禁（评审 R13 列为「R35-e 门禁缺失但注释宣称」）。
//
// 本门禁是**结构性**的：断言注册面（两个 mux 四个注册）+ 裁决点唯一 + 读管线唯一，
// 因此「新增一个审计导出端点却忘记加门」「给裁决函数加 `?owner=` 放宽」都会红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"testing"
)

// auditReadHandlerNames 是审计读面 handler（新增读端点必须加入本表，否则门禁覆盖不到）。
var auditReadHandlerNames = []string{"auditHandler", "auditExportHandler", "auditColdSegHandler"}

// auditReadPipelineHandlers 是**返回条目**的读 handler（必须走统一读管线 readAuditEvents）；
// 冷段区间读返回原始字节流，不经过条目管线，故不在本表（但仍必须过管理员门）。
var auditReadPipelineHandlers = []string{"auditHandler", "auditExportHandler"}

// auditReadPatterns 是必须同时存在于两个 mux 的读面 pattern。
var auditReadPatterns = []string{"GET /api/audit", "GET /api/audit/export"}

// parseHandlersFuncs 解析 pkg/server 非测试源码里的 `func (h *Handlers) name(...)`。
func parseHandlersFuncs(t *testing.T, root string) map[string]*ast.FuncDecl {
	t.Helper()
	fset := token.NewFileSet()
	out := map[string]*ast.FuncDecl{}
	for _, p := range goSourceFiles(t, filepath.Join(root, "pkg", "server"), false) {
		af, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("解析 %s: %v", p, err)
		}
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || fd.Name == nil {
				continue
			}
			out[fd.Name.Name] = fd
		}
	}
	if len(out) == 0 {
		t.Fatal("未解析到任何 (*Handlers) 方法 —— 解析器失效（门禁假绿）")
	}
	return out
}

// calledNames 返回函数体内被调用的方法/函数名（`h.x(...)` 与 `x(...)` 两种形态）。
func calledNames(fd *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := call.Fun.(type) {
		case *ast.Ident:
			out[fn.Name] = true
		case *ast.SelectorExpr:
			out[fn.Sel.Name] = true
		}
		return true
	})
	return out
}

// usesSelector 判定函数体是否访问了 `recv.attr`（如 `r.URL`）。
func usesSelector(fd *ast.FuncDecl, attr string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel != nil && sel.Sel.Name == attr {
			found = true
		}
		return true
	})
	return found
}

// containsStringLit 判定函数体内是否出现指定字符串字面量。
func containsStringLit(fd *ast.FuncDecl, lit string) bool {
	found := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if b, ok := n.(*ast.BasicLit); ok && b.Kind == token.STRING && b.Value == `"`+lit+`"` {
			found = true
		}
		return true
	})
	return found
}

// TestR35e_AuditReadFaceIsGated 断言审计读面：双 mux 注册 + 单一裁决点 + 单一读管线。
func TestR35e_AuditReadFaceIsGated(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// ① 两个 pattern 必须同时注册在 localMux 与 srvMux（CLI 隧道面 + 主面各一条）。
	classified := classifiedRoutes(t)
	var missing []string
	for _, mux := range []string{"localMux", "srvMux"} {
		for _, pat := range auditReadPatterns {
			if _, ok := classified[mux+"\t"+pat]; !ok {
				missing = append(missing, mux+" "+pat)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("R35-e：审计读面路由缺注册（须同时存在于 localMux 与 srvMux）: %v", missing)
	}

	funcs := parseHandlersFuncs(t, root)

	// ② 每个读 handler 都必须经 `auditReadAllowed` 裁决，且走同一读管线 `readAuditEvents`。
	var noGate, noPipeline []string
	for _, name := range auditReadHandlerNames {
		fd, ok := funcs[name]
		if !ok {
			t.Fatalf("R35-e：未找到读面 handler %s（更名为 %s？请同步本门禁）", name, name)
		}
		calls := calledNames(fd)
		if !calls["auditReadAllowed"] {
			noGate = append(noGate, name)
		}
	}
	if len(noGate) > 0 {
		t.Fatalf("R35-e：读面 handler 未经 auditReadAllowed 裁决（读面必须管理员门）: %v", noGate)
	}
	for _, name := range auditReadPipelineHandlers {
		if !calledNames(funcs[name])["readAuditEvents"] {
			noPipeline = append(noPipeline, name)
		}
	}
	if len(noPipeline) > 0 {
		t.Fatalf("R35-e：读面 handler 未走统一读管线 readAuditEvents（出现第二条读路径）: %v", noPipeline)
	}

	// ③ 裁决函数内部：必须是「主体角色 == admin / isAuditAdmin」语义，且**不得**读请求参数
	// （防止将来用 `?owner=`/`?all=` 之类放宽为非管理员全量视图）。
	gate, ok := funcs["auditReadAllowed"]
	if !ok {
		t.Fatal("R35-e：未找到裁决函数 auditReadAllowed")
	}
	if usesSelector(gate, "URL") {
		t.Fatal("R35-e：裁决函数不得读 URL 参数（请求参数不得放宽读权限）")
	}
	if !containsStringLit(gate, "admin") && !calledNames(gate)["isAuditAdmin"] {
		t.Fatal("R35-e：裁决函数未见管理员语义（`Role == \"admin\"` 或 isAuditAdmin）")
	}
	// 主体自身角色优先（否则「无 AK 的管理员主体」会被误判为非管理员）。
	if !calledNames(gate)["PrincipalFrom"] {
		t.Fatal("R35-e：裁决函数应以主体自身角色为准（PrincipalFrom），再回退按 AK 查环")
	}
}
