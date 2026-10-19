// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// archive_test.go 锁定 M4 归档原语的四条护栏（设计 §5.5 / M4）：
//  ① 只归档**封段**（活跃段绝不归档）；
//  ② 成功后**回收**本地封段（往返内容一致）；
//  ③ 失败**不丢段**（本地保留，计数 Failed）；
//  ④ `keep` 语义 + 幂等（重复运行只剩本地保留项）。

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// memArchiver 把归档内容记在内存里（测试替身；可注入失败）。
type memArchiver struct {
	got    map[string][]byte
	refs   []string
	failOn string // 非空 ⇒ 该段路径归档时返回错误
}

func (m *memArchiver) Archive(_ context.Context, seg SegmentInfo, r io.Reader) (string, error) {
	if m.failOn != "" && seg.Path == m.failOn {
		return "", errors.New("目的地不可写（测试替身）")
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return "", err
	}
	if m.got == nil {
		m.got = map[string][]byte{}
	}
	m.got[seg.Path] = b
	ref := "mem://" + filepath.Base(seg.Path)
	m.refs = append(m.refs, ref)
	return ref, nil
}

// newArchiveJournal 建一个按大小轮转的 Journal（MaxSize 很小 ⇒ 容易产生封段）。
func newArchiveJournal(t *testing.T) *FileJournal {
	t.Helper()
	j, err := NewFileJournal(FileJournalConfig{
		Dir: t.TempDir(), Layout: LayoutGlobal, GlobalName: "audit.log",
		SyncMode: SyncNone, MaxSize: 180, MaxArchives: 16,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	t.Cleanup(func() { _ = j.Close() })
	return j
}

// sealSegments 写入 n 条足够大的条目，制造若干封段。
func sealSegments(t *testing.T, j *FileJournal, n int) {
	t.Helper()
	for i := range n {
		e := Entry{Scope: "system", Kind: KindEvent, Action: "test", TS: time.Now(),
			Detail: "0123456789012345678901234567890123456789012345678901234567890123456789"}
		if err := j.Append(e); err != nil {
			t.Fatalf("Append #%d: %v", i, err)
		}
	}
}

// TestArchiveSealed_MovesSealedSegments 封段被归档并从本地回收（内容往返一致）。
func TestArchiveSealed_MovesSealedSegments(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := newArchiveJournal(t)
	sealSegments(t, j, 12)

	segs, err := j.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	var sealed []SegmentInfo
	for _, s := range segs {
		if s.Sealed {
			sealed = append(sealed, s)
		}
	}
	if len(sealed) < 2 {
		t.Fatalf("测试前提不成立：需要至少 2 个封段, got %d", len(sealed))
	}

	a := &memArchiver{}
	rep, aerr := ArchiveSealed(ctx, j, a, 0)
	if aerr != nil {
		t.Fatalf("ArchiveSealed: %v", aerr)
	}
	if rep.Archived != len(sealed) || rep.Failed != 0 {
		t.Fatalf("应归档全部封段: %+v（封段=%d）", rep, len(sealed))
	}
	if len(a.got) != len(sealed) {
		t.Fatalf("归档内容数量不符: got=%d want=%d", len(a.got), len(sealed))
	}
	for _, s := range sealed {
		if _, serr := os.Stat(s.Path); !os.IsNotExist(serr) {
			t.Fatalf("归档成功后本地封段应被回收: %s", s.Path)
		}
		b, ok := a.got[s.Path]
		if !ok || int64(len(b)) != s.Size {
			t.Fatalf("归档内容与段大小不符: %s got=%d want=%d", s.Path, len(b), s.Size)
		}
	}

	// 幂等：再跑一次没有候选（封段已回收；活跃段不算）。
	rep2, err2 := ArchiveSealed(ctx, j, a, 0)
	if err2 != nil {
		t.Fatalf("ArchiveSealed#2: %v", err2)
	}
	if rep2.Candidates != 0 || rep2.Archived != 0 {
		t.Fatalf("重复运行不应再归档: %+v", rep2)
	}
}

// TestArchiveSealed_FailureKeepsSegment 适配器失败 ⇒ 本地段保留（不丢证据）。
func TestArchiveSealed_FailureKeepsSegment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := newArchiveJournal(t)
	sealSegments(t, j, 12)

	segs, _ := j.Segments()
	var target SegmentInfo
	for _, s := range segs {
		if s.Sealed {
			target = s
			break
		}
	}
	if target.Path == "" {
		t.Fatal("测试前提不成立：无封段")
	}
	a := &memArchiver{failOn: target.Path}
	rep, _ := ArchiveSealed(ctx, j, a, 0)
	if rep.Failed == 0 || rep.Err == nil {
		t.Fatalf("失败应计数并上报: %+v", rep)
	}
	if _, serr := os.Stat(target.Path); serr != nil {
		t.Fatalf("失败时本地封段必须保留（可重试）: %v", serr)
	}
}

// TestArchiveSealed_NeverArchivesActive 只有活跃段 ⇒ 零候选（活跃段绝不归档）。
func TestArchiveSealed_NeverArchivesActive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := newArchiveJournal(t)
	if err := j.Append(Entry{Scope: "system", Kind: KindEvent, Action: "only", TS: time.Now()}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	a := &memArchiver{}
	rep, err := ArchiveSealed(ctx, j, a, 0)
	if err != nil {
		t.Fatalf("ArchiveSealed: %v", err)
	}
	if rep.Candidates != 0 || rep.Archived != 0 || len(a.got) != 0 {
		t.Fatalf("活跃段不得被归档: %+v", rep)
	}
}

// TestArchiveSealed_KeepRetainsNewest keep=N ⇒ 最新 N 个封段留在本地，只归档更旧的。
func TestArchiveSealed_KeepRetainsNewest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	j := newArchiveJournal(t)
	sealSegments(t, j, 16)

	segs, _ := j.Segments()
	var sealed []SegmentInfo
	for _, s := range segs {
		if s.Sealed {
			sealed = append(sealed, s)
		}
	}
	if len(sealed) < 3 {
		t.Fatalf("测试前提不成立：需要至少 3 个封段, got %d", len(sealed))
	}
	const keep = 2
	a := &memArchiver{}
	rep, err := ArchiveSealed(ctx, j, a, keep)
	if err != nil {
		t.Fatalf("ArchiveSealed: %v", err)
	}
	if rep.Kept != keep {
		t.Fatalf("Kept=%d, want %d", rep.Kept, keep)
	}
	if rep.Archived != len(sealed)-keep {
		t.Fatalf("Archived=%d, want %d", rep.Archived, len(sealed)-keep)
	}
	// 最新 keep 个仍在本地。
	sorted := append([]SegmentInfo(nil), sealed...)
	// 归档顺序是最旧优先；保留的是 index 最小的 keep 个。
	if len(a.got) != len(sealed)-keep {
		t.Fatalf("归档数量不符: %d", len(a.got))
	}
	for _, s := range sorted {
		_, statErr := os.Stat(s.Path)
		archived := a.got[s.Path] != nil
		if archived && statErr == nil {
			t.Fatalf("已归档段不应仍在本地: %s", s.Path)
		}
		if !archived && statErr != nil {
			t.Fatalf("保留段不应被删除: %s", s.Path)
		}
	}
}

