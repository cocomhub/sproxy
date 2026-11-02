// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// route_classification_gate_test.go 是门禁 **R38**：**路由分类表 + deny-by-default**。
//
// 设计 docs/designs/2026-10-19-audit-service.md §2.5（tunnel 鉴权 A 改良版）要求
// 「**单一路由分类表**，两个 mux 同源」：每个已注册路由都必须显式归类（public / auth /
// reader / user / node / admin / tunnel），未登记的新路由即红 —— 防止「顺手加一条无鉴权或
// 无角色门的路由」这类**静默扩大攻击面**的改动（历史上 `/api/audit` 就是这样漏掉角色门的，
// 见 M1-11 审核 P0）。
//
// 断言是**双向**的（严格一一对应）：
//   - 已注册但未登记 ⇒ 红（新增路由必须显式归类）；
//   - 已登记但未注册 ⇒ 红（删路由必须同步删表，避免表腐化成噪声）；
//   - 分类值必须在已知集合内 ⇒ 防拼写错误静默通过。

import (
	"bufio"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// routeRegistrationFiles 是路由注册所在文件（新增注册文件需加入本表）。
var routeRegistrationFiles = []string{
	"pkg/server/routes.go",
	"pkg/server/routes_setup.go",
	"pkg/server/routes_extra.go",
	"pkg/server/routes_hub.go",
	"pkg/server/routes_hub_extra.go",
}

// routeClasses 是允许的分类值（与 route_classes.tsv 表头注释同源）。
var routeClasses = map[string]bool{
	"public": true, "auth": true, "reader": true, "user": true,
	"node": true, "admin": true, "tunnel": true,
}

// routeRegRe 匹配 `<mux>.HandleFunc("METHOD /path", ...)`（允许跨行注册）。
var routeRegRe = regexp.MustCompile(`(\w+Mux)\.HandleFunc\(\s*"([A-Z]+ [^"]+)"`)

// parenBalanced 报告 s 中左右圆括号数量是否相等（跨行注册拼接的终止条件）。
func parenBalanced(s string) bool {
	return strings.Count(s, "(") == strings.Count(s, ")")
}

// registeredRoutes 解析路由注册文件，返回 (mux, pattern) 集合。
func registeredRoutes(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]string{}
	for _, rel := range routeRegistrationFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		f, err := os.Open(full)
		if err != nil {
			continue // 文件不存在 = 该文件无注册（允许列表冗余）
		}
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
		line := 0
		for sc.Scan() {
			line++
			text := sc.Text()
			if !strings.Contains(text, ".HandleFunc(") {
				continue
			}
			// 跨行注册：拼接到括号平衡（最多 5 行，足够覆盖现有形态）。
			for i := 0; i < 5 && !parenBalanced(text); i++ {
				if !sc.Scan() {
					break
				}
				line++
				text += " " + strings.TrimSpace(sc.Text())
			}
			m := routeRegRe.FindStringSubmatch(text)
			if m == nil {
				continue
			}
			out[m[1]+"\t"+m[2]] = rel + ":" + strconv.Itoa(line)
		}
		_ = f.Close()
	}
	return out
}

// classifiedRoutes 读取 route_classes.tsv，返回 (mux, pattern) → class。
func classifiedRoutes(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	f, err := os.Open(filepath.Join(root, "internal", "archcheck", "route_classes.tsv"))
	if err != nil {
		t.Fatalf("读取 route_classes.tsv: %v", err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) < 3 {
			t.Fatalf("route_classes.tsv 行格式非法（需 <mux>\\t<pattern>\\t<class>）: %q", line)
		}
		key := parts[0] + "\t" + parts[1]
		if _, dup := out[key]; dup {
			t.Fatalf("route_classes.tsv 重复条目: %q", line)
		}
		out[key] = parts[2]
	}
	return out
}

// routeTableFiles 是**数据化路由表**所在文件（`router.Route` 字面量表 + RoutePrefix 常量）。
// 子包把 URL 子路径声明成数据后，注册不再出现在 mux 调用上，因此必须从表里解析，
// 否则 R38 会把「已迁入子包的路由」当成已删除（假红）或漏检（假绿）。
var routeTableFiles = []string{
	"pkg/server/cloudtasks/routes.go",
}

