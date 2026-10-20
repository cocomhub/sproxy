// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// core_test.go 覆盖 P1 统一核心（设计 docs/designs/2026-10-19-audit-service.md）：
// 条目过滤、内存索引有界保留、两种 Journal 布局与轮转、Service 的作用域/能力门/有界保留/
// panic 隔离/惰性过时轮转/读取触发轮转/防递归/提交语义/fail-closed/实例隔离。
//
// 纪律：全部 t.Parallel()；**零 time.Sleep**（用注入时钟驱动过时与防抖）。

// ---------------------------------------------------------------- 测试替身

// fakeClock 是可推进的确定性时钟。
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1700000000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Since(t time.Time) time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(t)
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// countingMetrics 记录审计自身指标。
type countingMetrics struct {
	mu        sync.Mutex
	drops     int
	evicts    int
	failures  int
	panics    int
	rotations map[string]int
}

func newCountingMetrics() *countingMetrics {
	return &countingMetrics{rotations: make(map[string]int)}
}

func (m *countingMetrics) IncrDrop(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drops += n
}

func (m *countingMetrics) IncrEvict(n int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.evicts += n
}

func (m *countingMetrics) IncrWriteFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.failures++
}

func (m *countingMetrics) IncrHookPanic() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.panics++
}

func (m *countingMetrics) IncrRotate(reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rotations[reason]++
}

func (m *countingMetrics) evictCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.evicts
}

func (m *countingMetrics) snapshot() (drops, failures, panics int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.drops, m.failures, m.panics
}

// panickyNotifier 是 panic 的 Notifier（验证 E21 隔离）。
type panickyNotifier struct{ calls int }

func (n *panickyNotifier) Notify(context.Context, Entry) {
	n.calls++
	panic("notifier boom")
}

// failingJournal 让 Append 持续失败（验证 best-effort 与 fail-closed 两条路径）。
type failingJournal struct {
	inner *MemoryJournal
	err   error
}

func (j *failingJournal) Append(Entry) error                  { return j.err }
func (j *failingJournal) Read(f EntryFilter) ([]Entry, error) { return j.inner.Read(f) }
func (j *failingJournal) Segments() ([]SegmentInfo, error)    { return j.inner.Segments() }
func (j *failingJournal) Rotate(string) error                 { return nil }
func (j *failingJournal) Rewrite(k func(Entry) bool) error    { return j.inner.Rewrite(k) }
func (j *failingJournal) Flush() error                        { return nil }
func (j *failingJournal) Close() error                        { return nil }

func mkEntry(scope ScopeID, action, owner string, seq uint64) Entry {
	return Entry{Scope: scope, Seq: seq, Kind: KindEvent, Action: action, Owner: owner, Result: ResultSuccess}
}

// ---------------------------------------------------------------- 条目与过滤

