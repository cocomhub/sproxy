// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package archcheck

// capability_parity_gate_test.go 是「透明装饰器必须声明全部可选能力」的门禁（R21）。
//
// 背景（2026-10-10 可信卷对抗评审）：装饰器（TrustedVolumeFS / MetaBucketGuard /
// CapacityFS / StagingQuotaGateFS）**无条件实现**可选能力接口以透明转发。若新增一个
// pkg/sync 能力接口而某个装饰器漏转发，编译仍通过，但上层 `fs.(能力)` 断言恒 false →
// **静默降级**（本分支已修 3 次的失效类：staging 门卫恒被跳过、Provider 探测失败、
// RangeReader/DirectURLProvider 落空）。
//
// 设计 §4 承诺「各装饰器带编译期能力断言」——本门禁把该承诺机制化：
//   1. 从 `pkg/sync` 源码**枚举**能力接口（导出 interface，排除 FS 与 StagingQuotaTracker
//      参数接口），额外纳入 `meta.Provider` ——新增接口自动进入检查集，无需改门禁；
//   2. 对登记的装饰器，要求其**包源码**中存在 `var _ <能力> = (*<类型>)(nil)` 编译期断言
//      （单行或 var 块内）；
//   3. 确实有意不实现的能力必须**显式豁免并写明理由**（默认拒绝，防漏网）。
//
// BlockAccessor 是全装饰器的一致豁免：直接转发会让 sync 引擎对无该能力的底层也走块级
// 路径（ErrUnsupported 无法表达「未实现」），需 Capability 探测语义（设计 §8）。

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
)

// capabilityExcluded 是从 pkg/sync 接口枚举中排除的非「FS 可选能力」接口。
var capabilityExcluded = map[string]bool{
	"FS":                  true, // 基础接口（装饰器必实现，另有单独断言）
	"StagingQuotaTracker": true, // 配额钩子**参数**接口，非 FS 能力
	"innerFS":             true, // 未导出（透明下探标记）
}

// capabilitySentinel 是必须出现在枚举结果中的哨兵能力——防解析失效后门禁静默通过。
const capabilitySentinel = "WriteIfAbsent"

var capabilityTypeRe = regexp.MustCompile(`(?m)^type\s+([A-Za-z_]\w*)\s+interface\b`)

// enumerateSyncCapabilities 从 pkg/sync 非测试源码枚举可选能力接口名（去重排序）。
func enumerateSyncCapabilities(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), "pkg", "sync")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 pkg/sync: %v", err)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("读取 %s: %v", e.Name(), rerr)
		}
		for _, m := range capabilityTypeRe.FindAllStringSubmatch(string(data), -1) {
			if capabilityExcluded[m[1]] {
				continue
			}
			seen[m[1]] = true
		}
	}
	// meta.Provider 是 files/meta 的可选能力（装饰器亦须转发）。
	seen["Provider"] = true
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// decoratorSpec 登记一个透明装饰器及其能力豁免。
type decoratorSpec struct {
	typeName string
	pkgDir   string            // module 相对目录
	exempt   map[string]string // 能力名 → 豁免理由
}

// assertLineRe 匹配 `_ <Capability> = (*<Type>)(nil)`（var 块与单行断言同形）。
func assertLineRe(typeName string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*_\s+([\w.]+)\s*=\s*\(\*` + regexp.QuoteMeta(typeName) + `\)\s*\(\s*nil\s*\)`)
}

// assertedCapabilities 返回装饰器包内声明的能力名集合（取 LHS 最后一段）。
func assertedCapabilities(t *testing.T, pkgDir, typeName string) map[string]bool {
	t.Helper()
	dir := filepath.Join(moduleRoot(t), filepath.FromSlash(pkgDir))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取 %s: %v", pkgDir, err)
	}
	re := assertLineRe(typeName)
	got := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		data, rerr := os.ReadFile(filepath.Join(dir, e.Name()))
		if rerr != nil {
			t.Fatalf("读取 %s/%s: %v", pkgDir, e.Name(), rerr)
		}
		for _, m := range re.FindAllStringSubmatch(string(data), -1) {
			lhs := m[1]
			if i := strings.LastIndexByte(lhs, '.'); i >= 0 {
				lhs = lhs[i+1:] // 去掉 syncpkg./meta. 限定
			}
			got[lhs] = true
		}
	}
	return got
}

// TestDecoratorsDeclareAllCapabilities 门禁 R21：透明装饰器必须为每个 pkg/sync 可选能力
// 声明编译期断言，或显式豁免并写明理由。
func TestDecoratorsDeclareAllCapabilities(t *testing.T) {
	t.Parallel()
	caps := enumerateSyncCapabilities(t)
	if len(caps) == 0 {
		t.Fatal("未能从 pkg/sync 枚举到任何能力接口（门禁解析失效）")
	}
	foundSentinel := false
	for _, c := range caps {
		if c == capabilitySentinel {
			foundSentinel = true
		}
	}
	if !foundSentinel {
		t.Fatalf("能力枚举缺少哨兵 %q（解析失效，门禁会静默通过）", capabilitySentinel)
	}

	blockExempt := map[string]string{
		"BlockAccessor": "有意不转发：引擎回退语义需 Capability 探测（设计 §8）",
	}
	decorators := []decoratorSpec{
		{typeName: "TrustedVolumeFS", pkgDir: "pkg/volume/trusted", exempt: blockExempt},
		{typeName: "MetaBucketGuard", pkgDir: "pkg/volume/trusted", exempt: blockExempt},
		{typeName: "CapacityFS", pkgDir: "pkg/volume/capacity", exempt: blockExempt},
		{typeName: "StagingQuotaGateFS", pkgDir: "pkg/sync", exempt: blockExempt},
	}

	var problems []string
	for _, d := range decorators {
		asserted := assertedCapabilities(t, d.pkgDir, d.typeName)
		for _, cap := range caps {
			if _, ok := d.exempt[cap]; ok {
				continue
			}
			if !asserted[cap] {
				problems = append(problems, d.pkgDir+": "+d.typeName+" 缺少能力断言 `_ "+cap+" = (*"+d.typeName+")(nil)`（漏转发会静默降级）")
			}
		}
		// 豁免项必须是真实能力名（防拼写错误导致豁免失效）。
		for cap := range d.exempt {
			if !contains(caps, cap) {
				problems = append(problems, d.pkgDir+": "+d.typeName+" 豁免了不存在的能力 "+cap)
			}
		}
	}
	if len(problems) > 0 {
		t.Fatalf("透明装饰器能力断言不完整（新增 pkg/sync 能力接口后必须同步声明或显式豁免）:\n  %s",
			strings.Join(problems, "\n  "))
	}
}

func contains(xs []string, want string) bool {
	return slices.Contains(xs, want)
}
