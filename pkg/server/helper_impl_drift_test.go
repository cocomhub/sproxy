// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// helper_impl_drift_test.go 守卫**同一纯函数的两份实现**：`pkg/files`（领域侧）与
// `pkg/server`（装配侧）各有一份。
//
// 受守卫的**函数体等价**共十一份（十一个函数名）：
//
//   - `atomicRenameRoot`（`pkg/files/service.go` ↔ `pkg/server/upload_handler.go`）；
//   - `volumePoolForTenant`（`pkg/files/version_store.go` ↔ `pkg/server/volumes.go`）；
//   - `volumeFileExists`（`pkg/files/read.go` ↔ `pkg/server/volumes.go`，只读面引入）；
//   - `copyWithContext`（`pkg/files/write.go` ↔ `pkg/server/upload_handler.go`，写面引入）；
//   - `defaultVolumeAllows` / `locateForRead`（`pkg/files/service.go` ↔ `pkg/server/volumes.go`，
//     写面引入）；
//   - `normalizeOwner` / `drainAndVerifyBody`（`pkg/files/service.go` ↔ `pkg/server/handlers.go`、
//     `pkg/server/auth.go`）；
//   - `formatContentDisposition`（`pkg/files/chunked_response.go` ↔ `pkg/server/response.go`）；
//   - `fileChecksumRoot` / `FileChecksumRoot`（`pkg/files/service.go` ↔ `pkg/server/checksum.go`）。
//
// 另守卫一条**委托契约**（不是函数体等价）：`checksumReader`（`pkg/files/service.go`）与
// `Checksum`（`pkg/server/checksum.go`）都必须委托 L0 顶层包 `checksum.Reader`——
// SHA-256 算法实现在那里是单一事实源，任一侧重新内联本地实现即红
// （见 TestChecksumImpls_DelegateToSharedReader）。
//
// 为什么需要它：领域侧那几份是"无法 import 装配层"的产物（见 pkg/files/service.go 的
// 「跨族共享的纯函数」小节与包文档）。两份实现各有一段**行为测试走不到的判定**：
//
//   - `atomicRenameRoot` 的 Windows 句柄释放延迟退避重试——Linux/CI 走不到慢速路径；
//   - `volumePoolForTenant` 的「租户根不在任何卷根下 → nil」分支——正常装配下不可达
//     （行为测试只覆盖「命中」路径的副作用，见 `version_crossvolume_test.go` 的卷池断言；
//     反查次序若改到仍命中同一卷池，行为测试不会红）；
//   - `volumeFileExists` 的「卷未知 → (false, nil)」与「stat 非 NotExist 错误 → 包装错误」
//     两个分支——前者要求传入集合外的卷名（调用点只遍历视图内卷，不可达），后者要求
//     I/O 权限类故障（测试环境难构造）；搜索用例只覆盖「存在/不存在」两条正常分支；
//   - `copyWithContext` 的 `ctx.Done()` 提前返回分支——上传/移动的行为测试不会中途取消；
//   - `defaultVolumeAllows` / `locateForRead` 的「VolSet 未装配 → 恒放行/未命中」与
//     「显式 volume 未知卷名」分支——前者只在旧装配路径可达，后者在 ACL 用例里也只覆盖
//     到「卷存在但不在视图」；且两份实现的差异面（ACL 判定次序）在多数用例矩阵下结果相同。
//
// 且没有测试**同时**驱动两份实现，故此处做**源码级等价断言**：抽取两处实现的**归一化
// 整段函数体**逐字比对（只归一化两份必然不同的书写形态：接收者/限定名、定位结果的
// 指针-值类型与字段大小写、探测调用的额外卷集合实参），改坏任一侧即红。
//
// 另守卫一条**跨层值契约**（非函数实现）：`uploadingLockUpload` 的字面量
// （见 TestUploadingLockMarker_NoDrift）。
//
// 与 `chunked_wire_drift_test.go` 同属"跨侧漂移守卫"，但对象不是 JSON 契约而是实现语义，
// 故独立成文件（前者只读字段形状与序列化字节）。

// renameSemantics 是 atomicRenameRoot 的语义骨架。
type renameSemantics struct {
	MaxAttempts string   // `const maxAttempts = <X>` 的字面值
	BaseDelay   string   // `const baseDelay = <Y>` 的字面值
	Ops         []string // 关键调用序列（Rename / Remove / Sleep / 退避位移），保序
}

