// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// chunked_inflight_test.go 验证任务 4 分块上传改造：init 建整临时文件 + 分片 seek 直写。
//
// 旧路径：init 只建 session，chunk 写独立 .chunk 文件，complete 时合并。
// 新路径：init 创建 `.inflight-<hash16>-<upload_id>.part` 整临时文件（user 桶目标同目录）
//
//	并 Truncate(TotalSize) + TryReserve(TotalSize)；chunk 用 seek+BoundWriter 直写临时文件。
//
// 本文件覆盖任务 4 范围（init/chunk/session/清理/恢复/续传）；complete 的最小可用实现
// （临时文件全文件校验 + rename）随任务 4 一并落地，保证旧路径 complete 仍工作。

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// inflightTempNameFor 构造与生产实现一致的临时名。name 为存储根相对正式路径
// （user/...，需与生产 tempRelForUser 的入参一致——散列取 rel 全路径）。
func inflightTempNameFor(name, uploadID string) string { return inflightTempName(name, uploadID) }

// mustUserRel 返回租户 user 桶内 filename 的存储根相对路径（user/<rel>）。
func mustUserRel(t *testing.T, h *Handlers, owner, filename string) string {
	t.Helper()
	tnt := h.tenantFor(owner)
	rel, ok := tnt.UserRel(filename)
	if !ok {
		t.Fatalf("UserRel(%q) 失败", filename)
	}
	return rel
}

// inflightFilesInUser 列出 user 桶根下全部 .inflight 临时名（供断言"无残留"）。
func inflightFilesInUser(t *testing.T, h *Handlers, owner string) []string {
	t.Helper()
	tnt := h.tenantFor(owner)
	userAbs, ok := tnt.Root().Abs("user")
	if !ok {
		t.Fatalf("派生 user 桶绝对路径失败")
	}
	entries, err := os.ReadDir(userAbs)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), inflightPrefix) {
			out = append(out, e.Name())
		}
	}
	return out
}

// TestChunkedInit_CreatesTempFileAndReserves 验证 init 完成 4 件事：
//  1. 创建 `.inflight-<hash16>-<upload_id>.part` 临时文件（user 桶目标同目录）；
//  2. Truncate(TotalSize)（文件大小 == totalSize，未写分片为空洞）；
//  3. TryReserve(TotalSize) 于 owner 的 "user" 桶 Scope；
//  4. session 记录 TempPath。
func TestChunkedInit_CreatesTempFileAndReserves(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := []byte(strings.Repeat("a", 6000))
	fileChecksum := sha256Hex(content)
	uploadID := "init-tmp-test"

	code, resp := env.initAs(t, "alice", uploadID, "f.bin", int64(len(content)), 4096, 2, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}

	// 断言 1：临时名存在于 user 桶目标同目录（生产散列取 rel 全路径 user/f.bin）。
	tnt := env.h.tenantFor("alice")
	rel, ok := tnt.UserRel("f.bin")
	if !ok {
		t.Fatal("UserRel 失败")
	}
	tempName := inflightTempNameFor(rel, uploadID)
	userAbs, ok := tnt.Root().Abs("user")
	if !ok {
		t.Fatal("user 桶绝对路径派生失败")
	}
	stat, err := os.Stat(filepath.Join(userAbs, tempName))
	if err != nil {
		t.Fatalf("init 应创建临时名 %s: %v", tempName, err)
	}
	if stat.Size() != int64(len(content)) {
		t.Fatalf("临时文件大小=%d want %d（Truncate(TotalSize)）", stat.Size(), len(content))
	}

	// 断言 2：user 桶 Scope 已 TryReserve(TotalSize)（预留中，尚未 commit）。
	if got := env.h.quotaBucketFor("alice", "user").Reserved(); got != int64(len(content)) {
		t.Fatalf("init 后 user 桶 Reserved()=%d want %d", got, len(content))
	}

	// 断言 3：session 记录 tempPath（可从 store GetSession 查到）。
	s := env.h.uploadStoreFor("alice").GetSession(uploadID)
	if s == nil {
		t.Fatal("init 后 session 应存在")
	}
	if s.TempPath == "" {
		t.Fatal("session 应记录 TempPath")
	}
}

