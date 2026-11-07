// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package share

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/state"
	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/testutil"
)

// fakeStateStore 是 state.StateStore 的进程内替身：支持注入读写错误、CAS 冲突与
// 伪键列表（容量上限场景）。Delete/CAS 删除沿用 LocalStateStore 的幂等语义。
type fakeStateStore struct {
	mu       sync.Mutex
	data     map[string][]byte
	hitAll   bool                   // Get 恒命中（伪值 {}）——用于耗尽 token 生成重试
	getHook  func(key string) error // 返回非 nil → Get 直接返回该错误
	putErr   error                  // 非 nil → Put 失败
	delErr   error                  // 非 nil → Delete 失败
	listErr  error                  // 非 nil → List 失败
	casHook  func(key string) error // 返回非 nil → CAS 直接返回该错误（nil = 正常执行）
	extraKey []string               // List 额外返回的键（不落 data）
	casCalls int
}

func newFakeState() *fakeStateStore { return &fakeStateStore{data: map[string][]byte{}} }

func (f *fakeStateStore) Get(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getHook != nil {
		if err := f.getHook(key); err != nil {
			return nil, err
		}
	}
	if v, ok := f.data[key]; ok {
		return append([]byte(nil), v...), nil
	}
	if f.hitAll {
		return []byte(`{}`), nil
	}
	return nil, state.ErrKeyNotFound
}

func (f *fakeStateStore) Put(_ context.Context, key string, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.data[key] = append([]byte(nil), data...)
	return nil
}

func (f *fakeStateStore) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.data, key) // 幂等（对齐 LocalStateStore）
	return nil
}

func (f *fakeStateStore) List(_ context.Context, prefix string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	keys := make([]string, 0, len(f.data)+len(f.extraKey))
	for k := range f.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	for _, k := range f.extraKey {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys, nil
}

func (f *fakeStateStore) CAS(_ context.Context, key string, old, newVal []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.casCalls++
	if f.casHook != nil {
		if err := f.casHook(key); err != nil {
			return err
		}
	}
	cur, ok := f.data[key]
	if old == nil {
		if ok {
			return state.ErrCASMismatch
		}
	} else if !ok || !bytes.Equal(cur, old) {
		return state.ErrCASMismatch
	}
	if newVal == nil {
		delete(f.data, key)
		return nil
	}
	f.data[key] = append([]byte(nil), newVal...)
	return nil
}

// seedStateLink 把一条分享写入 fake StateStore（share/<token> key）。
func seedStateLink(t *testing.T, f *fakeStateStore, l *Link) {
	t.Helper()
	data, err := json.Marshal(l.ToPersist())
	if err != nil {
		t.Fatalf("序列化分享失败: %v", err)
	}
	if perr := f.Put(context.Background(), shareKey(l.Token), data); perr != nil {
		t.Fatalf("seed 分享失败: %v", perr)
	}
}

// loadStateLink 从 fake StateStore 解出分享（断言存在）。
func loadStateLink(t *testing.T, f *fakeStateStore, token string) *Link {
	t.Helper()
	f.mu.Lock()
	raw, ok := f.data[shareKey(token)]
	f.mu.Unlock()
	if !ok {
		return nil
	}
	var pl LinkPersist
	if err := json.Unmarshal(raw, &pl); err != nil {
		t.Fatalf("解出分享失败: %v", err)
	}
	return pl.toLink()
}

// legacyFixture 是旧 <meta>/share 目录夹具（真实卷句柄 + 绝对路径 + 卷内相对路径）。
type legacyFixture struct {
	root *storage.Root
	abs  string
	rel  string
}

func newLegacyFixture(t *testing.T) legacyFixture {
	t.Helper()
	dir := t.TempDir()
	abs := filepath.Join(dir, "meta", "share")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatalf("准备旧 meta 目录失败: %v", err)
	}
	return legacyFixture{root: testRootAt(t, dir), abs: abs, rel: "meta/share"}
}

// legacyFilePath 返回 token 对应的旧落盘绝对路径。
func (f legacyFixture) legacyFilePath(token string) string {
	return filepath.Join(f.abs, token+".json")
}

// write 写入一条旧分享文件，返回绝对路径。
func (f legacyFixture) write(t *testing.T, l *Link) string {
	t.Helper()
	data, err := json.Marshal(l.ToPersist())
	if err != nil {
		t.Fatalf("序列化旧分享失败: %v", err)
	}
	path := f.legacyFilePath(l.Token)
	if werr := os.WriteFile(path, data, 0o600); werr != nil {
		t.Fatalf("写入旧分享失败: %v", werr)
	}
	return path
}

