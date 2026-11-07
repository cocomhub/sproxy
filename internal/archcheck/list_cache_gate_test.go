// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// list_cache_gate_test.go 是「list 级缓存唯一提供方」门禁（R40）。
//
// 背景（设计 2026-10-28-volume-state-maintenance §六 R40）：派生状态（目录列表 / 文件索引）
// 在历史上被重复实现多套——`pkg/files.searchIndex`（本地卷索引）、`pkg/volumestate.Cache`
// （外部卷 list 缓存）、secretdata 自管索引、以及若干局部缓存。每多一套就多一个「忘记失效」
// 的面（#853/#874 事故族）。
//
// 裁定：**新增长期驻留的 list/index 级缓存类型必须复用 `pkg/volumestate`**；只有「卷自身是
// 唯一能算出真值的地方」（如 secretdata 的内容分块索引：值是密文）才允许自管，但必须在此
// 登记理由。
//
// 判据：非测试源码中，类型名以 `Cache`/`Index` 结尾的结构体即候选，必须登记在
// `list_cache_types.tsv`（file<TAB>Type<TAB>reason）。登记表与实际必须完全一致（棘轮语义：
// 新增须登记、删除须删行），避免「登记后无人清理」。
//
// 边界：本门禁扫**类型声明**而非行为——它防的是「悄悄再造一套」，不评判既有实现的正确性
// （既有实现由 R46 棘轮与各自的失效测试守住）。
//
// 变异：在任意包新增 `type fooIndex struct{}`（未登记）→ 门禁红。

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// listCacheTypesFile 是登记表文件名。
const listCacheTypesFile = "list_cache_types.tsv"

// listCacheTypeName 匹配候选类型名（Cache/Index 结尾）。
var listCacheTypeName = regexp.MustCompile(`(Cache|Index)$`)

// listCacheCandidate 是一条候选（文件 + 类型名）。
type listCacheCandidate struct {
	File string
	Type string
}

// scanListCacheTypes 扫描内部包的 Cache/Index 类型声明（非测试源码）。
func scanListCacheTypes(t *testing.T) []listCacheCandidate {
	t.Helper()
	root := moduleRoot(t)
	var out []listCacheCandidate
	for _, dir := range []string{"pkg", "internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil //nolint:nilerr // 遍历容错
			}
			if d.IsDir() {
				if name := d.Name(); name == "testdata" || name == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil //nolint:nilerr
			}
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/archcheck/") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				t.Fatalf("解析 %s: %v", rel, perr)
			}
			for _, decl := range f.Decls {
				gd, ok := decl.(*ast.GenDecl)
				if !ok || gd.Tok != token.TYPE {
					continue
				}
				for _, spec := range gd.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok || ts.Name == nil {
						continue
					}
					if _, isStruct := ts.Type.(*ast.StructType); !isStruct {
						continue
					}
					if listCacheTypeName.MatchString(ts.Name.Name) {
						out = append(out, listCacheCandidate{File: rel, Type: ts.Name.Name})
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s: %v", dir, err)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// loadListCacheTypes 读取登记表（"file\tType" → reason）。
func loadListCacheTypes(t *testing.T) map[string]string {
	t.Helper()
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", listCacheTypesFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s: %v（先跑 ARCHCHECK_WRITE_BASELINE=1 生成基线）", listCacheTypesFile, err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			t.Fatalf("%s 行格式应为 file<TAB>Type<TAB>reason: %q", listCacheTypesFile, line)
		}
		key := strings.TrimSpace(cols[0]) + "\t" + strings.TrimSpace(cols[1])
		reason := strings.TrimSpace(strings.Join(cols[2:], "\t"))
		if reason == "" {
			t.Fatalf("%s 条目缺少 reason: %q", listCacheTypesFile, line)
		}
		out[key] = reason
	}
	return out
}

// TestListCacheTypesRegistered 钉住「list/index 级缓存类型」全部登记（R40）。
func TestListCacheTypesRegistered(t *testing.T) {
	t.Parallel()
	found := scanListCacheTypes(t)
	if len(found) < 5 {
		t.Fatalf("只扫描到 %d 个候选类型（期望 ≥5）——扫描面被改窄", len(found))
	}
	allowed := loadListCacheTypes(t)
	seen := make(map[string]bool, len(found))
	for _, c := range found {
		key := c.File + "\t" + c.Type
		seen[key] = true
		if _, ok := allowed[key]; !ok {
			t.Errorf("%s 的 %s 是新的 list/index 级缓存类型但未登记（R40）：请复用 pkg/volumestate，"+
				"或在 %s 登记「为什么只能由该组件自管」", c.File, c.Type, listCacheTypesFile)
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("%s 已不存在 ⇒ 请从 %s 删除该行", strings.ReplaceAll(key, "\t", " "), listCacheTypesFile)
		}
	}
}

// TestListCacheTypesBaselineHelper 重写登记表（ARCHCHECK_WRITE_BASELINE=1 时）。
func TestListCacheTypesBaselineHelper(t *testing.T) {
	if os.Getenv("ARCHCHECK_WRITE_BASELINE") != "1" {
		t.Skip("设置 ARCHCHECK_WRITE_BASELINE=1 才会重写 list_cache_types.tsv 基线")
	}
	found := scanListCacheTypes(t)
	var b strings.Builder
	b.WriteString("# list/index 级缓存类型登记（R40：新增必须复用 pkg/volumestate 或在此说明理由）\n")
	b.WriteString("# 格式：file<TAB>Type<TAB>reason\n")
	for _, c := range found {
		b.WriteString(c.File + "\t" + c.Type + "\t既有实现；本次登记为基线（新增请优先复用 pkg/volumestate）\n")
	}
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", listCacheTypesFile)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 %s: %v", listCacheTypesFile, err)
	}
	t.Logf("已写入 %d 条候选类型", len(found))
}
