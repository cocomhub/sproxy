// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResetHybridState 锁定「GCID 失配后必须清空续传状态」（2026-11-25 事故）：
// 工作文件与 manifest 都必须被删除，否则续传会跳过坏分块、永远拼不出正确内容。
func TestResetHybridState(t *testing.T) {
	t.Parallel()
	dest := filepath.Join(t.TempDir(), "movie.mp4")
	working := WorkingPath(dest)
	manifest := ManifestPath(dest)
	for _, p := range []string{working, manifest} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(p, []byte("stale"), 0o600); err != nil {
			t.Fatalf("write %s: %v", p, err)
		}
	}

	if err := resetHybridState(dest); err != nil {
		t.Fatalf("resetHybridState: %v", err)
	}
	for _, p := range []string{working, manifest} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("必须删除 %s（否则续传会跳过坏分块）", p)
		}
	}
	// 幂等：状态本就不存在时不得报错。
	if err := resetHybridState(dest); err != nil {
		t.Fatalf("重复清理应幂等: %v", err)
	}
}