// TestChunkedInit_SameFilenameDifferentChecksum_Conflict 验证同名存活 session 但不同
// checksum 时 init 直接 409 拒绝（不复用续传、不建新临时名）。
func TestChunkedInit_SameFilenameDifferentChecksum_Conflict(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	filename := "conflict-session.bin"
	uploadID := "conflict-session-1"
	content := []byte(strings.Repeat("a", 100))
	fileChecksum := sha256Hex(content)

	if code, resp := env.initAs(t, "alice", uploadID, filename, int64(len(content)), 4096, 1, fileChecksum); code != http.StatusOK {
		t.Fatalf("首次 init 应 200, got %d: %v", code, resp)
	}

	// 同名不同 checksum（不同 upload_id）→ 409，且不新建临时名。
	otherCS := sha256Hex([]byte("other content"))
	code, resp := env.initAs(t, "alice", "conflict-session-2", filename, int64(len(content)), 4096, 1, otherCS)
	if code != http.StatusConflict {
		t.Fatalf("同名不同 checksum init 应 409, got %d: %v", code, resp)
	}
	if n := len(inflightFilesInUser(t, env.h, "alice")); n != 1 {
		t.Fatalf("冲突 init 不应新建临时名（已有 1 个，got %d）", n)
	}
}

// TestChunkedInit_AlreadyExists_NoTempFile 验证已存在同名文件且 checksum 匹配时
// init 直接 already_exists 成功，不创建临时名、不预留。
func TestChunkedInit_AlreadyExists_NoTempFile(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := []byte("already on disk")
	fileChecksum := sha256Hex(content)
	filename := "exists-no-temp.bin"

	mux := actorUploadDeleteMux(env.h, "alice")
	if code, _ := uploadAs(t, mux, filename, content); code != http.StatusOK {
		t.Fatalf("预置文件上传应 200, got %d", code)
	}

	code, resp := env.initAs(t, "alice", "already-exists-1", filename, int64(len(content)), 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	uploadID, _ := resp["upload_id"].(string)
	if uploadID != "already_exists" {
		t.Fatalf("expected upload_id=already_exists, got %q", uploadID)
	}
	if n := len(inflightFilesInUser(t, env.h, "alice")); n != 0 {
		t.Fatalf("already_exists 不应创建临时名（got %d: %v）", n, inflightFilesInUser(t, env.h, "alice"))
	}
	if got := env.h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("already_exists 不应预留（Reserved=%d want 0）", got)
	}
}

// TestChunkedInit_QuotaExceeded_RejectsAndCleans 验证 TryReserve 超限（507）时：
// session 被清理、临时名不残留、预留不泄漏。
func TestChunkedInit_QuotaExceeded_RejectsAndCleans(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	env.h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 50}

	content := []byte(strings.Repeat("b", 100)) // 100 > 50 配额
	fileChecksum := sha256Hex(content)
	uploadID := "quota-507-1"

	code, resp := env.initAs(t, "alice", uploadID, "big.bin", int64(len(content)), 4096, 1, fileChecksum)
	if code != http.StatusInsufficientStorage {
		t.Fatalf("超配额 init 应 507, got %d: %v", code, resp)
	}
	if env.h.uploadStoreFor("alice").GetSession(uploadID) != nil {
		t.Fatal("507 后 session 应被清理")
	}
	if n := len(inflightFilesInUser(t, env.h, "alice")); n != 0 {
		t.Fatalf("507 后不应残留临时名（got %d: %v）", n, inflightFilesInUser(t, env.h, "alice"))
	}
	if got := env.h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("507 后不应泄漏预留（Reserved=%d want 0）", got)
	}
}

// TestChunkedUpload_SeekDirectWrite_OutOfOrder 验证 chunk 经 seek 直写临时名：
// 乱序上传分块后，临时文件内容 == 原文件（seek 固定 offset + BoundWriter 防越界），
// 且 complete 后落盘 user 桶内容正确。
func TestChunkedUpload_SeekDirectWrite_OutOfOrder(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := make([]byte, 0, 10000)
	for i := range 10000 {
		content = append(content, byte(i%251))
	}
	fileChecksum := sha256Hex(content)
	chunkSize := int64(4096)
	totalChunks := 3
	uploadID := "outoforder-test"

	code, resp := env.initAs(t, "alice", uploadID, "dir/ooo.bin", int64(len(content)), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}

	// 乱序上传：先 chunk2，再 chunk0，最后 chunk1。
	for _, idx := range []int{2, 0, 1} {
		start := idx * int(chunkSize)
		end := min(start+int(chunkSize), len(content))
		if c := env.chunkAs(t, "alice", uploadID, idx, content[start:end]); c != http.StatusOK {
			t.Fatalf("chunk %d 应 200, got %d", idx, c)
		}
	}

	// 临时名内容 == 原文件（乱序 seek 直写仍完整）。散列取 rel 全路径（user/dir/ooo.bin）。
	rel, ok := env.h.tenantFor("alice").UserRel("dir/ooo.bin")
	if !ok {
		t.Fatal("UserRel 失败")
	}
	tempName := inflightTempNameFor(rel, uploadID)
	dataOnDisk, err := os.ReadFile(filepath.Join(env.dir, "alice", "user", "dir", tempName))
	if err != nil {
		t.Fatalf("读取临时名失败: %v", err)
	}
	if !bytes.Equal(dataOnDisk, content) {
		t.Fatalf("临时名内容乱序直写后不正确: len=%d want %d", len(dataOnDisk), len(content))
	}

	// complete 后落盘 user 桶（临时名 rename 为正式名）。
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}
	finalPath := filepath.Join(env.dir, "alice", "user", "dir", "ooo.bin")
	finalData, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("最终文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, content) {
		t.Fatalf("最终文件内容不正确")
	}
}

