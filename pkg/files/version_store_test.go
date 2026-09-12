// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// version_store_test.go 是版本族**存储侧**的域级测试：用真实下层能力（pkg/storage 租户根、
// pkg/checksum 台账、pkg/quota 池）+ 注入的最小装配件直接驱动本包的版本存储方法，
// 不经装配层（pkg/server 的 /api/versions 处理器测试在那边，覆盖同一份代码的 HTTP 面）。
//
// 用例来源：两条自 `pkg/server` 迁入（原文件 `uploading_lock_test.go` 与 `version_test.go`），
// **用例名逐字保留**、断言语义未改，只把夹具从 pkg/server 的 Handlers 换成域级替身
// （`newDirsEnv`，见 dirs_test.go 的等价范围声明）。

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestCollectVersionEntries_IsNotExistSkipped CollectVersionEntries 对「目录不存在」
// （ReadDir IsNotExist）按空目录跳过返回空列表（M-1 回归）：VolSet 未装配的旧装配路径下
// version/<rel> 目录尚未创建属常态，若把 IsNotExist 当错误返回会令 GET /api/versions
// 从 200 空列表退化 500。
func TestCollectVersionEntries_IsNotExistSkipped(t *testing.T) {
	env := newDirsEnv(t)

	// VolSet 未装配（单卷唯一根）：version/f.txt 目录不存在。
	entries, err := env.svc.CollectVersionEntries("", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries 对 IsNotExist 应返回空列表而非错误: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("无版本目录应返回空列表, got %d", len(entries))
	}

	// 反向：目录存在且含一个版本 → 返回 1 条（确认不是恒空）。
	tnt := env.tenantFor("alice")
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("alice 租户不可用")
	}
	verRel, ok := tnt.FeatureRel("version", "f.txt")
	if !ok {
		t.Fatal("FeatureRel(version, f.txt) 失败")
	}
	if mkerr := tnt.Root().MkdirAll(verRel, 0o755); mkerr != nil {
		t.Fatal(mkerr)
	}
	verFile := verRel + "/1000000000001"
	f, oerr := tnt.Root().OpenFile(verFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if oerr != nil {
		t.Fatalf("写版本文件: %v", oerr)
	}
	_, _ = f.Write([]byte("v1"))
	_ = f.Close()
	entries, err = env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("版本目录存在应返回 1 条, got %d err=%v", len(entries), err)
	}
}

// TestCleanupOldVersions_NoMaxVersions MaxVersions 默认 0 → cleanup 直接返回，不报错。
func TestCleanupOldVersions_NoMaxVersions(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	if tnt == nil {
		t.Fatal("创建 alice 租户失败")
	}
	// MaxVersions 默认 0（夹具 VersioningMaxVersions 恒返回 0）→ cleanup 直接返回，不报错。
	env.svc.cleanupOldVersions("test.txt", tnt, "alice")
}

