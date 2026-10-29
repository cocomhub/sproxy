// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/volumestate"
)

// TestMetrics_IndexCacheStats 钉住派生状态指标输出（审计 2026-10-29 P2-5）：
// 缓存命中/未命中/条目数 + 索引 owner/条目/重建次数必须出现在 /metrics。
//
// 变异：删 renderIndexCacheMetrics 调用 ⇒ 指标缺失 ⇒ 红。
func TestMetrics_IndexCacheStats(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	userDir := filepath.Join(root, "alice", "user")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg := Default()
	cfg.StorageRoot = root
	cfg.Placement = "prefer-default"
	cfg.Volumes = []VolumeConfig{{Name: "main", Root: root}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	h := buildVolSetHandlers(t, cfg)
	h.stateCache = volumestate.New(volumestate.Options{})
	t.Cleanup(func() { _ = h.stateCache.RefreshAll(context.Background()) })
	svc := h.fileService()
	if svc == nil {
		t.Fatal("files.Service 未装配")
	}
	if res, err := svc.Search(files.SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("search: n=%d err=%v", len(res.Files), err)
	}

	var b strings.Builder
	h.metrics.renderIndexCacheMetrics(&b, h)
	out := b.String()
	// 接线门禁：/metrics 渲染**必须**调用该函数（只加函数不加调用 = 指标永远不输出）。
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatalf("read metrics.go: %v", err)
	}
	if !strings.Contains(string(src), "m.renderIndexCacheMetrics(&b, h)") {
		t.Fatal("/metrics 渲染未调用 renderIndexCacheMetrics（指标不会输出）")
	}
	for _, want := range []string{
		"sproxy_volume_cache_hits_total",
		"sproxy_volume_cache_misses_total",
		"sproxy_volume_cache_entries",
		"sproxy_search_index_owners",
		"sproxy_search_index_entries",
		"sproxy_search_index_rebuilds_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("指标 %s 缺失；输出:\n%s", want, out)
		}
	}
}
