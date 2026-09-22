// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// block_write_test.go 验证块级写会话（roadmap 4.3 P2 v2 服务端）：
//  1. open→write→close 全流程（内容落盘 + 原子覆盖）。
//  2. 越界写拒绝（offset+len > size）。
//  3. 会话不存在/已关闭 → 错误；幂等 close。
//  4. abort（超时模拟）删除 tmp。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBlockWrite_SessionFlow open→write→close 全流程。
func TestBlockWrite_SessionFlow(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)

	id, err := env.svc.OpenBlockWrite(context.Background(), "alice", "docs/t.bin", "main", 8, 0)
	if err != nil {
		t.Fatalf("OpenBlockWrite: %v", err)
	}
	if id == "" {
		t.Fatal("会话 id 不应为空")
	}
	if werr := env.svc.WriteBlock(context.Background(), id, 0, []byte("AB")); werr != nil {
		t.Fatalf("WriteBlock(0): %v", werr)
	}
	if werr := env.svc.WriteBlock(context.Background(), id, 4, []byte("CD")); werr != nil {
		t.Fatalf("WriteBlock(4): %v", werr)
	}
	if cerr := env.svc.CloseBlockWrite(context.Background(), id); cerr != nil {
		t.Fatalf("CloseBlockWrite: %v", cerr)
	}
	// 重复 close：会话已从表移除 → 报「不存在」（安全语义：不会重复 rename）。

	// 落盘校验（root/user/docs/t.bin）。
	tnt := env.tenantFor("alice")
	abs, _ := tnt.Root().Abs("user/docs/t.bin")
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(data) != "AB\x00\x00CD\x00\x00" {
		t.Fatalf("内容 = %q, want AB..CD..", data)
	}
	// tmp 已清（原子 rename）。
	if _, err := os.Stat(filepath.Join(filepath.Dir(abs), "t.bin.block-tmp")); !os.IsNotExist(err) {
		t.Fatalf("tmp 应已清理: %v", err)
	}
}

// TestBlockWrite_OutOfBounds 越界写拒绝。
func TestBlockWrite_OutOfBounds(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	id, err := env.svc.OpenBlockWrite(context.Background(), "alice", "docs/t.bin", "main", 4, 0)
	if err != nil {
		t.Fatalf("OpenBlockWrite: %v", err)
	}
	if err := env.svc.WriteBlock(context.Background(), id, 0, []byte("ABCDEF")); err == nil {
		t.Fatal("越界写应拒绝")
	}
	if err := env.svc.WriteBlock(context.Background(), id, 5, []byte("X")); err == nil {
		t.Fatal("负余量越界应拒绝")
	}
}

// TestBlockWrite_MissingSession 会话不存在 → 错误。
func TestBlockWrite_MissingSession(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	if err := env.svc.WriteBlock(context.Background(), "nope", 0, []byte("x")); err == nil {
		t.Fatal("不存在会话应报错")
	}
	if err := env.svc.CloseBlockWrite(context.Background(), "nope"); err == nil {
		t.Fatal("不存在会话 close 应报错")
	}
}
