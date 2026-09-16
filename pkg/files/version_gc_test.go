// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// version_gc_test.go 覆盖版本 GC 的**保留期维度**（`versioning.retention`）：
// `cleanupOldVersions` 在 max_versions 截断之外，按版本创建时间删除超过保留期的旧版本。
//
// 版本 ID = 毫秒时间戳×1000 + 3 位随机后缀（见 newVersionID），`VersionIDTime(id)` 可还原
// 时间。测试夹具直接构造 version/<file>/<id> 目录项（id 用历史毫秒时间戳编码），绕开对
// newVersionID 当前时间的依赖——保留期判定的正确性完全由「目录项文件名 → 时间」这条语义
// 决定，因此夹具必须从磁盘目录项出发，而不是经 SaveVersion 生成（后者无法控制时间）。

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// writeVersionFileAt 以指定版本 ID 写一个版本目录项（并给出版本内容大小）。
func writeVersionFileAt(t *testing.T, env *dirsEnv, owner, file string, versionID int64, content string) {
	t.Helper()
	tnt := env.tenantFor(owner)
	if tnt == nil || tnt.Root() == nil {
		t.Fatal("租户不可用")
	}
	verRel, ok := tnt.FeatureRel("version", file)
	if !ok {
		t.Fatalf("FeatureRel(version, %s) 失败", file)
	}
	if err := tnt.Root().MkdirAll(verRel, 0o755); err != nil {
		t.Fatalf("MkdirAll 版本目录: %v", err)
	}
	verFile := verRel + "/" + strconv.FormatInt(versionID, 10)
	f, err := tnt.Root().OpenFile(verFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("写版本文件 %d: %v", versionID, err)
	}
	if _, err := f.Write([]byte(content)); err != nil {
		_ = f.Close()
		t.Fatalf("写版本内容: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("关闭版本文件: %v", err)
	}
	// 与 SaveVersion 一致：版本字节计入 version 桶（测试仅断言清理逻辑时，配额对账由
	// ReleaseVersionUsage 承担——本夹具不预记账，清理侧 ReleaseVersionUsage 对未记账的
	// 超额释放按本层实际持有量扣减（超额部分不传播），语义安全。
}

// retentionVersionID 由距今 duration 前的毫秒时间戳编码出版本 ID（newVersionID 的
// 毫秒×1000 形态，后缀取 1）。返回的 ID 满足 parseVersionID 且 VersionIDTime 可还原。
func retentionVersionID(ago time.Duration) int64 {
	ms := time.Now().Add(-ago).UnixMilli()
	return ms*1000 + 1
}

// TestGCExpiredVersions_Retention 保留期开启：超过保留期的旧版本被删除，保留期内的保留。
func TestGCExpiredVersions_Retention(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0 // 关闭上限截断，单独验证保留期维度
	env.versioningRetention = time.Hour
	env.rebuild()

	// 两个过期版本（2h/4h 前）+ 一个在保留期内（30m 前）。
	old1 := retentionVersionID(4 * time.Hour)
	old2 := retentionVersionID(2 * time.Hour)
	fresh := retentionVersionID(30 * time.Minute)
	writeVersionFileAt(t, env, "alice", "f.txt", old1, "old1")
	writeVersionFileAt(t, env, "alice", "f.txt", old2, "old2")
	writeVersionFileAt(t, env, "alice", "f.txt", fresh, "fresh")

	env.svc.cleanupOldVersions("f.txt", env.tenantFor("alice"), "alice")

	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("保留期清理后应只剩 1 个版本, got %d（全部: %+v）", len(entries), entries)
	}
	if entries[0].VersionID != fresh {
		t.Fatalf("保留的应是最新版本 %d, got %d", fresh, entries[0].VersionID)
	}
}

