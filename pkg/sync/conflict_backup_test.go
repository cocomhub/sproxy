// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"strings"
	"testing"
)

// 保留败方副本（keep-both 防静默丢失）引擎测试：
// 覆盖型策略（overwrite/lww/merge3 二进制回退）在覆盖前检测到目标与源存在分歧改动时，
// 旧目标以 `<dst>.conflict-<ts>` 副本保留 + 登记 ConflictRecord；无分歧不备份（防噪）。

// collectRecorder 收集引擎回调的冲突/备份登记。
func collectRecorder() (func(ConflictRecord), *[]ConflictRecord) {
	var recs []ConflictRecord
	return func(r ConflictRecord) { recs = append(recs, r) }, &recs
}

// readMockFS 读 mockFS 中条目的内容字符串（仅文件；目录/symlink 报错）。
func readMockFS(dst *mockFS, p string) string {
	dst.mu.Lock()
	defer dst.mu.Unlock()
	e, _ := dst.resolvePath(p)
	if e == nil || e.IsDir || e.IsSymlink {
		return ""
	}
	return string(e.Data)
}

// mockKeys 返回 mockFS 全部条目键（诊断用）。
func mockKeys(m *mockFS) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.entries))
	for k := range m.entries {
		out = append(out, k)
	}
	return out
}

// findBackup 在 dst FS 根下找 `<base>.conflict-*` 副本，返回（相对路径, 是否找到）。
func findBackup(t *testing.T, dst *mockFS, base string) (string, bool) {
	t.Helper()
	for _, k := range mockKeys(dst) {
		if strings.HasPrefix(k, base+".conflict-") {
			return k, true
		}
	}
	return "", false
}

// TestEngineSync_OverwriteDivergent_KeepsLoserBackup 覆盖型策略（overwrite）分歧：
// 源与目标都改过（内容不同）→ 旧目标保留为 .conflict-* 副本 + 登记，
// 新内容落主路径；副本内容 = 旧目标内容。
func TestEngineSync_OverwriteDivergent_KeepsLoserBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	src := newMockFS()
	src.setFile("a.txt", []byte("v2-src"), 2000)
	dst := newMockFS()
	dst.setFile("a.txt", []byte("v1-dst"), 1000)

	rec, recs := collectRecorder()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	eng := &Engine{Concurrency: 1, ConflictRecorder: rec, BackupLoser: true}
	if err := eng.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// 主路径 = 源内容
	if got := readMockFS(dst, "a.txt"); got != "v2-src" {
		t.Fatalf("overwrite 后主路径应为 v2-src，got %q", got)
	}
	// 副本存在且内容 = 旧目标
	backup, ok := findBackup(t, dst, "a.txt")
	if !ok {
		t.Fatalf("分歧覆盖应保留败方副本 a.txt.conflict-*，entries=%v", mockKeys(dst))
	}
	if got := readMockFS(dst, backup); got != "v1-dst" {
		t.Fatalf("副本内容应为 v1-dst，got %q", got)
	}
	// 登记
	if len(*recs) != 1 {
		t.Fatalf("应登记 1 条 ConflictRecord，got %d", len(*recs))
	}
	r := (*recs)[0]
	if r.Path != "a.txt" || r.Kind != "backup_loser" {
		t.Fatalf("登记字段不符: %+v", r)
	}
	if r.BackupPath != backup || r.BaseSHA != conflictSHA256Hex([]byte("v1-dst")) {
		t.Fatalf("登记副本路径/BaseSHA 不符: %+v", r)
	}
}

// TestEngineSync_OverwriteSame_NoBackup 无分歧（内容一致仅 mtime 不同）：覆盖后
// 不产生副本（防噪）。
func TestEngineSync_OverwriteSame_NoBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	src := newMockFS()
	src.setFile("a.txt", []byte("same"), 2000)
	dst := newMockFS()
	dst.setFile("a.txt", []byte("same"), 1000)

	rec, recs := collectRecorder()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	eng := &Engine{Concurrency: 1, ConflictRecorder: rec, BackupLoser: true}
	if err := eng.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if _, ok := findBackup(t, dst, "a.txt"); ok {
		t.Fatalf("内容一致（无分歧）不应产生副本")
	}
	if len(*recs) != 0 {
		t.Fatalf("无分歧不应登记，got %d", len(*recs))
	}
}

