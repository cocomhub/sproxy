// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestNewFS_CleansStalePlaintextTemp 钉住 Step 3 的「启动清理陈旧明文残留」。
//
// 为什么安全且必要：Step 2 起读路径**按需解密、不落任何临时文件** ⇒ temp 目录里任何
// `decrypt-*` 都只可能是**升级前崩溃**遗留的明文（旧实现把整份明文解密到 temp）⇒ 启动时清除
// 可避免明文长期驻留磁盘。写路径的临时物是**密文**（分块目录），前缀不同、不得被误删（本用例
// 同时钉住这一点）。
func TestNewFS_CleansStalePlaintextTemp(t *testing.T) {
	t.Parallel()
	tempDir := t.TempDir()
	// 模拟旧版本崩溃残留：明文临时文件 + 写路径的密文分块目录。
	stale := filepath.Join(tempDir, "decrypt-stale123")
	if err := os.WriteFile(stale, []byte("PLAINTEXT-RESIDUE"), 0o600); err != nil {
		t.Fatalf("预置残留失败: %v", err)
	}
	keepDir := filepath.Join(tempDir, "view-keepme") // 写路径产物（密文），不得删
	if err := os.MkdirAll(keepDir, 0o700); err != nil {
		t.Fatalf("预置密文目录失败: %v", err)
	}

	if _, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
	}); err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if _, serr := os.Stat(stale); serr == nil {
		t.Error("启动应清除陈旧明文解密临时文件（decrypt-*）✗")
	}
	if _, serr := os.Stat(keepDir); serr != nil {
		t.Error("不得删除写路径的临时目录（密文产物）✗")
	}
}

// TestOpenRead_RespectsMaxFileBytes 钉住 Step 3 的「读侧 MaxFileBytes 生效」：
// 写入时用较大上限，读取时用较小上限 ⇒ `OpenRead` 必须 fail-closed 且返回同一哨兵。
func TestOpenRead_RespectsMaxFileBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storageDir := t.TempDir()
	raw := syncpkg.NewLocalFS(storageDir, nil)
	payload := bytes.Repeat([]byte("maxfilebytes-"), 4096) // 52KiB

	big, err := NewFS(raw, Options{
		Secret:       []byte("test-secret-key-000"),
		Algorithm:    testAlgo,
		TempDir:      t.TempDir(),
		MaxFileBytes: 1 << 20, // 1MiB：允许写入
	})
	if err != nil {
		t.Fatalf("NewFS(big): %v", err)
	}
	if err := big.WriteFile(ctx, syncpkg.LogicalKey{}, "f.bin",
		bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// 同一底层、更小上限重新挂载 ⇒ 读必须被拒（写侧同哨兵）。
	small, serr := NewFS(raw, Options{
		Secret:       []byte("test-secret-key-000"),
		Algorithm:    testAlgo,
		TempDir:      t.TempDir(),
		MaxFileBytes: 4096, // 4KiB：小于文件 ⇒ 读侧拦截
	})
	if serr != nil {
		t.Fatalf("NewFS(small): %v", serr)
	}
	if _, rerr := small.OpenRead(ctx, "f.bin"); rerr == nil {
		t.Fatal("读侧 MaxFileBytes 应拦截超限文件 ✗")
	} else if !errors.Is(rerr, ErrMaxFileBytes) {
		t.Fatalf("应返回 ErrMaxFileBytes 哨兵（与写侧同口径），got %v", rerr)
	}
	// 上限足够时可正常读。
	okFS, oerr := NewFS(raw, Options{
		Secret:       []byte("test-secret-key-000"),
		Algorithm:    testAlgo,
		TempDir:      t.TempDir(),
		MaxFileBytes: 1 << 20,
	})
	if oerr != nil {
		t.Fatalf("NewFS(ok): %v", oerr)
	}
	rc, rerr := okFS.OpenRead(ctx, "f.bin")
	if rerr != nil {
		t.Fatalf("上限足够时应可读: %v", rerr)
	}
	_ = rc.Close()
}
