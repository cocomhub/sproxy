// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// newMemStore 构造纯内存 Store（未启用持久化）。
func newMemStore() *Store { return NewStore(testutil.DiscardLogger()) }

// writeLinkFile 在 dir 下写入一条分享的持久化形态（<token>.json），返回文件路径。
func writeLinkFile(t *testing.T, dir string, l *Link) string {
	t.Helper()
	data, err := json.Marshal(l.ToPersist())
	if err != nil {
		t.Fatalf("序列化分享链接失败: %v", err)
	}
	path := filepath.Join(dir, l.Token+".json")
	if werr := os.WriteFile(path, data, 0o600); werr != nil {
		t.Fatalf("写入分享链接文件失败: %v", werr)
	}
	return path
}

func TestStore_CreatePeekListLen(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	l1, err := s.Create(Spec{
		Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Volume: "vol-1",
		Owner: "ak-1", TTL: time.Hour, MaxDownloads: 3, ReadOnly: true, WatermarkSeed: "seed-1",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if len(l1.Token) != 32 {
		t.Fatalf("token 应为 16 字节 hex（32 字符），got %q", l1.Token)
	}
	if l1.Downloads != 0 || l1.MaxDownloads != 3 || !l1.ReadOnly {
		t.Fatalf("创建字段不一致: downloads=%d max=%d readonly=%v", l1.Downloads, l1.MaxDownloads, l1.ReadOnly)
	}
	if l1.Volume != "vol-1" || l1.WatermarkSeed != "seed-1" || l1.TenantID != "alice" || l1.Rel != "user/a.txt" {
		t.Fatalf("创建字段未固化: %+v", l1)
	}
	if !l1.ExpiresAt.Equal(l1.CreatedAt.Add(time.Hour)) {
		t.Fatalf("ExpiresAt 应为 CreatedAt+TTL: created=%v expires=%v", l1.CreatedAt, l1.ExpiresAt)
	}

	// Peek 返回副本：改副本不影响存储。
	got := s.Peek(l1.Token)
	if got == nil {
		t.Fatalf("Peek 应命中 %s", l1.Token)
	}
	got.Downloads = 99
	if again := s.Peek(l1.Token); again.Downloads != 0 {
		t.Fatalf("Peek 必须返回副本，存储被改写: downloads=%d", again.Downloads)
	}
	if s.Peek("no-such-token") != nil {
		t.Fatal("Peek 未命中应返回 nil")
	}

	if _, cerr := s.Create(Spec{Filename: "b.txt", TenantID: "bob", Rel: "user/b.txt", Owner: "ak-2", TTL: time.Hour}); cerr != nil {
		t.Fatalf("Create: %v", cerr)
	}
	if n := s.Len(); n != 2 {
		t.Fatalf("Len=%d want 2", n)
	}

	// List：空 owner 返回全部；非空只返回自己的。
	if all := s.List(""); len(all) != 2 {
		t.Fatalf("List(\"\") 应返回全部 2 条，got %d", len(all))
	}
	mine := s.List("ak-1")
	if len(mine) != 1 || mine[0].Token != l1.Token {
		t.Fatalf("List(\"ak-1\") 应只返回自己的 1 条，got %d", len(mine))
	}
	mine[0].Owner = "hijacked"
	if s.Peek(l1.Token).Owner != "ak-1" {
		t.Fatal("List 必须返回副本，存储被改写")
	}
	if other := s.List("ak-nobody"); len(other) != 0 {
		t.Fatalf("List(不存在的 owner) 应为空，got %d", len(other))
	}
}

func TestStore_ConsumeUnlimitedIncrementsDownloads(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	link, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Owner: "ak-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for want := 1; want <= 3; want++ {
		got := s.Consume(link.Token)
		if got == nil {
			t.Fatalf("第 %d 次 Consume 应成功（不限次数）", want)
		}
		if got.Downloads != want {
			t.Fatalf("第 %d 次 Consume 后 Downloads=%d", want, got.Downloads)
		}
	}
}

func TestStore_ConsumeExpired(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	// TTL=0：到创建时刻即过期（「恰好到期」语义：now >= expires 即无效）。
	atExpiry, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: 0})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Consume(atExpiry.Token) != nil {
		t.Fatal("恰好到期（TTL=0）应视为过期")
	}
	if s.Len() != 0 {
		t.Fatal("过期消费应删除条目")
	}

	past, err := s.Create(Spec{Filename: "b.txt", TenantID: "alice", Rel: "user/b.txt", TTL: -time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Consume(past.Token) != nil {
		t.Fatal("TTL 为负（已过期）应返回 nil")
	}
}

func TestStore_ConsumeMaxDownloadsReached(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	link, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: time.Hour, MaxDownloads: 1})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := s.Consume(link.Token); got == nil || got.Downloads != 1 {
		t.Fatalf("第一次 Consume 应成功: %+v", got)
	}
	if got := s.Consume(link.Token); got != nil {
		t.Fatalf("达到 MaxDownloads 后应返回 nil，got %+v", got)
	}
	if s.Len() != 0 {
		t.Fatal("达到上限的条目应被删除")
	}
}

