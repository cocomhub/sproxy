// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

import "testing"

// TestIsReservedBucketName_CaseInsensitive 保留桶名判定大小写不敏感（2026-10-10）：
// macOS/Windows 大小写不敏感文件系统上 `META/` 与 `meta/` 同目录，必须按小写口径统一拒绝。
func TestIsReservedBucketName_CaseInsensitive(t *testing.T) {
	t.Parallel()
	reserved := []string{
		"user", "USER", "User",
		"meta", "META", "Meta",
		"cloud", "Cloud",
		"archive", "Archive",
		"chunk", "Chunk",
		"version", "Version",
		"trash", "Trash",
	}
	for _, n := range reserved {
		if !IsReservedBucketName(n) {
			t.Errorf("IsReservedBucketName(%q) 应为 true（大小写不敏感）", n)
		}
	}
	for _, n := range []string{"", "notes", "users", "metadata", "meta2", "user2"} {
		if IsReservedBucketName(n) {
			t.Errorf("IsReservedBucketName(%q) 应为 false", n)
		}
	}
}
