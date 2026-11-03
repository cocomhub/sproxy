// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// docs_api_gate_test.go 是门禁 **R55**：**API 文档与路由表一致**。
//
// 背景（2026-11-02 实测）：`docs/api.md` 是 API 的权威文档，但当时只覆盖 122 条路由中的 64 条
// （59 条无文档），且文档里还有 1 条已不存在的路径（`/` vs `/{$}`）。文档漂移不会让任何测试
// 变红——只能靠人记得改，实际结果就是越拖越多。
//
// 判据（路径级，方法不参与——同一路径的多方法通常在同一节说明）：
//   - `route_classes.tsv` 的每条路由路径必须在 `docs/api.md` 有 `### METHOD /path` 标题
//     （查询串不参与比较：文档标题常带 `?subdir=...` 示例）；
//   - 未文档化路径必须**逐条**登记在 `docs_api_undocumented.tsv`（**驱动清单**：补文档后
//     必须同步删除该行；新增未文档化路径必须显式登记并写理由）；
//   - 文档里有、路由表里没有 ⇒ 红（防止文档腐化）。
//
// 这两侧的集合必须**精确相等**（不是「≤」）：既防退化，也逼着补文档时把清单消掉。
// **全部文档化后清单文件应删除**（缺失 = 空清单，是合法终态）；防「删文件把门禁静音」
// 由两侧解析结果的下限护栏承担（见 minR55RoutePaths / minR55DocPaths）。

import (
	"bufio"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// apiDocHeadingRe 匹配 `### GET /api/xxx` 形式的端点标题（路径后可带查询示例）。
var apiDocHeadingRe = regexp.MustCompile(`(?m)^###+ (?:GET|POST|PUT|DELETE|HEAD|PATCH) (/[^\s]*)`)

// normalizeRoutePath 归一化路径：去查询串；Go 1.22 的根匹配 `/{$}` 记为 `/`。
func normalizeRoutePath(p string) string {
	if i := strings.IndexByte(p, '?'); i >= 0 {
		p = p[:i]
	}
	if p == "/{$}" {
		return "/"
	}
	return p
}

// routePathsFromTable 从 route_classes.tsv 取全部路由路径（去掉方法列）。
func routePathsFromTable(t *testing.T) map[string]bool {
	t.Helper()
	f, err := os.Open("route_classes.tsv")
	if err != nil {
		t.Fatalf("读取 route_classes.tsv: %v", err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 2 {
			continue
		}
		pattern := parts[1]
		if i := strings.IndexByte(pattern, ' '); i >= 0 { // 去方法前缀
			pattern = pattern[i+1:]
		}
		out[normalizeRoutePath(pattern)] = true
	}
	return out
}

// documentedPaths 从 docs/api.md 取已文档化的路径集合。
func documentedPaths(t *testing.T) map[string]bool {
	t.Helper()
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "docs", "api.md"))
	if err != nil {
		t.Fatalf("读取 docs/api.md: %v", err)
	}
	out := map[string]bool{}
	for _, m := range apiDocHeadingRe.FindAllStringSubmatch(string(data), -1) {
		out[normalizeRoutePath(m[1])] = true
	}
	return out
}

// readUndocumentedBaseline 读取驱动清单。
//
// 空清单是**有语义的状态**（没有未文档化路由）：文件不存在或有效条目为 0 均视为空集合，
// 不是「门禁失效」——全部文档化后本文件本就应删除（终态）。防「删文件静音门禁」由
// TestR55 的路由/文档标题下限护栏承担（两侧解析同时失效时判据会恒真，必须显式拦住）。
func readUndocumentedBaseline(t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	f, err := os.Open("docs_api_undocumented.tsv")
	if errors.Is(err, fs.ErrNotExist) {
		return out // 终态：全部已文档化，清单文件已删除
	}
	if err != nil {
		t.Fatalf("读取 docs_api_undocumented.tsv: %v", err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		out[normalizeRoutePath(strings.TrimSpace(parts[0]))] = true
	}
	return out
}

// minR55RoutePaths / minR55DocPaths 是两侧解析结果的下限护栏（当前实际 ~122 条，留余量）。
// 防假绿：解析器或输入文件失效 ⇒ 两侧都为空 ⇒ 「集合精确相等」恒真，门禁静默失效。
const (
	minR55RoutePaths = 100
	minR55DocPaths   = 100
)

// TestR55_APIDocsCoverRoutes 断言 docs/api.md 与路由表路径集合一致（差异必须逐条登记）。
func TestR55_APIDocsCoverRoutes(t *testing.T) {
	t.Parallel()
	routes := routePathsFromTable(t)
	docs := documentedPaths(t)
	if len(routes) < minR55RoutePaths {
		t.Fatalf("R55：解析到的路由过少（%d < %d），route_classes.tsv 或解析器失效", len(routes), minR55RoutePaths)
	}
	if len(docs) < minR55DocPaths {
		t.Fatalf("R55：解析到的文档标题过少（%d < %d），docs/api.md 或解析器失效", len(docs), minR55DocPaths)
	}
	baseline := readUndocumentedBaseline(t)

	undocumented := map[string]bool{}
	for p := range routes {
		if !docs[p] {
			undocumented[p] = true
		}
	}

	// 1) 文档里有、路由里没有 ⇒ 文档腐化（必须改文档）。
	var stale []string
	for p := range docs {
		if !routes[p] {
			stale = append(stale, p)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Fatalf("R55：docs/api.md 记录了不存在的路由（文档腐化，请改文档）：\n  %s", strings.Join(stale, "\n  "))
	}

	// 2) 未文档化集合必须与清单**精确相等**。
	var missingDoc, gotDocumented []string
	for p := range undocumented {
		if !baseline[p] {
			missingDoc = append(missingDoc, p)
		}
	}
	for p := range baseline {
		if !undocumented[p] {
			gotDocumented = append(gotDocumented, p)
		}
	}
	sort.Strings(missingDoc)
	sort.Strings(gotDocumented)
	if len(missingDoc) > 0 {
		t.Errorf("R55：以下路由既无文档也未登记（新增端点必须写文档；确有困难才登记 docs_api_undocumented.tsv 并写理由）：\n  %s",
			strings.Join(missingDoc, "\n  "))
	}
	if len(gotDocumented) > 0 {
		t.Errorf("R55：以下条目已补文档，请从 docs_api_undocumented.tsv 删除（清单只减不增）：\n  %s",
			strings.Join(gotDocumented, "\n  "))
	}
}
