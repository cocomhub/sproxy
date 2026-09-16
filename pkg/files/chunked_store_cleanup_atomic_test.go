// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_store_cleanup_atomic_test.go 钉住「身份闸门 + 产物删除」的**原子性**
// （RV9-CHUNK-FINAL F-1）：
//
// cleanupSessionIfCurrent 此前是「RLock 读 → RUnlock → 比较 → DeleteSession(id)」，而
// DeleteSession 自己加锁后**按 id 重新取值**再删 ⇒ 两次加锁之间存在窗口：期间 expect 被并发
// 删除（cancel / 过期清理）且同 id 被新 init 接管时，DeleteSession 会删掉**新会话**的 map
// 条目、预留与产物 —— 正是该函数注释声称要避免的事（本仓在 RV8 对同类问题已判 must-fix）。
// 修后「身份判定 + 产物删除」在同一 us.mu 临界区内完成，窗口消失。

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestUploadStore_CleanupSessionIfCurrent_DeletesArtifactsUnderStoreLock 是该原子性的**回归门禁**：
// 在产物删除入口注入探针，用 us.mu.TryLock() 判定当时是否**已持有** us.mu 写锁。
//
//	修复前：该路径经 DeleteSession（先解锁再删产物）⇒ TryLock 成功 ⇒ 本用例红；
//	修复后：删除在 us.mu 临界区内 ⇒ TryLock 失败 ⇒ 绿。
//
// 同时断言删除确实发生（防「探针未被调用」的空洞通过）。
func TestUploadStore_CleanupSessionIfCurrent_DeletesArtifactsUnderStoreLock(t *testing.T) {
	// 并行化：探针按 store 实例注入，不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	const uploadID = "atomic-1"
	s, err := us.CreateSession(uploadID, "f.txt", 4, 4, 1, sha256Hex([]byte("AAAA")), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tempRel := TempRelForUser(s, "user/f.txt")
	tempAbs := filepath.Join(tenantRoot, filepath.FromSlash(tempRel))
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tempAbs, []byte("AAAA"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if !us.SetSessionTempPath(uploadID, tempRel) {
		t.Fatal("SetSessionTempPath 命中会话应返回 true")
	}

	probed := 0
	us.artifactsProbe = func(id string) {
		if id != uploadID {
			return
		}
		probed++
		if us.mu.TryLock() {
			// TryLock 成功 ⇒ 删除产物时未持 us.mu ⇒ 身份判定与删除之间仍存在窗口。
			us.mu.Unlock()
			t.Errorf("产物删除时未持有 us.mu：身份判定与删除之间仍存在窗口（RV9-CHUNK-FINAL F-1）")
		}
	}
	t.Cleanup(func() { us.artifactsProbe = nil })

	us.cleanupSessionIfCurrent(uploadID, s)

	if probed == 0 {
		t.Fatal("探针未被调用：本用例未覆盖产物删除路径")
	}
	if us.GetSession(uploadID) != nil {
		t.Fatal("绑定当前会话的清理应删除该会话")
	}
	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("绑定当前会话的清理应删除在途临时文件, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(chunkDir, uploadID)); !os.IsNotExist(err) {
		t.Fatalf("绑定当前会话的清理应删除会话目录, stat err=%v", err)
	}
}

// TestUploadStore_CleanupSessionIfCurrent_SkipsTakeoverAfterExpectDeleted 覆盖 F-1 的**行为面**：
// 三步交错「expect 被并发删除 → 同 id 被新会话接管（含目录与在途临时名）→ 绑定旧对象的清理才
// 执行」时，新会话（map 条目 / 会话目录 / 在途临时文件）必须完好无损。
//
// 手法与 TestUploadStore_CleanupSessionIfCurrent_KeepsReplacedSession 同形：白盒构造状态后直接
// 调被保护的入口（真实调度无法确定性复现该交错）；该用例同时是「不许把身份闸门简化成按 id 删」
// 的反向守卫。
func TestUploadStore_CleanupSessionIfCurrent_SkipsTakeoverAfterExpectDeleted(t *testing.T) {
	// 并行化：本测试不依赖 t.Setenv/全局可变状态。
	t.Parallel()
	base := t.TempDir()
	tenantRoot := filepath.Join(base, "main", "alice")
	chunkDir := filepath.Join(tenantRoot, "chunk")

	us := MustNewUploadStore(chunkDir, time.Hour, nil)
	defer us.Stop()

	const uploadID = "takeover-1"
	csA := sha256Hex([]byte("AAAA"))
	csB := sha256Hex([]byte("BBBB"))

	a, errA := us.CreateSession(uploadID, "f.txt", 4, 4, 1, csA, 0)
	if errA != nil {
		t.Fatalf("CreateSession(A): %v", errA)
	}
	tempRelA := TempRelForUser(a, "user/f.txt")
	tempAbsA := filepath.Join(tenantRoot, filepath.FromSlash(tempRelA))
	if err := os.MkdirAll(filepath.Dir(tempAbsA), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(tempAbsA, []byte("AAAA"), 0o600); err != nil {
		t.Fatalf("WriteFile(A 在途临时文件): %v", err)
	}
	if !us.SetSessionTempPath(uploadID, tempRelA) {
		t.Fatal("SetSessionTempPath(A) 应返回 true")
	}

	// 第一步：expect 被并发删除（cancel / 过期清理），A 的产物随之删除。
	us.DeleteSession(uploadID)
	if us.GetSession(uploadID) != nil {
		t.Fatal("DeleteSession 后不应还有会话")
	}

	// 第二步：同 id 被新会话 B 接管（SDK 的 upload_id 由元数据派生 ⇒ 同 id 会被复用）；
	// 临时名只依赖 (rel, uploadID) ⇒ 与 A 同路径（这正是删掉它的危害）。
	b, err := us.CreateSession(uploadID, "f.txt", 4, 4, 1, csB, 0)
	if err != nil {
		t.Fatalf("CreateSession(B): %v", err)
	}
	tempRelB := TempRelForUser(b, "user/f.txt")
	if tempRelB != tempRelA {
		t.Fatalf("同 id 同目标的临时名应同路径: A=%s B=%s", tempRelA, tempRelB)
	}
	tempAbsB := filepath.Join(tenantRoot, filepath.FromSlash(tempRelB))
	if err := os.WriteFile(tempAbsB, []byte("BBBB"), 0o600); err != nil {
		t.Fatalf("WriteFile(B 在途临时文件): %v", err)
	}
	if !us.SetSessionTempPath(uploadID, tempRelB) {
		t.Fatal("SetSessionTempPath(B) 应返回 true")
	}

	// 第三步：绑定 A 的延迟清理到期 ⇒ 必须整项放弃（不得按 id 删除）。
	us.cleanupSessionIfCurrent(uploadID, a)

	got := us.GetSession(uploadID)
	if got == nil {
		t.Fatal("绑定旧对象的清理不得删除已接管该 id 的新会话")
	}
	if got.FileChecksum != csB {
		t.Fatalf("存活会话应是新会话 B, got checksum=%s", got.FileChecksum)
	}
	if _, err := os.Stat(filepath.Join(chunkDir, uploadID)); err != nil {
		t.Fatalf("新会话目录仍应存在: %v", err)
	}
	if data, err := os.ReadFile(tempAbsB); err != nil || string(data) != "BBBB" {
		t.Fatalf("新会话的在途临时文件仍应存在且未被删改: data=%q err=%v", data, err)
	}
}
