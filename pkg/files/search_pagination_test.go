// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// TestSearch_OptionalPagination 钉住搜索的可选分页（审计 2026-10-29 P3-2）：
// 不传 offset/limit ⇒ 返回全部（兼容既有契约）；显式传 ⇒ 按 List 语义切片，Total 恒为全部命中数。
func TestSearch_OptionalPagination(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	userAbs, ok := env.tenantFor("alice").Root().Abs("user")
	if !ok {
		t.Fatal("Abs(user) 失败")
	}
	if err := os.MkdirAll(userAbs, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, n := range []string{"hit1.txt", "hit2.txt", "hit3.txt"} {
		if err := os.WriteFile(filepath.Join(userAbs, n), []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", n, err)
		}
	}
	env.rebuild()

	t.Run("no_params_returns_all", func(t *testing.T) {
		rr := env.serve(env.svc.SearchFiles, "alice", http.MethodGet, "/api/files/search?q=hit")
		if rr.Code != http.StatusOK {
			t.Fatalf("status=%d", rr.Code)
		}
		resp := decodeList(t, rr)
		if resp.Total != 3 || len(resp.Files) != 3 {
			t.Fatalf("不传分页参数应返回全部: total=%d len=%d", resp.Total, len(resp.Files))
		}
	})

	t.Run("limit_slices_but_total_is_full", func(t *testing.T) {
		rr := env.serve(env.svc.SearchFiles, "alice", http.MethodGet, "/api/files/search?q=hit&limit=2")
		resp := decodeList(t, rr)
		if resp.Total != 3 || len(resp.Files) != 2 {
			t.Fatalf("limit=2: total=%d len=%d, want 3/2", resp.Total, len(resp.Files))
		}
		if resp.Limit != 2 {
			t.Fatalf("响应 limit=%d, want 2", resp.Limit)
		}
	})

	t.Run("offset_pages", func(t *testing.T) {
		rr := env.serve(env.svc.SearchFiles, "alice", http.MethodGet, "/api/files/search?q=hit&offset=2&limit=2")
		resp := decodeList(t, rr)
		if resp.Total != 3 || len(resp.Files) != 1 {
			t.Fatalf("offset=2&limit=2: total=%d len=%d, want 3/1", resp.Total, len(resp.Files))
		}
		if got := resp.Files[0].Name; got != "hit3.txt" {
			t.Fatalf("第二页首条 = %q, want hit3.txt", got)
		}
	})
}
