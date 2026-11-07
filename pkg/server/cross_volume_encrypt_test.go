// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// cross_volume_encrypt_test.go 钉住评审第 2 轮发现的 P1 回归：`crossVolumeCopy` 经解密流复制后
// 返回的是**明文**字节数，而调用方（move/mirror）的 TOCTOU 判据用 `Stat().Size()`（加密卷为
// **密文**长度）⇒ 加密源卷的跨卷 move/mirror 恒失败。本用例锁定「复制返回长度 == 用户可见长度」
// 这一坐标配对，并验证两个方向的落盘形态。

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/cocomhub/sproxy/pkg/rootfs"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// TestCrossVolumeCopy_EncryptedCoordinates 加密源 → 明文目标 / 明文源 → 加密目标 两向。
// 变异：crossVolumeCopy 改回裸 root.Open/OpenFile → ① 断言（密文复制）与 ③（明文落盘）红；
// 调用方若改回 `srcInfo.Size()` 比较 → 生产 move 在加密源上失败（由 ① 的坐标断言守护）。
func TestCrossVolumeCopy_EncryptedCoordinates(t *testing.T) {
	t.Parallel()
	plain := []byte("cross-volume-plaintext-payload")

	// ① 加密源 → 明文目标：返回**明文**长度（与 userVisibleSize 一致）、目标落明文。
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src, err := storage.OpenRoot(srcDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src.Close() })
	dst, err := storage.OpenRoot(dstDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dst.Close() })
	if serr := src.SetEncryption(bytes.Repeat([]byte{5}, 32)); serr != nil {
		t.Fatal(serr)
	}
	if merr := src.MkdirAll("user", 0o755); merr != nil {
		t.Fatal(merr)
	}
	w, werr := src.OpenFileEncrypted("user/a.bin", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		t.Fatal(werr)
	}
	if _, werr2 := w.Write(plain); werr2 != nil {
		t.Fatal(werr2)
	}
	if cerr2 := w.Close(); cerr2 != nil {
		t.Fatal(cerr2)
	}

	n, cerr := crossVolumeCopy(context.Background(), src, dst, "user/a.bin", "user/a.bin")
	if cerr != nil {
		t.Fatalf("crossVolumeCopy: %v", cerr)
	}
	vis, verr := rootfs.UserVisibleSize(src, "user/a.bin")
	if verr != nil {
		t.Fatal(verr)
	}
	if n != vis {
		t.Fatalf("复制返回长度必须等于**用户可见长度** %d（调用方 TOCTOU 判据坐标），got %d", vis, n)
	}
	got, rerr := os.ReadFile(dstDir + "/user/a.bin")
	if rerr != nil {
		t.Fatalf("读目标: %v", rerr)
	}
	if !bytes.Equal(got, plain) {
		t.Fatalf("加密源 → 明文目标应落**明文**，got %q", got)
	}

	// ② 明文源 → 加密目标：目标落**密文**（不含明文片段），返回长度仍为明文长度。
	srcDir2, dstDir2 := t.TempDir(), t.TempDir()
	src2, err := storage.OpenRoot(srcDir2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = src2.Close() })
	dst2, err := storage.OpenRoot(dstDir2)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dst2.Close() })
	if serr := dst2.SetEncryption(bytes.Repeat([]byte{6}, 32)); serr != nil {
		t.Fatal(serr)
	}
	if merr := src2.MkdirAll("user", 0o755); merr != nil {
		t.Fatal(merr)
	}
	if werr := os.WriteFile(srcDir2+"/user/b.bin", plain, 0o644); werr != nil {
		t.Fatal(werr)
	}
	n2, cerr2 := crossVolumeCopy(context.Background(), src2, dst2, "user/b.bin", "user/b.bin")
	if cerr2 != nil {
		t.Fatalf("crossVolumeCopy(→加密): %v", cerr2)
	}
	if n2 != int64(len(plain)) {
		t.Fatalf("返回长度应为明文长度 %d, got %d", len(plain), n2)
	}
	raw, rerr2 := os.ReadFile(dstDir2 + "/user/b.bin")
	if rerr2 != nil {
		t.Fatalf("读密文目标: %v", rerr2)
	}
	if bytes.Contains(raw, []byte("cross-volume-plaintext")) {
		t.Fatalf("明文源 → 加密目标必须透明加密落盘，got %q", raw)
	}
}