var (
	renameConstRe = regexp.MustCompile(`const\s+maxAttempts\s*=\s*([0-9]+)`)
	baseDelayRe   = regexp.MustCompile(`const\s+baseDelay\s*=\s*([^/\n]+)`)
	renameOpRe    = regexp.MustCompile(`root\.(Rename|Remove)\(|time\.Sleep\(|baseDelay\s*<<\s*i`)
)

// funcBody 截取源码中 `func ... <name>(` 起始到下一个顶层 `}` 之间的函数体。
// 找不到该函数（被改名/删除）即 fail——这本身也是漂移的一种。
func funcBody(t *testing.T, src, name string) string {
	t.Helper()
	decl := regexp.MustCompile(`(?m)^func [^\n]*\b` + regexp.QuoteMeta(name) + `\(`)
	loc := decl.FindStringIndex(src)
	if loc == nil {
		t.Fatalf("源码中未找到函数 %s（被改名/删除？）", name)
	}
	rest := src[loc[1]:]
	before, _, ok := strings.Cut(rest, "\n}\n")
	if !ok {
		t.Fatalf("函数 %s 的函数体未找到结束花括号", name)
	}
	return before
}

func extractRenameSemantics(t *testing.T, src, name string) renameSemantics {
	t.Helper()
	body := funcBody(t, src, name)
	m := renameConstRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("%s 中未找到 `const maxAttempts = <N>`", name)
	}
	d := baseDelayRe.FindStringSubmatch(body)
	if d == nil {
		t.Fatalf("%s 中未找到 `const baseDelay = <Y>`", name)
	}
	var ops []string
	for _, op := range renameOpRe.FindAllString(body, -1) {
		ops = append(ops, strings.Join(strings.Fields(op), " "))
	}
	if len(ops) == 0 {
		t.Fatalf("%s 中未抽取到任何关键调用（Rename/Remove/Sleep）", name)
	}
	return renameSemantics{MaxAttempts: m[1], BaseDelay: strings.TrimSpace(d[1]), Ops: ops}
}

// TestAtomicRenameRoot_ImplParity 断言 pkg/files 与 pkg/server 两份 atomicRenameRoot 的
// 语义骨架逐项一致：重试次数、退避基数、以及"快速路径 Rename → 慢速路径 Remove 后 Rename
// → 失败时 Sleep(退避位移)"的调用次序。
func TestAtomicRenameRoot_ImplParity(t *testing.T) {
	domain := extractRenameSemantics(t, readRepoFile(t, "pkg/files/service.go"), "atomicRenameRoot")
	assembly := extractRenameSemantics(t, readRepoFile(t, "pkg/server/upload_handler.go"), "atomicRenameRoot")

	if domain.MaxAttempts != assembly.MaxAttempts {
		t.Fatalf("atomicRenameRoot 重试次数漂移：pkg/files=%s pkg/server=%s", domain.MaxAttempts, assembly.MaxAttempts)
	}
	if domain.BaseDelay != assembly.BaseDelay {
		t.Fatalf("atomicRenameRoot 退避基数漂移：pkg/files=%q pkg/server=%q", domain.BaseDelay, assembly.BaseDelay)
	}
	if !reflect.DeepEqual(domain.Ops, assembly.Ops) {
		t.Fatalf("atomicRenameRoot 调用次序漂移：\n pkg/files =%v\n pkg/server=%v", domain.Ops, assembly.Ops)
	}
	// 正探针：确认两份实现都真的被抽到了内容（否则上面的相等是"两个空值相等"的假绿）。
	if domain.MaxAttempts == "" || len(domain.Ops) < 4 {
		t.Fatalf("抽取结果过弱，守卫可能失效：%+v", domain)
	}
}

// poolReceiverRe 归一化两份实现的**卷集合接收者**：领域侧是形参 `vs`，装配侧是字段
// `h.volSet`；其余标识符（`volumePoolForTenant` 的 tnt / tenantAbs / volOwnerAbs / rt / v，
// `volumeFileExists` 的 volName / owner / rel / rt / err）两份实现逐字相同，可比。
var poolReceiverRe = regexp.MustCompile(`\b(?:h\.volSet|vs)\b`)

// poolProbeRe 是"抽取确实抓到了真函数体"的正探针锚点（两份实现都必须含这些标记）。
var poolProbeRe = regexp.MustCompile(`filepath\.Clean\(|\.Pool\(v\.Name\)`)

// existsProbeRe 是 volumeFileExists 的正探针锚点（两份实现都必须含这两处判定）。
var existsProbeRe = regexp.MustCompile(`errors\.Is\(err, fs\.ErrNotExist\)|探测卷 %q 文件状态失败`)

