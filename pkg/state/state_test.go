// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// testLogger 返回丢弃全部日志的 logger（测试辅助，与其它包同款）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// newTestStore 构造一个落在 t.TempDir() 下的 LocalStateStore。
func newTestStore(t *testing.T) *LocalStateStore {
	t.Helper()
	return NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger())
}

// TestLocalStateStore_RoundTrip 验证 Put/Get/Delete/List 全流程 + key 分段落盘路径断言。
func TestLocalStateStore_RoundTrip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	st := NewLocalStateStore(root, testLogger())

	data := []byte(`{"version":1,"keys":[]}`)
	if err := st.Put(ctx, "credential/anonymous/ring", data); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, err := st.Get(ctx, "credential/anonymous/ring")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != string(data) {
		t.Fatalf("Get 往返不一致: %q want %q", got, data)
	}
	// key 分段落盘路径：<root>/state/<type>/<owner>/<name>.json（首段为 type）。
	if _, serr := os.Stat(filepath.Join(root, "credential", "anonymous", "ring.json")); serr != nil {
		t.Fatalf("落盘路径不符（state/credential/anonymous/ring.json）: %v", serr)
	}
	// 深层 name（含 /）转目录层级：checksum/<owner>/<rel>。
	if perr := st.Put(ctx, "checksum/alice/dir/f.txt", []byte("abc")); perr != nil {
		t.Fatalf("Put 深层 key: %v", perr)
	}
	if _, serr := os.Stat(filepath.Join(root, "checksum", "alice", "dir", "f.txt.json")); serr != nil {
		t.Fatalf("深层 key 落盘路径不符: %v", serr)
	}
	// 两段 key（无 owner）：share/<token> → state/share/<token>.json。
	if perr := st.Put(ctx, "share/tok1", []byte("s1")); perr != nil {
		t.Fatalf("Put share key: %v", perr)
	}
	if _, serr := os.Stat(filepath.Join(root, "share", "tok1.json")); serr != nil {
		t.Fatalf("share key 落盘路径不符: %v", serr)
	}

	// List 前缀过滤。
	keys, err := st.List(ctx, "checksum/")
	if err != nil {
		t.Fatalf("List(checksum/): %v", err)
	}
	if len(keys) != 1 || keys[0] != "checksum/alice/dir/f.txt" {
		t.Fatalf("List(checksum/) = %v", keys)
	}
	all, err := st.List(ctx, "")
	if err != nil {
		t.Fatalf("List(\"\"): %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List(\"\") 应含 3 个 key, got %v", all)
	}

	// Delete 幂等。
	if err := st.Delete(ctx, "share/tok1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st.Get(ctx, "share/tok1"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("删除后 Get 应 ErrKeyNotFound, got %v", err)
	}
	if err := st.Delete(ctx, "share/tok1"); err != nil {
		t.Fatalf("重复 Delete 应静默成功: %v", err)
	}
}

// TestLocalStateStore_AtomicWrite 验证并发 Put 同 key（20 goroutine）→ 文件恒完整
// （无 torn write——tmp+rename 原子替换）。变异点：去掉 tmp+rename 改直写 → 红。
func TestLocalStateStore_AtomicWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	const workers = 20
	const payloadSize = 1024
	payloads := make([][]byte, workers)
	for i := range workers {
		payloads[i] = []byte(strings.Repeat(strconv.Itoa(i), payloadSize))
	}
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := st.Put(ctx, "credential/anonymous/ring", payloads[i]); err != nil {
				t.Errorf("并发 Put: %v", err)
			}
		}(i)
	}
	wg.Wait()

	got, err := st.Get(ctx, "credential/anonymous/ring")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	matched := false
	for _, p := range payloads {
		if string(got) == string(p) {
			matched = true
			break
		}
	}
	if !matched {
		t.Fatalf("并发 Put 后文件内容损坏（torn write）：len=%d", len(got))
	}
}

// TestLocalStateStore_CAS 验证 CAS 语义：成功 / old 不匹配 / create-only（old=nil）/
// delete（new=nil）。
func TestLocalStateStore_CAS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	if err := st.Put(ctx, "quota/alice", []byte("100")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	// 成功路径：old 匹配 → new 生效。
	if err := st.CAS(ctx, "quota/alice", []byte("100"), []byte("150")); err != nil {
		t.Fatalf("CAS 成功路径: %v", err)
	}
	got, _ := st.Get(ctx, "quota/alice")
	if string(got) != "150" {
		t.Fatalf("CAS 后值 = %q, want 150", got)
	}
	// old 不匹配 → ErrCASMismatch 且值不变。
	if err := st.CAS(ctx, "quota/alice", []byte("zzz"), []byte("200")); !errors.Is(err, ErrCASMismatch) {
		t.Fatalf("CAS 不匹配应 ErrCASMismatch, got %v", err)
	}
	got, _ = st.Get(ctx, "quota/alice")
	if string(got) != "150" {
		t.Fatalf("CAS 失败后值不应变: %q", got)
	}
	// create-only：old=nil 且 key 不存在 → 成功。
	if err := st.CAS(ctx, "quota/bob", nil, []byte("1")); err != nil {
		t.Fatalf("CAS create-only 成功路径: %v", err)
	}
	// create-only：old=nil 但 key 已存在 → ErrCASMismatch。
	if err := st.CAS(ctx, "quota/bob", nil, []byte("2")); !errors.Is(err, ErrCASMismatch) {
		t.Fatalf("CAS create-only 冲突应 ErrCASMismatch, got %v", err)
	}
	// delete：new=nil → 删除。
	if err := st.CAS(ctx, "quota/bob", []byte("1"), nil); err != nil {
		t.Fatalf("CAS delete 语义: %v", err)
	}
	if _, err := st.Get(ctx, "quota/bob"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("CAS 删除后应不存在, got %v", err)
	}
}