func TestStore_ConsumeOneTime(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	link, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: time.Hour, OneTime: true})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first := s.Consume(link.Token)
	if first == nil {
		t.Fatal("一次性分享首次 Consume 应返回链接（供流式传输）")
	}
	if first.Downloads != 1 {
		t.Fatalf("首次消费计数应为 1，got %d", first.Downloads)
	}
	if s.Consume(link.Token) != nil {
		t.Fatal("一次性分享二次 Consume 应返回 nil")
	}
	if s.Len() != 0 {
		t.Fatal("一次性分享消费后应删除")
	}
}

func TestStore_ConsumeUnknownToken(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()
	if s.Consume("no-such-token") != nil {
		t.Fatal("未知 token 应返回 nil")
	}
}

func TestStore_Revoke(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	link, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Owner: "ak-1", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if rerr := s.Revoke(link.Token, "ak-other"); rerr == nil {
		t.Fatal("跨租户撤销必须拒绝")
	}
	if s.Peek(link.Token) == nil {
		t.Fatal("被拒绝的撤销不得删除链接")
	}
	if rerr := s.Revoke(link.Token, "ak-1"); rerr != nil {
		t.Fatalf("本人撤销应成功: %v", rerr)
	}
	if s.Peek(link.Token) != nil {
		t.Fatal("撤销后 Peek 应为 nil")
	}
	if rerr := s.Revoke(link.Token, "ak-1"); rerr == nil {
		t.Fatal("重复撤销（不存在）应返回 error")
	}
}

func TestStore_CleanupExpiredKeepsActive(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	expired, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: -time.Minute})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	active, err := s.Create(Spec{Filename: "b.txt", TenantID: "alice", Rel: "user/b.txt", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	s.CleanupExpired()
	if s.Peek(expired.Token) != nil {
		t.Fatal("过期条目应被清理")
	}
	if s.Peek(active.Token) == nil {
		t.Fatal("未过期条目不得被清理")
	}
}

func TestStore_StopIsIdempotent(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	s.Stop()
	s.Stop() // 重复 Stop 不得 panic（stopOnce）
}

func TestStore_EnablePersistNilRootIsNoop(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	s.EnablePersist(nil, "meta/share")
	if rel := s.persistRel("tok"); rel != "" {
		t.Fatalf("未启用持久化时 persistRel 应为空，got %q", rel)
	}
	// 未启用持久化：写/删均为 no-op（不得 panic）。
	s.PersistWrite(&Link{Token: "tok"})
	s.persistRemove("tok")

	if _, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: time.Hour}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Len() != 1 {
		t.Fatalf("纯内存 Store 应仍可创建链接，Len=%d", s.Len())
	}
}