// TestChunkedUpload_SeekDirectWrite_IdempotentRetransmit 验证 covered 单块重复 upload
// 校验幂等逻辑保留：已接收且 checksum 匹配的分块重传 → skip（不重复写、不报错）。
func TestChunkedUpload_SeekDirectWrite_IdempotentRetransmit(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := []byte(strings.Repeat("c", 100))
	fileChecksum := sha256Hex(content)
	uploadID := "idem-session-1"

	code, resp := env.initAs(t, "alice", uploadID, "idem.bin", int64(len(content)), 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	if c := env.chunkAs(t, "alice", uploadID, 0, content); c != http.StatusOK {
		t.Fatalf("首次 chunk 应 200, got %d", c)
	}
	// 重传同一分块（同 checksum）→ 幂等成功。
	if c := env.chunkAs(t, "alice", uploadID, 0, content); c != http.StatusOK {
		t.Fatalf("重传同 checksum chunk 应 200, got %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}
}

// TestChunkedUpload_RestartRecover_TempFileVerifies 验证重启恢复：session.json 读取后
// 从临时名逐分片 seek 重算校验比对 checksum 表，匹配的 bitmap 保留、不匹配的分片需重传。
// 与生产 init 一致：临时名在 user 桶、session.TempPath 持久化、Truncate(TotalSize) 占位。
func TestChunkedUpload_RestartRecover_TempFileVerifies(t *testing.T) {
	dir := t.TempDir()

	// 第一代 handlers：创建 alice 会话；分片 0 正确落盘并标记；分片 1 的临时名区域
	// 写成错误内容但 checksum 表记录正确（模拟 crash 前 bitmap 已刷、内容被破坏）。
	h1 := newChunkedTestHandlers(t, dir, 4096)
	filename := "recover-verify.bin"
	chunkSize := int64(4096)
	content0 := bytes.Repeat([]byte("X"), 4096)
	content1 := bytes.Repeat([]byte("Y"), 100) // 最后分片 100 字节
	total := append(append([]byte{}, content0...), content1...)

	uploadID := "recover-verify"
	us1 := h1.uploadStoreFor("alice")
	session, err := us1.CreateSession(uploadID, filename, int64(len(total)), chunkSize, 2, sha256Hex(total), 0)
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
	// 生产 init 的临时名创建等价物：user 桶 MkdirAll + O_EXCL + Truncate + TempPath + 持久化。
	tnt := h1.tenantFor("alice")
	rel, ok := tnt.UserRel(filename)
	if !ok {
		t.Fatal("UserRel 失败")
	}
	session.TempPath = tempRelForUser(session, rel)
	tempAbs, _ := tnt.Root().Abs(session.TempPath)
	if mkErr := os.MkdirAll(filepath.Dir(tempAbs), 0o755); mkErr != nil {
		t.Fatalf("mkdir: %v", mkErr)
	}
	tmpF, err := os.OpenFile(tempAbs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("创建临时名: %v", err)
	}
	if err := tmpF.Truncate(int64(len(total))); err != nil {
		tmpF.Close()
		t.Fatalf("truncate: %v", err)
	}
	tmpF.Close()

	if err := writeInflightTempEntry(t, h1, "alice", uploadID, filename, 0, content0); err != nil {
		t.Fatalf("写临时名分片 0: %v", err)
	}
	if err := us1.MarkChunkReceived(uploadID, 0, sha256Hex(content0)); err != nil {
		t.Fatalf("标记分片 0: %v", err)
	}
	// 分片 1：内容写成错误数据，checksum 表记录正确 checksum + bitmap true。
	if err := writeInflightTempEntry(t, h1, "alice", uploadID, filename, 1, bytes.Repeat([]byte("N"), 100)); err != nil {
		t.Fatalf("写临时名分片 1: %v", err)
	}
	if err := us1.MarkChunkReceived(uploadID, 1, sha256Hex(content1)); err != nil {
		t.Fatalf("标记分片 1: %v", err)
	}
	if err := us1.PersistNow(uploadID); err != nil {
		t.Fatalf("持久化: %v", err)
	}
	h1.Close() // 模拟重启

	// 第二代 handlers：恢复后分片 0 匹配保留、分片 1 不匹配须重传。
	h2 := newChunkedTestHandlers(t, dir, 4096)
	t.Cleanup(func() { _ = h2.Close() })
	recovered := h2.uploadStoreFor("alice").GetSession(uploadID)
	if recovered == nil {
		t.Fatal("重启后 session 未恢复")
	}
	if !recovered.ReceivedChunks[0] {
		t.Fatal("分片 0 校验匹配应保留 bitmap=true")
	}
	if recovered.ReceivedChunks[1] {
		t.Fatal("分片 1 校验不匹配应 bitmap=false（需重传）")
	}
}

// writeInflightTempEntry 在临时名（user 桶在途文件）offset 处写入分片内容（模拟 chunk 直写）。
func writeInflightTempEntry(t *testing.T, h *Handlers, owner, uploadID, filename string, idx int, data []byte) error {
	t.Helper()
	tnt := h.tenantFor(owner)
	rel, ok := tnt.UserRel(filename)
	if !ok {
		t.Fatalf("UserRel(%q) 失败", filename)
	}
	tempRel := tempRelForUser(sessionFromFilename(h, owner, uploadID, filename), rel)
	abs, ok := tnt.Root().Abs(tempRel)
	if !ok {
		t.Fatalf("临时名绝对路径派生失败: %s", tempRel)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteAt(data, int64(idx*4096))
	return err
}

// sessionFromFilename 构造一个仅含 uploadID/filename 的最小 session（供 tempRelForUser 推导临时名）。
func sessionFromFilename(h *Handlers, owner, uploadID, filename string) *ChunkedUploadSession {
	return &ChunkedUploadSession{UploadID: uploadID, Filename: filename}
}

// TestUploadStore_DeleteSession_RemovesTempFile 验证 DeleteSession 删除临时名并释放预留。
func TestUploadStore_DeleteSession_RemovesTempFile(t *testing.T) {
	dir := t.TempDir()
	h := newChunkedTestHandlers(t, dir, 4096)
	t.Cleanup(func() { _ = h.Close() })

	us := h.uploadStoreFor("alice")
	uploadID := "del-tempfile"
	content := bytes.Repeat([]byte("d"), 100)

	scope := h.quotaBucketFor("alice", "user")
	rr, err := scope.TryReserve(int64(len(content)))
	if err != nil {
		t.Fatalf("TryReserve: %v", err)
	}

	session, err := us.CreateSession(uploadID, "del-tempfile.bin", int64(len(content)), 4096, 1, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	session.TempPath = "user/" + inflightTempNameFor(mustUserRel(t, h, "alice", "del-tempfile.bin"), uploadID)
	session.Reservation = rr
	us.MarkChunkReceived(uploadID, 0, sha256Hex(content))

	// 在 user 桶写一个临时名文件（模拟在途数据）。
	tnt := h.tenantFor("alice")
	userAbs, _ := tnt.Root().Abs("user")
	tempAbs := filepath.Join(userAbs, inflightTempNameFor(mustUserRel(t, h, "alice", "del-tempfile.bin"), uploadID))
	if err := os.MkdirAll(filepath.Dir(tempAbs), 0o755); err != nil {
		t.Fatalf("mkdir user: %v", err)
	}
	if err := os.WriteFile(tempAbs, content, 0o600); err != nil {
		t.Fatalf("写临时名: %v", err)
	}

	us.DeleteSession(uploadID)

	if _, err := os.Stat(tempAbs); !os.IsNotExist(err) {
		t.Fatalf("DeleteSession 后临时名应被删除（err=%v）", err)
	}
	if got := scope.Reserved(); got != 0 {
		t.Fatalf("DeleteSession 后预留应释放（Reserved=%d want 0）", got)
	}
}

// TestChunkedUpload_Completed_RejectsNewChunks 验证 complete 后 session 标记完成，
// 后续 chunk 上传被拒绝（410 Gone）。
func TestChunkedUpload_Completed_RejectsNewChunks(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := []byte(strings.Repeat("e", 5000))
	fileChecksum := sha256Hex(content)
	chunkSize := int64(4096)
	totalChunks := 2
	uploadID := "completed-rejects"

	code, resp := env.initAs(t, "alice", uploadID, "done.bin", int64(len(content)), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	chunk0 := content[:4096]
	chunk1 := content[4096:]
	if c := env.chunkAs(t, "alice", uploadID, 0, chunk0); c != http.StatusOK {
		t.Fatalf("chunk 0 应 200, got %d", c)
	}
	if c := env.chunkAs(t, "alice", uploadID, 1, chunk1); c != http.StatusOK {
		t.Fatalf("chunk 1 应 200, got %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}
	// completed 后上传新 chunk 应被拒（410）。
	if c := env.chunkAs(t, "alice", uploadID, 0, chunk0); c != http.StatusGone {
		t.Fatalf("completed 后 chunk 应 410, got %d", c)
	}
}
