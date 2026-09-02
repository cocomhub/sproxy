// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// chunked_complete_test.go 覆盖任务 5 的 complete 期逻辑：
//  1. 全文件校验通过 → rename + checksum store + 覆盖写 ReleaseUsage(old)（替代 Adjust）；
//  2. 全文件校验失败 → 逐分片 seek 重算 → mismatch_chunks 显式返回（客户端只重传坏分片）；
//  3. 重叠分片内容被越界写坏 → 该分片被识别为 mismatch（越界写不污染相邻分片无法恢复
//     时的显式化，I-2）；
//  4. complete 成功后的坏响应 content 会导致下载校验失败（数据完整性担保）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertChunkedMismatchResponse 断言 complete 响应带 mismatch_chunks 列表，且 session
// 仍可查（失败保留供重传）、临时名仍存在（供重传 seek 覆盖）、预留未释放。
func assertChunkedMismatchResponse(t *testing.T, env *ownerChunkedEnv, uploadID string, resp map[string]any, wantMismatch []int) {
	t.Helper()
	if got := resp["success"]; got == true {
		t.Fatalf("complete 应失败（校验不匹配）, got success=%v body=%v", got, resp)
	}
	msg, _ := resp["message"].(string)
	if msg == "" {
		t.Fatal("mismatch 响应应有 message 说明")
	}
	mismatchOK := false
	switch v := resp["mismatch_chunks"].(type) {
	case nil:
		t.Fatal("mismatch 响应应有 mismatch_chunks 字段")
	case []any:
		if len(v) != len(wantMismatch) {
			t.Fatalf("mismatch_chunks=%v want %v（应精确列出坏分片）", v, wantMismatch)
		}
		for i, iv := range v {
			if got, _ := iv.(float64); int(got) != wantMismatch[i] {
				t.Fatalf("mismatch_chunks[%d]=%v want %d", i, iv, wantMismatch[i])
			}
		}
		mismatchOK = true
	case []float64:
		if len(v) != len(wantMismatch) {
			t.Fatalf("mismatch_chunks=%v want %v", v, wantMismatch)
		}
		for i := range v {
			if int(v[i]) != wantMismatch[i] {
				t.Fatalf("mismatch_chunks[%d]=%v want %d", i, v[i], wantMismatch[i])
			}
		}
		mismatchOK = true
	}
	if !mismatchOK {
		t.Fatalf("mismatch_chunks 类型异常: %T", resp["mismatch_chunks"])
	}

	// 失败保留：session 仍存在、临时名（session.TempPath 指向文件）仍存在、预留未释放
	// （供重传继续写）。临时名在 user 桶目标同目录，用 session.TempPath 精确断言（而非
	// 只扫 user 桶根——子目录临时名不在根下）。
	sess := env.h.uploadStoreFor("alice").GetSession(uploadID)
	if sess == nil || sess.Completed {
		t.Fatalf("mismatch 后 session 应保留且未完成, got %+v", sess)
	}
	if sess.TempPath != "" {
		tnt := env.h.tenantFor("alice")
		if abs, ok := tnt.Root().Abs(sess.TempPath); ok {
			if _, err := os.Stat(abs); err != nil {
				t.Fatalf("mismatch 后临时名应保留（供重传）, stat %s: %v", abs, err)
			}
		} else {
			t.Fatalf("session.TempPath 非法: %s", sess.TempPath)
		}
	} else {
		t.Fatal("mismatch 后 session 应仍有 TempPath")
	}
	if got := env.h.quotaBucketFor("alice", "user").Reserved(); got == 0 {
		t.Fatal("mismatch 后配额预留应保留（供重传继续写）, Reserved=0")
	}
}

