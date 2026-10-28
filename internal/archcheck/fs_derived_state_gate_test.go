// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// fs_derived_state_gate_test.go 是「FS 实现自述派生状态」门禁（R42）。
//
// 背景（设计 2026-10-28-volume-state-maintenance §六 R42）：卷状态缓存（`pkg/volumestate`）在
// 命中前会探测代际（`StateGen(ctx, path) (uint64, error)`）——**跨进程/外部改动**的收敛依赖它。
// 若某个 `sync.FS` 实现持有进程内派生状态（自己的 list 缓存/索引）却不实现 `StateGen`，
// 缓存层就看不到它的变化 ⇒ 陈旧的视图会一直命中（#853/#874 事故族的另一种形态）。
//
// 判据：非测试源码中，**带 `ListDir(ctx, ...)` 方法的类型**（即 `sync.FS` 实现或装饰器）必须
// 满足其一：
//  1. 实现 `StateGen(ctx context.Context, path string) (uint64, error)`；
//  2. 登记在 `fs_derived_state.tsv`（file<TAB>Type<TAB>reason），理由必须说明「为什么它不需要
//     自述代际」（通常是：纯透传、列表即远端真值、或派生状态由更外层唯一提供方负责）。
//
// 与 R40 的关系：R40 管「有没有多造一套缓存」，R42 管「多造的（或继承的）那套能不能被探测」。
//
// 变异：新增一个只有 ListDir 的 FS 实现（未登记、无 StateGen）→ 门禁红。

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

// fsDerivedStateFile 是登记表文件名。
const fsDerivedStateFile = "fs_derived_state.tsv"

// fsImplCandidate 是一个「看起来像 sync.FS 实现」的类型。
type fsImplCandidate struct {
	File     string
	Type     string
	HasState bool // 是否实现了 StateGen
}

// scanFSImplementations 扫描带 ListDir 方法的类型（非测试源码）。
func scanFSImplementations(t *testing.T) []fsImplCandidate {
	t.Helper()
	root := moduleRoot(t)
	type key struct{ file, typ string }
	hasListDir := map[key]bool{}
	hasStateGen := map[key]bool{}
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
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 || fn.Name == nil {
					continue
				}
				typ := receiverTypeName(fn.Recv.List[0].Type)
				if typ == "" {
					continue
				}
				k := key{file: rel, typ: typ}
				switch fn.Name.Name {
				case "ListDir":
					hasListDir[k] = true
				case "StateGen":
					hasStateGen[k] = true
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("扫描 %s: %v", dir, err)
		}
	}
	out := make([]fsImplCandidate, 0, len(hasListDir))
	for k := range hasListDir {
		out = append(out, fsImplCandidate{File: k.file, Type: k.typ, HasState: hasStateGen[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Type < out[j].Type
	})
	return out
}

// receiverTypeName 取方法接收者类型名（去掉指针/泛型下标）。
func receiverTypeName(e ast.Expr) string {
	switch typ := e.(type) {
	case *ast.StarExpr:
		return receiverTypeName(typ.X)
	case *ast.Ident:
		return typ.Name
	case *ast.IndexExpr: // 泛型实例化
		return receiverTypeName(typ.X)
	case *ast.IndexListExpr:
		return receiverTypeName(typ.X)
	}
	return ""
}

// loadFSDerivedState 读取登记表（"file\tType" → reason）。
func loadFSDerivedState(t *testing.T) map[string]string {
	t.Helper()
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", fsDerivedStateFile)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("读取 %s: %v（先跑 ARCHCHECK_WRITE_BASELINE=1 生成基线）", fsDerivedStateFile, err)
	}
	out := map[string]string{}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) < 3 {
			t.Fatalf("%s 行格式应为 file<TAB>Type<TAB>reason: %q", fsDerivedStateFile, line)
		}
		reason := strings.TrimSpace(strings.Join(cols[2:], "\t"))
		if reason == "" {
			t.Fatalf("%s 条目缺少 reason: %q", fsDerivedStateFile, line)
		}
		out[strings.TrimSpace(cols[0])+"\t"+strings.TrimSpace(cols[1])] = reason
	}
	return out
}

