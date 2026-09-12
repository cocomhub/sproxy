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
// versionIDStr 必须是**正整数**（版本 ID 的十进制形态，见 newVersionID），否则一律
// not-found（found=false, err=nil，与「版本不存在」同一条路径）。
//
// 为什么必须校验：verRel = verDir + "/" + versionIDStr 由它拼接，未校验的 "../../meta/x"
// 会越出 version/<file>/ 子目录落到同租户其它桶（os.Root 只保证不逃出**租户根**）。
// 本用例同时给出**越界读的否定证据**：meta 桶内的哨兵文件存在，仍不得被函数命中。
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

	// 反向：畸形 ID 一律 not-found（且不得因 os.Root 放行 ".." 而读到 meta 桶）。
	for _, id := range []string{
		"../../meta/sentinel.txt",
		"../../../alice/meta/sentinel.txt",
		"..",
		"-1",
		"0",
		"1e3",
		"0x10",
		"1000000000001/../../meta/sentinel.txt",
		"abc",
		"",
	} {
		if _, _, _, found, err := env.svc.FindVersionFile("alice", "f.txt", id); found || err != nil {
			t.Fatalf("畸形 version_id %q 应为 not-found（found=false, err=nil）, got found=%v err=%v", id, found, err)
		}
	}
}