// volSetNormBody 返回函数体（自签名行的 `{` 之后开始，**丢弃形参列表**——两份的接收者形态
// 不同，只有体可比），并把卷集合接收者归一为 `VS`。
func volSetNormBody(t *testing.T, src, name string) string {
	t.Helper()
	extracted := funcBody(t, src, name)
	_, body, ok := strings.Cut(extracted, "\n")
	if !ok {
		t.Fatalf("%s 的签名行未找到换行（funcBody 抽取结果异常）：%q", name, extracted)
	}
	return poolReceiverRe.ReplaceAllString(body, "VS")
}

// TestVolumePoolForTenant_ImplParity 断言 pkg/files 与 pkg/server 两份 volumePoolForTenant
// 的**归一化函数体逐字一致**（只归一化卷集合接收者名）。
//
// 为什么比对整段体而不是"关键调用序列"：只比调用序列会漏掉**比较表达式与循环实参**
// ——实测把 `filepath.Clean(volOwnerAbs) == tenantAbs` 反转为 `!=`、或把
// `vs.Root(v.Name)` 写成 `vs.Root(tnt.ID)`，调用序列一字不变却在语义上完全走样
// （前者的守卫会全绿）。整段比对把这些一并覆盖，代价只是两份代码必须保持同构——
// 这正是"同一纯函数的两份实现"应有的约束。
func TestVolumePoolForTenant_ImplParity(t *testing.T) {
	domain := volSetNormBody(t, readRepoFile(t, "pkg/files/version_store.go"), "volumePoolForTenant")
	assembly := volSetNormBody(t, readRepoFile(t, "pkg/server/volumes.go"), "volumePoolForTenant")

	if domain != assembly {
		t.Fatalf("volumePoolForTenant 函数体漂移：\n pkg/files =\n%s\n pkg/server=\n%s", domain, assembly)
	}
	// 正探针：确认抽取到的是真函数体（否则"两个空值相等"会假绿）。判据不绑定具体行数/条数，
	// 只要求：非平凡长度 + 含两份实现共有的锚点标记（未来合法地增删行也不会误报）。
	if len(domain) < 200 || !poolProbeRe.MatchString(domain) {
		t.Fatalf("抽取结果过弱，守卫可能失效（len=%d）：%q", len(domain), domain)
	}
}

// TestVolumeFileExists_ImplParity 断言 pkg/files 与 pkg/server 两份 volumeFileExists 的
// **归一化函数体逐字一致**（只归一化卷集合接收者名）。判据与 TestVolumePoolForTenant_ImplParity
// 相同：整段体比对覆盖比较表达式与实参（只比调用序列会漏掉 `fs.ErrNotExist` 之类的判定细节）。
func TestVolumeFileExists_ImplParity(t *testing.T) {
	domain := volSetNormBody(t, readRepoFile(t, "pkg/files/read.go"), "volumeFileExists")
	assembly := volSetNormBody(t, readRepoFile(t, "pkg/server/volumes.go"), "volumeFileExists")

	if domain != assembly {
		t.Fatalf("volumeFileExists 函数体漂移：\n pkg/files =\n%s\n pkg/server=\n%s", domain, assembly)
	}
	// 正探针：非平凡长度 + 含两份实现共有的两条判定标记（"不存在"与"探测失败"）。
	if len(domain) < 150 || !existsProbeRe.MatchString(domain) {
		t.Fatalf("抽取结果过弱，守卫可能失效（len=%d）：%q", len(domain), domain)
	}
}