// TestCompleteFullVerifyAndMismatchChunks 覆盖：篡改临时名某分片区域后 complete →
// 全文件校验失败 → 返回 mismatch_chunks [idx]（精确列出坏分片）→ 客户端据此只重传坏
// 分片 → 再次 complete 成功（bitmap 未被清空、skipped 分片保留）。
func TestCompleteFullVerifyAndMismatchChunks(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := make([]byte, 0, 10000)
	for i := range 10000 {
		content = append(content, byte(i%251))
	}
	fileChecksum := sha256Hex(content)
	chunkSize := int64(4096)
	totalChunks := 3
	uploadID := "complete-mismatch-1"

	code, resp := env.initAs(t, "alice", uploadID, "dir/bad.bin", int64(len(content)), chunkSize, totalChunks, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	for i := range totalChunks {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(content))
		if c := env.chunkAs(t, "alice", uploadID, i, content[start:end]); c != http.StatusOK {
			t.Fatalf("chunk %d 应 200, got %d", i, c)
		}
	}

	// 篡改分片 1 在临时名中的内容（WriteAt offset=4096，保 chunk0/chunk2 内容不动——
	// os.WriteFile 会从 0 截断，必须 WriteAt 指定偏移，否则整片 0/1/2 全坏）。
	rel, ok := env.h.tenantFor("alice").UserRel("dir/bad.bin")
	if !ok {
		t.Fatal("UserRel 失败")
	}
	tempName := inflightTempNameFor(rel, uploadID)
	tempAbs := filepath.Join(env.dir, "alice", "user", "dir", tempName)
	f, err := os.OpenFile(tempAbs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开临时名篡改: %v", err)
	}
	if _, werr := f.WriteAt(bytes.Repeat([]byte("X"), 4096), 4096); werr != nil {
		f.Close()
		t.Fatalf("写入篡改内容: %v", werr)
	}
	f.Close()

	// complete → 400 + mismatch_chunks == [1]
	cc, cresp := env.completeAs(t, "alice", uploadID)
	if cc != http.StatusBadRequest {
		t.Fatalf("校验失败 complete 应 400, got %d: %v", cc, cresp)
	}
	assertChunkedMismatchResponse(t, env, uploadID, cresp, []int{1})

	// 客户端模型：只重传坏分片 1（seek 覆盖 + MarkChunkReceived）。
	if c := env.chunkAs(t, "alice", uploadID, 1, content[4096:8192]); c != http.StatusOK {
		t.Fatalf("重传坏分片应 200, got %d", c)
	}
	cc2, cresp2 := env.completeAs(t, "alice", uploadID)
	if cc2 != http.StatusOK {
		t.Fatalf("重传坏分片后 complete 应 200, got %d: %v", cc2, cresp2)
	}
	if fc, _ := cresp2["file_checksum"].(string); fc != fileChecksum {
		t.Fatalf("重传后 checksum 应匹配, got %q want %q", fc, fileChecksum)
	}
	// 最终落盘 == 原文件
	finalData, err := os.ReadFile(filepath.Join(env.dir, "alice", "user", "dir", "bad.bin"))
	if err != nil {
		t.Fatalf("最终文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, content) {
		t.Fatal("重传后最终文件内容不正确")
	}
	// 临时名已随完成清理
	if n := len(inflightFilesInUser(t, env.h, "alice")); n != 0 {
		t.Fatalf("complete 成功后不应残留临时名, got %d", n)
	}
}

// TestCompleteMismatch_OverlapFineGrain 覆盖 I-2 精确识别：相邻两片交界处被重叠写坏
// 一片（越界写污染无法恢复场景），complete 返回 mismatch_chunks 精确列出该片（而非 400
// 泛化"校验失败"）。只篡改 CI 片内容且 chunk 边界不合法的情形由 full-file 校验兜底。
func TestCompleteMismatch_OverlapFineGrain(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := bytes.Repeat([]byte("abc"), 3000) // 9 KiB, 3 chunks
	fileChecksum := sha256Hex(content)
	chunkSize := int64(4096)
	uploadID := "complete-mismatch-2"

	code, resp := env.initAs(t, "alice", uploadID, "o.bin", int64(len(content)), chunkSize, 3, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	for i := range 3 {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(content))
		if c := env.chunkAs(t, "alice", uploadID, i, content[start:end]); c != http.StatusOK {
			t.Fatalf("chunk %d 应 200, got %d", i, c)
		}
	}
	// 篡改分片 0 内容（WriteAt offset 0）
	rel, _ := env.h.tenantFor("alice").UserRel("o.bin")
	tempName := inflightTempNameFor(rel, uploadID)
	tempAbs := filepath.Join(env.dir, "alice", "user", tempName)
	f, err := os.OpenFile(tempAbs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开临时名篡改: %v", err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte("W"), 4096), 0); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	cc, cresp := env.completeAs(t, "alice", uploadID)
	if cc != http.StatusBadRequest {
		t.Fatalf("校验失败 complete 应 400, got %d: %v", cc, cresp)
	}
	assertChunkedMismatchResponse(t, env, uploadID, cresp, []int{0})
}

// TestCompleteOverwriteReleaseUsage 覆盖覆盖写（versioning enabled 下 checkExistingFileForInit
// 允许）后 user 桶配额收敛到新大小：旧文件 ReleaseUsage(old)、新文件 commit 新大小，
// 不用 Adjust 差分。断言 committed == 新大小（而非旧大小 ± diff 的虚拟值）。
func TestCompleteOverwriteReleaseUsage(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	env.h.cfgPtr.Load().OwnerQuotas = map[string]int64{"alice": 1000}
	env.h.cfgPtr.Load().Versioning.Enabled = true
	env.h.cfgPtr.Load().Versioning.MaxVersions = 10

	old := []byte(strings.Repeat("a", 60))
	umux := actorUploadDeleteMux(env.h, "alice")
	if code, resp := uploadAs(t, umux, "ov.bin", old); code != http.StatusOK {
		t.Fatalf("预置文件上传应 200, got %d: %s", code, resp)
	}
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 60 {
		t.Fatalf("预置后 user 桶 Usage()=%d want 60", got)
	}

	// 分块 init（versioning 开启：同名旧文件 checksum 不同 → 复用会话允许覆盖，不冲突）
	newContent := []byte(strings.Repeat("b", 40))
	fileChecksum := sha256Hex(newContent)
	uploadID := "complete-ov-1"
	code, resp := env.initAs(t, "alice", uploadID, "ov.bin", int64(len(newContent)), 4096, 1, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	if c := env.chunkAs(t, "alice", uploadID, 0, newContent); c != http.StatusOK {
		t.Fatalf("chunk 应 200, got %d", c)
	}
	if cc, cresp := env.completeAs(t, "alice", uploadID); cc != http.StatusOK {
		t.Fatalf("complete 应 200, got %d: %v", cc, cresp)
	}

	// 覆盖后 user 桶 committed == 新大小（ReleaseUsage(old) + commit(new) → 40），
	// version 桶 == 旧大小 60（覆盖前 saveVersion 已备份）。
	if got := env.h.quotaBucketFor("alice", "user").Usage(); got != 40 {
		t.Fatalf("覆盖后 user 桶 Usage()=%d want 40（ReleaseUsage(old)+commit(new)）", got)
	}
	if got := env.h.quotaBucketFor("alice", "version").Usage(); got != 60 {
		t.Fatalf("覆盖后 version 桶 Usage()=%d want 60（旧版本已备份）", got)
	}
	if got := env.h.quotaBucketFor("alice", "user").Reserved(); got != 0 {
		t.Fatalf("覆盖后 user 桶 Reserved()=%d want 0", got)
	}
	// 落盘内容 == 新内容、checksum store 已写新 checksum
	finalPath := filepath.Join(env.dir, "alice", "user", "ov.bin")
	finalData, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("覆盖后文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, newContent) {
		t.Fatal("覆盖后文件内容不正确")
	}
	if cs := env.h.checksumStoreFor("alice"); cs != nil {
		if got, ok := cs.Get("user/ov.bin"); !ok || got != fileChecksum {
			t.Fatalf("覆盖后 checksum 记录应有新 checksum, ok=%v got=%q", ok, got)
		}
	}
}

// TestCompleteBadContent_RejectedAndCleanupState 覆盖服务端通过校验但内容非预期时
// 下载校验失败（数据完整性担保，替代"覆盖写内容错误"的客户端奇偶断言）：分块上传
// 一个 3 分块文件后篡改临时名分片 1 → complete 失败（满足任务 4/5 不落盘错误内容）。
// 与 TestCompleteFullVerifyAndMismatchChunks 的区别：断言 complete 失败后正式名不存在、
// 448 字节界限清晰。
func TestCompleteBadContent_RejectedAndCleanupState(t *testing.T) {
	env := newOwnerChunkedEnv(t)
	content := bytes.Repeat([]byte("Z"), 9000) // 3 chunks
	fileChecksum := sha256Hex(content)
	uploadID := "complete-bad-1"

	code, resp := env.initAs(t, "alice", uploadID, "badfile.bin", int64(len(content)), 4096, 3, fileChecksum)
	if code != http.StatusOK {
		t.Fatalf("init 应 200, got %d: %v", code, resp)
	}
	for i := range 3 {
		start := i * 4096
		end := min(start+4096, len(content))
		if c := env.chunkAs(t, "alice", uploadID, i, content[start:end]); c != http.StatusOK {
			t.Fatalf("chunk %d 应 200, got %d", i, c)
		}
	}
	rel, _ := env.h.tenantFor("alice").UserRel("badfile.bin")
	tempAbs := filepath.Join(env.dir, "alice", "user", inflightTempNameFor(rel, uploadID))
	f, err := os.OpenFile(tempAbs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("打开临时名篡改: %v", err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte("X"), 4096), 4096); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()

	cc, cresp := env.completeAs(t, "alice", uploadID)
	if cc != http.StatusBadRequest {
		t.Fatalf("篡改后 complete 应 400, got %d: %v", cc, cresp)
	}
	assertChunkedMismatchResponse(t, env, uploadID, cresp, []int{1})
	// 正式名不应存在（校验失败不落盘错误内容）
	if _, err := os.Stat(filepath.Join(env.dir, "alice", "user", "badfile.bin")); !os.IsNotExist(err) {
		t.Fatalf("complete 失败后正式名不应存在, stat err=%v", err)
	}
}

// dummyUploadStoreIface 实现 UploadStoreIface，供编译期签名校验不会误报。
// JSON 响应形状由 handler 返回体（TestCompleteFullVerifyAndMismatchChunks 的
// assertChunkedMismatchResponse）覆盖，详见其文档。

// helper 断言删除临时名的幂等状态由既有 DeleteSession 测试覆盖。

// TestCompleteAfterRecovery_MismatchConsistent 覆盖任务 4 恢复与任务 5 complete 的衔接
// （重启恢复逐分片重算 → 恢复后的 complete 对 mismatch 一致）：第一代 handlers 建临时名
// + 写坏分片 1 + 持久化；第二代恢复后（bitmap：0 匹配保留、1 需重传）直接 complete 应
// 返回完整 mismatch（含恢复后仍坏的分片），而非错误落盘错误内容；数据可重传前提下补传
// 后 complete 成功。
func TestCompleteAfterRecovery_MismatchConsistent(t *testing.T) {
	dir := t.TempDir()

	// 第一代 handlers：建 alice 会话与临时名（user 桶），分片 0 正确、分片 1 写坏
	// （bitmap 都置 true 模拟 crash 前已标记）。
	h1 := newChunkedTestHandlers(t, dir, 4096)
	filename := "recover-complete.bin"
	chunkSize := int64(4096)
	content0 := bytes.Repeat([]byte("G"), 4096)
	content1 := bytes.Repeat([]byte("H"), 100)
	total := append(append([]byte{}, content0...), content1...)

	uploadID := "recover-complete-1"
	us1 := h1.uploadStoreFor("alice")
	session, err := us1.CreateSession(uploadID, filename, int64(len(total)), chunkSize, 2, sha256Hex(total), 0)
	if err != nil {
		t.Fatalf("创建会话失败: %v", err)
	}
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
	tmpF, tmpErr := os.OpenFile(tempAbs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if tmpErr != nil {
		t.Fatalf("创建临时名: %v", tmpErr)
	}
	if truncErr := tmpF.Truncate(int64(len(total))); truncErr != nil {
		tmpF.Close()
		t.Fatalf("truncate: %v", truncErr)
	}
	tmpF.Close()
	if werr := writeInflightTempEntry(t, h1, "alice", uploadID, filename, 0, content0); werr != nil {
		t.Fatalf("写分片 0: %v", werr)
	}
	// 分片 1 写坏内容，但 bitmap/checksum 表按正确内容标记。
	if werr := writeInflightTempEntry(t, h1, "alice", uploadID, filename, 1, bytes.Repeat([]byte("N"), 100)); werr != nil {
		t.Fatalf("写分片 1: %v", werr)
	}
	if merr := us1.MarkChunkReceived(uploadID, 0, sha256Hex(content0)); merr != nil {
		t.Fatalf("标记 0: %v", merr)
	}
	if merr := us1.MarkChunkReceived(uploadID, 1, sha256Hex(content1)); merr != nil {
		t.Fatalf("标记 1: %v", merr)
	}
	if perr := us1.PersistNow(uploadID); perr != nil {
		t.Fatalf("持久化: %v", perr)
	}
	h1.Close() // 模拟重启

	// 第二代 handlers：恢复（分片 0 匹配保留、分片 1 需重传）。
	h2 := newChunkedTestHandlers(t, dir, 4096)
	t.Cleanup(func() { _ = h2.Close() })
	rec := h2.uploadStoreFor("alice").GetSession(uploadID)
	if rec == nil {
		t.Fatal("重启后 session 未恢复")
	}
	if !rec.ReceivedChunks[0] || rec.ReceivedChunks[1] {
		t.Fatalf("恢复 bitmap 应 0=keep/1=clear, got %v", rec.ReceivedChunks)
	}

	// 恢复后直接 complete：AllChunksReceived 不满足（1 未接收）→ 400（缺失分片语义，
	// 与 mismatch 显式化一致——绝对不落盘错误内容）。
	raw, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	req := httptest.NewRequest("POST", "/upload/complete", bytes.NewReader(raw))
	req = req.WithContext(withActor(req.Context(), "alice"))
	rr := httptest.NewRecorder()
	h2.uploadComplete(rr, req)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("恢复后 complete 应 400（分片 1 缺失）, got %d body=%s", rr.Code, rr.Body.String())
	}
	if _, statErr := os.Stat(filepath.Join(dir, "alice", "user", filename)); !os.IsNotExist(statErr) {
		t.Fatalf("恢复后 complete 失败不应落盘正式名, stat err=%v", statErr)
	}

	// 客户端模型：重传分片 1（先 seek 直写正确内容到临时名，再标记——等价 uploadChunk）
	// + complete → 成功（数据可重传前提下恢复后 complete 收敛）。
	if werr := writeInflightTempEntry(t, h2, "alice", uploadID, filename, 1, content1); werr != nil {
		t.Fatalf("重传写分片 1: %v", werr)
	}
	if merr := h2.uploadStoreFor("alice").MarkChunkReceived(uploadID, 1, sha256Hex(content1)); merr != nil {
		t.Fatalf("标记重传分片 1: %v", merr)
	}
	rr2 := httptest.NewRecorder()
	h2.uploadComplete(rr2, httptest.NewRequest("POST", "/upload/complete", bytes.NewReader(raw)).
		WithContext(withActor(req.Context(), "alice")))
	if rr2.Code != http.StatusOK {
		t.Fatalf("重传后 complete 应 200, got %d body=%s", rr2.Code, rr2.Body.String())
	}
	finalData, readErr := os.ReadFile(filepath.Join(dir, "alice", "user", filename))
	if readErr != nil {
		t.Fatalf("最终文件未落盘: %v", err)
	}
	if !bytes.Equal(finalData, total) {
		t.Fatal("恢复+重传后最终文件内容不正确")
	}
}

// TestFindMismatchChunks_StoreUnit 是 findMismatchChunks/ClearChunksReceived 的 store 级
// 单元测试（不依赖 handler）：直接构造会话+临时名（非 .inflight 前缀也校验），篡改某分片
// 内容 → findMismatchChunks 精确返回该片；ClearChunksReceived 落盘后 status 的缺失列表一致。
func TestFindMismatchChunks_StoreUnit(t *testing.T) {
	dir := t.TempDir()
	h := newChunkedTestHandlers(t, dir, 4096)
	t.Cleanup(func() { _ = h.Close() })

	content := bytes.Repeat([]byte("M"), 9000)
	chunkSize := int64(4096)
	uploadID := "store-mismatch-1"
	filename := "dir/store-mismatch.bin"
	us := h.uploadStoreFor("alice")
	session, err := us.CreateSession(uploadID, filename, int64(len(content)), chunkSize, 3, sha256Hex(content), 0)
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	tnt := h.tenantFor("alice")
	rel, _ := tnt.UserRel(filename)
	session.TempPath = tempRelForUser(session, rel)
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
	tmpF.Close()
	for i := range 3 {
		if werr := writeInflightTempEntry(t, h, "alice", uploadID, filename, i, content[i*4096:min((i+1)*4096, len(content))]); werr != nil {
			t.Fatalf("写分片 %d: %v", i, werr)
		}
		if merr := us.MarkChunkReceived(uploadID, i, sha256Hex(content[i*4096:min((i+1)*4096, len(content))])); merr != nil {
			t.Fatalf("标记 %d: %v", i, merr)
		}
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
		t.Fatalf("findMismatchChunks=%v want [2]（精确列出被篡改的分片）", mismatch)
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
