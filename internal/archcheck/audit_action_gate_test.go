// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// R39：**审计动作常量必须有非测试使用点**。
//
// 背景（真实缺陷，2026-10-20）：`ActionAIEmbed = "ai.embed"` 在 `pkg/server/ai_insight.go` 定义后
// **全仓零使用**——既没有埋点，也没有任何功能路径会产生该动作。表现是：`docs/testing/audit-scenarios.md`
// 的覆盖率统计里永远挂着一个「永不出现的动作」，看起来像缺口，实际是**死常量** ✗。
// 「动作常量存在」≠「有埋点」≠「有功能」，三者必须分别核实。
//
// 本门禁（两级判据）：
//  1. `pkg/server` 非测试源码里形如 `Action*` / `auditAction*` 且值为动作名的常量，必须在非测试
//     源码中**被引用至少一次**（定义行本身不算）；
//  2. **引用它的那个函数本身必须可达**（一层判据）：该函数名必须出现在「其它非测试文件」中，
//     或在本文件内出现超过 1 次（声明 + 至少一处使用）。否则形如「导出包装函数 `NewEmbedClient`
//     引用了常量，但全仓无人调用该包装」的情况会让门禁**假绿**（评审 R13：`ai.embed` 就是这样）。
// 允许例外（登记到 `audit_action_allowlist.tsv` 并写明理由），且**清单不得腐化**：
// 已登记但实际已有引用的条目必须删除，避免清单变成「免检名单」。

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

// auditActionNameRe 匹配审计动作常量名（`ActionAIEmbed` / `auditActionCredAKAdd` …）。
var auditActionNameRe = regexp.MustCompile(`^(Action[A-Za-z0-9]*|auditAction[A-Za-z0-9]*)$`)