// implNormRules 是**写面引入的四份实现**的归一化规则（按序应用）。
// 两份实现只有书写形态不同，语义必须逐字一致；每条规则都对应一处**无法写成同一形态**：
//
//  1. 探测调用的额外实参——领域侧的 volumeFileExists 以形参收卷集合
//     （`volumeFileExists(s.rt.volSet(), v.Name, …)`），装配侧是方法（`h.volumeFileExists(v.Name, …)`）；
//  2. 定位结果的构造——领域侧是值类型 + 导出字段（`FileLocation{VolumeName: …, Tenant: …}`），
//     装配侧是指针 + 小写字段（`&fileLocation{volumeName: …, tenant: …}`）；
//  3. 定位结果的空值返回——领域侧零值（`FileLocation{}`），装配侧 `nil`；
//     4~6. 卷集合 / 卷租户 / 读定位接缝的取用——领域侧 `s.rt.X()`（能力访问器），
//     装配侧 `h.X`（或形参 `vs`）。
//
// 规则 1~3 **必须先于** 4~6 应用（1 与 2 的文本里含有 `s.rt.volSet()` / 字段名，先归一化
// 才能被 4~6 正确折叠）。归一化只消除"同一语义的两种写法"，不隐藏任何判定：
// 比较表达式、调用次序、错误分支全部原样进入比对。
var implNormRules = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`\b(?:h\.)?volumeFileExists\((?:s\.deps\.VolSet, |s\.rt\.volSet\(\), )?`), ""},
	{regexp.MustCompile(`&?[fF]ileLocation\{(?:volumeName|VolumeName): ([^,]+), (?:tenant|Tenant): ([^}]+)\}`), "LOC{$1,$2}"},
	{regexp.MustCompile(`return (?:nil|FileLocation\{\}), false`), "return LOCNULL, false"},
	{regexp.MustCompile(`\b(?:h\.volSet|s\.deps\.VolSet|vs)\b|s\.rt\.volSet\(\)`), "VS"},
	{regexp.MustCompile(`\b(?:h\.volumeTenant|s\.deps\.VolumeTenant|s\.rt\.volumeTenant)\b`), "VT"},
	{regexp.MustCompile(`\b(?:h\.locateOwnerFile|s\.deps\.LocateOwnerFile|s\.rt\.locateOwnerFile)\b`), "LOF"},
}

// implNormBody 返回函数体（自签名行的 `{` 之后开始，**丢弃形参列表**——两份的接收者形态
// 与返回类型不同，只有体可比），并按 implNormRules 归一化书写形态差异。
func implNormBody(t *testing.T, src, name string) string {
	t.Helper()
	extracted := funcBody(t, src, name)
	_, body, ok := strings.Cut(extracted, "\n")
	if !ok {
		t.Fatalf("%s 的签名行未找到换行（funcBody 抽取结果异常）：%q", name, extracted)
	}
	for _, r := range implNormRules {
		body = r.re.ReplaceAllString(body, r.repl)
	}
	return body
}

// assertImplParity 断言两份实现的归一化函数体逐字一致，并用探针确认抽取结果非空转
// （minLen + 必须命中的语义锚点）。
func assertImplParity(t *testing.T, name, domainFile, assemblyFile string, minLen int, probe *regexp.Regexp) {
	t.Helper()
	domain := implNormBody(t, readRepoFile(t, domainFile), name)
	assembly := implNormBody(t, readRepoFile(t, assemblyFile), name)
	if domain != assembly {
		t.Fatalf("%s 函数体漂移：\n pkg/files(%s) =\n%s\n pkg/server(%s) =\n%s", name, domainFile, domain, assemblyFile, assembly)
	}
	if len(domain) < minLen || !probe.MatchString(domain) {
		t.Fatalf("%s 抽取结果过弱，守卫可能失效（len=%d）：%q", name, len(domain), domain)
	}
}

// copyCtxProbeRe / defaultVolProbeRe / locateReadProbeRe 是各守卫的正探针锚点
// （两份实现归一化后都必须命中的语义标记）。
var (
	copyCtxProbeRe    = regexp.MustCompile(`ctx\.Done\(\)|32\*1024`)
	defaultVolProbeRe = regexp.MustCompile(`VS\.Default\(\)\.Name|Authorize\(owner\)`)
	locateReadProbeRe = regexp.MustCompile(`LOC\{v\.Name,tnt\}|LOF\(owner, rel\)|VS\.ByName\(explicitVol\)`)
)

// TestCopyWithContext_ImplParity 断言 pkg/files 与 pkg/server 两份 copyWithContext 的
// 归一化函数体逐字一致（该函数无 receiver，两份文本应完全相同）。
func TestCopyWithContext_ImplParity(t *testing.T) {
	assertImplParity(t, "copyWithContext", "pkg/files/write.go", "pkg/server/upload_handler.go", 200, copyCtxProbeRe)
}

// TestDefaultVolumeAllows_ImplParity 断言两份 defaultVolumeAllows 的归一化函数体逐字一致。
func TestDefaultVolumeAllows_ImplParity(t *testing.T) {
	assertImplParity(t, "defaultVolumeAllows", "pkg/files/service.go", "pkg/server/volumes.go", 100, defaultVolProbeRe)
}