// mustNotExist 断言旧落盘文件已删除。
func (f legacyFixture) mustNotExist(t *testing.T, token string) {
	t.Helper()
	if _, err := os.Stat(f.legacyFilePath(token)); !os.IsNotExist(err) {
		t.Fatalf("旧分享文件应已删除 %s: %v", token, err)
	}
}

// mustExist 断言旧落盘文件仍存在。
func (f legacyFixture) mustExist(t *testing.T, token string) {
	t.Helper()
	if _, err := os.Stat(f.legacyFilePath(token)); err != nil {
		t.Fatalf("旧分享文件应仍存在 %s: %v", token, err)
	}
}

func TestStateBackedStore_RealStateStoreLifecycle(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	fx := newLegacyFixture(t)
	st := state.NewLocalStateStore(filepath.Join(dir, "state"), testutil.DiscardLogger())
	s := NewStateBackedStore(st, fx.root, fx.rel, testutil.DiscardLogger())
	defer s.Stop()

	link, err := s.Create(Spec{
		Filename: "a.txt", TenantID: "alice", Rel: "user/a.txt", Volume: "vol-1",
		Owner: "ak-1", TTL: time.Hour, MaxDownloads: 2, ReadOnly: true, WatermarkSeed: "w",
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if link.Token == "" {
		t.Fatal("Create 应返回 token")
	}
	if _, gerr := st.Get(context.Background(), shareKey(link.Token)); gerr != nil {
		t.Fatalf("Create 应写入 share/<token> key: %v", gerr)
	}

	got := s.Peek(link.Token)
	if got == nil {
		t.Fatalf("Peek 应命中 %s", link.Token)
	}
	if got.Owner != "ak-1" || got.Volume != "vol-1" || got.Rel != "user/a.txt" ||
		got.MaxDownloads != 2 || !got.ReadOnly || got.WatermarkSeed != "w" {
		t.Fatalf("Peek 字段不一致: %+v", got)
	}
	got.Downloads = 42 // 副本语义
	if again := s.Peek(link.Token); again.Downloads != 0 {
		t.Fatalf("Peek 必须返回副本，got downloads=%d", again.Downloads)
	}
	if s.Peek("0123456789abcdef0123456789abcdef") != nil {
		t.Fatal("未知 token 应返回 nil")
	}

	if n := len(s.List("")); n != 1 {
		t.Fatalf("List(\"\")=%d want 1", n)
	}
	if n := len(s.List("ak-1")); n != 1 {
		t.Fatalf("List(owner)=%d want 1", n)
	}
	if n := len(s.List("ak-other")); n != 0 {
		t.Fatalf("List(他人)=%d want 0", n)
	}

	if c := s.Consume(link.Token); c == nil || c.Downloads != 1 {
		t.Fatalf("Consume 应成功且计数 1: %+v", c)
	}
	if p := s.Peek(link.Token); p == nil || p.Downloads != 1 {
		t.Fatalf("Consume 计数应持久化: %+v", p)
	}

	if rerr := s.Revoke(link.Token, "ak-other"); rerr == nil {
		t.Fatal("跨租户撤销必须拒绝")
	}
	if rerr := s.Revoke(link.Token, "ak-1"); rerr != nil {
		t.Fatalf("本人撤销应成功: %v", rerr)
	}
	if s.Peek(link.Token) != nil {
		t.Fatal("撤销后 Peek 应为 nil")
	}
	if rerr := s.Revoke(link.Token, "ak-1"); rerr == nil {
		t.Fatal("撤销不存在的分享应报错")
	}
}

func TestStateBackedStore_CreateErrors(t *testing.T) {
	t.Parallel()

	t.Run("写入失败", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.putErr = errors.New("put boom")
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if _, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour}); err == nil {
			t.Fatal("Put 失败时 Create 必须报错")
		}
	})

	t.Run("token 重试耗尽", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.hitAll = true // 任何 token 都被视为已占用
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		_, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour})
		if err == nil || !strings.Contains(err.Error(), "无法生成唯一的分享 token") {
			t.Fatalf("token 重试耗尽应报错，got %v", err)
		}
	})

	t.Run("读取错误不中断生成", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.getHook = func(string) error { return errors.New("get boom") }
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		link, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour})
		if err != nil {
			t.Fatalf("非 KeyNotFound 读取错误不应阻塞创建: %v", err)
		}
		if link == nil {
			t.Fatal("Create 应返回链接")
		}
	})

	t.Run("List 失败跳过容量淘汰", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.listErr = errors.New("list boom")
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if _, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour}); err != nil {
			t.Fatalf("List 失败不应阻塞创建: %v", err)
		}
	})
}

