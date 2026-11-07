// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// deps_field_cap_test.go 是门禁 **R52**：`pkg/server/<cap>` 切片的 `Deps` 字段数上限。
//
// 背景：server 治理把能力拆成 `pkg/server/<cap>` 包，依赖经构造时的 `Deps` 结构体注入
// （装配层构造、切片不反向依赖装配根，见 R6）。`Deps` 的字段数就是切片的**入边宽度**：
// 字段越多，切片与装配层耦合越深，测试替身越难构造，也说明它大概率把多个能力塞进了
// 同一个包。`Handlers` 的 100+ 扁平字段（R7 已收敛为 12 个分域 deps）就是反面教材。
//
// 判据：直接子包 `pkg/server/*/` 中声明的 `type Deps struct`，字段数（含嵌入字段）
// 不得超过 depsFieldCap。超过即红——先追问「这些依赖是否属于同一个能力」，优先拆包/
// 收敛注入点；确属不可拆时再显式上调上限并在 PR 说明理由（上调必须是一次显式决策）。
//
// 用 go/ast 而非文本扫描：注释里的 `X *T`、字段注释、结构体字面量都会骗过正则，
// 且 AST 能给出精确行号。
//
// 已知规模（2026-10-30 实测）：dav=9、s3=10、verify=8，最大 10；上限 12 留有 2 个
// 字段余量，同时低于 `Handlers` 的 12 个分域 deps（切片不应急剧变重）。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// depsFieldCap 是切片 Deps 结构体的字段数上限（含嵌入字段）。
const depsFieldCap = 12

// minDepsStructs 是覆盖探针下限：实测有 3 个切片使用 Deps 注入（dav / s3 / verify）。
// 低于该值说明扫描面被改窄（目录遍历失效、结构体被改名），门禁会静默空转；
// 若确有切片删掉 Deps（能力迁出/改注入方式），请同步下调本值。
const minDepsStructs = 3

// depsFieldCounter 统计直接子包里的 Deps 结构体与最大字段数。
type depsFieldCounter struct {
	fset  *token.FileSet
	found int
}

// scanPackageDeps 解析单个包目录下的非测试源码，检查其中的 `type Deps struct`。
func (c *depsFieldCounter) scanPackageDeps(t *testing.T, pkgDir string) {
	t.Helper()
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("读取 %s: %v", pkgDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(pkgDir, name)
		f, perr := parser.ParseFile(c.fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			t.Fatalf("解析 %s: %v", path, perr)
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name.Name != "Deps" {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				c.countDeps(t, pkgDir, path, st, c.fset.Position(ts.Pos()).Line)
			}
		}
	}
}

// countDeps 记录一个 Deps 结构体并断言字段数在上限内。
func (c *depsFieldCounter) countDeps(t *testing.T, pkgDir, path string, st *ast.StructType, line int) {
	t.Helper()
	n := len(st.Fields.List)
	if n == 0 {
		t.Fatalf("R52：%s:%d 的 Deps 无字段——解析失效或结构被清空（门禁假绿）", path, line)
	}
	pkg := filepath.Base(pkgDir)
	c.found++
	if n > depsFieldCap {
		t.Errorf("R52：pkg/server/%s 的 Deps 有 %d 个字段 > 上限 %d（%s:%d）——"+
			"字段数就是切片入边宽度：先确认这些依赖是否属于同一能力，优先拆包或收敛注入点；"+
			"确属不可拆请显式上调 depsFieldCap 并在 PR 说明理由",
			pkg, n, depsFieldCap, path, line)
	}
}

// TestR52_DepsFieldCap 断言每个切片的 Deps 字段数不超过上限。
func TestR52_DepsFieldCap(t *testing.T) {
	t.Parallel()
	base := filepath.Join(moduleRoot(t), "pkg", "server")
	entries, err := os.ReadDir(base)
	if err != nil {
		t.Fatalf("读取 %s: %v", base, err)
	}
	c := &depsFieldCounter{fset: token.NewFileSet()}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "testdata" {
			continue
		}
		c.scanPackageDeps(t, filepath.Join(base, e.Name()))
	}
	// 覆盖探针：一个 Deps 都没找到（目录遍历失效/结构改名）时静态门禁会静默变绿。
	if c.found < minDepsStructs {
		t.Fatalf("R52：只找到 %d 个切片 Deps 结构体（期望 ≥%d）——"+
			"扫描面被改窄或 Deps 被改名；确属有意移除请同步下调 minDepsStructs",
			c.found, minDepsStructs)
	}
}
