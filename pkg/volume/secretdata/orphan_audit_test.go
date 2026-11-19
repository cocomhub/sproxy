// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// TestOrphanChunks_TreeReferencesSurviveEmptyIndex 是本轮修复的**核心回归**（2026-11-19 生产事故）：
// 生产卷按设计启动跳过全量扫描 ⇒ 内存索引为空。此时审计必须**以磁盘树为准**重建引用，
// 报告「0 个孤儿」，而不是把全部分块当作孤儿（曾误报 19,073 个 = 全卷分块总数）。
//
// 变异：把 `collectTreeReferences` 调用删掉（退回纯索引）⇒ 本用例变红（返回 1 个孤儿）。
func TestOrphanChunks_TreeReferencesSurviveEmptyIndex(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("tree-ref-"), 1200)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d3/h.bin", bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 模拟「启动跳过全量扫描」：内存索引清空。
	sfs.mu.Lock()
	sfs.index = map[string]*metaEntry{}
	sfs.mu.Unlock()

	rep, rerr := sfs.OrphanChunks(ctx)
	if rerr != nil {
		t.Fatalf("索引为空时审计应改用磁盘树（不应报错）: %v", rerr)
	}
	if len(rep.Chunks) != 0 {
		t.Fatalf("存活文件的分块不得判为孤儿，got %d 个", len(rep.Chunks))
	}
}

// TestOrphanChunks_FailsClosedOnCorruptFileMeta 锁定 fail-closed：file meta 无法解密时
// 无法判定分块死活 ⇒ 必须报错（不得给出可能误导的清单）。
func TestOrphanChunks_FailsClosedOnCorruptFileMeta(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	payload := bytes.Repeat([]byte("corrupt-"), 900)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d4/i.bin", bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	// 找一个 file meta blob 并写坏它。
	var cname, mname string
	sfs.mu.RLock()
	for k, e := range sfs.index {
		if e != nil && e.metaName != "" {
			cname, mname = e.dirSeg, e.metaName
		}
		_ = k
	}
	sfs.mu.RUnlock()
	if cname == "" || mname == "" {
		t.Skip("夹具未拿到 file meta 名称")
	}
	if werr := sfs.inner.WriteFile(ctx, syncpkg.LogicalKey{}, path.Join(cname, mname),
		bytes.NewReader([]byte("not-a-valid-sealed-meta")), 24, time.Now().Unix()); werr != nil {
		t.Fatalf("写坏 meta: %v", werr)
	}
	if _, rerr := sfs.OrphanChunks(ctx); !errors.Is(rerr, ErrOrphanAuditUnsafe) {
		t.Fatalf("meta 损坏时必须返回 ErrOrphanAuditUnsafe（实际 %v）", rerr)
	}
}

// TestOrphanChunks_ParallelTreeWalk 锁定「有界并发」正确性：多容器多文件并发重建引用后，
// 存活文件的分块一个都不得判为孤儿（并发 map 写入必须有锁，-race 下若缺失即报竞态）。
func TestOrphanChunks_ParallelTreeWalk(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh"), OrphanAuditConcurrency: 8})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	for i := range 12 {
		for j := range 2 {
			rel := fmt.Sprintf("pd%d/f%d.bin", i, j)
			body := bytes.Repeat([]byte(rel), 700)
			if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body),
				int64(len(body)), time.Now().Unix()); werr != nil {
				t.Fatalf("WriteFile %s: %v", rel, werr)
			}
		}
	}
	// 索引清空：强制走磁盘树（并发路径）。
	sfs.mu.Lock()
	sfs.index = map[string]*metaEntry{}
	sfs.mu.Unlock()

	rep, rerr := sfs.OrphanChunks(ctx)
	if rerr != nil {
		t.Fatalf("OrphanChunks: %v", rerr)
	}
	if len(rep.Chunks) != 0 {
		t.Fatalf("并发重建引用后不得有孤儿，got %d", len(rep.Chunks))
	}
}