func TestStore_EnablePersistReadDirErrorWarns(t *testing.T) {
	t.Parallel()

	t.Run("非 NotExist 错误告警", func(t *testing.T) {
		t.Parallel()
		// 已关闭的卷句柄：ReadDir 返回 "file already closed"（非 NotExist）→ 告警分支。
		closedRoot, err := storage.OpenRoot(t.TempDir())
		if err != nil {
			t.Fatalf("打开 Root 失败: %v", err)
		}
		if cerr := closedRoot.Close(); cerr != nil {
			t.Fatalf("关闭 Root 失败: %v", cerr)
		}
		s := NewStore(testutil.DiscardLogger())
		defer s.Stop()
		s.EnablePersist(closedRoot, "meta/share")
		if s.Len() != 0 {
			t.Fatalf("恢复失败时不应载入链接，Len=%d", s.Len())
		}
	})

	t.Run("持久化目录创建失败不阻塞创建", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		// 用普通文件占住 "meta" 段：EnsureDir 失败 → 告警后不阻塞创建。
		if err := os.WriteFile(filepath.Join(dir, "meta"), []byte("x"), 0o600); err != nil {
			t.Fatalf("准备阻塞文件失败: %v", err)
		}
		s := NewStore(testutil.DiscardLogger())
		defer s.Stop()
		s.EnablePersist(testRootAt(t, dir), "meta/share")

		if rel := s.persistRel("tok"); rel != "meta/share/tok.json" {
			t.Fatalf("persistRel=%q want meta/share/tok.json", rel)
		}
		if _, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: time.Hour}); err != nil {
			t.Fatalf("落盘失败不得阻塞创建: %v", err)
		}
		if s.Len() != 1 {
			t.Fatalf("Len=%d want 1", s.Len())
		}
	})
}

func TestStore_RestoreSkipsUnopenableEntry(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shareDir := filepath.Join(dir, "meta", "share")
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	// 断链符号链接：列举可见但打开失败（root 的 O_NOFOLLOW/ENOENT）→ 告警跳过。
	if err := os.Symlink(filepath.Join(shareDir, "missing-target"), filepath.Join(shareDir, "broken.json")); err != nil {
		t.Skipf("平台不支持符号链接，跳过：%v", err)
	}
	valid := &Link{Token: "dddddddddddddddddddddddddddddddd", ExpiresAt: time.Now().Add(time.Hour)}
	writeLinkFile(t, shareDir, valid)

	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	s.EnablePersist(testRootAt(t, dir), "meta/share")
	if s.Len() != 1 {
		t.Fatalf("断链条目应跳过、其他条目正常恢复，Len=%d", s.Len())
	}
	if s.Peek(valid.Token) == nil {
		t.Fatal("有效条目应恢复")
	}
}

func TestStore_EnablePersistCreatesSubdirAndRestores(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	// 目标子目录不存在：恢复扫描静默跳过（IsNotExist，非告警）。
	s.EnablePersist(testRootAt(t, dir), "meta/share")

	link, err := s.Create(Spec{
		Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Volume: "vol-1",
		Owner: "ak-1", TTL: time.Hour, MaxDownloads: 2, OneTime: false, ReadOnly: true, WatermarkSeed: "w",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	file := filepath.Join(dir, "meta", "share", link.Token+".json")
	if _, serr := os.Stat(file); serr != nil {
		t.Fatalf("Create 应懒建子目录并落盘 %s: %v", file, serr)
	}

	restored := NewStore(testutil.DiscardLogger())
	defer restored.Stop()
	restored.EnablePersist(testRootAt(t, dir), "meta/share")
	got := restored.Peek(link.Token)
	if got == nil {
		t.Fatalf("子目录形态应可恢复 %s", link.Token)
	}
	if got.Volume != "vol-1" || got.Owner != "ak-1" || got.MaxDownloads != 2 ||
		!got.ReadOnly || got.WatermarkSeed != "w" {
		t.Fatalf("恢复字段不一致: %+v", got)
	}
}

func TestStore_PersistWriteOpenErrorWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	s.EnablePersist(testRootAt(t, dir), "")

	// 用目录占住 tmp 路径：WriteUserVisible 打开失败 → 告警后返回（不 panic）。
	const token = "00112233445566778899aabbccddeeff"
	if err := os.Mkdir(filepath.Join(dir, token+".json.tmp"), 0o755); err != nil {
		t.Fatalf("准备阻塞目录失败: %v", err)
	}
	s.PersistWrite(&Link{Token: token})
	if _, serr := os.Stat(filepath.Join(dir, token+".json")); !os.IsNotExist(serr) {
		t.Fatalf("写入失败不应产生落盘文件: %v", serr)
	}
}

func TestStore_PersistWriteRenameErrorWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	s.EnablePersist(testRootAt(t, dir), "")

	// 目标路径是**目录**：原子重命名失败 → 告警 + 清理 tmp 残留。
	const token = "ffeeddccbbaa99887766554433221100"
	if err := os.Mkdir(filepath.Join(dir, token+".json"), 0o755); err != nil {
		t.Fatalf("准备目标目录失败: %v", err)
	}
	s.PersistWrite(&Link{Token: token})
	if _, serr := os.Stat(filepath.Join(dir, token+".json.tmp")); !os.IsNotExist(serr) {
		t.Fatalf("重命名失败后 tmp 残留应被清理: %v", serr)
	}
	if st, serr := os.Stat(filepath.Join(dir, token+".json")); serr != nil || !st.IsDir() {
		t.Fatalf("目标目录不应被删除: %v", serr)
	}
}