// TestGCExpiredVersions_RetentionDisabled 保留期关闭（0）：不按时间清理。
func TestGCExpiredVersions_RetentionDisabled(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0
	env.versioningRetention = 0 // 默认关闭
	env.rebuild()

	old1 := retentionVersionID(100 * 24 * time.Hour)
	old2 := retentionVersionID(200 * 24 * time.Hour)
	writeVersionFileAt(t, env, "alice", "f.txt", old1, "old1")
	writeVersionFileAt(t, env, "alice", "f.txt", old2, "old2")

	env.svc.cleanupOldVersions("f.txt", env.tenantFor("alice"), "alice")

	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("保留期关闭不应按时间清理, got %d", len(entries))
	}
}

// TestGCExpiredVersions_MaxVersions 保留期关闭时回归 max_versions 截断（上限清理）。
func TestGCExpiredVersions_MaxVersions(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0
	env.versioningRetention = 0
	env.rebuild()

	// 用真实 SaveVersion 攒 3 个版本（内容不同以产生 3 个版本 ID）。
	tnt := env.tenantFor("alice")
	for _, c := range []string{"v1-content", "v2-content", "v3-content"} {
		writeUserFile(t, env, "alice", "user/f.txt", c)
		if _, err := env.svc.SaveVersion("f.txt", tnt, "alice"); err != nil {
			t.Fatalf("SaveVersion(%s): %v", c, err)
		}
	}
	before, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(before) != 3 {
		t.Fatalf("前置应有 3 个版本, got %d err=%v", len(before), err)
	}

	env.versioningMaxVersions = 2
	env.svc.cleanupOldVersions("f.txt", tnt, "alice")

	after, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil || len(after) != 2 {
		t.Fatalf("上限截断后应保留 2 个版本, got %d err=%v", len(after), err)
	}
	for _, e := range after {
		if e.VersionID == before[0].VersionID {
			t.Fatalf("最旧版本 %d 应被删除", e.VersionID)
		}
	}
}

// TestGCExpiredVersions_Both 保留期 + 上限同时生效：先按保留期删，再按上限截断。
func TestGCExpiredVersions_Both(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	env := newDirsEnv(t)
	env.versioningEnabled = true
	env.versioningMaxVersions = 0
	env.versioningRetention = time.Hour
	env.rebuild()

	// 2 个过期 + 3 个在保留期内（保留期内版本按 ID 单调递增）。
	old1 := retentionVersionID(5 * time.Hour)
	old2 := retentionVersionID(3 * time.Hour)
	fresh1 := retentionVersionID(50 * time.Minute)
	fresh2 := retentionVersionID(40 * time.Minute)
	fresh3 := retentionVersionID(30 * time.Minute)
	writeVersionFileAt(t, env, "alice", "f.txt", old1, "old1")
	writeVersionFileAt(t, env, "alice", "f.txt", old2, "old2")
	writeVersionFileAt(t, env, "alice", "f.txt", fresh1, "fresh1")
	writeVersionFileAt(t, env, "alice", "f.txt", fresh2, "fresh2")
	writeVersionFileAt(t, env, "alice", "f.txt", fresh3, "fresh3")

	// 先开上限 2：预期先删 2 个过期（保留期），再在 3 个保留期内截断到最新的 2 个。
	env.versioningMaxVersions = 2
	env.svc.cleanupOldVersions("f.txt", env.tenantFor("alice"), "alice")

	entries, err := env.svc.CollectVersionEntries("alice", "f.txt")
	if err != nil {
		t.Fatalf("CollectVersionEntries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("双维清理后应剩 2 个版本, got %d（全部: %+v）", len(entries), entries)
	}
	for _, e := range entries {
		if e.VersionID == old1 || e.VersionID == old2 {
			t.Fatalf("过期版本 %d 应被保留期清理删除", e.VersionID)
		}
		if e.VersionID == fresh1 {
			t.Fatalf("最旧保留期内版本 %d 应被上限截断删除", e.VersionID)
		}
	}
}
