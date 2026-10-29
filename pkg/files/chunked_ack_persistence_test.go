// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// chunked_ack_persistence_test.go 钉住分块上传的**确认即持久化**契约：
// `POST /upload/chunk` 返回 200 时，该块的校验值必须已落盘，否则进程被 kill -9 后重启
// 会把「已确认」的块判定为需重传（客户端白传，且 `TestE2E_Restart_ChunkedUploadResumesAfterKill9`
// 偶发红：异步 session.json 持久化尚未落盘）。
//
// 修法：每块校验值同步写独立 sidecar（`chunk-<i>.sha256`，原子 rename），恢复时优先采信
// sidecar；session.json 仍按原异步（元信息批量持久化，避免每块重写整份 bitmap）。

// TestUploadStore_ChunkAckSurvivesStaleSessionJSON：模拟「返回值已确认、session.json 仍落后」
// 的崩溃点——重启后该块必须仍被识别为已接收。
//
// 变异：删掉 `loadChunkChecksumSidecars` 调用 ⇒ ChunkChecksums[0] 为空 ⇒ 红。
func TestUploadStore_ChunkAckSurvivesStaleSessionJSON(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	us := MustNewUploadStore(dir, time.Hour, nil)

	const uploadID = "ack-1"
	if _, err := us.CreateSession(uploadID, "f.bin", 1024, 512, 2, strings.Repeat("a", 64), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	// 在途临时文件（块直写目标）：内容与即将标记的 checksum 一致。
	payload := []byte(strings.Repeat("x", 512))
	sum := fmt.Sprintf("%x", sha256.Sum256(payload))
	tenantRoot := filepath.Dir(dir) // chunk 目录的父目录 = 租户根（tempAbsPath 口径）
	tmpAbs := filepath.Join(tenantRoot, "user", "inflight.bin")
	if err := os.MkdirAll(filepath.Dir(tmpAbs), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(tmpAbs, payload, 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}
	us.mu.Lock()
	us.sessions[uploadID].TempPath = "user/inflight.bin"
	us.mu.Unlock()

	// 基线：session.json 记录「0 块」。
	us.PersistNow(uploadID)
	stale, err := os.ReadFile(filepath.Join(dir, uploadID, "session.json"))
	if err != nil {
		t.Fatalf("read session.json: %v", err)
	}
	// 关闭后台持久化通道 ⇒ 本测试确定性模拟「异步 session.json 落盘未发生」的崩溃窗口
	// （不关闭时 MarkChunkReceived 触发的异步持久化会与后续写回旧快照竞态，掩盖缺陷）。
	us.Stop()

	// 客户端收到 200 的这一刻：MarkChunkReceived 返回 nil ⇒ 必须已持久化（sidecar）。
	if err := us.MarkChunkReceived(uploadID, 0, sum); err != nil {
		t.Fatalf("MarkChunkReceived: %v", err)
	}
	// 模拟 kill9：磁盘 session.json 仍是旧快照。
	if err := os.WriteFile(filepath.Join(dir, uploadID, "session.json"), stale, 0o644); err != nil {
		t.Fatalf("restore stale session.json: %v", err)
	}

	// 重启恢复。
	us2 := MustNewUploadStore(dir, time.Hour, nil)
	defer us2.Stop()
	s := us2.GetSession(uploadID)
	if s == nil {
		t.Fatal("重启后会话应恢复")
	}
	if s.ChunkChecksums[0] != sum {
		t.Fatalf("已确认块的校验值应跨崩溃存活（sidecar），got %q", s.ChunkChecksums[0])
	}
	if !s.ReceivedChunks[0] {
		t.Fatal("已确认块重启后应判为已接收（数据校验通过）")
	}
	if got := countReceived(s.ReceivedChunks); got != 1 {
		t.Fatalf("重启后已收块数 = %d, want 1", got)
	}
}

// TestUploadStore_ChunkSidecarIsPerChunk：sidecar 按块隔离（不因后写的块覆盖前面的块）。
func TestUploadStore_ChunkSidecarIsPerChunk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	us := MustNewUploadStore(dir, time.Hour, nil)
	defer us.Stop()

	const uploadID = "ack-2"
	if _, err := us.CreateSession(uploadID, "f.bin", 2048, 1024, 2, strings.Repeat("a", 64), 0); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	sumA, sumB := strings.Repeat("1", 64), strings.Repeat("2", 64)
	if err := us.MarkChunkReceived(uploadID, 0, sumA); err != nil {
		t.Fatalf("mark 0: %v", err)
	}
	if err := us.MarkChunkReceived(uploadID, 1, sumB); err != nil {
		t.Fatalf("mark 1: %v", err)
	}
	for idx, want := range []string{sumA, sumB} {
		got, err := os.ReadFile(filepath.Join(dir, uploadID, fmt.Sprintf("chunk-%d.sha256", idx)))
		if err != nil {
			t.Fatalf("chunk-%d sidecar 缺失: %v", idx, err)
		}
		if strings.TrimSpace(string(got)) != want {
			t.Fatalf("chunk-%d sidecar = %q, want %q", idx, got, want)
		}
	}
}
