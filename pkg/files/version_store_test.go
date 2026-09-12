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