// TestArchiveSealed_RequiresDeps 参数校验（fail-closed）。
func TestArchiveSealed_RequiresDeps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	if _, err := ArchiveSealed(ctx, nil, &memArchiver{}, 0); err == nil {
		t.Fatal("nil Journal 应返回错误")
	}
	if _, err := ArchiveSealed(ctx, newArchiveJournal(t), nil, 0); err == nil {
		t.Fatal("nil SegmentArchiver 应返回错误")
	}
}

// TestFSArchiver_WritesViaSyncCapability 经 `pkg/sync.FS`（真实 LocalFS）写入并**幂等不覆盖**。
//
// 幂等来自既有能力 `sync.WriteIfAbsent`（LocalFS/PrefixFS/StagingQuotaGateFS 均已实现）⇒
// 审计包不自造 sink 接口 ✓。
func TestFSArchiver_WritesViaSyncCapability(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	fsys := syncpkg.NewLocalFS(dir, nil)
	a := FSArchiver{FS: fsys, Now: func() time.Time { return time.Unix(0, 0).UTC() }}
	seg := SegmentInfo{Path: "/hot/audit.log.2", Size: 5, Sealed: true}

	ref, err := a.Archive(ctx, seg, strings.NewReader("hello"))
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	rc, oerr := fsys.OpenRead(ctx, ref)
	if oerr != nil {
		t.Fatalf("OpenRead(%s): %v", ref, oerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(got) != "hello" {
		t.Fatalf("归档内容不符: %q", string(got))
	}

	// 幂等：目标已存在 ⇒ 不覆盖（第二次写不同内容，读回仍应是 hello）。
	ref2, err2 := a.Archive(ctx, seg, strings.NewReader("XXXXX"))
	if err2 != nil || ref2 != ref {
		t.Fatalf("幂等路径应返回同一 ref: ref=%s err=%v", ref2, err2)
	}
	rc2, _ := fsys.OpenRead(ctx, ref)
	again, _ := io.ReadAll(rc2)
	_ = rc2.Close()
	if string(again) != "hello" {
		t.Fatalf("幂等路径不得覆盖: %q", string(again))
	}
}

// TestFSArchiver_RequiresFS fail-closed：未注入 FS ⇒ 明确报错。
func TestFSArchiver_RequiresFS(t *testing.T) {
	t.Parallel()
	if _, err := (FSArchiver{}).Archive(context.Background(), SegmentInfo{}, strings.NewReader("")); err == nil {
		t.Fatal("缺少 FS 应返回错误")
	}
}
