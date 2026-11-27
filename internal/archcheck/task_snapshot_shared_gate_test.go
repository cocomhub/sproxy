// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// task_snapshot_shared_gate_test.go 是「**含可变引用字段的任务快照必须深拷贝**」的泛化门禁（R62-b）。
//
// R62 只覆盖 `pkg/cloud` 的 `*CloudTask`。同族缺陷在 `pkg/syncmgr` 复现（本轮修）：
// `CreateTask` 去重命中时 `c := *t` 后把 `&c` **直接返回给调用方**，而 `SyncTask.Results`
// 会被 `backfillRetryResults` 在**写锁内原地改写**（`stored.Results[i].Action = ...`）⇒ 共享
// 底层数组；`Carriers` map 同样在执行结束回填。
//
// 判据（泛化、不依赖具体类型名）：
//   `pkg/cloud` 与 `pkg/syncmgr` 的非测试源码里，返回值含**指针型任务结构**的函数，
//   函数体不得只用 `x := *y` 后 `return &x`（浅拷贝）——必须调用本包的深拷贝助手
//   （`deepCopy*` / `snapshot*` 前缀，或显式 `append([]T(nil), ...)` / `copyCarriers(` 形态）。
//
// 为什么允许「显式逐字段复制」：有些出口在浅拷贝后手工复制了全部引用字段（合法且明确）。
// 判据因此是「浅拷贝后**没有任何**深拷贝动作」才算违规——这正是历史缺陷的形态。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// snapshotScanDirs 是本门禁覆盖的包（相对 module root）。
var snapshotScanDirs = []string{"pkg/cloud", "pkg/syncmgr"}

// deepCopyCalls 是「深拷贝动作」的识别函数名（前缀/精确）。
var deepCopyCalls = map[string]bool{
	"deepCopyTask":     true,
	"snapshotTask":     true,
	"deepCopySyncTask": true,
	"copyCarriers":     true,
	"CloneTask":        true,
	"cloneMetaExtra":   true,
}

// TestR62b_TaskSnapshotsDeepCopied 断言任务快照出口不裸浅拷贝。
func TestR62b_TaskSnapshotsDeepCopied(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	totalFiles, checked, ok := 0, 0, 0
	var violations []string

	for _, dir := range snapshotScanDirs {
		abs := filepath.Join(root, dir)
		entries, err := os.ReadDir(abs)
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			totalFiles++
			path := filepath.Join(abs, name)
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				t.Fatalf("解析 %s 失败: %v", name, perr)
			}
			for _, decl := range file.Decls {
				fn, isFn := decl.(*ast.FuncDecl)
				if !isFn || fn.Body == nil || !returnsTaskPtr(fn) {
					continue
				}
				checked++
				// 构造函数返回的是**新对象**（非既有任务的快照），不属本判据。
				if strings.HasPrefix(fn.Name.Name, "New") || strings.HasPrefix(fn.Name.Name, "new") {
					ok++
					continue
				}
				// 深拷贝助手自身是「值拷贝 + 逐字段复制」的实现体 ⇒ 合法起点。
				if strings.HasPrefix(fn.Name.Name, "deepCopy") || strings.HasPrefix(fn.Name.Name, "snapshot") ||
					strings.HasPrefix(fn.Name.Name, "clone") {
					ok++
					continue
				}
				if shallowOnly(fn.Body) {
					pos := fset.Position(fn.Pos())
					violations = append(violations, relPos(root, pos, fn.Name.Name))
				} else {
					ok++
				}
			}
		}
	}

	if totalFiles < 20 {
		t.Fatalf("只扫描到 %d 个源码文件（下限 20）⇒ 扫描面被改窄", totalFiles)
	}
	if checked < 10 {
		t.Fatalf("只判定到 %d 个「返回任务指针」的函数（下限 10）⇒ 判定失效", checked)
	}
	if ok < 8 {
		t.Fatalf("只有 %d 个函数通过判定（下限 8）⇒ 判据可能失效", ok)
	}
	if len(violations) > 0 {
		t.Fatalf("以下任务快照出口是**裸浅拷贝**（结构体含被原地改写的 Results/Carriers/Chunks，"+
			"快照逃逸后即读到半更新条目或 DATA RACE）：\n  %s\n"+
			"处置：改用本包 deepCopy* 助手（同 pkg/cloud deepCopyTask / pkg/syncmgr deepCopySyncTask）。",
			strings.Join(violations, "\n  "))
	}
}

