// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestStorageManager_TryReserve_Success(t *testing.T) {
	// 阶段一：红灯 — 功能未实现，测试应失败
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())

	err := sm.TryReserve(100, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve(100) should succeed: %v", err)
	}
	if sm.Usage() != 100 {
		t.Fatalf("expected Usage=100, got %d", sm.Usage())
	}
}

func TestStorageManager_TryReserve_ExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	err := sm.TryReserve(200, CategoryUserFiles)
	if err != ErrStorageFull {
		t.Fatalf("expected ErrStorageFull, got %v", err)
	}
	if sm.Usage() != 0 {
		t.Fatalf("expected Usage=0 after failed reserve, got %d", sm.Usage())
	}
}

func TestStorageManager_TryReserve_ZeroLimit(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 0, nil, testLogger())

	err := sm.TryReserve(1024*1024*1024, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve with 0 limit should always succeed: %v", err)
	}
}

func TestStorageManager_TryReserve_ExactFit(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	err := sm.TryReserve(100, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve(100) with limit=100 should succeed: %v", err)
	}
	// 第二次应超限
	err = sm.TryReserve(1, CategoryUserFiles)
	if err != ErrStorageFull {
		t.Fatalf("expected ErrStorageFull on second reserve, got %v", err)
	}
}

func TestStorageManager_Release(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 200, nil, testLogger())

	sm.TryReserve(100, CategoryUserFiles)
	sm.Release(50, CategoryUserFiles)

	if sm.Usage() != 50 {
		t.Fatalf("expected Usage=50 after Release, got %d", sm.Usage())
	}

	// 释放后应能再 Reserve
	err := sm.TryReserve(150, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve(150) after release should succeed: %v", err)
	}
	if sm.Usage() != 200 {
		t.Fatalf("expected Usage=200, got %d", sm.Usage())
	}
}

func TestStorageManager_DifferentCategories(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 500, nil, testLogger())

	sm.TryReserve(100, CategoryUserFiles)
	sm.TryReserve(200, CategoryCloud)
	sm.TryReserve(50, CategoryChunked)

	if sm.Usage() != 350 {
		t.Fatalf("expected Usage=350, got %d", sm.Usage())
	}
	usage := sm.UsageByCategory()
	if usage[CategoryUserFiles] != 100 {
		t.Fatalf("expected UserFiles=100, got %d", usage[CategoryUserFiles])
	}
	if usage[CategoryCloud] != 200 {
		t.Fatalf("expected Cloud=200, got %d", usage[CategoryCloud])
	}
	if usage[CategoryChunked] != 50 {
		t.Fatalf("expected Chunked=50, got %d", usage[CategoryChunked])
	}
}

func TestStorageManager_SetMaxBytes(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	sm.TryReserve(50, CategoryUserFiles)
	sm.SetMaxBytes(200)

	if sm.MaxBytes() != 200 {
		t.Fatalf("expected MaxBytes=200, got %d", sm.MaxBytes())
	}

	// 扩大后应能继续 Reserve
	err := sm.TryReserve(150, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve(150) after expanding limit should succeed: %v", err)
	}

	// 缩小限制（但仍大于当前使用量）应不影响
	sm.SetMaxBytes(250)
	if sm.MaxBytes() != 250 {
		t.Fatalf("expected MaxBytes=250, got %d", sm.MaxBytes())
	}
}

func TestStorageManager_ScanAndRecalculate(t *testing.T) {
	dir := t.TempDir()

	// 创建一些真实文件
	subDir := filepath.Join(dir, "subdir")
	if err := os.MkdirAll(subDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file1.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(subDir, "file2.txt"), []byte("world!"), 0644); err != nil {
		t.Fatal(err)
	}

	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())

	// 手动注入错误计数，然后扫描修复
	sm.TryReserve(9999, CategoryUserFiles)
	if err := sm.ScanAndRecalculate(); err != nil {
		t.Fatalf("ScanAndRecalculate failed: %v", err)
	}

	// 预期：5 (hello) + 6 (world!) = 11
	expected := int64(11)
	if sm.Usage() != expected {
		t.Fatalf("expected Usage=%d after scan, got %d", expected, sm.Usage())
	}
	usage := sm.UsageByCategory()
	if usage[CategoryUserFiles] != expected {
		t.Fatalf("expected UserFiles=%d, got %d", expected, usage[CategoryUserFiles])
	}
}