func TestStateBackedStore_CreateEviction(t *testing.T) {
	t.Parallel()

	t.Run("未解析键跳过", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		// List 报满，但键都不存在（loadFromState 返回 nil）→ 全部跳过、无淘汰、创建成功。
		f.extraKey = fakeKeys("phantom", MaxShareEntries)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if _, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	})

	t.Run("Create 前清理过期与达上限条目", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		now := time.Now()
		expired := &Link{Token: "aaaa0000000000000000000000000001", ExpiresAt: now.Add(-time.Hour)}
		seedStateLink(t, f, expired)
		consumed := &Link{Token: "aaaa0000000000000000000000000002", ExpiresAt: now.Add(time.Hour), MaxDownloads: 1, Downloads: 1}
		seedStateLink(t, f, consumed)
		active := &Link{Token: "aaaa0000000000000000000000000003", ExpiresAt: now.Add(time.Hour)}
		seedStateLink(t, f, active)
		f.extraKey = fakeKeys("phantom", MaxShareEntries)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if _, err := s.Create(Spec{Filename: "a.txt", TTL: time.Hour}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		// Create 先走 CleanupExpired（过期 + 达上限都清理）。
		if loadStateLink(t, f, expired.Token) != nil {
			t.Fatal("过期条目应在 Create 前的清理中删除")
		}
		if loadStateLink(t, f, consumed.Token) != nil {
			t.Fatal("达上限条目应在 Create 前的清理中删除")
		}
		if loadStateLink(t, f, active.Token) == nil {
			t.Fatal("有效条目不得被删除")
		}
	})

	// evictLocked 的「过期条目」分支在 Create 路径上不可达（前一步 CleanupExpired 已清干净），
	// 因此白盒直调覆盖：evictLocked 只看 ExpiresAt 与容量，不重复看下载上限。
	t.Run("evictLocked 清理过期", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		expired := &Link{Token: "bbbb0000000000000000000000000001", ExpiresAt: time.Now().Add(-time.Hour)}
		seedStateLink(t, f, expired)
		active := &Link{Token: "bbbb0000000000000000000000000002", ExpiresAt: time.Now().Add(time.Hour)}
		seedStateLink(t, f, active)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		keys, err := f.List(context.Background(), sharePrefix)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if eerr := s.evictLocked(context.Background(), append(keys, sharePrefix+"ghost")); eerr != nil {
			t.Fatalf("evictLocked: %v", eerr)
		}
		if loadStateLink(t, f, expired.Token) != nil {
			t.Fatal("evictLocked 应删除过期条目")
		}
		if loadStateLink(t, f, active.Token) == nil {
			t.Fatal("evictLocked 不得删除未过期条目（未达容量）")
		}
	})

	t.Run("仍满淘汰最旧10%", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		base := time.Now().Add(-24 * time.Hour)
		tokens := make([]string, 0, MaxShareEntries)
		for i := range MaxShareEntries {
			token := fmt.Sprintf("%032x", i+1)
			seedStateLink(t, f, &Link{
				Token: token, Filename: token + ".txt", Owner: "ak-1",
				CreatedAt: base.Add(time.Duration(i) * time.Second), ExpiresAt: base.Add(48 * time.Hour),
			})
			tokens = append(tokens, token)
		}
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if _, err := s.Create(Spec{Filename: "new.txt", Owner: "ak-1", TTL: time.Hour}); err != nil {
			t.Fatalf("Create: %v", err)
		}
		evictCount := MaxShareEntries / 10
		if n, want := len(s.List("")), MaxShareEntries-evictCount+1; n != want {
			t.Fatalf("淘汰后条目数=%d want %d", n, want)
		}
		for i := range evictCount {
			if loadStateLink(t, f, tokens[i]) != nil {
				t.Fatalf("最旧的第 %d 条应被淘汰: %s", i, tokens[i])
			}
		}
		if loadStateLink(t, f, tokens[evictCount]) == nil {
			t.Fatalf("第 %d 条（未被淘汰）应保留", evictCount)
		}
	})
}

