// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// file_lock_token_test.go 锁定评审 R14 C7：文件级排他锁必须**令牌化**。
//
// 修复前的两个真实后果：
//  1. 超龄回收 `Load→Delete` 会删掉**刚被他人重获的新锁**；
//  2. 旧持有者的 `defer release()` 在新持有者取锁后执行 ⇒ 删掉**新锁** ⇒ 同一 rel 出现两个写者
//     （主文件/sidecar 错位固化的根因）。
//
// 修复：锁值带进程内唯一令牌 + 释放/回收都用 `CompareAndDelete`，只删「自己那把」；
// 长事务（转存/跨卷复制）用 `acquireFileLockKeepAlive` 续期，异常退出仍由 TTL 回收。

import (
	"sync"
	"testing"
	"time"
)

// ageFileLock 把某把锁的获取时间改成「超龄」，模拟持锁者异常退出。
func ageFileLock(h *Handlers, key string) {
	h.uploadingLockAt.Store(key, time.Now().Add(-2*uploadingLockTTL))
}

// TestFileLock_StaleReclaimKeepsNewHolder 超龄回收后重获的锁，不得被下一次回收误删。
func TestFileLock_StaleReclaimKeepsNewHolder(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	key := normalizeOwner("alice") + "\x00user/a.txt"

	rel1, ok := h.acquireFileLock("alice", "user/a.txt")
	if !ok {
		t.Fatal("首次取锁应成功")
	}
	ageFileLock(h, key)
	if !h.reclaimStaleLock(key) {
		t.Fatal("超龄锁应被回收")
	}

	// 新持有者取锁（值与前一把不同 ⇒ 令牌不同）。
	rel2, ok2 := h.acquireFileLock("alice", "user/a.txt")
	if !ok2 {
		t.Fatal("回收后应能重新取锁")
	}

	// 旧持有者的 release 晚到：不得删除新锁。
	rel1()
	if _, held := h.uploadingFiles.Load(key); !held {
		t.Fatal("旧持有者的 release 不得删除新持有者的锁")
	}
	// 第三个写者仍应 409。
	if _, ok3 := h.acquireFileLock("alice", "user/a.txt"); ok3 {
		t.Fatal("新锁仍在持有时并发取锁必须失败")
	}

	rel2()
	if _, held := h.uploadingFiles.Load(key); held {
		t.Fatal("持有者自己的 release 必须释放")
	}
}

// TestFileLock_ReclaimDoesNotDeleteRereacquiredLock 回收观察到旧值、但期间已被重获 ⇒ 不删新锁。
func TestFileLock_ReclaimDoesNotDeleteRereacquiredLock(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	key := normalizeOwner("bob") + "\x00user/b.txt"

	rel1, _ := h.acquireFileLock("bob", "user/b.txt")
	ageFileLock(h, key)
	// 模拟「回收读到旧值后、CompareAndDelete 之前」被释放并由他人重获：
	rel1()
	rel2, ok := h.acquireFileLock("bob", "user/b.txt")
	if !ok {
		t.Fatal("释放后应能取锁")
	}
	// 此时回收用旧值比较必然失败 ⇒ 不得删除新锁。
	if h.reclaimStaleLock(key) {
		t.Fatal("值已变（新持有者）时不得回收")
	}
	if _, held := h.uploadingFiles.Load(key); !held {
		t.Fatal("新锁必须保留")
	}
	rel2()
}

// TestFileLock_KeepAliveRefreshes 续期刷新时间戳；条目消失后刷新失败（循环退出）。
func TestFileLock_KeepAliveRefreshes(t *testing.T) {
	t.Parallel()
	h := &Handlers{logger: testLogger()}
	key := normalizeOwner("carol") + "\x00user/c.txt"

	rel, ok := h.acquireFileLockKeepAlive("carol", "user/c.txt")
	if !ok {
		t.Fatal("取锁应成功")
	}
	ageFileLock(h, key)
	if !h.refreshFileLockOnce(key) {
		t.Fatal("持锁期间刷新应成功")
	}
	if h.reclaimStaleLock(key) {
		t.Fatal("续期后不得被判超龄回收（长事务保护）")
	}
	rel()

	if h.refreshFileLockOnce(key) {
		t.Fatal("锁已释放 ⇒ 刷新应失败（续期循环退出，不复活时间戳）")
	}
}

// TestFileLock_MarkerDetection 令牌形态仍被识别为锁标记（清理循环不得误删长事务的锁）。
func TestFileLock_MarkerDetection(t *testing.T) {
	t.Parallel()
	for _, marker := range []string{uploadingLockUpload, uploadingLockMove, uploadingLockTxn} {
		v := newLockValue(marker)
		if !isUploadingLockMarker(v) {
			t.Fatalf("令牌形态 %q 必须被识别为锁标记", v)
		}
		if m, ok := lockMarkerOf(v); !ok || m != marker {
			t.Fatalf("lockMarkerOf(%q) = %q,%v", v, m, ok)
		}
	}
	// 裸标记（领域侧 pkg/files 写入的 "upload"）仍识别（零回归）。
	if !isUploadingLockMarker(uploadingLockUpload) {
		t.Fatal("裸标记必须继续被识别")
	}
	// 上传会话 id 不得被误判为锁。
	if isUploadingLockMarker("deadbeef0123") {
		t.Fatal("裸 upload_id 不得被识别为锁标记")
	}
}

// TestFileLock_TokenUnique 两次取锁的令牌必须不同（否则 CompareAndDelete 无法区分持有者）。
func TestFileLock_TokenUnique(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	seen := make(map[string]struct{}, 64)
	var mu sync.Mutex
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v := newLockValue(uploadingLockTxn)
			mu.Lock()
			if _, dup := seen[v]; dup {
				t.Errorf("令牌重复: %q", v)
			}
			seen[v] = struct{}{}
			mu.Unlock()
		}()
	}
	wg.Wait()
}
