// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mongo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// mongoEnv 返回 mongo 连接配置（MONGO_URI 环境变量可覆盖，默认 127.0.0.1:27017）。
func mongoEnv() (uri, db string) {
	uri = os.Getenv("MONGO_URI")
	if uri == "" {
		uri = "mongodb://127.0.0.1:27017"
	}
	db = os.Getenv("MONGO_DB")
	if db == "" {
		db = "sproxy_state_test"
	}
	return uri, db
}

// requireMongo 探测 mongo 可达性（Ping 5s 超时）——不可达 t.Skip（本地无 mongo /
// CI 非 mongo job 自动跳过，不失败；docker 容器 / CI services.mongo 存在时自动实跑）。
func requireMongo(t *testing.T) (*testClient, string) {
	t.Helper()
	uri, db := mongoEnv()
	client, err := newTestClient(t, uri, db)
	if err != nil {
		t.Skipf("Mongo 不可达（%v）——跳过集成测试（docker run mongo:7 或 CI services.mongo 存在时自动实跑）", err)
	}
	return client, db
}

// TestMongoStateStore_RoundTrip 核心往返：Put → Get → List → Delete 全流程 +
// 值跨实例可见（同一 mongo 两个 store 实例互见——多节点共享语义）。
func TestMongoStateStore_RoundTrip(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "roundtrip")
	st, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore: %v", err)
	}
	// 两个独立实例（模拟两个节点）。
	st2, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore#2: %v", err)
	}

	key := "credential/anonymous/ring"
	val := []byte(`{"version":1,"keys":[{"ak":"x"}]}`)
	if err2 := st.Put(ctx, key, val); err2 != nil {
		t.Fatalf("Put: %v", err2)
	}
	// 跨实例可见。
	got, err := st2.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, val) {
		t.Fatalf("Get = %q, want %q", got, val)
	}
	// Get 未命中 → ErrKeyNotFound（绝不返回 (nil, nil)）。
	if _, err2 := st.Get(ctx, "no/such/key"); !errors.Is(err2, state.ErrKeyNotFound) {
		t.Fatalf("Get 未命中应返回 ErrKeyNotFound, got %v", err2)
	}
	// List 前缀。
	keys, err := st.List(ctx, "credential/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !contains(keys, key) {
		t.Fatalf("List(credential/) 应含 %q, got %v", key, keys)
	}
	// Delete（幂等：不存在静默成功）。
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := st2.Get(ctx, key); !errors.Is(err, state.ErrKeyNotFound) {
		t.Fatalf("Delete 后 Get 应 ErrKeyNotFound, got %v", err)
	}
	if err := st.Delete(ctx, key); err != nil {
		t.Fatalf("重复 Delete 应幂等: %v", err)
	}
}

// TestMongoStateStore_CAS 核心 CAS 语义：成功路径 / old 不匹配 → ErrCASMismatch /
// create-only（old=nil 且已存在 → 失败）/ delete 语义（new=nil）。
func TestMongoStateStore_CAS(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "cas")
	st, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore: %v", err)
	}
	key := "quota/owner-a"

	// create-only：不存在 → 成功。
	if err := st.CAS(ctx, key, nil, []byte("v1")); err != nil {
		t.Fatalf("CAS create-only: %v", err)
	}
	// create-only：已存在 → ErrCASMismatch。
	if err := st.CAS(ctx, key, nil, []byte("v2")); !errors.Is(err, state.ErrCASMismatch) {
		t.Fatalf("CAS create-only 已存在应 ErrCASMismatch, got %v", err)
	}
	// 值匹配 → 成功替换。
	if err := st.CAS(ctx, key, []byte("v1"), []byte("v2")); err != nil {
		t.Fatalf("CAS 匹配替换: %v", err)
	}
	if got, _ := st.Get(ctx, key); string(got) != "v2" {
		t.Fatalf("CAS 后 Get = %q, want v2", got)
	}
	// 值不匹配 → ErrCASMismatch 且值不变。
	if err := st.CAS(ctx, key, []byte("stale"), []byte("v3")); !errors.Is(err, state.ErrCASMismatch) {
		t.Fatalf("CAS 不匹配应 ErrCASMismatch, got %v", err)
	}
	if got, _ := st.Get(ctx, key); string(got) != "v2" {
		t.Fatalf("CAS 失败不应改值, got %q", got)
	}
	// delete 语义：new=nil 且值匹配 → 删除。
	if err := st.CAS(ctx, key, []byte("v2"), nil); err != nil {
		t.Fatalf("CAS delete: %v", err)
	}
	if _, err := st.Get(ctx, key); !errors.Is(err, state.ErrKeyNotFound) {
		t.Fatalf("CAS delete 后 Get 应 ErrKeyNotFound, got %v", err)
	}
	// delete：值不匹配 → ErrCASMismatch（不误删）。
	if err := st.CAS(ctx, key, []byte("v2"), nil); !errors.Is(err, state.ErrCASMismatch) {
		t.Fatalf("CAS delete 不匹配应 ErrCASMismatch, got %v", err)
	}
}

