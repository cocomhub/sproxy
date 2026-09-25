// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// audit_rotation_test.go 钉住审计日志轮转（roadmap 11.5-⑥，audit.max_size +
// audit.max_archives）：
//  1. 边界：size 恰 == maxSize 不轮转、> maxSize 轮转（变异：`>`/`>=` 互换 → 红）。
//  2. 轮转后新事件进新文件，旧事件仍在归档文件（变异：漏 Rename → 红）。
//  3. maxArchives 修剪：N+1 次轮转后最旧档被删（变异：不删 → 红）。
//  4. max_size=0 恒不轮转（零回归断言；变异：无条件轮转 → 红）。
//  5. 轮转后 NewAuditStore 只载入新文件历史，Len 不含归档（变异：load 读归档 → 红）。
//  6. 并发 Append + 轮转内存不丢行（-race，≥1000 事件）+ 磁盘归档有界。

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestAuditStoreRotating 创建启用了轮转的 AuditStore（maxSize 字节阈值）。
func newTestAuditStoreRotating(t *testing.T, maxSize int64, maxArchives int) *AuditStore {
	t.Helper()
	dir := t.TempDir()
	st, err := NewAuditStore(filepath.Join(dir, "audit.log"), testLogger(), AuditRotationConfig{
		MaxSize:     maxSize,
		MaxArchives: maxArchives,
	})
	if err != nil {
		t.Fatalf("NewAuditStore(rotating): %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// auditEventOfLen 构造固定 actor 长度的事件（控制单行落盘字节数）。
// TS 用固定纳秒值：RFC3339Nano 对尾零省略，行宽必须完全确定。
func auditEventOfLen(n int) AuditEvent {
	return AuditEvent{
		Action: "delete",
		Actor:  strings.Repeat("a", n),
		Object: "f.txt",
		Result: "success",
		TS:     time.Date(2026, 9, 24, 0, 0, 0, 123456789, time.UTC),
	}
}

// mustAuditSize 返回文件大小（测试辅助）。
func mustAuditSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// TestAuditRotation_BoundarySize 验证轮转边界：当前文件 size 恰 == maxSize 不轮转，
// size + incoming > maxSize 才轮转（变异：`>`/`>=` 互换 → 红）。
func TestAuditRotation_BoundarySize(t *testing.T) {
	t.Parallel()
	// 第一步：探测单行落盘字节数（独立目录）。
	probe := newTestAuditStore(t)
	_ = probe.Append(auditEventOfLen(0))
	lineLen := mustAuditSize(t, probe.logPath)
	probe.Close()

	// 阈值 = 行宽：追加 1 行恰好 == maxSize → 不轮转；再追加 1 行超限 → 轮转。
	st2 := newTestAuditStoreRotating(t, lineLen, 1)
	_ = st2.Append(auditEventOfLen(0)) // 当前文件 size == maxSize
	if _, err := os.Stat(st2.archivePath(1)); !os.IsNotExist(err) {
		t.Fatalf("size == maxSize 不应轮转，归档已存在")
	}
	if fi := mustAuditSize(t, st2.logPath); fi != lineLen {
		t.Fatalf("第一行应留在 audit.log（size=%d, want %d）", fi, lineLen)
	}
	_ = st2.Append(auditEventOfLen(0)) // size 2×lineLen > maxSize → 轮转
	if _, err := os.Stat(st2.archivePath(1)); err != nil {
		t.Fatalf("> maxSize 应轮转，归档缺失: %v", err)
	}
}

// TestAuditRotation_NewEventsGoToNewFile 验证轮转后新事件进新 audit.log，
// 旧事件仍在归档文件（变异：漏 Rename → 红）。
func TestAuditRotation_NewEventsGoToNewFile(t *testing.T) {
	t.Parallel()
	// 单行 ~147B，MaxSize=200：1 行不轮转，2 行触发轮转。
	st := newTestAuditStoreRotating(t, 200, 2)
	_ = st.Append(AuditEvent{Action: "delete", Actor: "old-a", Object: "a.txt", Result: "success", TS: time.Now()})
	if _, err := os.Stat(st.archivePath(1)); !os.IsNotExist(err) {
		t.Fatalf("1 行未超限不应轮转，归档已存在")
	}
	_ = st.Append(AuditEvent{Action: "delete", Actor: "old-b", Object: "b.txt", Result: "success", TS: time.Now()})
	// 第二次 append 超限：audit.log → .1（old-a），新文件写 old-b。
	if _, err := os.Stat(st.archivePath(1)); err != nil {
		t.Fatalf("轮转后应有归档 audit.log.1: %v", err)
	}
	assertFileContains(t, st.archivePath(1), "old-a")
	assertFileContains(t, st.logPath, "old-b")

	_ = st.Append(AuditEvent{Action: "delete", Actor: "new-c", Object: "c.txt", Result: "success", TS: time.Now()})
	_ = st.Append(AuditEvent{Action: "delete", Actor: "new-d", Object: "d.txt", Result: "success", TS: time.Now()})
	// 第二次轮转：.1（old-b）→ .2；audit.log（new-c）→ .1；新文件写 new-d。
	assertFileContains(t, st.archivePath(2), "old-b")
	assertFileContains(t, st.archivePath(1), "new-c")
	assertFileContains(t, st.logPath, "new-d")
	// 内存全量不变（旧 + 新都在查询面）。
	if got := st.Len(); got != 4 {
		t.Errorf("轮转后内存事件数 = %d, want 4", got)
	}
}

// assertFileContains 断言文件包含子串（测试辅助）。
func assertFileContains(t *testing.T, path, substr string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", path, err)
	}
	if !strings.Contains(string(raw), substr) {
		t.Errorf("%s 应包含 %q，实际:\n%s", path, substr, string(raw))
	}
}

// TestAuditRotation_MaxArchivesPrune 验证 maxArchives 修剪：轮转次数超过保留份数后
// 最旧档被删（变异：不删最旧 → 红）。
func TestAuditRotation_MaxArchivesPrune(t *testing.T) {
	t.Parallel()
	st := newTestAuditStoreRotating(t, 100, 2)
	// 每轮 append 3 行（3×~147 > 100）触发轮转；共 4 轮 → 归档只保留最近 2 份。
	for range 4 {
		for range 3 {
			_ = st.Append(auditEventOfLen(40))
		}
	}
	if _, serr := os.Stat(st.archivePath(1)); serr != nil {
		t.Fatalf("archive.1 缺失: %v", serr)
	}
	if _, serr := os.Stat(st.archivePath(2)); serr != nil {
		t.Fatalf("archive.2 缺失: %v", serr)
	}
	if _, err := os.Stat(st.archivePath(3)); !os.IsNotExist(err) {
		t.Errorf("最旧归档 .3 应被修剪删除, 实际存在")
	}
}

// TestAuditRotation_ZeroMaxSizeNeverRotates 验证 max_size=0 恒不轮转（零回归；
// 变异：无条件轮转 → 红）。
func TestAuditRotation_ZeroMaxSizeNeverRotates(t *testing.T) {
	t.Parallel()
	st := newTestAuditStore(t) // 默认无轮转
	for range 20 {
		_ = st.Append(auditEventOfLen(64))
	}
	if _, err := os.Stat(st.archivePath(1)); !os.IsNotExist(err) {
		t.Errorf("max_size=0 不应产生任何归档")
	}
	if fi := mustAuditSize(t, st.logPath); fi == 0 {
		t.Errorf("audit.log 应有内容")
	}
}

// TestAuditRotation_ReloadOnlyCurrentFile 验证轮转后 NewAuditStore 只载入当前
// audit.log 历史，Len 不含归档（变异：load 读归档 → 红）。
func TestAuditRotation_ReloadOnlyCurrentFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "audit.log")
	st, err := NewAuditStore(logPath, testLogger(), AuditRotationConfig{MaxSize: 150, MaxArchives: 2})
	if err != nil {
		t.Fatalf("NewAuditStore: %v", err)
	}
	_ = st.Append(AuditEvent{Action: "delete", Actor: "archived-1", Object: "a", Result: "success", TS: time.Now()})
	_ = st.Append(AuditEvent{Action: "delete", Actor: "archived-2", Object: "b", Result: "success", TS: time.Now()})
	_ = st.Append(AuditEvent{Action: "delete", Actor: "current-3", Object: "c", Result: "success", TS: time.Now()})
	_ = st.Append(AuditEvent{Action: "delete", Actor: "current-4", Object: "d", Result: "success", TS: time.Now()})
	// 单行 ~147B：append2 轮转（archived-1 进 .1），append4 轮转（archived-2/current-3 进归档）。
	if _, serr := os.Stat(st.archivePath(1)); serr != nil {
		t.Fatalf("轮转应已发生: %v", serr)
	}
	_ = st.Close()

	st2, err := NewAuditStore(logPath, testLogger(), AuditRotationConfig{MaxSize: 150, MaxArchives: 2})
	if err != nil {
		t.Fatalf("NewAuditStore(2): %v", err)
	}
	defer st2.Close()
	if got := st2.Len(); got != 1 {
		t.Errorf("重启后 Len = %d, want 1（只载入当前 audit.log 末行 current-4）", got)
	}
	recent := st2.Recent(10, AuditFilter{})
	for _, ev := range recent {
		if strings.HasPrefix(ev.Actor, "archived-") || ev.Actor == "current-3" {
			t.Errorf("重启后不应载入归档/已轮转事件 %q", ev.Actor)
		}
	}
}

// TestAuditRotation_ConcurrentAppend 验证并发 Append + 轮转内存不丢行（-race，
// ≥1000 事件）+ 磁盘归档有界（current + maxArchives 份）。
func TestAuditRotation_ConcurrentAppend(t *testing.T) {
	t.Parallel()
	st := newTestAuditStoreRotating(t, 1024, 5)

	const n = 1000
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = st.Append(AuditEvent{Action: "delete", Actor: strings.Repeat("x", i%20), Object: "f", Result: "success", TS: time.Now()})
		}(i)
	}
	wg.Wait()
	if got := st.Len(); got != n {
		t.Errorf("并发 append 后 Len = %d, want %d（-race 下轮转不丢行）", got, n)
	}
	// 磁盘有界：当前文件 + 最多 5 份归档（更老的已被修剪）。
	if _, err := os.Stat(st.archivePath(6)); !os.IsNotExist(err) {
		t.Errorf("归档最多保留 %d 份，.6 不应存在", 5)
	}
}