// TestLocateForRead_ImplParity 断言两份 locateForRead 的归一化函数体逐字一致。
// 该函数是读/删/改名路径的卷定位统一入口（显式卷的 ACL 门禁 + fail-closed 未命中），
// 两份分叉会让「经显式 ?volume= 定位」在删/改名与下载之间出现 ACL 判定差异。
func TestLocateForRead_ImplParity(t *testing.T) {
	assertImplParity(t, "locateForRead", "pkg/files/service.go", "pkg/server/volumes.go", 150, locateReadProbeRe)
}

// uploadingLockMarkerInDomainRe 抽取领域侧 uploadingLockUpload 常量的字面值：兼容独立
// `const x = "…"` 与 const 块内两种写法（`(?:const\s+)?` + 容忍对齐空格）。
var uploadingLockMarkerInDomainRe = regexp.MustCompile(`(?m)^\s*(?:const\s+)?uploadingLockUpload\s*=\s*"([^"]+)"`)

// TestUploadingLockMarker_NoDrift 守卫一条**跨层值契约**（不是函数实现）：pkg/files 单次上传
// 写进锁池（Deps.Uploading）的条目值，必须是装配层 isUploadingLockMarker 认识的字面量之一。
// 值不被识别时，过期清理（cleanupUploadingFilesPass）会把它当成 upload_id → GetSession 失败
// → 删除锁条目：>10 分钟的上传在持锁期间被解除互斥（同 rel 并发写窗口重新打开）。
//
// 三条断言互为纵深：装配侧常量未被改动、领域侧字面量与之相等、该字面量确实被
// isUploadingLockMarker 放行（最后一条是正探针，确保本守卫不是在比对两个错误值）。
func TestUploadingLockMarker_NoDrift(t *testing.T) {
	if uploadingLockUpload != "upload" {
		t.Fatalf("pkg/server 侧锁标记契约变更：%q（本测试与 pkg/files 侧副本需同步复核）", uploadingLockUpload)
	}
	m := uploadingLockMarkerInDomainRe.FindStringSubmatch(readRepoFile(t, "pkg/files/write.go"))
	if m == nil {
		t.Fatalf("pkg/files/write.go 中未找到 uploadingLockUpload 常量（被改名/删除？）")
	}
	if m[1] != uploadingLockUpload {
		t.Fatalf("锁标记漂移：pkg/files=%q pkg/server=%q", m[1], uploadingLockUpload)
	}
	if !isUploadingLockMarker(m[1]) {
		t.Fatalf("pkg/files 的锁标记 %q 不被 isUploadingLockMarker 识别（清理循环会误删锁条目）", m[1])
	}
}

// ---- 补守卫：写面/只读面迁入后仍存在的四份同构双份实现（2026-09 补） ----
//
// 下列四份此前**逐字相同但无机械约束**（只有行为测试覆盖正常分支，判定分支走不到）：
// `normalizeOwner`、`drainAndVerifyBody`、`formatContentDisposition` 与两个 `*ChecksumRoot`
// 包装。补守卫的理由与前述一致：两份实现分处两包、无测试同时驱动二者，一旦分叉只会在
// 特定边界（空 owner / 非 ASCII 文件名 / storage.Root 相对路径）才显形。

// normalizeProbeRe / drainProbeRe / dispositionProbeRe 是各守卫的正探针锚点
// （两份实现归一化后都必须命中的语义标记）。
var (
	normalizeProbeRe   = regexp.MustCompile(`anonymousOwner`)
	drainProbeRe       = regexp.MustCompile(`io\.Discard`)
	dispositionProbeRe = regexp.MustCompile(`FormatMediaType|filename\*`)
)

// TestNormalizeOwner_ImplParity 断言两份 normalizeOwner 的函数体逐字一致。
// 空 owner → anonymous 是跨层契约（审计行/租户目录名依赖它），两侧分叉会让同一请求在
// 领域侧与装配侧归属到不同租户。
func TestNormalizeOwner_ImplParity(t *testing.T) {
	assertImplParity(t, "normalizeOwner", "pkg/files/service.go", "pkg/server/handlers.go", 40, normalizeProbeRe)
}

// TestDrainAndVerifyBody_ImplParity 断言两份 drainAndVerifyBody 的函数体逐字一致。
// 该函数触发 SproxySig bodyValidator 的 EOF 哈希比对（I-3）：一侧漏读会让篡改的请求体
// 在部分路径上绕过验签。
func TestDrainAndVerifyBody_ImplParity(t *testing.T) {
	assertImplParity(t, "drainAndVerifyBody", "pkg/files/service.go", "pkg/server/auth.go", 40, drainProbeRe)
}