// auditActionValueRe 匹配「像审计动作的值」（`ai.embed` / `volume_move` / `credential_ak_add`）。
// 用于排除 `resultActionError` 这类同样含 "Action" 但语义无关的常量。
var auditActionValueRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)?$`)

// auditActionRef 是一个动作常量的定义与引用计数。
type auditActionRef struct {
	Name  string
	Value string
	File  string
	Refs  int
	Where []string
	// UnreachableRefs 记录「引用点所在的函数本身无调用方」的次数（假绿来源，评审 R13）。
	UnreachableRefs int
}

// TestAuditActionConstantsAreReferenced R39：动作常量必须在非测试源码中被使用。
func TestAuditActionConstantsAreReferenced(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	dir := filepath.Join(root, "pkg", "server")

	files := auditGateGoFiles(t, dir)
	if len(files) == 0 {
		t.Fatalf("pkg/server 下未找到非测试源码（路径错误？%s）", dir)
	}

	fset := token.NewFileSet()
	parsed := make(map[string]*ast.File, len(files))
	for _, f := range files {
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("解析 %s: %v", f, err)
		}
		parsed[f] = af
	}

	// ① 收集候选常量（名 + 值 + 定义文件）。
	var refs []auditActionRef
	defLines := make(map[string]token.Pos)
	for path, af := range parsed {
		rel := auditGateRel(root, path)
		for _, decl := range af.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !auditActionNameRe.MatchString(name.Name) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val := strings.Trim(lit.Value, `"`)
					if !auditActionValueRe.MatchString(val) || !strings.Contains(val, "_") && !strings.Contains(val, ".") {
						continue // `Action` 之类的非动作值（如常量组里的零值）跳过
					}
					refs = append(refs, auditActionRef{Name: name.Name, Value: val, File: rel})
					defLines[name.Name] = name.Pos()
				}
			}
		}
	}
	if len(refs) == 0 {
		t.Fatal("未识别到任何审计动作常量（解析口径失效？）")
	}

	// ①b 收集每个函数的名称/区间，并统计「标识符按文件出现次数」（用于一层可达判据）。
	type funcSpan struct {
		name       string
		start, end token.Pos
		file       string
	}
	var spans []funcSpan
	identCount := make(map[string]map[string]int) // name -> file -> count
	for path, af := range parsed {
		rel := auditGateRel(root, path)
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name == nil {
				continue
			}
			spans = append(spans, funcSpan{name: fd.Name.Name, start: fd.Pos(), end: fd.End(), file: rel})
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok {
				if identCount[id.Name] == nil {
					identCount[id.Name] = map[string]int{}
				}
				identCount[id.Name][rel]++
			}
			return true
		})
	}
	// 该函数是否**可达**（一层）：名字出现在其它非测试文件，或本文件内出现 >1 次。
	reachable := func(name, declFile string) bool {
		for f, n := range identCount[name] {
			if f != declFile && n > 0 {
				return true
			}
		}
		return identCount[name][declFile] > 1
	}
	enclosingFunc := func(pos token.Pos) (funcSpan, bool) {
		for _, sp := range spans {
			if pos >= sp.start && pos <= sp.end {
				return sp, true
			}
		}
		return funcSpan{}, false
	}

	// ② 统计非测试源码中的引用（排除定义处）。
	//    注意：必须按下标取地址（`for _, r := range` 是拷贝 ⇒ 累加会丢失，本门禁第一版就踩到 ✗）。
	for i := range refs {
		r := &refs[i]
		for path, af := range parsed {
			rel := auditGateRel(root, path)
			ast.Inspect(af, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok || id.Name != r.Name {
					return true
				}
				if defLines[r.Name] == id.Pos() {
					return true
				}
				r.Refs++
				if len(r.Where) < 3 && !containsStr(r.Where, rel) {
					r.Where = append(r.Where, rel)
				}
				if sp, ok := enclosingFunc(id.Pos()); ok && !reachable(sp.name, sp.file) {
					r.UnreachableRefs++
				}
				return true
			})
		}
	}

	allow := readAuditActionAllowlist(t, root)

	// ③ 断言：零引用必须在 allowlist；allowlist 条目必须有实际引用（防腐化）。
	var problems []string
	for _, r := range refs {
		_, allowed := allow[r.Name]
		switch {
		case r.Refs == 0 && !allowed:
			problems = append(problems, "死常量（无任何非测试使用点）: "+r.Name+" = "+r.Value+" @ "+r.File)
		case r.Refs > 0 && r.UnreachableRefs == r.Refs && !allowed:
			problems = append(problems, "假绿（只被『无调用方的函数』引用，实际永不产生）: "+
				r.Name+" = "+r.Value+" @ "+r.File)
		case r.Refs > 0 && r.UnreachableRefs < r.Refs && allowed:
			problems = append(problems, "allowlist 已失效（该常量已有可达引用，应删除登记）: "+r.Name)
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("R39 违规（%d 条）:\n  %s\n\n提示：动作常量必须有非测试使用点；确属预留请登记到\n"+
			"internal/archcheck/audit_action_allowlist.tsv（name|理由），并在删除该动作时同步删除登记。",
			len(problems), strings.Join(problems, "\n  "))
	}
}

// auditGateGoFiles 返回 dir 下**非测试** Go 文件（递归；跳过子模块，与仓库门禁口径一致）。
func auditGateGoFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir {
				if _, serr := os.Stat(filepath.Join(path, "go.mod")); serr == nil {
					return filepath.SkipDir // 子模块不归根 module 管
				}
			}
			return nil
		}
		if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", dir, err)
	}
	sort.Strings(out)
	return out
}

// auditGateRel 返回相对 root 的斜杠路径（门禁用统一形态）。
func auditGateRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(rel)
}

// readAuditActionAllowlist 读取 `audit_action_allowlist.tsv`（name|reason），返回 name 集合。
//
// 文件不存在 ⇒ 空清单（最严格）；格式与仓库其它 TSV 一致（`|` 分隔，`#` 注释）。
func readAuditActionAllowlist(t *testing.T, root string) map[string]string {
	t.Helper()
	path := filepath.Join(root, "internal", "archcheck", "audit_action_allowlist.tsv")
	out := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("读取 %s: %v", path, err)
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "name|") {
			continue
		}
		parts := strings.SplitN(line, "|", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[1]) == "" {
			t.Fatalf("audit_action_allowlist.tsv 行格式错误（需 name|reason）: %q", line)
		}
		out[strings.TrimSpace(parts[0])] = strings.TrimSpace(parts[1])
	}
	return out
}
