// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"path/filepath"
	"testing"
)

// TestPersistDirFor_RejectsRelativePath 回归：持久化目录必须是绝对路径。
// 缺陷背景（2026-10-31）：测试替身把 owner 名（相对路径）当 persistDir 返回，
// syncmgr 直接用它落盘 ⇒ sync-*.json 被写进进程 cwd（pkg/server/alice/）。
func TestPersistDirFor_RejectsRelativePath(t *testing.T) {
	t.Parallel()
	abs := filepath.Join(t.TempDir(), "alice")
	cases := []struct {
		name string
		dir  string
		want string
	}{
		{"相对路径被拒绝", "alice", ""},
		{"空串被拒绝", "", ""},
		{"绝对路径保留", abs, abs},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := NewManager(ManagerOptions{
				TenantRoot: func(string) (string, string, bool) { return "/tmp/user", tc.dir, true },
				Logger:     discardLogger(),
				Config:     &Config{MaxConcurrent: 1},
			})
			t.Cleanup(m.Stop)
			if got := m.persistDirFor("alice"); got != tc.want {
				t.Fatalf("persistDirFor = %q，want %q", got, tc.want)
			}
		})
	}
}
