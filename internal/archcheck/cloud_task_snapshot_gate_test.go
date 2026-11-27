// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// cloud_task_snapshot_gate_test.go 是「**云任务快照必须深拷贝**」的门禁（R62）。
//
// 背景（2026-11-27，同类缺陷已犯两次）：
//   - #1327/#1329 把 `SnapshotTask`/`SnapshotTasks` 改成 `deepCopyTask`，但 `ListTasks` 仍是
//     浅拷贝 `c := *t` ⇒ HTTP 解锁后序列化与锁内原地写（`recordChunkState` 改 `Chunks[i]`、
//     `recordKeptPartial` 写诊断）竞争；
//   - 本轮又在 `findByURL` 发现同样的浅拷贝（该快照被 `CreateTask` **直接返回给调用方**）。
//
// 共同点：`CloudTask` 含**被原地改写的**切片/指针（`Chunks` 底层数组、`*Diagnostics`、
// `Progress`、`MetaExtra` map），所以「值拷贝 + 返回指针」= 共享可变状态 = 逃逸后必竞态。
// 这类缺陷**不会红**（没有内容断言能发现，-race 也只在特定逃逸形态下命中），只能机械化。
//
// 判据：`pkg/cloud` 的非测试源码里，凡「返回 `*CloudTask` 的查询/快照函数」都不得用
// `c := *t` 浅拷贝返回；必须经 `deepCopyTask`（或 `snapshotTask` 这一别名）。
//
// 为什么用「函数级」判据而不是"扫所有 `c := *t`"：值拷贝本身合法（如 `cloneTransferSpec`
// 拷贝的是顶层字段无指针的 `TransferSpec`）。判据锚在**返回值类型**上才精确。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestR62_CloudTaskSnapshotsDeepCopied 断言返回 *CloudTask 的函数不浅拷贝。
func TestR62_CloudTaskSnapshotsDeepCopied(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(moduleRoot(t), "pkg", "cloud")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", dir, err)
	}

	scanned, checked, deep := 0, 0, 0
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		path := filepath.Join(dir, name)
		fset := token.NewFileSet()
		file, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s 失败: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || !returnsCloudTaskPtr(fn) {
				continue
			}
			checked++
			shallow, usesDeep := bodyShape(fn.Body)
			// `deepCopyTask` 自身就是「值拷贝 + 逐字段深拷贝」的实现体（浅拷贝是它的起点），
			// 是唯一的合法浅拷贝点；`snapshotTask` 是它的别名。
			if usesDeep || fn.Name.Name == "deepCopyTask" || fn.Name.Name == "snapshotTask" {
				deep++
				continue
			}
			if shallow {
				pos := fset.Position(fn.Pos())
				violations = append(violations, path[len(moduleRoot(t))+1:]+":"+
					strings.TrimPrefix(pos.String()[len(path):], ":")+" "+fn.Name.Name)
			}
		}
	}

	// 双侧护栏：扫描面/判定数低于下限 ⇒ 红（防目录被搬走或判定失效后空转）。
	if scanned < 10 {
		t.Fatalf("只扫描到 %d 个源码文件（下限 10）⇒ 扫描面被改窄，门禁失效", scanned)
	}
	if checked < 6 {
		t.Fatalf("只判定到 %d 个「返回 *CloudTask」的函数（下限 6）⇒ 判定失效", checked)
	}
	if deep < 3 {
		t.Fatalf("只有 %d 个函数走 deepCopyTask（下限 3）⇒ 判据可能失效", deep)
	}
	if len(violations) > 0 {
		t.Fatalf("以下返回 *CloudTask 的函数用**浅拷贝**返回（CloudTask 含被原地改写的切片/指针，"+
			"快照逃逸后即 DATA RACE）：\n  %s\n处置：改用 deepCopyTask(t)（同 SnapshotTask/ListTasks/findByURL）。",
			strings.Join(violations, "\n  "))
	}
}

// returnsCloudTaskPtr 判断函数返回值里是否含 *CloudTask（单值或切片）。
func returnsCloudTaskPtr(fn *ast.FuncDecl) bool {
	if fn.Type.Results == nil {
		return false
	}
	for _, r := range fn.Type.Results.List {
		if isCloudTaskPtr(r.Type) {
			return true
		}
		if arr, ok := r.Type.(*ast.ArrayType); ok && arr != nil && isCloudTaskPtr(arr.Elt) {
			return true
		}
	}
	return false
}

// isCloudTaskPtr 判断表达式是否为 *CloudTask（含带包名的写法）。
func isCloudTaskPtr(e ast.Expr) bool {
	star, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	if id, ok := star.X.(*ast.Ident); ok {
		return id.Name == "CloudTask"
	}
	if sel, ok := star.X.(*ast.SelectorExpr); ok {
		return sel.Sel != nil && sel.Sel.Name == "CloudTask"
	}
	return false
}

// bodyShape 返回（是否出现 `x := *y` 浅拷贝形态，是否调用了 deepCopyTask/snapshotTask）。
func bodyShape(body *ast.BlockStmt) (shallow, usesDeep bool) {
	ast.Inspect(body, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.AssignStmt:
			for _, rhs := range v.Rhs {
				if star, ok := rhs.(*ast.StarExpr); ok {
					if _, isIdent := star.X.(*ast.Ident); isIdent {
						shallow = true
					}
				}
			}
		case *ast.CallExpr:
			if id, ok := v.Fun.(*ast.Ident); ok && (id.Name == "deepCopyTask" || id.Name == "snapshotTask") {
				usesDeep = true
			}
		}
		return true
	})
	return shallow, usesDeep
}