// TestFSImplementationsDeclareDerivedState 钉住「FS 实现要么自述代际、要么登记理由」（R42）。
func TestFSImplementationsDeclareDerivedState(t *testing.T) {
	t.Parallel()
	found := scanFSImplementations(t)
	if len(found) < 15 {
		t.Fatalf("只扫描到 %d 个 FS 实现（期望 ≥15）——扫描面被改窄", len(found))
	}
	allowed := loadFSDerivedState(t)
	seen := make(map[string]bool, len(found))
	for _, c := range found {
		key := c.File + "\t" + c.Type
		seen[key] = true
		if c.HasState {
			if _, ok := allowed[key]; ok {
				t.Errorf("%s 的 %s 已实现 StateGen ⇒ 请从 %s 删除该行（登记仅用于「无代际」实现）",
					c.File, c.Type, fsDerivedStateFile)
			}
			continue
		}
		if _, ok := allowed[key]; !ok {
			t.Errorf("%s 的 %s 有 ListDir（sync.FS 实现）但没有 StateGen 也未登记（R42）："+
				"请实现 StateGen(ctx, path) (uint64, error)，或在 %s 说明为什么不需要自述代际",
				c.File, c.Type, fsDerivedStateFile)
		}
	}
	for key := range allowed {
		if !seen[key] {
			t.Errorf("%s 已不存在 ⇒ 请从 %s 删除该行", strings.ReplaceAll(key, "\t", " "), fsDerivedStateFile)
		}
	}
}

// TestFSDerivedStateBaselineHelper 重写登记表（ARCHCHECK_WRITE_BASELINE=1 时）。
func TestFSDerivedStateBaselineHelper(t *testing.T) {
	// sproxy:serial: 基线重写是手动一次性操作（ARCHCHECK_WRITE_BASELINE=1），会改写共享 tsv 文件
	if os.Getenv("ARCHCHECK_WRITE_BASELINE") != "1" {
		t.Skip("设置 ARCHCHECK_WRITE_BASELINE=1 才会重写 fs_derived_state.tsv 基线")
	}
	found := scanFSImplementations(t)
	var b strings.Builder
	b.WriteString("# FS 实现的派生状态自述登记（R42：无 StateGen 的实现必须说明理由）\n")
	b.WriteString("# 格式：file<TAB>Type<TAB>reason\n")
	for _, c := range found {
		if c.HasState {
			continue // 已实现 StateGen 的实现不需要登记
		}
		b.WriteString(c.File + "\t" + c.Type + "\t" + fsDerivedStateReason(c.File) + "\n")
	}
	p := filepath.Join(moduleRoot(t), "internal", "archcheck", fsDerivedStateFile)
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("写 %s: %v", fsDerivedStateFile, err)
	}
	t.Logf("已写入 %d 条 FS 实现", len(found))
}

// fsDerivedStateReason 按实现所在层给出「为什么不需要自述代际」的默认理由（基线用）。
// 新增实现请按实际语义改写，不要照抄。
func fsDerivedStateReason(file string) string {
	switch {
	case strings.HasPrefix(file, "pkg/sync/"):
		return "通用装饰器/本地实现：无进程内 list 派生状态（结构视图即底层真值）"
	case strings.HasPrefix(file, "pkg/rootfs/"):
		return "本地卷写面：结构事件由 storage.Root 广播、内容写由 MarkWritten 声明，不自管列表缓存"
	case strings.HasPrefix(file, "pkg/volume/secretdata/"):
		return "保密卷：内容是密文，列表由卷自管（内容缓存不参与用户可见列表）"
	case strings.HasPrefix(file, "pkg/volume/"):
		return "卷包装器/远端后端：列表即底层真值或纯透传，无进程内派生状态"
	case strings.HasPrefix(file, "pkg/server/"):
		return "装配层适配器：纯透传，派生状态由 pkg/files / pkg/volumestate 唯一提供方负责"
	default:
		return "纯透传适配器：无进程内 list 派生状态"
	}
}