// returnsTaskPtr 判断返回值是否为「指针型结构体/其切片」（*X 或 []*X）。
func returnsTaskPtr(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if _, isStar := r.Type.(*ast.StarExpr); isStar {
			return true
		}
		if arr, isArr := r.Type.(*ast.ArrayType); isArr && arr != nil {
			if _, isStar := arr.Elt.(*ast.StarExpr); isStar {
				return true
			}
		}
	}
	return false
}

// shallowOnly 判断函数体是否「只做了值拷贝、没有任何**针对该快照字段**的深拷贝动作」。
//
// 判据必须精确到「字段级」：早期版本把**任意** append/copy 调用当成深拷贝信号 ⇒ 大函数里
// 随便一个 `append(xs, y)` 就会让真实的浅拷贝放行（实测漏报 `CreateTask` 的变异）。
// 现在只认两类信号：
//   - 调用深拷贝助手（`deepCopy*` / `snapshot*` / `clone*`）；
//   - **字段赋值 + 容器复制**：`X.Field = append([]T(nil), ...)` / `X.Field = copyCarriers(...)`。
func shallowOnly(body *ast.BlockStmt) bool {
	shallow, deep := false, false
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for i, rhs := range v.Rhs {
				if star, isStar := rhs.(*ast.StarExpr); isStar {
					if _, isIdent := star.X.(*ast.Ident); isIdent {
						shallow = true
					}
				}
				// 字段级深拷贝：LHS 是 X.Field，RHS 是容器复制调用。
				if i < len(v.Lhs) && isSelectorOnIdent(v.Lhs[i]) && callsContainerCopy(rhs) {
					deep = true
				}
				// 字段级深拷贝：X.Field = []T(nil) 之类不常见，忽略（用 append 形态覆盖）。
			}
		case *ast.CallExpr:
			if id, isIdent := v.Fun.(*ast.Ident); isIdent {
				name := id.Name
				if deepCopyCalls[name] || strings.HasPrefix(name, "deepCopy") ||
					strings.HasPrefix(name, "snapshot") || strings.HasPrefix(name, "clone") {
					deep = true
				}
			}
		}
		return true
	})
	return shallow && !deep
}

// isSelectorOnIdent 判断表达式是否为 `x.Field`（接收者是标识符，不含调用链）。
func isSelectorOnIdent(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	_, isIdent := sel.X.(*ast.Ident)
	return isIdent
}

// callsContainerCopy 判断表达式是否为容器复制（append([]T(nil), ...) / copy* / clone*）。
func callsContainerCopy(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	id, ok := call.Fun.(*ast.Ident)
	if !ok {
		return false
	}
	if strings.HasPrefix(id.Name, "copy") || strings.HasPrefix(id.Name, "clone") ||
		deepCopyCalls[id.Name] || strings.HasPrefix(id.Name, "deepCopy") {
		return true
	}
	if id.Name == "append" && len(call.Args) > 0 {
		// append([]T(nil), src...) 的形态：第一个实参是「空切片字面量/零值转换」。
		_, isCall := call.Args[0].(*ast.CallExpr)
		_, isComp := call.Args[0].(*ast.CompositeLit)
		return isCall || isComp
	}
	return false
}

// relPos 把位置格式化为 `pkg/x/y.go:行 函数名`。
func relPos(root string, pos token.Position, fn string) string {
	p := strings.ReplaceAll(pos.String(), "\\", "/")
	if i := strings.Index(p, root); i >= 0 {
		p = strings.TrimPrefix(p[i+len(root):], "/")
	}
	return p + " " + fn
}
