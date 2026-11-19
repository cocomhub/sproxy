// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"path"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestOrphanChunks_ReportsWithoutDeleting 锁定只读审计语义（用户 2026-11-18「先出清单再判断」）：
// 孤儿分块**必须被列出**，且审计**不得删除**任何数据（GC 才会删）。
//
// 变异：把收集改成直接删除 ⇒ 本用例「审计后文件仍在」断言变红。
func TestOrphanChunks_ReportsWithoutDeleting(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("orphan-audit-"), 1000)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 制造一个孤儿块：直接往该容器里放一个「看起来像分块」的 blob（无任何 meta 引用）。
	sfs.mu.RLock()
	container := sfs.dirSegs["d"]
	sfs.mu.RUnlock()
	if container == "" {
		t.Fatal("夹具异常：无容器")
	}
	orphan := "AAAorphanChunkNameForTest000000000000"
	if werr := sfs.inner.WriteFile(ctx, syncpkg.LogicalKey{}, path.Join(container, orphan),
		bytes.NewReader([]byte("deadbeef")), 8, time.Now().Unix()); werr != nil {
		t.Fatalf("写孤儿块: %v", werr)
	}

	rep, rerr := sfs.OrphanChunks(ctx)
	if rerr != nil {
		t.Fatalf("OrphanChunks: %v", rerr)
	}
	found := false
	for _, b := range rep.Chunks {
		if b.Name == orphan {
			found = true
		}
	}
	if !found {
		t.Fatalf("孤儿块未被列出（容器 %s，报告 %d 条）", container, len(rep.Chunks))
	}
	// **不得删除**：审计后该 blob 仍必须存在。
	if _, serr := sfs.inner.Stat(ctx, path.Join(container, orphan)); serr != nil {
		t.Fatalf("只读审计不得删除数据（孤儿块已消失）: %v", serr)
	}
}

// TestOrphanChunks_RefusesDuringInFlightWrite 锁定 fail-closed：在途写入时拒绝审计
// （否则「已落盘但 meta 未提交」的块会被误判为孤儿 ⇒ 人工误删）。
func TestOrphanChunks_RefusesDuringInFlightWrite(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	sfs.mu.Lock()
	sfs.writesInFlight = 1
	sfs.mu.Unlock()
	if _, rerr := sfs.OrphanChunks(context.Background()); rerr == nil {
		t.Fatal("在途写入存在时必须拒绝审计（fail-closed）")
	}
}