// TestFormatContentDisposition_ImplParity 断言两份 formatContentDisposition 的函数体逐字一致。
// 非 ASCII 文件名的 RFC 5987 编码是 HTTP 契约，两侧分叉会让同一文件在不同端点上得到
// 不同的 Content-Disposition（客户端另存文件名不一致）。
func TestFormatContentDisposition_ImplParity(t *testing.T) {
	assertImplParity(t, "formatContentDisposition", "pkg/files/chunked_response.go", "pkg/server/response.go", 100, dispositionProbeRe)
}

// checksumHashCallRe 归一化两份 `*ChecksumRoot` 包装内的哈希调用名：领域侧 `checksumReader(f)`、
// 装配侧 `Checksum(f)`（`\b` 保证不误匹配 `FileChecksumRoot` 自身）。
var checksumHashCallRe = regexp.MustCompile(`\b(?:checksumReader|Checksum)\(`)

// normChecksumImpl 把两份实现的哈希调用名归一为 `HASH(`，其余（root.Open / defer Close /
// 错误分支）保持逐字，供整段体比对。
func normChecksumImpl(body string) string {
	return checksumHashCallRe.ReplaceAllString(body, "HASH(")
}

// funcBodyOnly 返回函数体（自签名行换行之后开始，**丢弃签名行**——两份实现的函数名与
// 接收者形态可能不同，只有体可比），并按 norm 归一化书写差异（nil = 不归一化）。
func funcBodyOnly(t *testing.T, src, name string, norm func(string) string) string {
	t.Helper()
	extracted := funcBody(t, src, name)
	_, body, ok := strings.Cut(extracted, "\n")
	if !ok {
		t.Fatalf("%s 的签名行未找到换行（funcBody 抽取结果异常）：%q", name, extracted)
	}
	if norm != nil {
		body = norm(body)
	}
	return body
}

// TestFileChecksumRoot_ImplParity 断言两份 `*ChecksumRoot` 包装（领域侧 fileChecksumRoot /
// 装配侧 FileChecksumRoot）的归一化函数体逐字一致：都必须「root.Open → defer Close →
// 委托哈希函数」。分叉会让两侧对同一 storage.Root 相对路径给出不同摘要或不同错误语义。
func TestFileChecksumRoot_ImplParity(t *testing.T) {
	domain := funcBodyOnly(t, readRepoFile(t, "pkg/files/service.go"), "fileChecksumRoot", normChecksumImpl)
	assembly := funcBodyOnly(t, readRepoFile(t, "pkg/server/checksum.go"), "FileChecksumRoot", normChecksumImpl)
	if domain != assembly {
		t.Fatalf("FileChecksumRoot 函数体漂移：\n pkg/files =\n%s\n pkg/server=\n%s", domain, assembly)
	}
	if len(domain) < 80 || !strings.Contains(domain, "HASH(") || !strings.Contains(domain, "root.Open(") {
		t.Fatalf("抽取结果过弱，守卫可能失效（len=%d）：%q", len(domain), domain)
	}
}

// TestChecksumImpls_DelegateToSharedReader 守卫**委托契约**（非函数体等价）：pkg/files 的
// checksumReader 与 pkg/server 的 Checksum 都必须委托 L0 顶层包 `checksum.Reader`。
//
// 为什么是"守卫委托"而不是"比对两份实现"：SHA-256 算法已下沉为单一事实源
// （pkg/checksum/hash.go），两侧只剩一行委托；任一侧重新内联 sha256 循环即破坏单一事实源，
// 本断言以「必须出现 checksum.Reader( 且函数体不含 sha256.」判红。
func TestChecksumImpls_DelegateToSharedReader(t *testing.T) {
	domain := funcBodyOnly(t, readRepoFile(t, "pkg/files/service.go"), "checksumReader", nil)
	assembly := funcBodyOnly(t, readRepoFile(t, "pkg/server/checksum.go"), "Checksum", nil)
	for name, body := range map[string]string{"pkg/files.checksumReader": domain, "pkg/server.Checksum": assembly} {
		if !strings.Contains(body, "checksum.Reader(") {
			t.Fatalf("%s 未委托 checksum.Reader（单一事实源被绕过）：%q", name, body)
		}
		if strings.Contains(body, "sha256.") {
			t.Fatalf("%s 重新内联了 sha256 实现（应改为委托 checksum.Reader）：%q", name, body)
		}
	}
}