// TestMongoStateStore_CAS_Concurrent 并发 CAS 递增：8 goroutine × 20 轮对同一 key
// 做「读-改-写」CAS → 最终计数 = 8×20（无丢失更新）。变异点：CAS 退化为「先读后写
// 非原子」→ 红。
func TestMongoStateStore_CAS_Concurrent(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "casconc")
	st, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore: %v", err)
	}
	key := "count/worker"
	const (
		workers = 8
		rounds  = 20
		want    = workers * rounds
	)
	if err2 := st.Put(ctx, key, []byte("0")); err2 != nil {
		t.Fatalf("Put init: %v", err2)
	}
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for range workers {
		wg.Go(func() {
			for range rounds {
				if err2 := bumpCAS(ctx, st, key); err2 != nil {
					errs <- err2
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("并发 CAS 递增失败: %v", err)
	}
	got, err := st.Get(ctx, key)
	if err != nil {
		t.Fatalf("Get 最终值: %v", err)
	}
	if string(got) != fmt.Sprintf("%d", want) {
		t.Fatalf("并发 CAS 后计数 = %q, want %d（存在丢失更新——CAS 不原子）", got, want)
	}
}

// bumpCAS 读-改-写 CAS 递增（十进制字符串 +1），失败重试（有界 100 轮）。
func bumpCAS(ctx context.Context, st state.StateStore, key string) error {
	for range 100 {
		cur, err := st.Get(ctx, key)
		if err != nil {
			return err
		}
		next := incrementDecimal(cur)
		if cerr := st.CAS(ctx, key, cur, next); cerr == nil {
			return nil
		} else if !errors.Is(cerr, state.ErrCASMismatch) {
			return cerr
		}
	}
	return errors.New("CAS 重试轮数耗尽")
}

func incrementDecimal(b []byte) []byte {
	n := 0
	for _, c := range b {
		n = n*10 + int(c-'0')
	}
	n++
	return []byte(fmt.Sprintf("%d", n))
}

// TestMongoStateStore_ListPrefix List 前缀范围：仅返回 prefix 下的 key。
func TestMongoStateStore_ListPrefix(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "list")
	st, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore: %v", err)
	}
	for _, k := range []string{
		"dedup/owner-a/abc",
		"dedup/owner-a/def",
		"dedup/owner-b/xyz",
		"share/tok1",
	} {
		if err2 := st.Put(ctx, k, []byte("v")); err2 != nil {
			t.Fatalf("Put %s: %v", k, err2)
		}
	}
	keys, err := st.List(ctx, "dedup/owner-a/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	sort.Strings(keys)
	want := []string{"dedup/owner-a/abc", "dedup/owner-a/def"}
	if len(keys) != len(want) {
		t.Fatalf("List(dedup/owner-a/) = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Fatalf("List(dedup/owner-a/) = %v, want %v", keys, want)
		}
	}
}

func contains(list []string, s string) bool {
	return slices.Contains(list, s)
}

// TestMongoStateStore_KeyInvalid 非法 key fail-closed（与 LocalStateStore 同语义）。
func TestMongoStateStore_KeyInvalid(t *testing.T) {
	t.Parallel()
	mc, db := requireMongo(t)
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	col := uniqueCollection(t, mc, db, "badkey")
	st, err := NewMongoStateStore(mc.URI, db, col, logger)
	if err != nil {
		t.Fatalf("NewMongoStateStore: %v", err)
	}
	for _, bad := range []string{"a/../b", "/lead/trail", "has\x00byte/x"} {
		if err := st.Put(ctx, bad, []byte("v")); err == nil {
			t.Errorf("非法 key %q 应被拒绝（fail-closed）", bad)
		}
		if _, err := st.Get(ctx, bad); err == nil {
			t.Errorf("非法 key %q Get 应报错", bad)
		}
		if _, err := st.List(ctx, bad); err == nil {
			t.Errorf("非法前缀 %q List 应报错", bad)
		}
	}
}
