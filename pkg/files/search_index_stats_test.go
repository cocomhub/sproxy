// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSearchIndex_StatsReportsOwnersEntriesRebuilds 钉住索引规模/重建指标（审计 2026-10-29 P2-5）。
func TestSearchIndex_StatsReportsOwnersEntriesRebuilds(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(userAbs, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	env.rebuild()
	svc := env.newService()
	if res, err := svc.Search(SearchQuery{Owner: "alice", Query: "a.txt"}); err != nil || len(res.Files) != 1 {
		t.Fatalf("search: n=%d err=%v", len(res.Files), err)
	}
	owners, entries, rebuilds := svc.IndexStats()
	if owners != 1 || entries < 1 {
		t.Fatalf("owners/entries = %d/%d, want 1/>=1", owners, entries)
	}
	if rebuilds != 1 {
		t.Fatalf("rebuilds = %d, want 1（首次全量构建）", rebuilds)
	}
}