func TestStorageManager_Clear(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1000, nil, testLogger())

	sm.TryReserve(100, CategoryUserFiles)
	sm.Clear()

	if sm.Usage() != 0 {
		t.Fatalf("expected Usage=0 after Clear, got %d", sm.Usage())
	}
	usage := sm.UsageByCategory()
	for _, v := range usage {
		if v != 0 {
			t.Fatalf("expected all categories 0 after Clear")
		}
	}
}

func TestStorageManager_ConcurrentTryReserve(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1000, nil, testLogger())

	const goroutines = 10
	const perGoroutine = 50
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)

	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				if err := sm.TryReserve(1, CategoryUserFiles); err != nil {
					errs <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent TryReserve failed: %v", err)
	}

	if sm.Usage() != goroutines*perGoroutine {
		t.Fatalf("expected Usage=%d, got %d", goroutines*perGoroutine, sm.Usage())
	}
}

func TestStorageManager_ConcurrentTryReserveExceedsLimit(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	const goroutines = 10
	const perGoroutine = 20 // total 200 > 100
	var wg sync.WaitGroup
	var failCount atomic.Int32

	for range goroutines {
		wg.Go(func() {
			for range perGoroutine {
				if err := sm.TryReserve(1, CategoryUserFiles); err != nil {
					if err == ErrStorageFull {
						failCount.Add(1)
					}
				}
			}
		})
	}
	wg.Wait()

	if sm.Usage() > 100 {
		t.Fatalf("Usage should not exceed limit, got %d", sm.Usage())
	}
	if failCount.Load() == 0 {
		t.Fatal("expected some TryReserve calls to fail with ErrStorageFull")
	}
}

func TestStorageManager_ScanAndRecalculateEmptyDir(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())

	// 空目录扫描后所有计数器应为 0
	if sm.Usage() != 0 {
		t.Fatalf("expected Usage=0 for empty dir, got %d", sm.Usage())
	}
}