func TestStateBackedStore_PeekBranches(t *testing.T) {
	t.Parallel()

	t.Run("StateStore 读取错误", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.getHook = func(string) error { return errors.New("get boom") }
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Peek("0123456789abcdef0123456789abcdef"); got != nil {
			t.Fatalf("读取错误应返回 nil，got %+v", got)
		}
	})

	t.Run("StateStore 值损坏", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		const token = "0123456789abcdef0123456789abcdef"
		if err := f.Put(context.Background(), shareKey(token), []byte("{bad")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Peek(token); got != nil {
			t.Fatalf("损坏值应返回 nil，got %+v", got)
		}
	})

	t.Run("回退旧 meta", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		legacy := &Link{
			Token: "99999999999999999999999999999999", Filename: "old.txt", TenantID: "alice",
			Rel: "user/old.txt", Owner: "ak-1", ExpiresAt: time.Now().Add(time.Hour),
		}
		fx.write(t, legacy)
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		got := s.Peek(legacy.Token)
		if got == nil || got.Filename != "old.txt" || got.Rel != "user/old.txt" {
			t.Fatalf("StateStore 未命中应回退旧 meta: %+v", got)
		}
	})

	t.Run("旧 meta 值损坏", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		const token = "88888888888888888888888888888888"
		if err := os.WriteFile(fx.legacyFilePath(token), []byte("{bad"), 0o600); err != nil {
			t.Fatalf("写入损坏旧文件: %v", err)
		}
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		if got := s.Peek(token); got != nil {
			t.Fatalf("损坏旧文件应返回 nil，got %+v", got)
		}
	})

	t.Run("旧 meta 超出读取上限", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		const token = "77777777777777777777777777777777"
		if err := os.WriteFile(fx.legacyFilePath(token), bytes.Repeat([]byte("x"), maxShareLinkBytes+1), 0o600); err != nil {
			t.Fatalf("写入超限旧文件: %v", err)
		}
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		if got := s.Peek(token); got != nil {
			t.Fatalf("超限旧文件应返回 nil，got %+v", got)
		}
	})
}

func TestStateBackedStore_ConsumeStateBranches(t *testing.T) {
	t.Parallel()

	t.Run("未知 token", func(t *testing.T) {
		t.Parallel()
		s := NewStateBackedStore(newFakeState(), nil, "", testutil.DiscardLogger())
		if got := s.Consume("0123456789abcdef0123456789abcdef"); got != nil {
			t.Fatalf("未知 token 应返回 nil，got %+v", got)
		}
	})

	t.Run("读取错误", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.getHook = func(string) error { return errors.New("get boom") }
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume("0123456789abcdef0123456789abcdef"); got != nil {
			t.Fatalf("读取错误应返回 nil，got %+v", got)
		}
	})

	t.Run("值损坏视为已消费", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		const token = "0123456789abcdef0123456789abcdef"
		if err := f.Put(context.Background(), shareKey(token), []byte("{bad")); err != nil {
			t.Fatalf("seed: %v", err)
		}
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume(token); got != nil {
			t.Fatalf("损坏值应视为已消费，got %+v", got)
		}
	})

	t.Run("已过期删除", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		f := newFakeState()
		link := &Link{Token: "0000000000000000000000000000dead", Owner: "ak-1", ExpiresAt: time.Now().Add(-time.Second)}
		seedStateLink(t, f, link)
		fx.write(t, link) // 旧文件也应同步清理
		s := NewStateBackedStore(f, fx.root, fx.rel, testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("过期应返回 nil，got %+v", got)
		}
		if loadStateLink(t, f, link.Token) != nil {
			t.Fatal("过期条目应从 StateStore 删除")
		}
		fx.mustNotExist(t, link.Token)
	})

	t.Run("达到下载上限删除", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		link := &Link{Token: "0000000000000000000000000000beef", ExpiresAt: time.Now().Add(time.Hour), MaxDownloads: 1, Downloads: 1}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("达到上限应返回 nil，got %+v", got)
		}
		if loadStateLink(t, f, link.Token) != nil {
			t.Fatal("达到上限的条目应删除")
		}
	})

	t.Run("一次性 CAS 删除", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		f := newFakeState()
		link := &Link{Token: "0000000000000000000000000000cafe", ExpiresAt: time.Now().Add(time.Hour), OneTime: true}
		seedStateLink(t, f, link)
		fx.write(t, link)
		s := NewStateBackedStore(f, fx.root, fx.rel, testutil.DiscardLogger())
		got := s.Consume(link.Token)
		// 一次性分享走 CAS 删除，不递增 Downloads（与 *Store 的「先 ++ 再删」形态不同；
		// 链接随即失效，计数无消费方依赖——此处锁定当前行为）。
		if got == nil || got.Token != link.Token || got.Downloads != 0 {
			t.Fatalf("一次性首次消费应返回链接（计数 0）: %+v", got)
		}
		if loadStateLink(t, f, link.Token) != nil {
			t.Fatal("一次性消费应删除 StateStore key")
		}
		fx.mustNotExist(t, link.Token)
		if again := s.Consume(link.Token); again != nil {
			t.Fatalf("一次性二次消费应返回 nil，got %+v", again)
		}
	})

	t.Run("计数递增持久化", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		link := &Link{Token: "00000000000000000000000000000abc", ExpiresAt: time.Now().Add(time.Hour)}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		for want := 1; want <= 2; want++ {
			got := s.Consume(link.Token)
			if got == nil || got.Downloads != want {
				t.Fatalf("第 %d 次消费应返回计数 %d: %+v", want, want, got)
			}
			if stored := loadStateLink(t, f, link.Token); stored == nil || stored.Downloads != want {
				t.Fatalf("第 %d 次消费计数未持久化: %+v", want, stored)
			}
		}
	})
}

