// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package volumestate

import (
	"context"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestCache_StatsCountsHitsAndMisses 钉住缓存可观测计数（审计 2026-10-29 P2-5）：
// 首次回源记 miss，第二次命中记 hit。
//
// 变异：Get 不计数 ⇒ hits/misses 恒 0 ⇒ 红。
func TestCache_StatsCountsHitsAndMisses(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	ctx := context.Background()
	fetch := func(context.Context) ([]syncpkg.Entry, error) {
		return []syncpkg.Entry{{Name: "a.txt", Path: "a.txt", Size: 1}}, nil
	}
	if _, err := c.Get(ctx, "vol", "dir", nil, fetch); err != nil {
		t.Fatalf("Get #1: %v", err)
	}
	if _, err := c.Get(ctx, "vol", "dir", nil, fetch); err != nil {
		t.Fatalf("Get #2: %v", err)
	}
	hits, misses, entries := c.Stats()
	if misses != 1 || hits != 1 {
		t.Fatalf("hits/misses = %d/%d, want 1/1", hits, misses)
	}
	if entries != 1 {
		t.Fatalf("entries = %d, want 1", entries)
	}
}