// TestOrphanAuditConcurrencyDefault 锁定并发度默认值与覆盖规则。
func TestOrphanAuditConcurrencyDefault(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if got := sfs.orphanAuditConcurrency(); got != defaultOrphanAuditConcurrency {
		t.Fatalf("默认并发度应为 %d，got %d", defaultOrphanAuditConcurrency, got)
	}
	sfs.opts.OrphanAuditConcurrency = 5
	if got := sfs.orphanAuditConcurrency(); got != 5 {
		t.Fatalf("覆盖后应为 5，got %d", got)
	}
}

// TestOrphanChunks_UsesIndexKeysWithoutListing 锁定「快照条目无 meta 时按 dirSeg/metaName 直读」的路径：
// 生产卷快照命中（895 容器 / 916 条目）但条目 meta 为 nil ⇒ 必须靠索引键读 meta 建引用，
// **不能再做 895 次容器列举**（那是实测主要耗时）。
//
// 变异：`orphanAuditTargets` 恒返回空（回退旧列举路径）⇒ 本用例统计的「列举次数」断言变红。
func TestOrphanChunks_UsesIndexKeysWithoutListing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	cfs := &countingFS{read: map[string]int{}, wrap: syncpkg.NewLocalFS(root, nil)}
	sfs, err := NewFS(cfs,
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh"), OrphanAuditConcurrency: 4})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	for i := range 6 {
		rel := fmt.Sprintf("ik%d/g.bin", i)
		body := bytes.Repeat([]byte(rel), 600)
		if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(body),
			int64(len(body)), time.Now().Unix()); werr != nil {
			t.Fatalf("WriteFile %s: %v", rel, werr)
		}
	}
	// 模拟快照命中：条目保留 dirSeg/metaName，但冷结构 meta 置空。
	sfs.mu.Lock()
	for _, e := range sfs.index {
		if e != nil {
			e.meta = nil
		}
	}
	sfs.mu.Unlock()

	targets := sfs.orphanAuditTargets() // 必须在释放写锁后取（其内部取 RLock）
	if len(targets) != 6 {
		t.Fatalf("应得到 6 个待读 meta 的工作项，got %d", len(targets))
	}

	// 断言「只做一遍容器列举」（索引键路径的特征）：
	// 索引键路径 = 仅清扫阶段每容器 1 次列举；树推导路径 = 建引用 + 清扫 共 2 遍。
	sfs.mu.RLock()
	nc := len(sfs.gcContainersLocked())
	sfs.mu.RUnlock()
	before := cfs.listCount()
	if _, rerr := sfs.OrphanChunks(ctx); rerr != nil {
		t.Fatalf("OrphanChunks: %v", rerr)
	}
	delta := cfs.listCount() - before
	if delta != nc {
		t.Fatalf("索引键路径应只列举一遍容器（want %d，got %d；2 遍即回退到树推导）", nc, delta)
	}
}

// TestOrphanChunks_ReportsOwnerRel 锁定 owner 展示：孤儿必须带上「所属文件 rel」，
// 以便人工先验证该文件完好再删除（2026-11-20 用户要求）。
func TestOrphanChunks_ReportsOwnerRel(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil),
		Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	rel := "d5/owned.bin"
	payload := bytes.Repeat([]byte("owner-"), 900)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(payload),
		int64(len(payload)), time.Now().Unix()); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	sfs.mu.RLock()
	container := sfs.dirSegs["d5"]
	sfs.mu.RUnlock()
	if container == "" {
		t.Fatal("夹具异常：无容器")
	}
	orphan := "AAAownerTestChunkName000000000000000"
	if werr := sfs.inner.WriteFile(ctx, syncpkg.LogicalKey{}, path.Join(container, orphan),
		bytes.NewReader([]byte("x")), 1, time.Now().Unix()); werr != nil {
		t.Fatalf("写孤儿块: %v", werr)
	}
	rep, rerr := sfs.OrphanChunks(ctx)
	if rerr != nil {
		t.Fatalf("OrphanChunks: %v", rerr)
	}
	for _, b := range rep.Chunks {
		if b.Name != orphan {
			continue
		}
		if b.Owner != rel {
			t.Fatalf("owner 应为 %q，got %q", rel, b.Owner)
		}
		return
	}
	t.Fatal("未报告孤儿块")
}