func TestStore_PersistRemoveErrorWarns(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	s.EnablePersist(testRootAt(t, dir), "")

	// 非空目录占住 <token>.json：Remove 返回 ENOTEMPTY（非 NotExist）→ 告警分支。
	const token = "0123456789abcdef0123456789abcdef"
	target := filepath.Join(dir, token+".json")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("准备目标目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备目标目录内容失败: %v", err)
	}
	s.persistRemove(token)
	if _, serr := os.Stat(target); serr != nil {
		t.Fatalf("删除失败不应静默吞掉目录: %v", serr)
	}
}

func TestStore_RestoreSkipsInvalidEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	shareDir := filepath.Join(dir, "meta", "share")
	if err := os.MkdirAll(shareDir, 0o755); err != nil {
		t.Fatalf("准备目录失败: %v", err)
	}
	now := time.Now()

	valid := &Link{
		Token: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Filename: "ok.txt", TenantID: "alice",
		Rel: "user/ok.txt", Volume: "vol-1", Owner: "ak-1", CreatedAt: now.Add(-time.Minute),
		ExpiresAt: now.Add(time.Hour), MaxDownloads: 2, Downloads: 1, ReadOnly: true, WatermarkSeed: "w",
	}
	writeLinkFile(t, shareDir, valid)

	expired := &Link{Token: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ExpiresAt: now.Add(-time.Hour)}
	expiredPath := writeLinkFile(t, shareDir, expired)

	consumed := &Link{Token: "cccccccccccccccccccccccccccccccc", ExpiresAt: now.Add(time.Hour), MaxDownloads: 1, Downloads: 1}
	consumedPath := writeLinkFile(t, shareDir, consumed)

	corruptPath := filepath.Join(shareDir, "corrupt.json")
	if err := os.WriteFile(corruptPath, []byte("{not-json"), 0o600); err != nil {
		t.Fatalf("写入损坏文件失败: %v", err)
	}
	bigPath := filepath.Join(shareDir, "big.json")
	if err := os.WriteFile(bigPath, bytes.Repeat([]byte("x"), maxShareLinkBytes+1), 0o600); err != nil {
		t.Fatalf("写入超限文件失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(shareDir, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("写入非 json 文件失败: %v", err)
	}
	dirEntry := filepath.Join(shareDir, "dir.json")
	if err := os.Mkdir(dirEntry, 0o755); err != nil {
		t.Fatalf("准备目录条目失败: %v", err)
	}

	s := NewStore(testutil.DiscardLogger())
	defer s.Stop()
	s.EnablePersist(testRootAt(t, dir), "meta/share")

	got := s.Peek(valid.Token)
	if got == nil {
		t.Fatalf("有效条目应恢复 %s", valid.Token)
	}
	if got.TenantID != "alice" || got.Rel != "user/ok.txt" || got.Volume != "vol-1" ||
		got.Owner != "ak-1" || got.Downloads != 1 || got.MaxDownloads != 2 ||
		!got.ReadOnly || got.WatermarkSeed != "w" {
		t.Fatalf("恢复字段不一致: %+v", got)
	}
	if s.Len() != 1 {
		t.Fatalf("只应恢复 1 条有效分享，got %d", s.Len())
	}
	for _, p := range []string{expiredPath, consumedPath} {
		if _, serr := os.Stat(p); !os.IsNotExist(serr) {
			t.Fatalf("过期/消费完的残留应删除落盘 %s: %v", p, serr)
		}
	}
	for _, p := range []string{corruptPath, bigPath, dirEntry} {
		if _, serr := os.Stat(p); serr != nil {
			t.Fatalf("损坏/超限/目录条目不应被删除 %s: %v", p, serr)
		}
	}
}

