// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cloud

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestRemoveDiscardedTaskFiles_CountsOnlyActuallyGoneBytes 钉住复核 F-1 的判据：force 续传回拨的
// 字节数必须等于**确实从磁盘消失**的常规文件字节数。删除失败（Windows 句柄占用/杀软短暂持有，
// 重试耗尽后仍失败）时字节仍占磁盘，照样回拨会让账本低于磁盘（fail-open：租户短时可越过
// max_storage_bytes）；偏高只由 ≤30 min 周期扫描收敛（fail-closed）。
//
// 删除实现以参数注入（生产传 removeTaskFile）：os.Remove 的失败无法在测试里跨平台确定性制造，
// 与同包 removeTaskFile seam 同一思路——但不改包级变量，故本组用例可并行。
func TestRemoveDiscardedTaskFiles_CountsOnlyActuallyGoneBytes(t *testing.T) {
	t.Parallel()
	const (
		destBytes    = 100
		partialBytes = 10
		etagBytes    = 5
	)
	t.Run("全部删除成功时按三者字节和对账", func(t *testing.T) {
		t.Parallel()
		testDiscardAllDeleted(t, destBytes, partialBytes, etagBytes)
	})
	t.Run("某个路径删除失败时不计入其字节", func(t *testing.T) {
		t.Parallel()
		testDiscardPartialFails(t, destBytes, partialBytes, etagBytes)
	})
	t.Run("全部删除失败时为 0 且产物全部保留", func(t *testing.T) {
		t.Parallel()
		testDiscardAllFails(t, destBytes, partialBytes, etagBytes)
	})
	t.Run("路径本就不存在时不尝试删除且计 0", func(t *testing.T) {
		t.Parallel()
		testDiscardMissing(t)
	})
	t.Run("非空目录删除失败时不计入（真实失败）", func(t *testing.T) {
		t.Parallel()
		testDiscardNonEmptyDir(t)
	})
}

// writeTestFile 写 n 字节空文件（t.Helper，失败直接 Fatal）。
func writeTestFile(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

// testDiscardAllDeleted 覆盖「三个产物全部删除成功」：按三者字节和对账。
func testDiscardAllDeleted(t *testing.T, destBytes, partialBytes, etagBytes int) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "f.bin")
	writeTestFile(t, dest, destBytes)
	writeTestFile(t, dest+".partial", partialBytes)
	writeTestFile(t, dest+".partial.etag", etagBytes)

	want := int64(destBytes + partialBytes + etagBytes)
	if got := removeDiscardedTaskFiles(dest, os.Remove); got != want {
		t.Fatalf("回拨字节=%d want %d", got, want)
	}
	for _, p := range []string{dest, dest + ".partial", dest + ".partial.etag"} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s 应已从磁盘消失: %v", p, err)
		}
	}
}

// testDiscardPartialFails 钉住「删除失败的 .partial 不计回拨」。
func testDiscardPartialFails(t *testing.T, destBytes, partialBytes, etagBytes int) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "f.bin")
	writeTestFile(t, dest, destBytes)
	writeTestFile(t, dest+".partial", partialBytes)
	writeTestFile(t, dest+".partial.etag", etagBytes)
	remove := func(path string) error {
		if path == dest+".partial" {
			return errors.New("sharing violation")
		}
		return os.Remove(path)
	}

	want := int64(destBytes + etagBytes)
	if got := removeDiscardedTaskFiles(dest, remove); got != want {
		t.Fatalf("回拨字节=%d want %d（删除失败的 %d 字节仍占磁盘，不得回拨）", got, want, partialBytes)
	}
	if _, err := os.Stat(dest + ".partial"); err != nil {
		t.Fatalf(".partial 应仍在盘上（删除失败）: %v", err)
	}
}

// testDiscardAllFails 全部删除失败时为 0 且产物全部保留。
func testDiscardAllFails(t *testing.T, destBytes, partialBytes, etagBytes int) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "f.bin")
	writeTestFile(t, dest, destBytes)
	writeTestFile(t, dest+".partial", partialBytes)
	writeTestFile(t, dest+".partial.etag", etagBytes)
	remove := func(string) error { return errors.New("sharing violation") }

	if got := removeDiscardedTaskFiles(dest, remove); got != 0 {
		t.Fatalf("回拨字节=%d want 0（无字节从磁盘消失）", got)
	}
	for _, p := range []string{dest, dest + ".partial", dest + ".partial.etag"} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s 应仍在盘上: %v", p, err)
		}
	}
}

// testDiscardMissing 路径本就不存在时不尝试删除且计 0。
func testDiscardMissing(t *testing.T) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "missing.bin")
	remove := func(path string) error {
		t.Errorf("不存在的路径不应尝试删除: %s", path)
		return nil
	}
	if got := removeDiscardedTaskFiles(dest, remove); got != 0 {
		t.Fatalf("回拨字节=%d（产物本就不存在 ⇒ 无可回拨字节）", got)
	}
}

// testDiscardNonEmptyDir 非空 .partial 目录删除失败时不计入字节。
func testDiscardNonEmptyDir(t *testing.T) {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "f.bin")
	dir := dest + ".partial"
	if err := os.MkdirAll(filepath.Join(dir, "child"), 0o755); err != nil {
		t.Fatalf("造非空目录: %v", err)
	}
	if got := removeDiscardedTaskFiles(dest, os.Remove); got != 0 {
		t.Fatalf("回拨字节=%d want 0（非空目录删不掉 ⇒ 无字节消失）", got)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf(".partial 目录应仍存在: %v", err)
	}
}
