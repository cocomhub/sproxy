// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// ---- UploadStore 测试 ----
func TestUploadStore_GetSessionByFilename(t *testing.T) {
	tmpDir := t.TempDir()
	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	us.CreateSession("id1", "file1.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
	us.CreateSession("id2", "file2.txt", 200, 4096, 1, strings.Repeat("b", 64), 0)

	s := us.GetSessionByFilename("file1.txt")
	if s == nil {
		t.Fatal("expected session for file1.txt")
		return
	}
	if s.UploadID != "id1" {
		t.Fatalf("expected id1, got %s", s.UploadID)
	}

	if us.GetSessionByFilename("nonexistent.txt") != nil {
		t.Fatal("expected nil for nonexistent filename")
	}
}

func TestUploadStore_DeleteSession(t *testing.T) {
	tmpDir := t.TempDir()
	// baseDir 直接是租户 chunk 桶（不再拼接 .__chunked__），会话目录位于其下。
	chunkDir := filepath.Join(tmpDir, "chunk")
	us := MustNewUploadStore(chunkDir, 0, nil)
	defer us.Stop()

	us.CreateSession("del-id", "del.txt", 100, 4096, 1, strings.Repeat("c", 64), 0)

	us.DeleteSession("del-id")

	if us.GetSession("del-id") != nil {
		t.Fatal("session should be nil after delete")
	}

	sessionDir := filepath.Join(chunkDir, "del-id")
	if _, err := os.Stat(sessionDir); !os.IsNotExist(err) {
		t.Fatal("session dir should be removed from disk")
	}
}

func TestUploadStore_CleanupExpired(t *testing.T) {
	tmpDir := t.TempDir()
	// Use a negative TTL so the session is already expired on creation
	us := MustNewUploadStore(tmpDir, -time.Nanosecond, nil)
	defer us.Stop()

	us.CreateSession("expired-id", "expired.txt", 100, 4096, 1, strings.Repeat("d", 64), 0)

	us.CleanupExpired()

	if us.GetSession("expired-id") != nil {
		t.Fatal("expired session should be cleaned up")
	}
}

func TestUploadStore_RecoverFromDisk(t *testing.T) {
	tmpDir := t.TempDir()

	us1 := MustNewUploadStore(tmpDir, 24*time.Hour, nil)
	us1.CreateSession("recover-id", "recover.txt", 8192, 4096, 2, strings.Repeat("e", 64), 0)
	us1.MarkChunkReceived("recover-id", 0, "chunk0hash")
	us1.Stop()

	us2 := MustNewUploadStore(tmpDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("recover-id")
	if s == nil {
		t.Fatal("session should be recovered from disk")
		return
	}
	if s.Filename != "recover.txt" {
		t.Fatalf("filename mismatch: %s", s.Filename)
	}
	if !s.ReceivedChunks[0] {
		t.Fatal("chunk 0 should be marked received after recovery")
	}
	if s.ReceivedChunks[1] {
		t.Fatal("chunk 1 should not be marked received")
	}
}

// TestUploadStore_ReconcileChunks 验证任务 4 改造后的恢复语义：分块内容与 checksum 表
// 逐分片校验匹配才置 bitmap。 该测试构造了一个通过 Checksum 恢复的会话——任务 4 起不再
// 有独立 .chunk 文件，磁盘孤儿 chunk 文件不再被 reconcile 计为已接收（临时整文件的
// 内容校验才是权威）。
func TestUploadStore_ReconcileChunks(t *testing.T) {
	tmpDir := t.TempDir()
	chunkDir := filepath.Join(tmpDir, "chunk")

	us1 := MustNewUploadStore(chunkDir, 24*time.Hour, nil)
	us1.CreateSession("reconcile-id", "reconcile.txt", 8192, 4096, 2, strings.Repeat("f", 64), 0)
	us1.MarkChunkReceived("reconcile-id", 0, "chunk0hash")
	us1.Stop()

	// 旧语义（改造前）：磁盘上的孤儿 .chunk 文件在恢复时被 reconcile 置 bitmap=true。
	// 任务 4 起分片直写整临时文件，.chunk 文件不再存在，孤儿 .chunk 应被忽略——
	// 该文件不会成为恢复依据（位图保持 session.json 的值）。
	sessionDir := filepath.Join(chunkDir, "reconcile-id")
	if err := os.WriteFile(filepath.Join(sessionDir, "00001.chunk"), []byte("fake chunk data"), 0644); err != nil {
		t.Fatalf("write chunk file: %v", err)
	}

	us2 := MustNewUploadStore(chunkDir, 24*time.Hour, nil)
	defer us2.Stop()

	s := us2.GetSession("reconcile-id")
	if s == nil {
		t.Fatal("session should be recovered")
		return
	}

	// 任务 4：孤儿 .chunk 文件不再被 reconcile 计为已接收（bitmap 保持 session.json）。
	if s.ReceivedChunks[1] {
		t.Fatal("chunk 1 should NOT be marked received: 任务 4 起孤儿 .chunk 不再作为恢复依据")
	}
}

func TestUploadStore_GetOrCreateSession_Reuse(t *testing.T) {
	tmpDir := t.TempDir()
	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	s1, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
	if err != nil {
		t.Fatalf("first GetOrCreate: %v", err)
	}
	if reused {
		t.Fatal("first call should not be reused")
	}

	s2, reused, err := us.GetOrCreateSession("rid", "r.txt", 100, 4096, 1, strings.Repeat("g", 64), 0)
	if err != nil {
		t.Fatalf("second GetOrCreate: %v", err)
	}
	if !reused {
		t.Fatal("second call should reuse session")
	}
	if s1.UploadID != s2.UploadID {
		t.Fatal("upload_id should match")
	}
}

// TestUploadStore_GetOrCreateSession_ReuseGuard 验证 F4 修复：按 key 复用旧会话时
// 若文件元数据不符（攻击者预置同 key 会话篡改文件名），必须拒绝而非静默复用。
func TestUploadStore_GetOrCreateSession_ReuseGuard(t *testing.T) {
	tmpDir := t.TempDir()
	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	// 首次：以 key "sid" 创建（正常文件名）
	first, _, err := us.GetOrCreateSession("sid", "target.txt", 100, 4096, 1, strings.Repeat("a", 64), 0)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_ = first

	// 攻击者预置同 key 但不同文件名/校验和 → 复用应失败
	if _, _, err = us.GetOrCreateSession("sid", "evil.txt", 100, 4096, 1, strings.Repeat("b", 64), 0); err == nil {
		t.Fatal("同 key 不同文件名应拒绝复用")
	}

	// 同 key 同元数据 → 仍正常复用（续传不受影响）
	if _, reused, err := us.GetOrCreateSession("sid", "target.txt", 100, 4096, 1, strings.Repeat("a", 64), 0); err != nil || !reused {
		t.Fatalf("同元数据应复用, reused=%v err=%v", reused, err)
	}
}

func TestUploadStore_ConcurrentMarkChunk(t *testing.T) {
	tmpDir := t.TempDir()
	us := MustNewUploadStore(tmpDir, 0, nil)
	defer us.Stop()

	const totalChunks = 100
	us.CreateSession("concurrent-id", "concurrent.txt", int64(totalChunks*4096), 4096, totalChunks, strings.Repeat("h", 64), 0)

	var wg sync.WaitGroup
	errCh := make(chan error, totalChunks)
	for i := range totalChunks {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			if err := us.MarkChunkReceived("concurrent-id", idx, fmt.Sprintf("chunk%dhash", idx)); err != nil {
				errCh <- fmt.Errorf("MarkChunkReceived(%d): %w", idx, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if !us.AllChunksReceived("concurrent-id") {
		t.Fatal("all chunks should be received after concurrent marking")
	}
}

func TestUploadStore_CleanupSessionAfter(t *testing.T) {
	us := MustNewUploadStore(t.TempDir(), 0, nil)
	defer us.Stop()

	sessionID := "cleanup-test"
	us.CreateSession(sessionID, "cleanup.txt", 1024, 256, 4, "abcd", 0)

	// 检查 session 存在
	if us.GetSession(sessionID) == nil {
		t.Fatal("expected session to exist")
	}

	// 计划在 50ms 后清理
	us.CleanupSessionAfter(sessionID, 50*time.Millisecond)

	// 轮询等待 session 被移除，最多 2s
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if us.GetSession(sessionID) == nil {
			return // 已清理，成功
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("expected session to be cleaned up after TTL")
}

// TestFindMismatchChunks_StoreUnit 验证 findMismatchChunks 精确列出被篡改的分片
// （完整文件哈希不符时，逐分片 seek 重算定位坏片）。
//
// 本用例原在 pkg/server（经完整 Handlers 装配取 store/tenant）；随被测代码迁入本包后改为
// **自建夹具**：直接以 <root>/<owner>/chunk 为 baseDir 构造 store（与生产装配同布局），
// 断言内容逐字未变。
func TestFindMismatchChunks_StoreUnit(t *testing.T) {
	dir := t.TempDir()
	root, err := storage.OpenRoot(dir)
	if err != nil {
		t.Fatalf("打开存储根失败: %v", err)
	}
	defer root.Close()
	tnt, err := storage.NewTenant("alice", root)
	if err != nil {
		t.Fatalf("建租户失败: %v", err)
	}

	content := bytes.Repeat([]byte("M"), 9000)
	chunkSize := int64(4096)
	uploadID := "store-mismatch-1"
	filename := "dir/store-mismatch.bin"

	rel, ok2 := tnt.UserRel(filename)
	if !ok2 {
		t.Fatal("UserRel 失败")
	}
	// baseDir 与生产装配同源：租户 chunk 桶（<租户根>/chunk）。
	chunkDir, ok := tnt.Root().Abs("chunk")
	if !ok {
		t.Fatal("派生租户 chunk 桶失败")
	}
	us := MustNewUploadStore(chunkDir, 0, nil)
	defer us.Stop()

	session, err := us.CreateSession(uploadID, filename, int64(len(content)), chunkSize, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session.TempPath = TempRelForUser(session, rel)
	tempAbs, _ := tnt.Root().Abs(session.TempPath)
	if mkErr := os.MkdirAll(filepath.Dir(tempAbs), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	tmpF, tmpErr := os.OpenFile(tempAbs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if tmpErr != nil {
		t.Fatalf("创建临时名: %v", tmpErr)
	}
	if truncErr := tmpF.Truncate(int64(len(content))); truncErr != nil {
		tmpF.Close()
		t.Fatalf("truncate: %v", truncErr)
	}
	for i := range 3 {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(content))
		if _, werr := tmpF.WriteAt(content[start:end], int64(start)); werr != nil {
			tmpF.Close()
			t.Fatalf("写分片 %d: %v", i, werr)
		}
		if merr := us.MarkChunkReceived(uploadID, i, sha256Hex(content[start:end])); merr != nil {
			tmpF.Close()
			t.Fatalf("标记 %d: %v", i, merr)
		}
	}
	if cerr := tmpF.Close(); cerr != nil {
		t.Fatalf("关闭临时名: %v", cerr)
	}

	// 篡改分片 2
	f, ferr := os.OpenFile(tempAbs, os.O_WRONLY, 0)
	if ferr != nil {
		t.Fatalf("打开临时名: %v", ferr)
	}
	if _, werr := f.WriteAt(bytes.Repeat([]byte("B"), 728), 2*4096); werr != nil {
		f.Close()
		t.Fatal(werr)
	}
	f.Close()

	sess := us.GetSession(uploadID)
	mismatch := us.findMismatchChunks(sess)
	if len(mismatch) != 1 || mismatch[0] != 2 {
		t.Fatalf("FindMismatchChunks=%v want [2]（精确列出被篡改的分片）", mismatch)
	}
	if err := us.ClearChunksReceived(uploadID, mismatch); err != nil {
		t.Fatalf("ClearChunksReceived: %v", err)
	}
	sess2 := us.GetSession(uploadID)
	if sess2.ReceivedChunks[2] {
		t.Fatal("ClearChunksReceived 后分片 2 bitmap 应为 false")
	}
	if !sess2.ReceivedChunks[0] || !sess2.ReceivedChunks[1] {
		t.Fatal("未涉及的 0/1 分片 bitmap 应保留")
	}
	if missing := MissingChunks(sess2); len(missing) != 1 || missing[0] != 2 {
		t.Fatalf("MissingChunks=%v want [2]", missing)
	}
	// 越界索引应报错（防御）
	if err := us.ClearChunksReceived(uploadID, []int{5}); err == nil {
		t.Fatal("越界 ClearChunksReceived 应返回错误")
	}
}