func TestStateBackedStore_ConsumeCASRetry(t *testing.T) {
	t.Parallel()

	t.Run("一次性冲突后重试成功", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		var hookMu sync.Mutex
		calls := 0
		// 首次 CAS 冲突（模拟并发消费抢占），第二次成功。
		f.casHook = func(string) error {
			hookMu.Lock()
			defer hookMu.Unlock()
			calls++
			if calls == 1 {
				return state.ErrCASMismatch
			}
			return nil
		}
		link := &Link{Token: "0000000000000000000000000000f00d", ExpiresAt: time.Now().Add(time.Hour), OneTime: true}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		got := s.Consume(link.Token)
		if got == nil || got.Token != link.Token {
			t.Fatalf("CAS 冲突重试后应消费成功: %+v", got)
		}
		if f.casCalls != 2 {
			t.Fatalf("应重试一次（CAS 调用 2 次），got %d", f.casCalls)
		}
		if loadStateLink(t, f, link.Token) != nil {
			t.Fatal("一次性消费最终应删除 key")
		}
	})

	t.Run("计数冲突后重试成功", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		var hookMu sync.Mutex
		calls := 0
		f.casHook = func(string) error {
			hookMu.Lock()
			defer hookMu.Unlock()
			calls++
			if calls == 1 {
				return state.ErrCASMismatch
			}
			return nil
		}
		link := &Link{Token: "0000000000000000000000000000f00e", ExpiresAt: time.Now().Add(time.Hour)}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		got := s.Consume(link.Token)
		if got == nil || got.Downloads != 1 {
			t.Fatalf("冲突重试后计数应为 1: %+v", got)
		}
		if stored := loadStateLink(t, f, link.Token); stored == nil || stored.Downloads != 1 {
			t.Fatal("冲突重试后计数应只递增一次")
		}
	})

	t.Run("恒冲突重试耗尽", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.casHook = func(string) error { return state.ErrCASMismatch }
		link := &Link{Token: "0000000000000000000000000000f00f", ExpiresAt: time.Now().Add(time.Hour), OneTime: true}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("CAS 恒冲突应返回 nil，got %+v", got)
		}
		if f.casCalls != maxConsumeRetries {
			t.Fatalf("CAS 调用应为 %d 次上限，got %d", maxConsumeRetries, f.casCalls)
		}
	})

	t.Run("CAS 其它错误不重试", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.casHook = func(string) error { return errors.New("cas boom") }
		link := &Link{Token: "0000000000000000000000000000f0a0", ExpiresAt: time.Now().Add(time.Hour)}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("CAS 非冲突错误应直接返回 nil，got %+v", got)
		}
		// 非 ErrCASMismatch 的 CAS 失败按「最终失败」处理（不重试，避免对不可恢复错误空转）。
		if f.casCalls != 1 {
			t.Fatalf("CAS 非冲突错误不应重试，调用 %d 次", f.casCalls)
		}
		if stored := loadStateLink(t, f, link.Token); stored == nil || stored.Downloads != 0 {
			t.Fatalf("CAS 失败不得改动计数: %+v", stored)
		}
	})

	t.Run("一次性 CAS 其它错误", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.casHook = func(string) error { return errors.New("cas boom") }
		link := &Link{Token: "0000000000000000000000000000f0a1", ExpiresAt: time.Now().Add(time.Hour), OneTime: true}
		seedStateLink(t, f, link)
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("一次性 CAS 非冲突错误应直接返回 nil，got %+v", got)
		}
		if f.casCalls != 1 {
			t.Fatalf("一次性 CAS 非冲突错误不应重试，调用 %d 次", f.casCalls)
		}
		if loadStateLink(t, f, link.Token) == nil {
			t.Fatal("CAS 失败不得删除条目")
		}
	})
}