// TestFindVersionFile_RejectsNonNumericVersionID 钉住 FindVersionFile 的入口契约：
// versionIDStr 必须通过 `parseVersionID`：**十进制整数且 > 0**，否则一律 not-found
// （found=false, err=nil，与「版本不存在」同一条路径）。
//
// 为什么必须校验：verRel = verDir + "/" + versionIDStr 由它拼接，未校验的 "../../meta/x"
// 会越出 version/<file>/ 子目录落到同租户其它桶（os.Root 只保证不逃出**租户根**）。
// 本用例同时给出**越界读的否定证据**：meta 桶内的哨兵文件存在，仍不得被函数命中。
//
// **两条拒绝规则各自独立、都要保留**：(A) 段逐形态证明 `ParseInt`（十进制字面量）是**路径
// 安全闸门**；(B) 段证明 `id > 0` 是**领域不变量**（version 必须为正），非正形态即便能过
// ParseInt 也一律拒绝；(C) 段是"通过闸门且为正 ⇒ 单段路径"的对照；(D) 段证明拒绝来自不变量
// 而非"文件不存在"（非正 ID 文件真实落盘仍被拒）。列表侧同判据另见
// TestCollectVersionEntries_SkipsNonPositiveIDs。
func TestFindVersionFile_RejectsNonNumericVersionID(t *testing.T) {
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	if tnt == nil {
		t.Fatal("创建 alice 租户失败")
	}
	verRel, ok := tnt.FeatureRel("version", "f.txt")
	if !ok {
		t.Fatal("FeatureRel(version, f.txt) 失败")
	}
	if err := tnt.Root().MkdirAll(verRel, 0o755); err != nil {
		t.Fatal(err)
	}
	const goodID = "1000000000001"
	f, err := tnt.Root().OpenFile(verRel+"/"+goodID, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("写版本文件: %v", err)
	}
	_, _ = f.Write([]byte("v1"))
	_ = f.Close()

	// 哨兵：同租户 meta 桶内的文件（越界版本 ID 拼出的落点）。
	if mkErr := tnt.Root().MkdirAll("meta", 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	sf, sErr := tnt.Root().OpenFile("meta/sentinel.txt", os.O_CREATE|os.O_WRONLY, 0o644)
	if sErr != nil {
		t.Fatalf("写哨兵: %v", sErr)
	}
	_, _ = sf.Write([]byte("sentinel"))
	_ = sf.Close()

	// 正向：合法数值 ID 命中，且返回的 rel 就是传进去的那个 ID。
	loc, rel, info, found, ferr := env.svc.FindVersionFile("alice", "f.txt", goodID)
	if ferr != nil || !found || loc == nil {
		t.Fatalf("合法 version_id 应命中: found=%v err=%v", found, ferr)
	}
	if rel != verRel+"/"+goodID || info == nil || info.Size() != 2 {
		t.Fatalf("命中结果异常: rel=%q size=%v", rel, info)
	}

	// (A) 穿越与畸形形态：一律 not-found，且**挡它的规则逐条可核**——这些形态连
	// ParseInt(base=10) 都过不了（base=10 不认 `_`、不 TrimSpace、只收 `[+-]?[0-9]`），
	// 故**不可能**拼出多段路径。下面同时断言"该形态确实被 ParseInt 拒"：若某形态其实
	// 能通过，说明本用例/注释声称的规则写错了，测试立即红（而不是靠"应该够"）。
	traversal := []struct{ name, id string }{
		{"父目录（POSIX 分隔符）", "../../meta/sentinel.txt"},
		{"父目录（Windows 分隔符）", `..\..\meta\sentinel.txt`},
		{"父目录嵌在数字之后", "1000000000001/../../meta/sentinel.txt"},
		{"单独父目录", ".."},
		{"多段路径", "a/b"},
		{"绝对路径", "/abs"},
		{"百分号编码斜杠", "..%2f.."},
		{"前导空格", " 5"},
		{"尾随换行", "5\n"},
		{"尾随制表符", "5\t"},
		{"内嵌空格", "5 5"},
		{"空字节", "5\x00"},
		{"十六进制", "0x10"},
		{"科学计数", "1e3"},
		{"下划线分组", "1_000"},
		{"非数字", "abc"},
		{"空串", ""},
		{"超长（int64 溢出）", "99999999999999999999999"},
	}
	for _, tc := range traversal {
		if _, perr := strconv.ParseInt(tc.id, 10, 64); perr == nil {
			t.Fatalf("形态 %s（%q）本应被 ParseInt 拒绝却通过了——「挡它的规则」写错（报告/注释需订正）", tc.name, tc.id)
		}
		if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", tc.id); found || err != nil {
			t.Fatalf("形态 %s（%q）应 not-found（found=false, err=nil），got found=%v err=%v", tc.name, tc.id, found, err)
		}
	}

	// (B) **领域不变量 `version > 0`**：ParseInt 能过、但**非正**的形态由「id > 0」这条规则拒绝。
	// 这些正是历史上 `UnixMilli*1000` 之前纳秒实现回绕产出的无效数据形态。
	nonPositive := []struct{ name, id string }{
		{"零", "0"},
		{"负零", "-0"},
		{"负一", "-1"},
		{"旧纳秒回绕为负", "-269429080180906331"},
	}
	for _, tc := range nonPositive {
		if _, perr := strconv.ParseInt(tc.id, 10, 64); perr != nil {
			t.Fatalf("形态 %s（%q）本应能过 ParseInt（它由「id > 0」这条规则拒绝，而非语法）", tc.name, tc.id)
		}
		if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", tc.id); found || err != nil {
			t.Fatalf("非正 version_id %s（%q）应被领域不变量拒绝（not-found），got found=%v err=%v", tc.name, tc.id, found, err)
		}
	}

	// (C) ParseInt 接受**且为正** ⇒ 走原有查找路径（此处文件不存在 → 自然 not-found）；
	// 关键是它们都是**单一路径段**（不含分隔符/父目录），故不构成穿越面。
	positive := []struct{ name, id string }{
		{"正号", "+5"},
		{"前导零", "010"},
		{"int64 最大值", "9223372036854775807"},
	}
	for _, tc := range positive {
		if _, perr := strconv.ParseInt(tc.id, 10, 64); perr != nil {
			t.Fatalf("形态 %s（%q）应通过 ParseInt（由查找路径自然 not-found）", tc.name, tc.id)
		}
		if strings.ContainsAny(tc.id, `/\`) || strings.Contains(tc.id, "..") {
			t.Fatalf("形态 %s（%q）含路径分隔符/父目录——「通过 parseVersionID 的值恒为单段」的前提被破坏", tc.name, tc.id)
		}
		if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", tc.id); found || err != nil {
			t.Fatalf("形态 %s（%q）应 not-found（文件不存在），got found=%v err=%v", tc.name, tc.id, found, err)
		}
	}

	// (D) **非正 ID 的版本文件即使真实存在于盘上，也不被承认**——证明拒绝来自领域不变量，
	// 而不是"文件不存在"这一巧合；且与列表侧同判据（见 TestCollectVersionEntries_SkipsNonPositiveIDs）。
	for _, bad := range []string{"0", "-269429080180906331"} {
		bf, bErr := tnt.Root().OpenFile(verRel+"/"+bad, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if bErr != nil {
			t.Fatalf("写非正 ID 版本文件 %q: %v", bad, bErr)
		}
		_, _ = bf.Write([]byte("invalid"))
		_ = bf.Close()
		if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", bad); found || err != nil {
			t.Fatalf("非正 ID 版本文件 %q 虽已落盘，仍应被拒（not-found），got found=%v err=%v", bad, found, err)
		}
	}
}

// TestCollectVersionEntries_SkipsNonPositiveIDs 钉住**列表侧与操作侧同判据**（`parseVersionID`）：
// 版本目录里混入的非正 ID（历史回绕产物）与非十进制名（损坏）**都不得出现在列表里**——
// 既然 `version > 0` 是领域不变量，非正条目就是无效数据，既不列出也不可操作。
func TestCollectVersionEntries_SkipsNonPositiveIDs(t *testing.T) {
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	if tnt == nil {
		t.Fatal("创建 alice 租户失败")
	}
	verRel, ok := tnt.FeatureRel("version", "f.txt")
	if !ok {
		t.Fatal("FeatureRel(version, f.txt) 失败")
	}
	if err := tnt.Root().MkdirAll(verRel, 0o755); err != nil {
		t.Fatal(err)
	}
	// 盘上混放：1 个合法正 ID + 2 个非正 ID + 2 个非十进制名（损坏）。
	names := []string{"1000000000001", "0", "-269429080180906331", "abc", "1e3"}
	for _, name := range names {
		f, fErr := tnt.Root().OpenFile(verRel+"/"+name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if fErr != nil {
			t.Fatalf("写版本文件 %q: %v", name, fErr)
		}
		_, _ = f.Write([]byte("x"))
		_ = f.Close()
	}

	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].VersionID != 1000000000001 || entries[0].Name != "1000000000001" {
		t.Fatalf("列表应只含唯一的正 ID 条目（非正/非十进制均须跳过）, got %d 条: %+v", len(entries), entries)
	}

	// 两侧一致：被列表跳过的非正 ID 在操作侧同样不可用（同一个 parseVersionID）。
	if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", "-269429080180906331"); found || err != nil {
		t.Fatalf("操作侧应与非正 ID 的列表侧一致拒绝, got found=%v err=%v", found, err)
	}
}
