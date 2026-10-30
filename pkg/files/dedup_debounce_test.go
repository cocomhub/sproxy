// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dedup_debounce_test.go 钉住 dedup 台账的写合并（2026-10-30）：每次写都全量落盘
// （本地 JSON 重写 / 集群一次 Put）⇒ 批量 N 文件 O(N²) I/O；合并后一个窗口只落盘一次。

import (
	"context"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/cocomhub/sproxy/pkg/state"
)

// putCounter 统计 StateStore Put 次数（嵌入真实 LocalStateStore 其它方法直通）。
type putCounter struct {
	*state.LocalStateStore
	puts atomic.Int64
}

func (c *putCounter) Put(ctx context.Context, key string, data []byte) error {
	c.puts.Add(1)
	return c.LocalStateStore.Put(ctx, key, data)
}

// TestDedupStore_WritesCoalesceToOnePut：窗口内 50 次 Add ⇒ Flush 只 Put 1 次。
//
// 变异：写方法直接 `ds.save()`（每写一次全量 Put）⇒ 窗口内 put>0 且总数 50 ⇒ 红。
func TestDedupStore_WritesCoalesceToOnePut(t *testing.T) {
	t.Parallel()
	counter := &putCounter{LocalStateStore: state.NewLocalStateStore(t.TempDir(), testLogger())}
	ds := NewDedupStore(filepath.Join(t.TempDir(), "dedup.json"), testLogger(), &DedupStateOptions{
		St:  counter,
		Key: "dedup/alice/all",
	})

	for i := range 50 {
		ds.Add(fmt.Sprintf("user/f%d.txt", i), "vol0", fmt.Sprintf("cs%d", i))
	}
	if got := counter.puts.Load(); got != 0 {
		t.Fatalf("合并窗口内不应 Put，got %d 次", got)
	}
	if err := ds.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := counter.puts.Load(); got != 1 {
		t.Fatalf("50 次写应合并为 1 次 Put，got %d", got)
	}
	if got := ds.RefCount("cs1"); got != 1 {
		t.Fatalf("内存台账应保留引用，RefCount(cs1)=%d", got)
	}
}

// TestDedupStore_CloseFlushesFinalWrite：Close 做最终落盘且幂等。
func TestDedupStore_CloseFlushesFinalWrite(t *testing.T) {
	t.Parallel()
	counter := &putCounter{LocalStateStore: state.NewLocalStateStore(t.TempDir(), testLogger())}
	ds := NewDedupStore(filepath.Join(t.TempDir(), "dedup.json"), testLogger(), &DedupStateOptions{
		St:  counter,
		Key: "dedup/alice/all",
	})

	ds.Add("user/a.txt", "vol0", "cs1")
	if err := ds.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := counter.puts.Load(); got != 1 {
		t.Fatalf("Close 应最终 Put 1 次，got %d", got)
	}
	if err := ds.Close(); err != nil {
		t.Fatalf("重复 Close: %v", err)
	}
	if got := counter.puts.Load(); got != 1 {
		t.Fatalf("重复 Close 不应再 Put，got %d", got)
	}
}
