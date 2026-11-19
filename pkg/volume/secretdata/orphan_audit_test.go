// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
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

// TestOrphanChunks_RefusesWhenIndexEmpty 锁定 fail-closed（**2026-11-19 生产实测事故**）：
// 内存索引为空时（生产卷启动跳过全量扫描），引用集合为 0 ⇒ 全部 19,073 个分块都会被判「孤儿」。
// 必须**报错**，绝不能返回"全部孤儿"的清单。
//
// 变异：删掉 `checkReferenceCoverage` 的 indexLen==0 判断 ⇒ 本用例变红（返回全部孤儿而非错误）。
func TestOrphanChunks_RefusesWhenIndexEmpty(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("idx-empty-"), 800)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d2/g.bin", bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 模拟「启动跳过全量扫描」：索引为空。
	sfs.mu.Lock()
	sfs.index = map[string]*metaEntry{}
	sfs.mu.Unlock()

	if _, rerr := sfs.OrphanChunks(ctx); !errors.Is(rerr, ErrOrphanAuditUnsafe) {
		t.Fatalf("索引为空时必须返回 ErrOrphanAuditUnsafe（实际 %v）", rerr)
	}
	// 注：GC 的既有契约是「尽力而为」（引用集合为 0 时属合法的"卷内无存活文件"场景，
	// 由 gcReconfirmContainer 重读确认后删除），不在此处加硬判据；**出清单**路径才 fail-closed。
}