func TestStateBackedStore_ConsumeLegacy(t *testing.T) {
	t.Parallel()

	t.Run("一次性", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		link := &Link{Token: "11111111111111111111111111111111", ExpiresAt: time.Now().Add(time.Hour), OneTime: true}
		fx.write(t, link)
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		got := s.Consume(link.Token)
		if got == nil || got.Token != link.Token {
			t.Fatalf("旧 meta 一次性消费应返回链接: %+v", got)
		}
		fx.mustNotExist(t, link.Token)
	})

	t.Run("计数递增并回写", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		link := &Link{Token: "22222222222222222222222222222222", ExpiresAt: time.Now().Add(time.Hour)}
		fx.write(t, link)
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		got := s.Consume(link.Token)
		if got == nil || got.Downloads != 1 {
			t.Fatalf("旧 meta 计数消费应返回计数 1: %+v", got)
		}
		raw, err := os.ReadFile(fx.legacyFilePath(link.Token))
		if err != nil {
			t.Fatalf("旧 meta 计数应回写: %v", err)
		}
		var pl LinkPersist
		if uerr := json.Unmarshal(raw, &pl); uerr != nil {
			t.Fatalf("解析回写文件失败: %v", uerr)
		}
		if pl.Downloads != 1 {
			t.Fatalf("回写计数=%d want 1", pl.Downloads)
		}
	})

	t.Run("已过期", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		link := &Link{Token: "33333333333333333333333333333333", ExpiresAt: time.Now().Add(-time.Second)}
		fx.write(t, link)
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("过期旧 meta 应返回 nil，got %+v", got)
		}
		fx.mustNotExist(t, link.Token)
	})

	t.Run("达到上限", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		link := &Link{Token: "44444444444444444444444444444444", ExpiresAt: time.Now().Add(time.Hour), MaxDownloads: 2, Downloads: 2}
		fx.write(t, link)
		s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
		if got := s.Consume(link.Token); got != nil {
			t.Fatalf("达到上限旧 meta 应返回 nil，got %+v", got)
		}
		fx.mustNotExist(t, link.Token)
	})
}

