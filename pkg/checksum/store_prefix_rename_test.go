// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

import "testing"

// TestRenamePrefixInVolume 目录重命名时子树台账键整体换前缀（其余键不动、旧前缀清空）。
func TestRenamePrefixInVolume(t *testing.T) {
	t.Parallel()
	cs := NewChecksumStore(testRootAt(t, t.TempDir()), "meta/checksums.json", stateTestLogger())
	// debounce 落盘协程（savemerger）必须停：否则测试结束后仍写一次，
	// 与 t.TempDir() 的 RemoveAll 撞车（Windows 实测 `The directory is not empty`）。
	t.Cleanup(func() { _ = cs.Close() })

	cs.SetInVolume("v1", "user/d1/a.txt", "aa")
	cs.SetInVolume("v1", "user/d1/sub/b.txt", "bb")
	cs.SetInVolume("v1", "user/keep.txt", "cc")
	cs.SetInVolume("v2", "user/d1/a.txt", "dd") // 另一卷同名不得受影响

	cs.RenamePrefixInVolume("v1", "user/d1/", "user/d2/")

	for _, tc := range []struct{ vol, rel, want string }{
		{"v1", "user/d2/a.txt", "aa"},
		{"v1", "user/d2/sub/b.txt", "bb"},
		{"v1", "user/keep.txt", "cc"},
		{"v2", "user/d1/a.txt", "dd"},
	} {
		got, ok := cs.GetInVolume(tc.vol, tc.rel)
		if !ok || got != tc.want {
			t.Fatalf("%s:%s = (%q,%v)，期望 %q", tc.vol, tc.rel, got, ok, tc.want)
		}
	}
	if _, ok := cs.GetInVolume("v1", "user/d1/a.txt"); ok {
		t.Fatal("旧前缀键应被移除")
	}
}
