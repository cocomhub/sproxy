// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// version_retention_test.go 补齐版本保留策略（`versioning.max_versions`）此前无包内覆盖的
// **实际清理**路径：`cleanupOldVersions` 超出上限时删除最旧版本并按文件大小释放 version 桶
// 占用；`SaveVersionBeforeOverwrite` 的边界分支（版本化关闭 / 租户不可用 / 路径非法 /
// 源不存在）全部为安全空操作。
//
// 此前只有 `TestCleanupOldVersions_NoMaxVersions` 覆盖「上限 <= 0 直接返回」一条分支——
// 版本保留这个用户可见功能（磁盘回收 + 配额回收）在域包内没有被验证过。

import (
	"net/http/httptest"
	"testing"
)

// TestService_CleanupOldVersions_DeletesOldestAndReleasesUsage 覆盖真实清理：上限设为 2 时
// 从 3 个版本中删除最旧的一个，且 version 桶已确认占用按被删版本大小回收。
func TestService_CleanupOldVersions_DeletesOldestAndReleasesUsage(t *testing.T) {
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0 // 先关闭自动清理，攒够 3 个版本
	env.enableWriteDefaults()

	tnt := env.tenantFor("alice")
	contents := []string{"v1-content", "v2-content", "v3-content"}
	for _, c := range contents {
		writeUserFile(t, env, "alice", "user/f.txt", c)
		if _, err := env.svc.SaveVersion("f.txt", tnt, "alice"); err != nil {
			t.Fatalf("SaveVersion(%s): %v", c, err)
		}
	}
	before, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(before) != 3 {
		t.Fatalf("前置应有 3 个版本, got %d err=%v", len(before), err)
	}

	scope := env.quotaScopeFor("alice", "version")
	if scope == nil {
		t.Fatal("version 桶 Scope 应可用")
	}
	const each = int64(10) // 每个版本内容 10 字节
	if got := scope.Usage(); got != 3*each {
		t.Fatalf("前置 version 桶占用=%d want %d", got, 3*each)
	}

	env.versioningMaxVersions = 2
	env.svc.cleanupOldVersions("f.txt", tnt, "alice")

	after, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(after) != 2 {
		t.Fatalf("清理后应保留 2 个版本, got %d err=%v", len(after), err)
	}
	// 被删的必须是最旧的那个（版本 ID 最大者保留）。
	for _, e := range after {
		if e.VersionID == before[0].VersionID {
			t.Fatalf("最旧版本 %d 应被删除", e.VersionID)
		}
	}
	if got := scope.Usage(); got != 2*each {
		t.Fatalf("清理后 version 桶占用=%d want %d（按被删版本大小回收）", got, 2*each)
	}
}

// TestService_CleanupOldVersions_NotExceedingLimitKeepsAll 覆盖「未超上限不删除」：
// 版本数 <= max_versions 时目录与占用都不变。
func TestService_CleanupOldVersions_NotExceedingLimitKeepsAll(t *testing.T) {
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0
	env.enableWriteDefaults()

	tnt := env.tenantFor("alice")
	writeUserFile(t, env, "alice", "user/f.txt", "only-v1")
	if _, err := env.svc.SaveVersion("f.txt", tnt, "alice"); err != nil {
		t.Fatalf("SaveVersion: %v", err)
	}

	env.versioningMaxVersions = 5
	env.svc.cleanupOldVersions("f.txt", tnt, "alice")

	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(entries) != 1 {
		t.Fatalf("未超上限应保留 1 个版本, got %d err=%v", len(entries), err)
	}
}

// TestService_SaveVersionBeforeOverwrite_Guards 覆盖覆盖写前置备份的四条安全空操作：
// 版本化关闭、租户不可用、路径非法、源文件不存在——都不得产生版本或 panic。
func TestService_SaveVersionBeforeOverwrite_Guards(t *testing.T) {
	req := httptest.NewRequest("POST", "/upload", nil)

	// 版本化关闭：直接返回（不创建任何版本）。
	offEnv := newDirsEnv(t)
	offEnv.versioningEnabled = false
	offEnv.enableWriteDefaults()
	offEnv.svc.SaveVersionBeforeOverwrite(req, "f.txt", offEnv.tenantFor("alice"))
	if entries, err := offEnv.svc.CollectVersionEntries("alice", "f.txt"); err != nil || len(entries) != 0 {
		t.Fatalf("版本化关闭不应保存版本, got %d err=%v", len(entries), err)
	}

	onEnv := newDirsEnv(t)
	onEnv.versioningEnabled = true
	onEnv.enableWriteDefaults()
	alice := onEnv.tenantFor("alice")

	onEnv.svc.SaveVersionBeforeOverwrite(req, "f.txt", nil)             // 租户不可用
	onEnv.svc.SaveVersionBeforeOverwrite(req, ".__internal.txt", alice) // 路径非法（UserRel 拒绝）
	onEnv.svc.SaveVersionBeforeOverwrite(req, "missing.txt", alice)     // 源文件不存在

	entries, err := onEnv.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("三条空操作分支都不应产生版本, got %d", len(entries))
	}
}