func TestEntryFilter_Match(t *testing.T) {
	t.Parallel()
	base := time.Unix(1700000000, 0)
	e := Entry{Scope: "s1", Kind: KindEvent, Action: "file.delete", Owner: "alice",
		Result: ResultSuccess, TS: base.Add(time.Minute)}
	cases := []struct {
		name string
		f    EntryFilter
		want bool
	}{
		{"empty matches", EntryFilter{}, true},
		{"scope hit", EntryFilter{Scope: "s1"}, true},
		{"scope miss", EntryFilter{Scope: "s2"}, false},
		{"kind hit", EntryFilter{Kind: KindEvent}, true},
		{"kind miss", EntryFilter{Kind: KindSpan}, false},
		{"owner hit", EntryFilter{Owner: "alice"}, true},
		{"owner miss", EntryFilter{Owner: "bob"}, false},
		{"action hit", EntryFilter{Action: "file.delete"}, true},
		{"result hit", EntryFilter{Result: ResultSuccess}, true},
		{"result miss", EntryFilter{Result: ResultDenied}, false},
		{"since earlier", EntryFilter{Since: base}, true},
		{"since later", EntryFilter{Since: base.Add(2 * time.Minute)}, false},
		{"multi AND", EntryFilter{Owner: "alice", Action: "file.delete"}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.f.Match(e); got != tc.want {
				t.Fatalf("Match = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------- 内存索引

func TestRingIndex_BoundedNewestFirstAndLimit(t *testing.T) {
	t.Parallel()
	r := NewRingIndex(3)
	for i := range 5 {
		r.Add(mkEntry("s", "a", "alice", uint64(i+1)))
	}
	if r.Len() != 3 {
		t.Fatalf("Len = %d, want 3（有界）", r.Len())
	}
	got := r.Query(EntryFilter{})
	if len(got) != 3 {
		t.Fatalf("Query len = %d, want 3", len(got))
	}
	// 最新在前：seq 5,4,3。
	for i, want := range []uint64{5, 4, 3} {
		if got[i].Seq != want {
			t.Fatalf("Query[%d].Seq = %d, want %d（最新在前）", i, got[i].Seq, want)
		}
	}
	if limited := r.Query(EntryFilter{Limit: 2}); len(limited) != 2 || limited[0].Seq != 5 {
		t.Fatalf("Limit 语义错误: %+v", limited)
	}
}

func TestRingIndex_EvictAndPrune(t *testing.T) {
	t.Parallel()
	r := NewRingIndex(4)
	for i := range 4 {
		r.Add(mkEntry("s", "a", "alice", uint64(i+1)))
	}
	ev := r.Evict(2)
	if len(ev) != 2 || ev[0].Seq != 1 || ev[1].Seq != 2 {
		t.Fatalf("Evict 应逐出最旧两条: %+v", ev)
	}
	if r.Len() != 2 {
		t.Fatalf("Len = %d, want 2", r.Len())
	}
	r.Add(mkEntry("s", "b", "bob", 10))
	dropped := r.Prune(func(e Entry) bool { return e.Owner == "bob" })
	if dropped != 2 {
		t.Fatalf("Prune dropped = %d, want 2", dropped)
	}
	rest := r.Query(EntryFilter{})
	if len(rest) != 1 || rest[0].Owner != "bob" {
		t.Fatalf("Prune 后剩余错误: %+v", rest)
	}
}

// ---------------------------------------------------------------- Journal

func TestMemoryJournal_AppendReadRewriteRotate(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	for i := range 3 {
		if aerr := j.Append(mkEntry("s", "a", "alice", uint64(i+1))); aerr != nil {
			t.Fatalf("Append: %v", aerr)
		}
	}
	got, err := j.Read(EntryFilter{})
	if err != nil || len(got) != 3 {
		t.Fatalf("Read = %d entries, err=%v", len(got), err)
	}
	if aerr := j.Rewrite(func(e Entry) bool { return e.Seq != 2 }); aerr != nil {
		t.Fatalf("Rewrite: %v", aerr)
	}
	got, _ = j.Read(EntryFilter{})
	if len(got) != 2 {
		t.Fatalf("Rewrite 后 len = %d, want 2", len(got))
	}
	if aerr := j.Rotate("manual"); aerr != nil {
		t.Fatalf("Rotate: %v", aerr)
	}
	if rs := j.Rotates(); len(rs) != 1 || rs[0] != "manual" {
		t.Fatalf("Rotates = %v", rs)
	}
	if aerr := j.Close(); aerr != nil {
		t.Fatalf("Close: %v", aerr)
	}
	if _, err := j.Read(EntryFilter{}); !errors.Is(err, ErrClosed) {
		t.Fatalf("关闭后 Read err = %v, want ErrClosed", err)
	}
}

func TestFileJournal_GlobalAndPerScopeLayouts(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	g, err := NewFileJournal(FileJournalConfig{Dir: filepath.Join(dir, "global"), Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal(global): %v", err)
	}
	p, err := NewFileJournal(FileJournalConfig{Dir: filepath.Join(dir, "per"), Layout: LayoutPerScope})
	if err != nil {
		t.Fatalf("NewFileJournal(per): %v", err)
	}
	defer g.Close()
	defer p.Close()
	if aerr := g.Append(mkEntry("s1", "a", "alice", 1)); aerr != nil {
		t.Fatalf("global Append: %v", aerr)
	}
	if aerr := p.Append(mkEntry("s1", "a", "alice", 1)); aerr != nil {
		t.Fatalf("per-scope Append: %v", aerr)
	}
	if aerr := p.Append(mkEntry("s2", "b", "bob", 1)); aerr != nil {
		t.Fatalf("per-scope Append(s2): %v", aerr)
	}
	segs, err := p.Segments()
	if err != nil || len(segs) != 2 {
		t.Fatalf("per-scope 段数 = %d (err=%v), want 2（按作用域分族）", len(segs), err)
	}
	gs, _ := g.Segments()
	if len(gs) != 1 {
		t.Fatalf("global 段数 = %d, want 1", len(gs))
	}
	got, err := p.Read(EntryFilter{Scope: "s2"})
	if err != nil || len(got) != 1 || got[0].Owner != "bob" {
		t.Fatalf("按作用域读取错误: %+v err=%v", got, err)
	}
}

func TestFileJournal_PrivatePermissions(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 Mode().Perm() 不反映 ACL（与 legacy hardening 测试同口径）")
	}
	dir := filepath.Join(t.TempDir(), "sealed")
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()
	if aerr := j.Append(mkEntry("s", "a", "alice", 1)); aerr != nil {
		t.Fatalf("Append: %v", aerr)
	}
	dst, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat dir: %v", err)
	}
	if dst.Mode().Perm() != 0o700 {
		t.Fatalf("段目录权限 = %o, want 700（设计 M1-12）", dst.Mode().Perm())
	}
	segs, _ := j.Segments()
	if len(segs) != 1 {
		t.Fatalf("段数 = %d", len(segs))
	}
	fst, err := os.Stat(segs[0].Path)
	if err != nil {
		t.Fatalf("Stat 段文件: %v", err)
	}
	if fst.Mode().Perm() != 0o600 {
		t.Fatalf("段文件权限 = %o, want 600（设计 M1-12）", fst.Mode().Perm())
	}
}

func TestFileJournal_RotationKeepsArchives(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileJournal(FileJournalConfig{
		Dir: dir, Layout: LayoutGlobal, MaxSize: 1, MaxArchives: 2, SyncMode: SyncNone,
	})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	defer j.Close()
	for i := range 3 {
		if aerr := j.Append(mkEntry("s", "a", "alice", uint64(i+1))); aerr != nil {
			t.Fatalf("Append(%d): %v", i, aerr)
		}
	}
	segs, err := j.Segments()
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	var sealed int
	for _, s := range segs {
		if s.Sealed {
			sealed++
		}
	}
	if sealed == 0 {
		t.Fatalf("期望存在已封段（MaxSize=1 触发轮转）: %+v", segs)
	}
	if sealed > 2 {
		t.Fatalf("归档份数超 MaxArchives=2: %d", sealed)
	}
}

func TestFileJournal_RewriteDropsAndKeepsValidJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	for i := range 4 {
		if aerr := j.Append(mkEntry("s", "a", "alice", uint64(i+1))); aerr != nil {
			t.Fatalf("Append: %v", aerr)
		}
	}
	if aerr := j.Rewrite(func(e Entry) bool { return e.Seq%2 == 0 }); aerr != nil {
		t.Fatalf("Rewrite: %v", aerr)
	}
	got, err := j.Read(EntryFilter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 2 || got[1].Seq != 4 {
		t.Fatalf("Rewrite 结果错误: %+v", got)
	}
	if aerr := j.Close(); aerr != nil {
		t.Fatalf("Close: %v", aerr)
	}
}

func TestFileJournal_TruncatedTailLineIgnored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	j, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal: %v", err)
	}
	if aerr := j.Append(mkEntry("s", "a", "alice", 1)); aerr != nil {
		t.Fatalf("Append: %v", aerr)
	}
	if aerr := j.Close(); aerr != nil {
		t.Fatalf("Close: %v", aerr)
	}
	// 模拟崩溃截断：追加半行（无换行结尾）。
	f, err := os.OpenFile(filepath.Join(dir, "compliance.audit.log"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	if _, aerr := f.WriteString(`{"scope":"s","seq":2,"kin`); aerr != nil {
		t.Fatalf("WriteString: %v", err)
	}
	if aerr := f.Close(); aerr != nil {
		t.Fatalf("Close file: %v", aerr)
	}
	j2, err := NewFileJournal(FileJournalConfig{Dir: dir, Layout: LayoutGlobal})
	if err != nil {
		t.Fatalf("NewFileJournal(2): %v", err)
	}
	defer j2.Close()
	got, err := j2.Read(EntryFilter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 1 || got[0].Seq != 1 {
		t.Fatalf("不完整尾行应被丢弃: %+v", got)
	}
}

// ---------------------------------------------------------------- Service

func TestService_ScopeEmitSeqMonotonicPerScope(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	s1, err := s.Open(ctx, "task-1")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s2, _ := s.Open(ctx, "task-2")
	for range 2 {
		if aerr := s1.Emit(ctx, mkEntry("", "a", "alice", 0)); aerr != nil {
			t.Fatalf("Emit: %v", aerr)
		}
	}
	if aerr := s2.Emit(ctx, mkEntry("", "b", "bob", 0)); aerr != nil {
		t.Fatalf("Emit: %v", aerr)
	}
	got, err := j.Read(EntryFilter{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("条目数 = %d, want 3", len(got))
	}
	seq := map[ScopeID][]uint64{}
	for _, e := range got {
		seq[e.Scope] = append(seq[e.Scope], e.Seq)
	}
	if len(seq["task-1"]) != 2 || seq["task-1"][0] != 1 || seq["task-1"][1] != 2 {
		t.Fatalf("task-1 内 seq 应 1,2（每作用域单调）: %v", seq["task-1"])
	}
	if len(seq["task-2"]) != 1 || seq["task-2"][0] != 1 {
		t.Fatalf("task-2 内 seq 应独立从 1 开始: %v", seq["task-2"])
	}
}

func TestService_ContextFieldsFilledFromInjectors(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s, err := New(Options{
		Journal: j, Clock: newFakeClock(), NodeID: "node-a",
		CtxFields: ContextFields{
			Actor:     func(context.Context) string { return "ak-alice" },
			Owner:     func(context.Context) string { return "alice" },
			Mesh:      func(context.Context) string { return "mesh-1" },
			RequestID: func(context.Context) string { return "req-9" },
			SourceIP:  func(context.Context) string { return "10.0.0.1" },
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "file.delete"}); aerr != nil {
		t.Fatalf("Emit: %v", aerr)
	}
	got, _ := j.Read(EntryFilter{})
	if len(got) != 1 {
		t.Fatalf("条目数 = %d", len(got))
	}
	e := got[0]
	if e.Actor != "ak-alice" || e.Owner != "alice" || e.Mesh != "mesh-1" || e.RequestID != "req-9" || e.SourceIP != "10.0.0.1" {
		t.Fatalf("ctx 字段未填充: %+v", e)
	}
	if e.Node != "node-a" {
		t.Fatalf("Node = %q, want node-a", e.Node)
	}
}

func TestService_DisabledProfileWritesNothing(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	disabled := DisabledCapabilities()
	s, err := New(Options{Journal: j, Caps: &disabled, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !s.Disabled() {
		t.Fatalf("Disabled() = false, want true")
	}
	sc, err := s.Open(context.Background(), "s")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "x"}); aerr != nil {
		t.Fatalf("禁用剖面 Emit 应静默成功（零回归）: %v", aerr)
	}
	got, _ := j.Read(EntryFilter{})
	if len(got) != 0 {
		t.Fatalf("禁用剖面不应写入: %+v", got)
	}
}

func TestService_BoundedRetentionEvictsAndCounts(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	m := newCountingMetrics()
	s, err := New(Options{
		Journal: j, Clock: newFakeClock(), Metrics: m,
		Policy: Policy{MaxMemoryEntries: 3},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	for range 7 {
		if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a", Object: "f"}); aerr != nil {
			t.Fatalf("Emit: %v", aerr)
		}
	}
	// 有界保留：内存索引不超上限；丢弃被计数（**先落盘再逐出** ⇒ Journal 仍有全部 7 条）。
	if s.eff.index.Len() != 3 {
		t.Fatalf("索引 len = %d, want 3（有界）", s.eff.index.Len())
	}
	// 已持久化条目的逐出**不是丢失** ⇒ 计入 Evict；Drop 仅用于未持久化丢失。
	if ev := m.evictCount(); ev != 4 {
		t.Fatalf("逐出计数 = %d, want 4（Journal 仍是权威）", ev)
	}
	if drops, _, _ := m.snapshot(); drops != 0 {
		t.Fatalf("已持久化逐出不应计入 Drop: %d", drops)
	}
	all, _ := j.Read(EntryFilter{})
	if len(all) != 7 {
		t.Fatalf("Journal 条目 = %d, want 7（逐出只作用于内存副本，绝不丢持久化）", len(all))
	}
}

func TestService_NotifierPanicIsolated(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	m := newCountingMetrics()
	pn := &panickyNotifier{}
	s, err := New(Options{Journal: j, Clock: newFakeClock(), Metrics: m, Notifier: pn})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
		t.Fatalf("Notifier panic 不得传播到业务: %v", aerr)
	}
	if _, _, panics := m.snapshot(); panics != 1 {
		t.Fatalf("hook panic 计数 = %d, want 1（E21）", panics)
	}
}

func TestService_StaleRotationIsLazyAndNonRecursive(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	j := NewMemoryJournal()
	m := newCountingMetrics()
	s, err := New(Options{Journal: j, Clock: clk, Metrics: m, Policy: Policy{MaxAge: time.Minute}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
		t.Fatalf("Emit: %v", aerr)
	}
	if rs := j.Rotates(); len(rs) != 0 {
		t.Fatalf("未过时不应轮转: %v", rs)
	}
	clk.Advance(2 * time.Minute) // 注入时钟推进（零 time.Sleep）
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "b"}); aerr != nil {
		t.Fatalf("Emit(2): %v", aerr)
	}
	rs := j.Rotates()
	if len(rs) != 1 || rs[0] != "age" {
		t.Fatalf("过时应触发一次 age 轮转: %v", rs)
	}
	// 防递归：轮转标记（KindRotate）自身不得再触发轮转。
	if aerr := s.Rotate(context.Background(), "manual"); aerr != nil {
		t.Fatalf("Rotate: %v", aerr)
	}
	if rs := j.Rotates(); len(rs) != 2 {
		t.Fatalf("Rotate 自身不应触发额外轮转: %v", rs)
	}
}

func TestService_ReadTriggeredRotationAndOptOut(t *testing.T) {
	t.Parallel()
	clk := newFakeClock()
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: clk, Policy: Policy{MinRotateInterval: time.Second}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
		t.Fatalf("Emit: %v", aerr)
	}
	clk.Advance(2 * time.Second)
	if _, aerr := s.Query(context.Background(), EntryFilter{}); aerr != nil {
		t.Fatalf("Query: %v", err)
	}
	if rs := j.Rotates(); len(rs) != 1 || rs[0] != "read" {
		t.Fatalf("读取应触发一次 read 轮转: %v", rs)
	}
	// opt-out：DisableRotateOnRead ⇒ 不再由读取触发。
	j2 := NewMemoryJournal()
	s2, err := New(Options{Journal: j2, Clock: clk, Policy: Policy{DisableRotateOnRead: true}})
	if err != nil {
		t.Fatalf("New(2): %v", err)
	}
	sc2, _ := s2.Open(context.Background(), "s")
	if aerr := sc2.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
		t.Fatalf("Emit(2): %v", aerr)
	}
	clk.Advance(time.Hour)
	if _, aerr := s2.Query(context.Background(), EntryFilter{}); aerr != nil {
		t.Fatalf("Query(2): %v", err)
	}
	if rs := j2.Rotates(); len(rs) != 0 {
		t.Fatalf("显式关闭后读取不得轮转: %v", rs)
	}
}

func TestService_MarkDoneCommitRecordAndCloseSemantics(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	sc, _ := s.Open(ctx, "task-x")
	if aerr := sc.Save(ctx, "progress", 42); aerr != nil {
		t.Fatalf("Save: %v", aerr)
	}
	if v, ok := sc.Get(ctx, "progress"); !ok || v.(int) != 42 {
		t.Fatalf("Get = %v, %v", v, ok)
	}
	if aerr := sc.MarkDone(ctx); aerr != nil {
		t.Fatalf("MarkDone: %v", aerr)
	}
	if aerr := sc.MarkDone(ctx); aerr != nil {
		t.Fatalf("MarkDone 幂等: %v", aerr)
	}
	got, _ := j.Read(EntryFilter{Scope: "task-x", Kind: KindDone})
	if len(got) != 1 {
		t.Fatalf("KindDone 应恰有一条（提交记录）: %d", len(got))
	}
	if aerr := sc.Close(); aerr != nil {
		t.Fatalf("Close: %v", aerr)
	}
	if err := sc.Emit(ctx, Entry{Kind: KindEvent, Action: "after-close"}); !errors.Is(err, ErrScopeClosed) {
		t.Fatalf("关闭后 Emit err = %v, want ErrScopeClosed", err)
	}
}

func TestService_BestEffortVersusFailClosed(t *testing.T) {
	t.Parallel()
	// best-effort（默认）：落盘失败仍返回 nil，且失败被计数。
	m1 := newCountingMetrics()
	s1, err := New(Options{Journal: &failingJournal{inner: NewMemoryJournal(), err: errors.New("disk full")},
		Clock: newFakeClock(), Metrics: m1})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc1, _ := s1.Open(context.Background(), "s")
	if aerr := sc1.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
		t.Fatalf("best-effort 不得阻断业务: %v", aerr)
	}
	if _, failures, _ := m1.snapshot(); failures == 0 {
		t.Fatalf("落盘失败应被计数（可观测）")
	}
	// fail-closed：向上传播 ErrAuditUnavailable（合规部署显式开启）。
	s2, err := New(Options{Journal: &failingJournal{inner: NewMemoryJournal(), err: errors.New("disk full")},
		Clock: newFakeClock(), FailClosed: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc2, _ := s2.Open(context.Background(), "s")
	if err := sc2.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); !errors.Is(err, ErrAuditUnavailable) {
		t.Fatalf("fail-closed err = %v, want ErrAuditUnavailable", err)
	}
}

func TestService_PruneRemovesOnlyMatching(t *testing.T) {
	t.Parallel()
	j := NewMemoryJournal()
	s, err := New(Options{Journal: j, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	sc, _ := s.Open(ctx, "s")
	for range 3 {
		if aerr := sc.Emit(ctx, Entry{Kind: KindEvent, Action: "a", Object: "f"}); aerr != nil {
			t.Fatalf("Emit: %v", aerr)
		}
	}
	if aerr := sc.Save(ctx, "k", "v"); aerr != nil {
		t.Fatalf("Save: %v", aerr)
	}
	// TTL 只剔合规事件，不触碰 WAL 状态（设计 E25：调用方按族调用；此处断言谓词语义）。
	if aerr := s.Prune(func(e Entry) bool { return e.Kind != KindEvent }); aerr != nil {
		t.Fatalf("Prune: %v", aerr)
	}
	events, _ := j.Read(EntryFilter{Kind: KindEvent})
	states, _ := j.Read(EntryFilter{Kind: KindState})
	if len(events) != 0 {
		t.Fatalf("合规事件应被剔除: %d", len(events))
	}
	if len(states) != 1 {
		t.Fatalf("WAL 状态应保留: %d", len(states))
	}
}

func TestService_InstancesAreIsolated(t *testing.T) {
	t.Parallel()
	j1, j2 := NewMemoryJournal(), NewMemoryJournal()
	s1, err := New(Options{Journal: j1, Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New(1): %v", err)
	}
	if _, aerr := New(Options{Journal: j2, Clock: newFakeClock()}); aerr != nil {
		t.Fatalf("New(2): %v", err)
	}
	ctx := context.Background()
	sc1, _ := s1.Open(ctx, "s")
	if aerr := sc1.Emit(ctx, Entry{Kind: KindEvent, Action: "only-1"}); aerr != nil {
		t.Fatalf("Emit: %v", aerr)
	}
	got2, _ := j2.Read(EntryFilter{})
	if len(got2) != 0 {
		t.Fatalf("实例间不得互相影响（零全局可变状态）: %+v", got2)
	}
	got1, _ := j1.Read(EntryFilter{})
	if len(got1) != 1 || !strings.Contains(got1[0].Action, "only-1") {
		t.Fatalf("实例 1 应写入自己的 journal: %+v", got1)
	}
}

func TestService_InvalidScopeRejected(t *testing.T) {
	t.Parallel()
	s, err := New(Options{Journal: NewMemoryJournal(), Clock: newFakeClock()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, bad := range []ScopeID{"", "../etc", "a/b", ".."} {
		if _, err := s.Open(context.Background(), bad); !errors.Is(err, ErrInvalidScope) {
			t.Fatalf("Open(%q) err = %v, want ErrInvalidScope", bad, err)
		}
	}
}

func TestService_UnpersistedEvictionCountsAsDrop(t *testing.T) {
	t.Parallel()
	m := newCountingMetrics()
	s, err := New(Options{
		Journal: &failingJournal{inner: NewMemoryJournal(), err: errors.New("disk full")},
		Clock:   newFakeClock(), Metrics: m,
		Policy: Policy{MaxMemoryEntries: 2},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	sc, _ := s.Open(context.Background(), "s")
	for range 5 {
		if aerr := sc.Emit(context.Background(), Entry{Kind: KindEvent, Action: "a"}); aerr != nil {
			t.Fatalf("best-effort 不得阻断业务: %v", aerr)
		}
	}
	drops, failures, _ := m.snapshot()
	if failures != 5 {
		t.Fatalf("落盘失败计数 = %d, want 5", failures)
	}
	if drops != 3 {
		t.Fatalf("未持久化逐出应计入 Drop = %d, want 3（有界保留代价必须可观测）", drops)
	}
	if s.eff.index.Len() != 2 {
		t.Fatalf("索引 len = %d, want 2（有界）", s.eff.index.Len())
	}
}
