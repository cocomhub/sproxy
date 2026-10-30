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

	decorators := decoratorsForGate()

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

// wrapperInnerRe 匹配「包装型 FS」的透明下探标记方法 `Inner() FS`（可为 `sync.FS`/
// `syncpkg.FS` 限定形式）。
var wrapperInnerRe = regexp.MustCompile(`(?m)^func \(\w+ \*?(\w+)\) Inner\(\) (\w+\.)?FS\b`)

// wrapperFSExempt 是「实现 Inner() 但不进入 R21 全能力断言」的显式豁免表（键 `<pkgDir>:<Type>`）。
// 每条必须写明理由——默认拒绝（防漏网：新装饰器必须要么登记、要么解释为什么不登记）。
var wrapperFSExempt = map[string]string{
	// 仅用基础 7 方法、不参与可选能力探测的包装层（记档为 latent：若将来上层对其断言可选
	// 能力，必须补转发并转为 R21 登记项）。
	"pkg/server:backupQuotaFS":      "备份目标写面只用基础方法（预留/写/删/改名）；可选能力未接（latent，记档）",
	"cmd/sproxy:carrierReportingFS": "仅统计载体用量，嵌入 FS 接口（可选能力本就不在方法集）",
	"pkg/gateway/webdav:subPathFS":  "WebDAV remote 子路径视图，仅基础方法（远端协议无语义）",
	"pkg/volume/federated:FS":       "联邦卷视图（远端协议），可选能力按能力探测语义不转发",
	"pkg/volume/ext/cluster:FS":     "集群卷视图（远端协议），同上",
	// 处理层（对字节做变换：加密）：可选能力坐标与明/密文语义不同，**不得**透明转发
	// （如 RangeReader 的 offset 在密文空间 vs 明文空间），故不做装饰器登记（设计 §2 正交：
	// 处理只改字节、视图只搬键）。
	"pkg/sync:EncryptedFS":               "处理层（字节加密）：可选能力坐标语义不同，不透明转发",
	"pkg/volume/secretdata:SecretdataFS": "处理层（分块加密+meta pad）：同上",
}

// wrapperStructRe 匹配结构体定义（含简单/嵌套大括号体的首层），wrapperEmbedsFSRe/
// wrapperInnerFieldRe 判定其是否「包装型 FS」——嵌入 FS 接口或持有 `inner ...FS` 字段
// （后者覆盖**无 Inner() 方法**的包装层，如 pkg/syncexec 的 quotaLocalFS：评审点名的漏网点）。
var (
	wrapperStructRe     = regexp.MustCompile(`(?ms)^type (\w+) struct \{(.*?)^\}`)
	wrapperInnerFieldRe = regexp.MustCompile(`(?m)^\s*inner\s+\S*FS\s*$`)
	wrapperEmbedFSRe    = regexp.MustCompile(`(?m)^\s*(?:\w+\.)?FS\s*$`)
)

// TestWrapperFSDiscoveredWithoutRegistration R23（评审 P2）：R21 只校验**硬编码装饰器清单**，
// 新增包装型 FS（实现 Inner()）不会被任何门禁拦住 ⇒ 本特性历史 P0 类（装饰器漏转发可选能力）
// 无机制兜底。本门禁**自动发现**仓库内全部包装型 FS，要求每个都登记进 R21 装饰器清单或出现在
// wrapperFSExempt（含书面理由），否则失败。
func TestWrapperFSDiscoveredWithoutRegistration(t *testing.T) {
	t.Parallel()
	root := moduleRoot(t)
	registered := map[string]bool{}
	for _, d := range decoratorsForGate() {
		registered[d.pkgDir+":"+d.typeName] = true
	}

	skipDir := map[string]bool{
		".git": true, ".worktrees": true, "node_modules": true, "testdata": true,
		"web": true, "docs": true, ".pi-review": true, "tool": true, "tools": true,
	}
	var problems []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if d.IsDir() {
			if skipDir[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		names := map[string]bool{}
		for _, m := range wrapperInnerRe.FindAllStringSubmatch(string(data), -1) {
			names[m[1]] = true
		}
		for _, m := range wrapperStructRe.FindAllStringSubmatch(string(data), -1) {
			body := m[2]
			if wrapperEmbedFSRe.MatchString(body) || wrapperInnerFieldRe.MatchString(body) {
				names[m[1]] = true
			}
		}
		for typeName := range names {
			rel, rerr2 := filepath.Rel(root, filepath.Dir(p))
			if rerr2 != nil {
				return rerr2
			}
			pkgDir := filepath.ToSlash(rel)
			key := pkgDir + ":" + typeName
			if registered[key] {
				continue
			}
			if _, ok := wrapperFSExempt[key]; ok {
				continue
			}
			problems = append(problems, key+" 既未登记进 R21 装饰器清单，也无 wrapperFSExempt 豁免理由")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("扫描仓库失败: %v", err)
	}
	if len(problems) > 0 {
		t.Fatalf("发现未登记的包装型 FS（R23：新增装饰器必须登记或显式豁免）:\n  %s", strings.Join(problems, "\n  "))
	}
}

// decoratorsForGate 返回 R21 登记的装饰器清单（TestDecoratorsDeclareAllCapabilities 与
// R23 自动发现门禁共用单一事实源）。
func decoratorsForGate() []decoratorSpec {
	blockExempt := map[string]string{
		"BlockAccessor": "有意不转发：引擎回退语义需 Capability 探测（设计 §8）",
	}
	return []decoratorSpec{
		{typeName: "TrustedVolumeFS", pkgDir: "pkg/volume/trusted", exempt: blockExempt},
		{typeName: "MetaBucketGuard", pkgDir: "pkg/volume/trusted", exempt: blockExempt},
		{typeName: "CapacityFS", pkgDir: "pkg/volume/capacity", exempt: blockExempt},
		{typeName: "StagingQuotaGateFS", pkgDir: "pkg/sync", exempt: blockExempt},
		{typeName: "PrefixFS", pkgDir: "pkg/sync", exempt: blockExempt},
		{typeName: "davMetaFS", pkgDir: "pkg/server/dav", exempt: blockExempt},
		{typeName: "FS", pkgDir: "pkg/volume/enclosure", exempt: blockExempt},
		// FS 级封印装饰器（外部卷/远端卷视图的封印）：本地卷封印在 storage.Root 层，外部卷
		// 无 Root ⇒ 在装配处（acc.external）套本装饰器；能力全量转发 + Inner() 特权解包。
		{typeName: "SealFS", pkgDir: "pkg/volume/enclosure", exempt: blockExempt},
		// owner 限定声明的键映射视图（owner 由声明给定、键只做精确相等校验）。
		{typeName: "ownerFS", pkgDir: "pkg/volume/enclosure", exempt: blockExempt},
		// 同步 pull 本地目标的配额装饰器（评审 P2：此前只实现基础 7 方法，把内层能力全剥掉
		// ⇒ meta.Provider/WriteIfAbsent/Mover/RangeReader 等断言恒 false 静默降级）。
		{typeName: "quotaLocalFS", pkgDir: "pkg/syncexec", exempt: blockExempt},
		// 卷状态缓存装饰器（设计 2026-10-28-volume-state-maintenance）：读 ListDir 走缓存、
		// 写（WriteFile/Delete/Rename/MakeDir/WriteIfAbsent/Move/Copy/Link）成功后自动失效。
		// 能力全量转发（转存/校验/容量/staging/直链等断言不得在此层丢失）。
		{typeName: "CacheFS", pkgDir: "pkg/volumestate", exempt: blockExempt},
	}
}
