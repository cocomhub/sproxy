// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"testing"
	"time"
)

// TestCleanupUploadingFiles_ReclaimsStaleLock 评审 P2：锁标记持有人异常退出（panic 逃出 defer /
// 无超时的远端写永久阻塞未执行 release）时，条目此前**永不被清理** ⇒ 该 rel 后续写请求持续 409
// 至重启。现按 TTL 回收；未超龄锁**不得**被回收（否则长事务持锁期间锁形同虚设）。
// 变异：去掉 reclaimStaleLock 调用 → 超龄用例红。
func TestCleanupUploadingFiles_ReclaimsStaleLock(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cfg := Default()
	cfg.StorageRoot = dir
	cfg.Volumes = []VolumeConfig{{Name: "default", Root: dir}}
	h := buildVolSetHandlers(t, cfg)

	release, ok := h.acquireFileLock("alice", "user/a.bin")
	if !ok {
		t.Fatal("首次取锁应成功")
	}
	// ① 未超龄：清理不得回收（长事务仍在持锁）。
	h.cleanupUploadingFilesPass()
	if _, ok2 := h.acquireFileLock("alice", "user/a.bin"); ok2 {
		t.Fatal("未超龄锁不得被回收（否则锁形同虚设）")
	}

	// ② 超龄（模拟持锁者异常退出）：清理应回收 → 同 rel 可再次取锁。
	key := normalizeOwner("alice") + "\x00" + "user/a.bin"
	h.uploadingLockAt.Store(key, time.Now().Add(-2*uploadingLockTTL))
	h.cleanupUploadingFilesPass()
	if _, ok3 := h.acquireFileLock("alice", "user/a.bin"); !ok3 {
		t.Fatal("超龄锁标记应被回收（否则该 rel 永久 409）")
	}
	release()
}
