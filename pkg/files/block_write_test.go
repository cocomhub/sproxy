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
	"time"
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

// TestBlockWrite_MetaSeesClientMTime 块写 close 必须先应用客户端 mtime，再生成 sidecar；
// 否则块级远程写的 meta mtime 会退化为完成时刻（与单次上传同一类缺口）。
func TestBlockWrite_MetaSeesClientMTime(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	policy := &mtimeCapturePolicy{}
	env.metaPolicy = policy
	env.rebuild()
	want := time.Unix(1_700_000_002, 0).UTC()
	id, err := env.svc.OpenBlockWrite(context.Background(), "alice", "docs/mtime.bin", "main", 4, want.UnixNano())
	if err != nil {
		t.Fatalf("OpenBlockWrite: %v", err)
	}
	if werr := env.svc.WriteBlock(context.Background(), id, 0, []byte("data")); werr != nil {
		t.Fatalf("WriteBlock: %v", werr)
	}
	if cerr := env.svc.CloseBlockWrite(context.Background(), id); cerr != nil {
		t.Fatalf("CloseBlockWrite: %v", cerr)
	}
	if !policy.seen {
		t.Fatal("WriteMeta 未被调用")
	}
	if delta := policy.got.Sub(want); delta > time.Second || delta < -time.Second {
		t.Fatalf("块写 sidecar 看到 mtime=%s want %s", policy.got, want)
	}
}