func TestStore_EvictExpiredThenInsert(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	// 灌满容量且全部过期：Create 前先全量清理，腾出空间后直接插入。
	base := time.Now()
	fillLinks(s, MaxShareEntries, func(int) time.Time { return base.Add(-time.Hour) }, base.Add(-time.Minute))

	link, err := s.Create(Spec{Filename: "new.txt", TenantID: "alice", Rel: "user/new.txt", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link == nil {
		t.Fatal("Create 应返回新链接")
	}
	if n := s.Len(); n != 1 {
		t.Fatalf("过期全清理后应只剩新链接，Len=%d", n)
	}
	if s.Peek(link.Token) == nil {
		t.Fatal("新链接应可见")
	}
}

func TestStore_EvictOldestWhenFull(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	defer s.Stop()

	// 灌满容量且全部有效（创建时间递增）：仍满 ⇒ 淘汰最旧 10%。
	base := time.Now().Add(-24 * time.Hour)
	tokens := fillLinks(s, MaxShareEntries, func(i int) time.Time { return base.Add(time.Duration(i) * time.Second) }, base.Add(48*time.Hour))

	if _, err := s.Create(Spec{Filename: "new.txt", TenantID: "alice", Rel: "user/new.txt", TTL: time.Hour}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	evictCount := MaxShareEntries / 10
	if n, want := s.Len(), MaxShareEntries-evictCount+1; n != want {
		t.Fatalf("Len=%d want %d（淘汰 %d 条后插入 1 条）", n, want, evictCount)
	}
	for i := range evictCount {
		if s.Peek(tokens[i]) != nil {
			t.Fatalf("最旧的第 %d 条应被淘汰: %s", i, tokens[i])
		}
	}
	if s.Peek(tokens[evictCount]) == nil {
		t.Fatalf("第 %d 条（未被淘汰）应保留", evictCount)
	}
	if s.Peek(tokens[len(tokens)-1]) == nil {
		t.Fatal("最新的条目应保留")
	}
}

func TestStore_CleanupLoopRunsOnStop(t *testing.T) {
	t.Parallel()
	s := newMemStore()
	if _, err := s.Create(Spec{Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", TTL: -time.Millisecond}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.cleanupLoop()
	}()
	// Stop 关闭 stopCh ⇒ cleanupLoop 先 CleanupExpired 再退出。
	s.Stop()
	testutil.WaitFor(t, 5*time.Second, func() bool { return s.Len() == 0 }, "cleanupLoop 应在 Stop 时清理过期链接")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanupLoop 未在 Stop 后退出")
	}
}

// fillLinks 直接填充 n 条链接（白盒构造，CreatedAt 由 created 决定），返回 token 列表。
func fillLinks(s *Store, n int, created func(i int) time.Time, expiresAt time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	tokens := make([]string, 0, n)
	for i := range n {
		token := fmt.Sprintf("%032x", i+1)
		s.links[token] = &Link{
			Token: token, Filename: token + ".txt", TenantID: "alice",
			Rel: "user/" + token + ".txt", Owner: "ak-1",
			CreatedAt: created(i), ExpiresAt: expiresAt,
		}
		tokens = append(tokens, token)
	}
	return tokens
}
