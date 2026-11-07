// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// root_mutation_gate_test.go 是「`storage.Root` 结构性变异必须发事件」门禁（R45）。
//
// 为什么需要：本地卷的派生状态（搜索索引、卷状态缓存代际）依赖 Root 发变异事件
// （设计 2026-10-28-volume-state-maintenance §4.1）。Root 一旦新增/改写结构性变异方法却
// 忘记 `notifyMutation`，机制会**静默失效**——写成功、视图不更新（#874 事故族的形态）。
// 本门禁把「机制完整性」变成编译期之外的第二道保险。
//
// 判据：`pkg/storage` 非测试源码中，`*Root` 的**结构性变异方法**（名匹配
// `^(AtomicRename|Chtimes|Chmod|Create|Delete|Link|Mkdir|MkdirAll|Remove|RemoveAll|Rename|
// Symlink|Truncate|Write)`）函数体必须出现 `notifyMutation(`，或在 `rootMutationDelegates`
// 中显式登记「委托给已发事件的方法」。
//
// **边界（有意排除）**：`OpenFile`/`OpenFileEncrypted` 的文件内容写入不发结构事件——它们返回
// `*os.File`/`io.WriteCloser`（30+ 调用点依赖具体类型），包装会破坏类型契约；内容写的派生
// 状态由写内核（`pkg/files`）在写完后自行维护，落位仍经 `AtomicRename`（发 rename 事件）。
//
// 变异：删掉 `Root.Remove` 里的 `rt.notifyMutation(...)` → 本门禁红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// rootStructuralMutation 匹配结构性变异方法名（新增此类方法必须发事件）。
var rootStructuralMutation = regexp.MustCompile(
	`^(AtomicRename|Chtimes|Chmod|Create|Delete|Link|Mkdir|MkdirAll|Remove|RemoveAll|Rename|Symlink|Truncate|Write)`)

// rootMutationDelegates 登记「本身不发事件、但委托给已发事件方法」的方法（必须写理由）。
var rootMutationDelegates = map[string]string{
	"AtomicRename": "以重试循环复用 Rename（Rename 成功后已发 MutationRename）",
}

// TestRootStructuralMutationsNotify 钉住 Root 全部结构性变异方法都在成功后发事件。
func TestRootStructuralMutationsNotify(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(moduleRoot(t), "pkg", "storage")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 pkg/storage: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if perr != nil {
			t.Fatalf("解析 %s: %v", name, perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Recv == nil || fn.Name == nil || !fn.Name.IsExported() {
				continue
			}
			if !isRootReceiver(fn.Recv) || !rootStructuralMutation.MatchString(fn.Name.Name) {
				continue
			}
			checked++
			if reason, ok := rootMutationDelegates[fn.Name.Name]; ok && reason != "" {
				continue
			}
			// notifyMutationKind 是 notifyMutation 的「显式 IsDir」变体（设计 2026-10-30 D1），
			// 两者都发放事件 ⇒ 任一出现即满足「机制生效」。
			if !funcBodyContainsCall(fset, fn, "notifyMutation") && !funcBodyContainsCall(fset, fn, "notifyMutationKind") {
				t.Errorf("%s:%s 是结构性变异方法但未调用 notifyMutation/notifyMutationKind（R45：机制会静默失效）；"+
					"若委托给已发事件的方法，请在 rootMutationDelegates 登记理由",
					name, fn.Name.Name)
			}
		}
	}
	// 覆盖探针：扫描面被改窄（例如 receiver 判定失效）时立刻红。
	if checked < 6 {
		t.Fatalf("只检查到 %d 个 Root 结构性变异方法（期望 ≥6）——扫描面被改窄", checked)
	}
}

// isRootReceiver 报告接收者是否为 `*Root` 或 `Root`。
func isRootReceiver(recv *ast.FieldList) bool {
	if recv == nil || len(recv.List) == 0 {
		return false
	}
	switch typ := recv.List[0].Type.(type) {
	case *ast.StarExpr:
		id, ok := typ.X.(*ast.Ident)
		return ok && id.Name == "Root"
	case *ast.Ident:
		return typ.Name == "Root"
	}
	return false
}

// funcBodyContainsCall 报告函数体内是否出现指定函数名的调用（形如 `x.notifyMutation(...)` 或
// `notifyMutation(...)`）。
func funcBodyContainsCall(fset *token.FileSet, fn *ast.FuncDecl, name string) bool {
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fun := call.Fun.(type) {
		case *ast.Ident:
			if fun.Name == name {
				found = true
			}
		case *ast.SelectorExpr:
			if fun.Sel != nil && fun.Sel.Name == name {
				found = true
			}
		}
		return !found
	})
	_ = fset
	return found
}
