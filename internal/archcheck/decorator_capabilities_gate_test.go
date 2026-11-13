// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// decorator_capabilities_gate_test.go 是「装饰器共同能力集合完整性」门禁（R24）。
//
// 解决的问题（历史两次踩坑）：新增**可选能力接口**后，若某透明装饰器漏转发，**编译仍通过**，
// 但上层 `fs.(能力)` 断言恒 false ⇒ **静默回落重路径** ⇒ 优化归零。
// （#1127 的 `meta.ChecksumProvider` 就曾在真实装饰链上失效 ⇒ #1128 才补上。）
//
// 机制（两层）：
//  1. **编译期**：透明装饰器断言 `sync.DecoratorCapabilities`（聚合接口）⇒ 新增能力若加入该接口，
//     所有装饰器**立即编译失败** ⇒ 强制补齐；
//  2. **本门禁**：断言「**全部**可选能力接口（`pkg/sync` + `pkg/files/meta`）**要么**在聚合接口里
//     **要么**在显式排除表（含理由）里」⇒ 新增能力**必须被显式决策**，不会静默漏出。
//
// ⇒ 两层合起来：**新增能力 ⇒ 门禁红 ⇒ 决策 ⇒ 若纳入聚合接口则编译期强制装饰器补齐**。

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// decoratorCapsFile 是聚合接口定义文件。
const decoratorCapsFile = "pkg/sync/decorator_capabilities.go"

// decoratorCapsExcluded 是**不纳入**聚合接口的能力接口（每条必须写明理由）。
var decoratorCapsExcluded = map[string]string{
	"FS":                    "基础接口（装饰器必实现，另有断言）",
	"StagingQuotaTracker":   "配额钩子的**参数**接口，非 FS 能力",
	"innerFS":               "未导出（透明下探标记）",
	"BlockAccessor":         "**非共同**能力：并非所有装饰器都转发（实测 PrefixFS/StagingQuotaGateFS 未实现）⇒ 由 R21 按「登记 + 豁免」逐项把关；若将来所有装饰器都支持，应移入聚合接口",
	"DecoratorCapabilities": "本聚合接口**自身**（不是待转发的能力）",
	"RangeVerifier":         "作用于 **ReadSeeker**（读流对象）而非 FS ⇒ 不属于 FS 可选能力，装饰器无转发语义",
}

// capabilityDeclRe 匹配能力接口声明 `type X interface`（含注释尾）。
var capabilityDeclRe = regexp.MustCompile(`(?m)^type\s+([A-Za-z_]\w*)\s+interface\b`)

// TestDecoratorCapabilitiesCoverAllCapabilities R24：聚合接口必须覆盖**全部**可选能力接口
// （`pkg/sync` + `pkg/files/meta`），未纳入者必须在排除表里写明理由。
//
// 判据有决定性：新增任一能力接口（任一包）⇒ 本用例红 ⇒ 强制显式决策。
func TestDecoratorCapabilitiesCoverAllCapabilities(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)

	// ① 枚举两个包的能力接口。
	found := map[string]string{} // 能力名 → 所在包
	for _, dir := range []string{"pkg/sync", "pkg/files/meta"} {
		entries, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(dir)))
		if err != nil {
			t.Fatalf("读取 %s: %v", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(root, filepath.FromSlash(dir), e.Name()))
			if rerr != nil {
				t.Fatalf("读取 %s/%s: %v", dir, e.Name(), rerr)
			}
			for _, m := range capabilityDeclRe.FindAllStringSubmatch(string(data), -1) {
				found[m[1]] = dir
			}
		}
	}
	// 哨兵：解析失效（正则/路径变动）时不得静默通过。
	for _, want := range []string{"WriteIfAbsent", "RangeReader", "ChecksumProvider"} {
		if _, ok := found[want]; !ok {
			t.Fatalf("能力枚举失效：未找到哨兵能力 %q（扫描面被改窄）", want)
		}
	}

	// ② 读聚合接口的嵌入清单。
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(decoratorCapsFile)))
	if err != nil {
		t.Fatalf("读取 %s: %v", decoratorCapsFile, err)
	}
	embedded := map[string]bool{}
	body := string(raw)
	if i := strings.Index(body, "type DecoratorCapabilities interface {"); i >= 0 {
		body = body[i:]
		if j := strings.Index(body, "\n}"); j >= 0 {
			body = body[:j]
		}
		for line := range strings.SplitSeq(body, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "type ") {
				continue
			}
			name := line
			if k := strings.LastIndexByte(name, '.'); k >= 0 {
				name = name[k+1:] // meta.ChecksumProvider → ChecksumProvider
			}
			embedded[name] = true
		}
	}
	if len(embedded) < 10 {
		t.Fatalf("聚合接口嵌入清单过少（%d）——解析失效或接口被削", len(embedded))
	}

	// ③ 逐项核对。
	var problems []string
	names := make([]string, 0, len(found))
	for n := range found {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if embedded[n] {
			continue
		}
		if reason, ok := decoratorCapsExcluded[n]; ok && reason != "" {
			continue
		}
		problems = append(problems, n+" ("+found[n]+"): 既不在 "+decoratorCapsFile+
			" 的 DecoratorCapabilities 里，也不在 decoratorCapsExcluded 里")
	}
	if len(problems) > 0 {
		t.Fatalf("新增可选能力接口必须显式决策（R24）：纳入聚合接口（⇒ 编译期强制装饰器补齐转发）"+
			"或加入 decoratorCapsExcluded 说明理由。未决策项：\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestDecoratorCapabilitiesAssertedByAllDecorators R24（续）：**每个透明装饰器都必须写聚合断言**。
//
// 为什么需要：断言是**单向检查** —— 删掉 `var _ DecoratorCapabilities = (*T)(nil)` 后
// 编译照样通过（实测），机制就静默失效了。本用例复用 R21 的装饰器清单（`decoratorsForGate`），
// 要求每个都出现该断言 ⇒ 保证「编译期强制」真的生效。
//
// 变异：删掉任一装饰器的聚合断言 ⇒ 本用例红。
func TestDecoratorCapabilitiesAssertedByAllDecorators(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	var missing []string
	for _, d := range decoratorsForGate() {
		dir := filepath.Join(root, filepath.FromSlash(d.pkgDir))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("读取 %s: %v", d.pkgDir, err)
		}
		// 断言形如 `_ [syncpkg.]DecoratorCapabilities = (*T)(nil)`（var 块与单行同形）。
		// 兼容两种写法：var 块内 `_ X = (*T)(nil)` 与单行 `var _ X = (*T)(nil)`。
		re := regexp.MustCompile(`(?m)^\s*(?:var\s+)?_\s+([\w.]+\.)?DecoratorCapabilities\s*=\s*\(\*` +
			regexp.QuoteMeta(d.typeName) + `\)\s*\(\s*nil\s*\)`)
		found := false
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
			if rerr != nil {
				t.Fatalf("读取 %s/%s: %v", d.pkgDir, e.Name(), rerr)
			}
			if re.Match(data) {
				found = true
				break
			}
		}
		if !found {
			missing = append(missing, d.pkgDir+":"+d.typeName)
		}
	}
	if len(missing) > 0 {
		t.Fatalf("以下透明装饰器缺少聚合断言 `var _ DecoratorCapabilities = (*T)(nil)`（R24）：\n  %s\n"+
			"（该断言是「新增能力 ⇒ 编译期强制补齐转发」的唯一入口，不能省）",
			strings.Join(missing, "\n  "))
	}
}