func TestStorageManager_ScanAndRecalculateSkipsHiddenDirs(t *testing.T) {
	dir := t.TempDir()

	// 创建隐藏目录（以 .__ 开头，但不是已知的存储目录）和普通文件
	unknownHiddenDir := filepath.Join(dir, ".__unknown_meta__")
	if err := os.MkdirAll(unknownHiddenDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unknownHiddenDir, "meta.dat"), []byte("hidden"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "visible.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	// 未知的 .__ 目录下的文件不应计入
	// visible.txt = 5 bytes
	if sm.Usage() != 5 {
		t.Fatalf("expected Usage=5 (only visible files), got %d", sm.Usage())
	}
}

func TestStorageManager_ScanAndRecalculateCountsInternalDirs(t *testing.T) {
	dir := t.TempDir()

	// 创建内部存储目录并放入文件
	chunkedDir := filepath.Join(dir, ".__chunked__")
	versionsDir := filepath.Join(dir, ".__versions__")
	cloudDir := filepath.Join(dir, ".__cloud__")
	for _, d := range []string{chunkedDir, versionsDir, cloudDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(chunkedDir, "chunk.dat"), []byte("12345"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(versionsDir, "v1.bin"), []byte("1234567890"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cloudDir, "download.zip"), []byte("abc"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}

	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())
	// chunked=5 + versions=10 + cloud=3 + user=5 = 23
	if sm.Usage() != 23 {
		t.Fatalf("expected Usage=23, got %d", sm.Usage())
	}
	u := sm.UsageByCategory()
	if u[CategoryChunked] != 5 {
		t.Fatalf("expected Chunked=5, got %d", u[CategoryChunked])
	}
	if u[CategoryVersions] != 10 {
		t.Fatalf("expected Versions=10, got %d", u[CategoryVersions])
	}
	if u[CategoryCloud] != 3 {
		t.Fatalf("expected Cloud=3, got %d", u[CategoryCloud])
	}
	if u[CategoryUserFiles] != 5 {
		t.Fatalf("expected UserFiles=5, got %d", u[CategoryUserFiles])
	}
}

func TestStorageManager_TryReserve_NegativeSize(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	// 负值应被忽略（不修改计数）
	err := sm.TryReserve(-10, CategoryUserFiles)
	if err != nil {
		t.Fatalf("TryReserve(-10) should succeed (no-op): %v", err)
	}
	if sm.Usage() != 0 {
		t.Fatalf("expected Usage=0, got %d", sm.Usage())
	}
}

func TestStorageManager_Release_NegativeSize(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 100, nil, testLogger())

	sm.TryReserve(50, CategoryUserFiles)
	sm.Release(-10, CategoryUserFiles)

	if sm.Usage() != 50 {
		t.Fatalf("expected Usage=50 after negative Release, got %d", sm.Usage())
	}
}

func TestStorageManager_Release_AllCategories(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1000, nil, testLogger())

	sm.TryReserve(100, CategoryUserFiles)
	sm.TryReserve(200, CategoryCloud)
	sm.TryReserve(300, CategoryChunked)
	sm.TryReserve(400, CategoryVersions)

	sm.Release(50, CategoryUserFiles)
	sm.Release(100, CategoryCloud)
	sm.Release(150, CategoryChunked)
	sm.Release(200, CategoryVersions)

	if sm.Usage() != 500 {
		t.Fatalf("expected Usage=500, got %d", sm.Usage())
	}
	u := sm.UsageByCategory()
	if u[CategoryUserFiles] != 50 {
		t.Fatalf("expected UserFiles=50, got %d", u[CategoryUserFiles])
	}
	if u[CategoryCloud] != 100 {
		t.Fatalf("expected Cloud=100, got %d", u[CategoryCloud])
	}
	if u[CategoryChunked] != 150 {
		t.Fatalf("expected Chunked=150, got %d", u[CategoryChunked])
	}
	if u[CategoryVersions] != 200 {
		t.Fatalf("expected Versions=200, got %d", u[CategoryVersions])
	}
}

func TestStorageManager_PeriodicScan_Stop(t *testing.T) {
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1000, nil, testLogger())

	// 验证 Stop 不 panic（多次调用安全）
	sm.Stop()
	sm.Stop()
}

func TestStoragePeriodicScan_StopsOnSignal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())

	done := make(chan struct{})
	go func() {
		sm.periodicScan()
		close(done)
	}()

	sm.Stop()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("periodicScan did not stop within 1s after Stop()")
	}
}

func TestStoragePeriodicScan_RecalculatesUsage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cs := NewChecksumStore(dir, nil)
	sm := NewStorageManager(dir, 1024*1024, cs, testLogger())

	filePath := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	cs.Set("test.txt", sha256hex([]byte("hello")))

	before := sm.Usage()
	if err := sm.ScanAndRecalculate(); err != nil {
		t.Fatal(err)
	}
	after := sm.Usage()
	if after <= before {
		t.Errorf("expected usage to increase after scan, before=%d after=%d", before, after)
	}
}

func TestStorageScanOnce_Error(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sm := NewStorageManager(dir, 1024*1024, nil, testLogger())

	// scanOnce 在空目录下不应报错
	before, after, err := sm.scanOnce()
	if err != nil {
		t.Fatalf("scanOnce on empty dir should succeed: %v", err)
	}
	if before != 0 || after != 0 {
		t.Errorf("expected 0/0 for empty dir, got %d/%d", before, after)
	}
}
