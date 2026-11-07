// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package capacity

import (
	"os"
	"path/filepath"
	"testing"
)

// TestScanStorageDir_EmptyDirDoesNotScanCwd 锁定回归：ScanStorageDir("") 必须短路返回空，
// **绝不**扫描进程 cwd。
//
// 背景：filepath.EvalSymlinks("") 在 Unix 归一为 "."（symlink.go 末 `if dest == "" { dest = "." }`），
// 随后 path/filepath.WalkDir(".") 会遍历**当前工作目录**而非任何卷根——实测 sproxy 多卷装配时
// （reconcileVolumesFromDisk 对无本地根的外部卷取 RootDir=""）默认 cwd=/root（5.4G）导致启动
// 挂起 >90s（Windows 侧 EvalSymlinks("") 报错回落空串故不扫，两端不一致）。
//
// 本用例把 cwd 切到一个含标记文件的临时目录：若实现误扫 cwd，标记文件会被计入 buckets/总量。
// t.Chdir 不可并行（archcheck R18 自动豁免）。
func TestScanStorageDir_EmptyDirDoesNotScanCwd(t *testing.T) {
	dir := t.TempDir()
	// 标记文件置于 user 桶内：若实现误扫 cwd，该文件会被归入 user 桶 → buckets/总量非空。
	if err := os.MkdirAll(filepath.Join(dir, "user"), 0o755); err != nil {
		t.Fatalf("建 user 桶: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "user", "marker.bin"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写标记文件: %v", err)
	}
	t.Chdir(dir)

	buckets, totals, err := ScanStorageDir("")
	if err != nil {
		t.Fatalf(`ScanStorageDir("") err=%v`, err)
	}
	if len(buckets) != 0 {
		t.Fatalf("空 dir 不得扫描 cwd：buckets=%v", buckets)
	}
	if totals.total != 0 || totals.userFileCount != 0 {
		t.Fatalf("空 dir 不得扫描 cwd：totals=%+v", totals)
	}
}