func TestStateBackedStore_ListWithLegacy(t *testing.T) {
	t.Parallel()
	fx := newLegacyFixture(t)
	f := newFakeState()
	now := time.Now()

	stateOwned := &Link{Token: "55555555555555555555555555555555", Owner: "ak-1", ExpiresAt: now.Add(time.Hour)}
	stateOther := &Link{Token: "66666666666666666666666666666666", Owner: "ak-2", ExpiresAt: now.Add(time.Hour)}
	seedStateLink(t, f, stateOwned)
	seedStateLink(t, f, stateOther)

	// 旧 meta：一条与 StateStore 同 token（去重）、一条独有、一条他人、一条损坏。
	fx.write(t, &Link{Token: stateOwned.Token, Owner: "ak-1", ExpiresAt: now.Add(time.Hour)})
	legacyOwned := &Link{Token: "77777777777777777777777777777777", Owner: "ak-3", ExpiresAt: now.Add(time.Hour)}
	fx.write(t, legacyOwned)
	fx.write(t, &Link{Token: "88888888888888888888888888888888", Owner: "ak-9", ExpiresAt: now.Add(time.Hour)})
	if err := os.WriteFile(fx.legacyFilePath("99999999999999999999999999999999"), []byte("{bad"), 0o600); err != nil {
		t.Fatalf("写入损坏旧文件: %v", err)
	}
	// StateStore 中一条损坏值（loadFromState 报错 → 跳过）与一条幽灵键（未命中 → 跳过）。
	if err := f.Put(context.Background(), shareKey("abababababababababababababababab"), []byte("{bad")); err != nil {
		t.Fatalf("seed 损坏值: %v", err)
	}
	f.extraKey = []string{sharePrefix + "ghost"}
	// 目录与非 json 条目：扫描时必须跳过。
	if err := os.Mkdir(filepath.Join(fx.abs, "dir.json"), 0o755); err != nil {
		t.Fatalf("准备目录条目失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fx.abs, "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备非 json 文件失败: %v", err)
	}

	s := NewStateBackedStore(f, fx.root, fx.rel, testutil.DiscardLogger())
	all := s.List("")
	tokens := map[string]int{}
	for _, l := range all {
		tokens[l.Token]++
	}
	// 期望：state 2 条 + 旧 meta 独有 2 条（同 token 去重；损坏/目录/非 json 跳过）。
	if len(all) != 4 {
		t.Fatalf("List(\"\")=%d want 4: %v", len(all), tokens)
	}
	if tokens[stateOwned.Token] != 1 {
		t.Fatalf("StateStore 与旧 meta 同 token 应去重，got %d", tokens[stateOwned.Token])
	}
	if tokens[legacyOwned.Token] != 1 {
		t.Fatal("旧 meta 独有分享应补列")
	}

	mine := s.List("ak-1")
	if len(mine) != 1 || mine[0].Token != stateOwned.Token {
		t.Fatalf("List(ak-1)=%+v want 仅 %s", mine, stateOwned.Token)
	}
	if got := s.List("ak-3"); len(got) != 1 || got[0].Token != legacyOwned.Token {
		t.Fatalf("List(ak-3)=%+v want 仅旧 meta 独有条目", got)
	}
}

func TestStateBackedStore_RevokeBranches(t *testing.T) {
	t.Parallel()

	t.Run("StateStore 删除失败", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		link := &Link{Token: "aaaa00000000000000000000000000aa", Owner: "ak-1", ExpiresAt: time.Now().Add(time.Hour)}
		seedStateLink(t, f, link)
		f.delErr = errors.New("delete boom")
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		if err := s.Revoke(link.Token, ""); err == nil {
			t.Fatal("StateStore 删除失败应报错")
		}
	})

	t.Run("StateStore 读取失败", func(t *testing.T) {
		t.Parallel()
		f := newFakeState()
		f.getHook = func(string) error { return errors.New("get boom") }
		s := NewStateBackedStore(f, nil, "", testutil.DiscardLogger())
		err := s.Revoke("aaaa00000000000000000000000000bb", "")
		if err == nil || !strings.Contains(err.Error(), "分享链接不存在") {
			t.Fatalf("读取失败应按不存在报错（防枚举），got %v", err)
		}
	})

	t.Run("旧 meta 独有条目可撤销", func(t *testing.T) {
		t.Parallel()
		fx := newLegacyFixture(t)
		link := &Link{Token: "aaaa00000000000000000000000000cc", Owner: "ak-1", ExpiresAt: time.Now().Add(time.Hour)}
		fx.write(t, link)
		f := newFakeState()
		s := NewStateBackedStore(f, fx.root, fx.rel, testutil.DiscardLogger())
		if err := s.Revoke(link.Token, "ak-1"); err != nil {
			t.Fatalf("旧 meta 独有条目应可撤销: %v", err)
		}
		fx.mustNotExist(t, link.Token)
	})

	t.Run("缺省回退路径不存在条目", func(t *testing.T) {
		t.Parallel()
		// legacyRoot 为 nil：LegacyRelPath 为空，双读/双删均为 no-op。
		s := NewStateBackedStore(newFakeState(), nil, "", testutil.DiscardLogger())
		if got := s.LegacyRelPath("tok"); got != "" {
			t.Fatalf("未装配旧 meta 时 LegacyRelPath 应为空，got %q", got)
		}
		if s.LegacyRoot() != nil {
			t.Fatal("未装配旧 meta 时 LegacyRoot 应为 nil")
		}
		if err := s.Revoke("aaaa00000000000000000000000000dd", ""); err == nil {
			t.Fatal("条目不存在时 Revoke 应报错")
		}
	})
}

