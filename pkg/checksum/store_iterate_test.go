// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

// store_iterate_test.go 钉住分批遍历能力（校验类运维入口用）：不含全量 map 拷贝、
// 支持提前终止、跨批覆盖全部条目。

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// TestChecksumStore_IterateAllBatches：不同 batch 大小都应遍历到全部条目。
func TestChecksumStore_IterateAllBatches(t *testing.T) {
	t.Parallel()
	for _, batch := range []int{0, 1, 7, 1000} {
		cs := newTestStore(t)
		want := map[string]string{}
		for i := range 25 {
			k := fmt.Sprintf("user/f%d.txt", i)
			cs.Set(k, "sum")
			want[k] = "sum"
		}
		got := map[string]string{}
		cs.Iterate(batch, func(k, v string) bool {
			got[k] = v
			return true
		})
		if len(got) != len(want) {
			t.Fatalf("batch=%d 遍历条目=%d want %d", batch, len(got), len(want))
		}
		for k, v := range want {
			if got[k] != v {
				t.Fatalf("batch=%d 缺条目 %q", batch, k)
			}
		}
	}
}

// TestChecksumStore_IterateEarlyStop：回调返回 false 立即终止。
func TestChecksumStore_IterateEarlyStop(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	for i := range 50 {
		cs.Set(fmt.Sprintf("user/f%d.txt", i), "sum")
	}
	var n atomic.Int64
	cs.Iterate(4, func(string, string) bool {
		n.Add(1)
		return n.Load() < 3
	})
	if got := n.Load(); got != 3 {
		t.Fatalf("提前终止后回调次数=%d want 3", got)
	}
}
