// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// encrypted_storage_interop_test.go 锁定 **sync.EncryptedFS 与 storage.EncryptedRoot 的密文互通性**。
//
// 两者使用**相同魔数**（`SPROXY-FS-ENC-v1`）⇒ 属**同一密钥域**：同一目录下由任一侧写出的密文，
// 另一侧都必须能解出原文。此前这个保证靠「两份源码必须逐字一致」的源码级门禁维持（R57 旧编号，
// 已废弃——那是重复的症状）：一旦两侧实现分叉，用户会看到「加密卷换一层包装就打不开」。
//
// 本用例用**真实加解密**（双向）把契约钉死，与源码文本无关：任何一侧改动帧布局/魔数/块大小
// 都会在这里红，且失败信息直接指出是哪个方向不通。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/storage"
)

func interopPlain(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((i * 7) % 253)
	}
	return b
}

func TestEncryptedFS_StorageInterop_BothDirections(t *testing.T) {
	t.Parallel()
	key := bytes.Repeat([]byte{0x2a}, 32)
	// 跨块（>64KiB）+ 尾部残块，覆盖分块与尾块两条路径。
	plain := interopPlain((64 << 10) + 4097)
	dir := t.TempDir()
	ctx := context.Background()

	// 方向 1：sync.EncryptedFS 写 → storage.EncryptedRoot 读。
	efs := NewEncryptedFS(NewLocalFS(dir, nil), key)
	if err := efs.WriteFile(ctx, LogicalKey{}, "from_sync.bin", bytes.NewReader(plain), int64(len(plain)), 0); err != nil {
		t.Fatalf("sync 写: %v", err)
	}
	rt, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("storage.OpenRoot: %v", err)
	}
	if serr := rt.SetEncryption(key); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	rc, err := rt.OpenDecrypted("from_sync.bin")
	if err != nil {
		t.Fatalf("storage 读 sync 产物: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("方向 1 失败（sync 写 → storage 读）：%d 字节，期望 %d", len(got), len(plain))
	}

	// 方向 2：storage.EncryptedRoot 写 → sync.EncryptedFS 读。
	w, err := rt.OpenFileEncrypted("from_storage.bin", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("storage 写: %v", err)
	}
	if _, werr := w.Write(plain); werr != nil {
		t.Fatalf("写入: %v", werr)
	}
	if cerr := w.Close(); cerr != nil {
		t.Fatalf("关闭写入: %v", cerr)
	}
	_ = rt.Close()

	rc2, err := efs.OpenRead(ctx, "from_storage.bin")
	if err != nil {
		t.Fatalf("sync 读 storage 产物: %v", err)
	}
	got2, err := io.ReadAll(rc2)
	_ = rc2.Close()
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if !bytes.Equal(got2, plain) {
		t.Fatalf("方向 2 失败（storage 写 → sync 读）：%d 字节，期望 %d", len(got2), len(plain))
	}

	// 密文与明文必须不同（防「两侧都用明文」的假通过）。
	raw, err := os.ReadFile(filepath.Join(dir, "from_sync.bin"))
	if err != nil {
		t.Fatalf("读密文: %v", err)
	}
	if bytes.Contains(raw, plain[:256]) {
		t.Fatalf("密文中出现明文片段 —— 加密未生效")
	}
}