func TestStateBackedStore_CleanupExpired(t *testing.T) {
	t.Parallel()
	fx := newLegacyFixture(t)
	f := newFakeState()
	now := time.Now()

	expired := &Link{Token: "eeee00000000000000000000000000e1", ExpiresAt: now.Add(-time.Hour)}
	maxed := &Link{Token: "eeee00000000000000000000000000e2", ExpiresAt: now.Add(time.Hour), MaxDownloads: 1, Downloads: 1}
	active := &Link{Token: "eeee00000000000000000000000000e3", ExpiresAt: now.Add(time.Hour)}
	seedStateLink(t, f, expired)
	seedStateLink(t, f, maxed)
	seedStateLink(t, f, active)
	f.extraKey = []string{sharePrefix + "eeee00000000000000000000000000ff"} // 未解析键（跳过）

	legacyExpired := &Link{Token: "ffff00000000000000000000000000f1", ExpiresAt: now.Add(-time.Hour)}
	legacyMaxed := &Link{Token: "ffff00000000000000000000000000f2", ExpiresAt: now.Add(time.Hour), MaxDownloads: 1, Downloads: 1}
	legacyActive := &Link{Token: "ffff00000000000000000000000000f3", ExpiresAt: now.Add(time.Hour)}
	fx.write(t, legacyExpired)
	fx.write(t, legacyMaxed)
	fx.write(t, legacyActive)
	if err := os.WriteFile(fx.legacyFilePath("ffff00000000000000000000000000f4"), []byte("{bad"), 0o600); err != nil {
		t.Fatalf("写入损坏旧文件: %v", err)
	}
	if err := os.Mkdir(filepath.Join(fx.abs, "ffff00000000000000000000000000f5.json"), 0o755); err != nil {
		t.Fatalf("准备目录条目失败: %v", err)
	}

	s := NewStateBackedStore(f, fx.root, fx.rel, testutil.DiscardLogger())
	s.CleanupExpired()

	if loadStateLink(t, f, expired.Token) != nil || loadStateLink(t, f, maxed.Token) != nil {
		t.Fatal("过期/达上限的 StateStore 条目应被清理")
	}
	if loadStateLink(t, f, active.Token) == nil {
		t.Fatal("有效 StateStore 条目不得被清理")
	}
	fx.mustNotExist(t, legacyExpired.Token)
	fx.mustNotExist(t, legacyMaxed.Token)
	fx.mustExist(t, legacyActive.Token)
}

func TestStateBackedStore_CleanupLegacyDirErrors(t *testing.T) {
	t.Parallel()
	// 旧目录不存在：ReadDir 失败 ⇒ List/清理均静默跳过。
	dir := t.TempDir()
	root := testRootAt(t, dir)
	s := NewStateBackedStore(newFakeState(), root, "meta/missing", testutil.DiscardLogger())
	if got := s.List(""); len(got) != 0 {
		t.Fatalf("旧目录不存在时 List 应为空，got %d", len(got))
	}
	s.CleanupExpired() // 不得 panic
	if s.LegacyRel() != "meta/missing" {
		t.Fatalf("LegacyRel()=%q", s.LegacyRel())
	}
}

func TestStateBackedStore_RemoveLegacyErrorWarns(t *testing.T) {
	t.Parallel()
	fx := newLegacyFixture(t)
	// 非空目录占住 <token>.json：Remove 失败（非 NotExist）→ 告警分支，不 panic。
	token := "dddd00000000000000000000000000d1"
	target := fx.legacyFilePath(token)
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatalf("准备目标目录失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(target, "keep"), []byte("x"), 0o600); err != nil {
		t.Fatalf("准备目标目录内容失败: %v", err)
	}
	s := NewStateBackedStore(newFakeState(), fx.root, fx.rel, testutil.DiscardLogger())
	s.removeLegacy(token)
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("删除失败不应静默吞掉目录: %v", err)
	}
}

func TestStateBackedStore_EnablePersistAndAccessors(t *testing.T) {
	t.Parallel()
	fx := newLegacyFixture(t)
	s := NewStateBackedStore(newFakeState(), nil, "", testutil.DiscardLogger())

	// 未装配回退：无旧路径。
	if got := s.LegacyRelPath("tok"); got != "" {
		t.Fatalf("未装配时 LegacyRelPath 应为空，got %q", got)
	}
	// root 为 nil / rel 为空：均为 no-op。
	s.EnablePersist(nil, "meta/share")
	s.EnablePersist(fx.root, "")
	if s.LegacyRoot() != nil {
		t.Fatal("空参数不应装配旧 meta")
	}
	// 正常装配。
	s.EnablePersist(fx.root, fx.rel)
	if s.LegacyRoot() == nil {
		t.Fatal("EnablePersist 应装配旧 meta 卷句柄")
	}
	if got, want := s.LegacyRelPath("tok"), "meta/share/tok.json"; got != want {
		t.Fatalf("LegacyRelPath=%q want %q", got, want)
	}
	if got := s.LegacyRel(); got != fx.rel {
		t.Fatalf("LegacyRel=%q want %q", got, fx.rel)
	}
	s.Stop() // 无后台 goroutine：调用即返回

	if got := shareKey("tok"); got != sharePrefix+"tok" {
		t.Fatalf("shareKey=%q", got)
	}
}

// fakeKeys 生成 n 个带前缀的伪 StateStore 键（share/<prefix>-<i>）。
func fakeKeys(prefix string, n int) []string {
	keys := make([]string, 0, n)
	for i := range n {
		keys = append(keys, sharePrefix+fmt.Sprintf("%s-%032d", prefix, i))
	}
	return keys
}