// routeTablePrefixRe 匹配路由表文件里的组前缀常量字面量。
var routeTablePrefixRe = regexp.MustCompile(`RoutePrefix\s*=\s*"([^"]+)"`)

// routeTableEntryRe 匹配 `{Method: "GET", Path: "/x", Server: router.ClassAuth, Local: router.ClassTunnel, ...}`。
var routeTableEntryRe = regexp.MustCompile(`\{Method:\s*"([A-Z]+)",\s*Path:\s*"([^"]+)",\s*Server:\s*router\.(\w+),\s*Local:\s*router\.(\w+)`)

// tableRoutes 解析数据化路由表，返回 (mux, pattern) 集合（两端分类非 ClassNone 时各记一条）。
func tableRoutes(t *testing.T) map[string]string {
	t.Helper()
	root := moduleRoot(t)
	out := map[string]string{}
	for _, rel := range routeTableFiles {
		full := filepath.Join(root, filepath.FromSlash(rel))
		data, err := os.ReadFile(full)
		if err != nil {
			continue // 文件不存在 = 该表未启用（允许列表冗余）
		}
		text := string(data)
		pm := routeTablePrefixRe.FindStringSubmatch(text)
		if pm == nil {
			t.Fatalf("R38：%s 缺少 RoutePrefix 常量字面量（解析器无法推导完整路径）", rel)
		}
		prefix := pm[1]
		for _, m := range routeTableEntryRe.FindAllStringSubmatch(text, -1) {
			method, path, srvClass, localClass := m[1], m[2], m[3], m[4]
			pattern := method + " " + prefix + path
			if srvClass != "ClassNone" {
				out["srvMux\t"+pattern] = rel
			}
			if localClass != "ClassNone" {
				out["localMux\t"+pattern] = rel
			}
		}
	}
	return out
}

// TestR38_RouteClassificationIsExhaustive 断言注册路由与分类表**严格一一对应**。
func TestR38_RouteClassificationIsExhaustive(t *testing.T) {
	t.Parallel()
	registered := registeredRoutes(t)
	maps.Copy(registered, tableRoutes(t))
	classified := classifiedRoutes(t)

	if len(registered) == 0 {
		t.Fatal("未解析到任何路由注册 —— 解析器或注册文件列表失效（门禁假绿）")
	}

	// 1) 已注册但未归类 ⇒ 红（deny-by-default）。
	var missing []string
	for key, loc := range registered {
		if _, ok := classified[key]; !ok {
			missing = append(missing, key+"  ("+loc+")")
		}
	}
	if len(missing) > 0 {
		t.Fatalf("R38：以下路由未在 internal/archcheck/route_classes.tsv 归类（deny-by-default）：\n  %s\n"+
			"新增路由必须显式归类（public/auth/reader/user/node/admin/tunnel）并说明理由。",
			strings.Join(missing, "\n  "))
	}

	// 2) 已归类但未注册 ⇒ 红（表腐化）。
	var stale []string
	for key := range classified {
		if _, ok := registered[key]; !ok {
			stale = append(stale, key)
		}
	}
	if len(stale) > 0 {
		t.Fatalf("R38：route_classes.tsv 中以下条目已无对应注册（删除路由需同步删表）：\n  %s",
			strings.Join(stale, "\n  "))
	}

	// 3) 分类值合法（防拼写错误静默通过）。
	for key, class := range classified {
		if !routeClasses[class] {
			t.Fatalf("R38：路由 %q 的分类 %q 非法（允许：public/auth/reader/user/node/admin/tunnel）", key, class)
		}
	}

	// 4) 两个 mux 的规模护栏（防止解析器退化到只读到一个 mux 而假绿）。
	byMux := map[string]int{}
	for key := range registered {
		byMux[strings.SplitN(key, "\t", 2)[0]]++
	}
	if byMux["localMux"] == 0 || byMux["srvMux"] == 0 {
		t.Fatalf("R38：两个 mux 都应解析到路由（srvMux=%d localMux=%d）——解析器退化即门禁假绿",
			byMux["srvMux"], byMux["localMux"])
	}
}
