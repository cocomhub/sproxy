// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"path"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestAuditMeta_HealthyVolume 锁定健康卷的审计结论：全部 meta 可解密、无墓碑、无缺失分块。
func TestAuditMeta_HealthyVolume(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	for i := range 3 {
		rel := fmt.Sprintf("ma%d/f.bin", i)
		body := bytes.Repeat([]byte(rel), 700)
		if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body),
			int64(len(body)), time.Now().Unix()); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
	}
	rep, rerr := sfs.AuditMeta(ctx)
	if rerr != nil {
		t.Fatalf("AuditMeta: %v", rerr)
	}
	if rep.FileMetas != 3 || rep.Live != 3 {
		t.Fatalf("应读到 3 个存活 meta，got file=%d live=%d", rep.FileMetas, rep.Live)
	}
	if rep.Tombstones != 0 || rep.Unreadable != 0 || rep.MissingChunks != 0 {
		t.Fatalf("健康卷不应有墓碑/损坏/缺失: %+v", rep)
	}
	if rep.DirMetas == 0 || rep.DirUnreadable != 0 {
		t.Fatalf("目录 meta 应全部可解密: %+v", rep)
	}
}

// TestAuditMeta_DetectsMissingChunk 锁定最关键的判据：存活 meta 引用的分块被删 ⇒ 必须报出缺失。
//
// 变异：跳过引用存在性检查 ⇒ 本用例红（missing 恒 0）。
func TestAuditMeta_DetectsMissingChunk(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	rel := "mb0/g.bin"
	body := bytes.Repeat([]byte("missing-chunk-"), 900)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body),
		int64(len(body)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 物理删除一个被引用的分块（模拟数据缺口）。
	sfs.mu.RLock()
	var victim string
	for _, e := range sfs.index {
		if e != nil && e.meta != nil && len(e.meta.Chunks) > 0 {
			victim = path.Join(e.dirSeg, e.meta.Chunks[0].FileName)
			break
		}
	}
	sfs.mu.RUnlock()
	if victim == "" {
		t.Skip("夹具未拿到分块名")
	}
	if rerr := sfs.inner.Delete(ctx, victim); rerr != nil {
		t.Fatalf("删除分块: %v", rerr)
	}

	rep, aerr := sfs.AuditMeta(ctx)
	if aerr != nil {
		t.Fatalf("AuditMeta: %v", aerr)
	}
	if rep.MissingChunks == 0 {
		t.Fatal("分块被删后必须报告 MissingChunks > 0")
	}
	if len(rep.MissingSamples) == 0 {
		t.Fatal("应给出缺失样例")
	}
}

// TestAuditMeta_RefusesDuringInFlightWrite 锁定 fail-closed。
func TestAuditMeta_RefusesDuringInFlightWrite(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	sfs.mu.Lock()
	sfs.writesInFlight = 1
	sfs.mu.Unlock()
	if _, rerr := sfs.AuditMeta(context.Background()); rerr == nil {
		t.Fatal("在途写入时必须拒绝审计")
	}
}
