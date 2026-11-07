// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package checksum

import (
	"io"
	"log/slog"
	"testing"
)

// newTestStore 构造内存态台账（root=nil ⇒ 只读空台账 + save 报错被日志吞掉，读路径可测）。
func newTestStore(t *testing.T) *ChecksumStore {
	t.Helper()
	return NewChecksumStore(nil, "meta/checksums.json", slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// TestStore_VolumeKeyedIsolation 钉住卷维度键：不同卷上的**同名文件**各自保留自己的 checksum
// （旧的无卷键在多卷下会互相覆盖——list 曾因此张冠李戴，见设计 §15.1.1）。
func TestStore_VolumeKeyedIsolation(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t) // 内存态（root=nil）
	cs.SetInVolume("default", "user/a.txt", "aaa")
	cs.SetInVolume("vol2", "user/a.txt", "bbb")

	if got, ok := cs.GetInVolume("default", "user/a.txt"); !ok || got != "aaa" {
		t.Fatalf("default 卷 checksum = %q ok=%v，want aaa", got, ok)
	}
	if got, ok := cs.GetInVolume("vol2", "user/a.txt"); !ok || got != "bbb" {
		t.Fatalf("vol2 卷 checksum = %q ok=%v，want bbb", got, ok)
	}
	snap := cs.SnapshotInVolume("vol2")
	if len(snap) != 1 || snap["user/a.txt"] != "bbb" {
		t.Fatalf("vol2 快照 = %+v，want {user/a.txt: bbb}", snap)
	}
	if _, ok := cs.SnapshotInVolume("default")["user/a.txt"]; !ok {
		t.Fatal("default 卷快照应含 user/a.txt")
	}
}

// TestStore_GetInVolumeLegacyFallback 钉住迁移兼容：卷维度键 miss 时回退旧无卷键
// （既有台账无卷维度，升级后仍能读到）。
func TestStore_GetInVolumeLegacyFallback(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	cs.Set("user/legacy.txt", "ccc") // 旧键（无卷维度）

	if got, ok := cs.GetInVolume("anyvol", "user/legacy.txt"); !ok || got != "ccc" {
		t.Fatalf("旧键应被卷维度读回退命中，got %q ok=%v", got, ok)
	}
}

// TestStore_RenameInVolume 钉住卷维度重命名（同卷），不影响其它卷的同名键。
func TestStore_RenameInVolume(t *testing.T) {
	t.Parallel()
	cs := newTestStore(t)
	cs.SetInVolume("default", "user/a.txt", "aaa")
	cs.SetInVolume("vol2", "user/a.txt", "bbb")

	cs.RenameInVolume("default", "user/a.txt", "user/b.txt")
	if _, ok := cs.GetInVolume("default", "user/a.txt"); ok {
		t.Fatal("rename 后源键应消失")
	}
	if got, _ := cs.GetInVolume("default", "user/b.txt"); got != "aaa" {
		t.Fatalf("rename 后目标键 = %q，want aaa", got)
	}
	if got, _ := cs.GetInVolume("vol2", "user/a.txt"); got != "bbb" {
		t.Fatalf("rename 不应影响其它卷的同名键，got %q", got)
	}
}