// TestEngineSync_LWWDivergent_KeepsLoserBackup lww 策略下 src 胜（mtime 更新）且
// 内容分歧 → 旧目标保留副本。
func TestEngineSync_LWWDivergent_KeepsLoserBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	src := newMockFS()
	src.setFile("a.txt", []byte("v2-src"), 3000)
	dst := newMockFS()
	dst.setFile("a.txt", []byte("v1-dst"), 1000)

	rec, recs := collectRecorder()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictLWW}
	eng := &Engine{Concurrency: 1, ConflictRecorder: rec, BackupLoser: true}
	if err := eng.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if got := readMockFS(dst, "a.txt"); got != "v2-src" {
		t.Fatalf("lww src 胜主路径应为 v2-src，got %q", got)
	}
	backup, ok := findBackup(t, dst, "a.txt")
	if !ok {
		t.Fatalf("lww 分歧覆盖应保留副本，entries=%v", mockKeys(dst))
	}
	if got := readMockFS(dst, backup); got != "v1-dst" {
		t.Fatalf("副本内容应为 v1-dst，got %q", got)
	}
	if len(*recs) != 1 {
		t.Fatalf("lww 分歧应登记 1 条，got %d", len(*recs))
	}
}

// TestEngineSync_BackupDisabled_NoBackup BackupLoser=false（默认零回归）→ 不保留副本。
func TestEngineSync_BackupDisabled_NoBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	src := newMockFS()
	src.setFile("a.txt", []byte("v2-src"), 2000)
	dst := newMockFS()
	dst.setFile("a.txt", []byte("v1-dst"), 1000)

	rec, recs := collectRecorder()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictOverwrite}
	eng := &Engine{Concurrency: 1, ConflictRecorder: rec, BackupLoser: false}
	if err := eng.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	if _, ok := findBackup(t, dst, "a.txt"); ok {
		t.Fatalf("BackupLoser=false 不应产生副本")
	}
	if len(*recs) != 0 {
		t.Fatalf("关闭备份不应登记，got %d", len(*recs))
	}
}

// TestEngineSync_Merge3Binary_KeepsLoserBackup merge3 二进制（NUL 字节）回退整文件
// 复制：旧目标不再被删除，保留为 .conflict-* 副本 + 登记（静默丢失修复）。
func TestEngineSync_Merge3Binary_KeepsLoserBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	src := newMockFS()
	src.setFile("bin.dat", []byte("v2\x00src"), 2000)
	dst := newMockFS()
	dst.setFile("bin.dat", []byte("v1\x00dst"), 1000)

	rec, recs := collectRecorder()
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", Recursive: true, ConflictPolicy: ConflictMerge3}
	eng := &Engine{Concurrency: 1, ConflictRecorder: rec, BackupLoser: true}
	if err := eng.Sync(context.Background(), src, dst, job); err != nil {
		t.Fatalf("Sync error: %v", err)
	}
	// 胜出版本 = 源（整文件复制语义）
	if got := readMockFS(dst, "bin.dat"); got != "v2\x00src" {
		t.Fatalf("merge3 二进制回退主路径应为源内容 v2\\x00src，got %q", got)
	}
	backup, ok := findBackup(t, dst, "bin.dat")
	if !ok {
		t.Fatalf("merge3 二进制回退应保留败方副本，entries=%v", mockKeys(dst))
	}
	if got := readMockFS(dst, backup); got != "v1\x00dst" {
		t.Fatalf("副本内容应为 v1\\x00dst，got %q", got)
	}
	if len(*recs) != 1 {
		t.Fatalf("应登记 1 条 ConflictRecord（二进制回退），got %d", len(*recs))
	}
	if (*recs)[0].Kind != "backup_loser" || (*recs)[0].Path != "bin.dat" {
		t.Fatalf("登记字段不符: %+v", (*recs)[0])
	}
}

// TestEngineSync_Merge3TextNoConflict_NoBackup merge3 文本可合并且无冲突 → 不备份
// （正常自动合并路径零回归）。
func TestEngineSync_Merge3TextNoConflict_NoBackup(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	srcRoot := t.TempDir()
	dstRoot := t.TempDir()
	writeTestFile(t, dstRoot, "a.txt", "a\nb\nc\n")
	setTestMTime(t, dstRoot, "a.txt", 1000000)
	writeTestFile(t, srcRoot, "a.txt", "a\nB\nc\n")

	rec, recs := collectRecorder()
	eng := &Engine{Concurrency: 2, ConflictRecorder: rec, BackupLoser: true}
	job := &Job{Direction: DirectionPush, Src: "", Dst: "", ConflictPolicy: ConflictMerge3}
	if err := eng.Sync(context.Background(), NewLocalFS(srcRoot, nil), NewLocalFS(dstRoot, nil), job); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(*recs) != 0 {
		t.Fatalf("无冲突自动合并不应登记，got %d", len(*recs))
	}
}

// TestEngineSync_Merge3TextConflict_RecordsOnly 已被 #464 已知模型限制取代：
// 单向 sync 无三方祖先（theirs=base），merge3 单侧修改不触发冲突（conflicted=false），
// 不登记冲突也不产生副本——该语义由上方 Merge3TextNoConflict_NoBackup 与既有
// merge3_engine_test 覆盖；真三方冲突登记见 pkg/syncmgr/conflict_index_test.go。