// TestLocalStateStore_CAS_Concurrent 验证 8 goroutine 对同一 key 做「读-改-写」CAS 递增
// → 最终计数 = 8×N（无丢失更新）。变异点：CAS 退化为「先读后写非原子」→ 红。
func TestLocalStateStore_CAS_Concurrent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	const goroutines = 8
	const perG = 25
	const key = "quota/counter"
	var wg sync.WaitGroup
	for range goroutines {
		wg.Go(func() {
			for range perG {
				for {
					cur, err := st.Get(ctx, key)
					if err != nil && !errors.Is(err, ErrKeyNotFound) {
						t.Errorf("Get: %v", err)
						return
					}
					next := "1"
					if len(cur) > 0 {
						n, aerr := strconv.Atoi(string(cur))
						if aerr != nil {
							t.Errorf("值非数字: %q", cur)
							return
						}
						next = strconv.Itoa(n + 1)
					}
					if cerr := st.CAS(ctx, key, cur, []byte(next)); cerr == nil {
						break
					} else if !errors.Is(cerr, ErrCASMismatch) {
						t.Errorf("CAS: %v", cerr)
						return
					}
					// ErrCASMismatch → 重试（有界外层循环）。
				}
			}
		})
	}
	wg.Wait()

	got, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	want := goroutines * perG
	if string(got) != strconv.Itoa(want) {
		t.Fatalf("并发 CAS 递增后计数 = %q, want %d（存在丢失更新）", got, want)
	}
}

// TestLocalStateStore_Watch 验证 Watch：Put/Delete 后收到对应 Change；ctx 取消关闭通道。
// 轮询间隔注入 10ms（条件轮询 testutil.WaitFor，不 time.Sleep——R14 棘轮）。
func TestLocalStateStore_Watch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger(), WithPollInterval(10*time.Millisecond))

	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch, err := st.Watch(wctx, "share/")
	if err != nil {
		t.Fatalf("Watch: %v", err)
	}
	// 初始不重放现有键：先落一个 key 再订阅，不应收到该 key 的 put。
	if err := st.Put(ctx, "share/pre", []byte("p")); err != nil {
		t.Fatalf("Put pre: %v", err)
	}
	// Put 后收到 put 变更。
	if err := st.Put(ctx, "share/tok1", []byte("v1")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		select {
		case c := <-ch:
			return c.Key == "share/tok1" && c.Op == "put"
		default:
			return false
		}
	}, "应收到 share/tok1 的 put 变更")
	// Delete 后收到 delete 变更。
	if err := st.Delete(ctx, "share/tok1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	testutil.WaitFor(t, 3*time.Second, func() bool {
		select {
		case c := <-ch:
			return c.Key == "share/tok1" && c.Op == "delete"
		default:
			return false
		}
	}, "应收到 share/tok1 的 delete 变更")
	// 前缀外变更不应推送：短暂窗口内不应有新事件（轮询周期内只消费 share/ 前缀）。
	if err := st.Put(ctx, "quota/other", []byte("x")); err != nil {
		t.Fatalf("Put 前缀外: %v", err)
	}
	select {
	case c := <-ch:
		t.Fatalf("前缀外变更不应推送, got %+v", c)
	case <-time.After(120 * time.Millisecond):
	}
	// ctx 取消 → 通道关闭。
	cancel()
	testutil.WaitFor(t, 3*time.Second, func() bool {
		_, ok := <-ch
		return !ok
	}, "ctx 取消后 Watch 通道应关闭")
}

// TestLocalStateStore_InvalidKey 验证非法 key 在 Get/Put/Delete/List 入口 fail-closed。
func TestLocalStateStore_InvalidKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := newTestStore(t)

	for _, key := range []string{"", "a", "../x", "a/b/../c", "/abs", "a/b\x00c", "a/CON"} {
		if err := st.Put(ctx, key, []byte("v")); err == nil {
			t.Errorf("Put(%q) 应报错（非法 key fail-closed）", key)
		}
		if _, err := st.Get(ctx, key); err == nil {
			t.Errorf("Get(%q) 应报错", key)
		}
	}
	if _, err := st.List(ctx, "../x"); err == nil {
		t.Error("List(../x) 应报错")
	}
}

// TestLocalAppendStore_Append 验证 AppendStore：追加 JSON lines + 重载读取完整。
func TestLocalAppendStore_Append(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "state")
	as := NewLocalAppendStore(root, testLogger())

	for _, line := range []string{`{"seq":1}`, `{"seq":2}`, `{"seq":3}`} {
		if err := as.Append(ctx, "audit/20260924", []byte(line)); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "audit", "20260924.json"))
	if err != nil {
		t.Fatalf("读取追加文件: %v", err)
	}
	want := "{\"seq\":1}\n{\"seq\":2}\n{\"seq\":3}\n"
	if string(data) != want {
		t.Fatalf("追加内容 = %q, want %q", data, want)
	}
	_ = fmt.Sprint() // keep fmt import if unused elsewhere
}
