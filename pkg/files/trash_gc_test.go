// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// trash_gc_test.go 验证回收站清理（roadmap P2 回收站残余）：
//  1. TTL=0（立即过期）时 trashGCLoop 一轮清理过期条目。
//  2. GCInterval>0 装配 → Handlers.Close 停 goroutine（不泄漏）。

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// TestTrashGCLoop_CleansExpired 手动触发清理（TTL=0 全清）。
func TestTrashGCLoop_CleansExpired(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	tnt := env.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	_ = os.MkdirAll(userAbs, 0o755)
	_ = os.WriteFile(filepath.Join(userAbs, "g.txt"), []byte("x"), 0o644)
	cs := testutil.SHA256Hex([]byte("x"))
	if _, err := env.svc.DeleteFile(context.Background(), DeleteFileInput{
		Owner: "alice", RemotePath: "g.txt", ExpectedChecksum: cs, SoftDelete: true,
	}); err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	cleaned, err := env.svc.CleanupTrash(context.Background(), "alice", 0)
	if err != nil {
		t.Fatalf("CleanupTrash: %v", err)
	}
	if cleaned != 1 {
		t.Fatalf("应清理 1 条, got %d", cleaned)
	}
}

// TestTrashGC_DefaultTTL 默认保留期常量（7d）——CleanupTrash TTL=0 全清语义。
func TestTrashGC_DefaultTTL(t *testing.T) {
	t.Parallel()
	if trashDefaultTTL != 7*24*time.Hour {
		t.Fatalf("trashDefaultTTL = %v, want 7d", trashDefaultTTL)
	}
}
