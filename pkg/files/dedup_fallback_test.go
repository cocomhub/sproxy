// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// TestService_Upload_DedupFallbackCopyOnLinkFail 钉住 FAT/exFAT 无硬链接 → 回退普通复制：
// Link 失败（注入 linkFunc 返回 ENOTSUP）→ 上传仍 200，两文件内容一致但不同 inode，
// 台账引用计数正确，配额按实际占用量计（两份物理）。
func TestService_Upload_DedupFallbackCopyOnLinkFail(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()
	// 注入 Link 失败（模拟 FAT/exFAT 无硬链接）。
	env.svc.linkFunc = func(oldRel, newRel string) error {
		return os.ErrNotExist // ENOTSUP 语义：不支持硬链接
	}

	const body = "fallback-content"
	cs := sha256Hex([]byte(body))
	rr := env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	if rr.Code != 200 {
		t.Fatalf("首个上传应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	rr = env.upload(t, "alice", "b.txt", []byte(body), cs, 0)
	if rr.Code != 200 {
		t.Fatalf("Link 失败回退复制应 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if sameInode(t, env, "alice", "user/a.txt", "alice", "user/b.txt") {
		t.Fatal("回退复制后两文件不应同 inode（独立物理文件）")
	}
	// 内容一致。
	if got := mustReadUserFile(t, env, "alice", "user/b.txt"); got != body {
		t.Fatalf("回退复制内容=%q want %q", got, body)
	}
	if got := env.dedupStoreFor("alice").RefCount(cs); got != 2 {
		t.Fatalf("回退复制后引用计数=%d want 2（台账仍登记）", got)
	}
	// 配额：复制形态下每文件独立物理 → 双计。
	size := int64(len(body))
	if got := env.quotaBucketRoot("alice", "user").Usage(); got != 2*size {
		t.Fatalf("回退复制后配额=%d want %d（两份物理）", got, 2*size)
	}
}

// TestService_Upload_DedupFallbackDelete 回退复制形态下删除：引用计数归零才真删（复制文件独立物理）。
func TestService_Upload_DedupFallbackDelete(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableDedup()
	env.enableWriteDefaults()
	env.svc.linkFunc = func(oldRel, newRel string) error { return os.ErrNotExist }

	const body = "fallback-del"
	cs := sha256Hex([]byte(body))
	env.upload(t, "alice", "a.txt", []byte(body), cs, 0)
	env.upload(t, "alice", "b.txt", []byte(body), cs, 0)

	// 删 a.txt：b.txt 应仍在（复制形态下是独立文件，但台账引用计数共享 → 删除仍走引用计数）。
	rr := httptest.NewRecorder()
	env.svc.Delete(rr, deleteReq("alice", "a.txt", cs))
	if rr.Code != 200 {
		t.Fatalf("删除引用应 200, got %d", rr.Code)
	}
	if _, err := os.Stat(filepath.Join(env.root, "alice", "user", "b.txt")); err != nil {
		t.Fatalf("删 a 后 b 应仍在: %v", err)
	}
	if got := env.dedupStoreFor("alice").RefCount(cs); got != 1 {
		t.Fatalf("删 a 后 RefCount=%d want 1", got)
	}
}
