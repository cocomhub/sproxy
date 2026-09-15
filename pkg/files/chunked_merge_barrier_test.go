// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// chunked_merge_barrier_test.go 覆盖独立只读审计 C-3：complete 的「全文件校验 → rename」
// 之间缺少独占屏障。
//
// 机制（读码确认，非推测）：chunk 写入只被 `session.Completed` 拦，而 `Completed` 由
// rename **之后**的 CompleteSession 置位；`prepareMergedTemp` 持有的 LockChunkMerge 写锁在
// 校验返回时即释放 ⇒ 校验通过之后、rename 之前到达的 chunk 仍会通过检查并改写临时文件，
// 最终 rename 落盘的内容可能**不等于**刚校验通过的内容（TOCTOU）。
//
// 本用例把该窗口确定性展开：测试先替真实在途 chunk 持住 chunk IO 读锁（complete 的 merge
// 写锁会等它），再启动 complete，最后在「合并中」调用 chunk 接口 ⇒ 断言该 chunk **立刻被
// 拒绝**（而不是排队等待、随后在 rename 之后改写临时文件），并断言落盘内容 == 校验内容。

// newChunkRequest 构造一个分块上传请求（multipart），供主 goroutine 内构造、goroutine 内投递。
func newChunkRequest(t *testing.T, uploadID string, idx int, data []byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.WriteField("upload_id", uploadID); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("chunk_index", itoaChunkIdx(idx)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("chunk_checksum", sha256Hex(data)); err != nil {
		t.Fatal(err)
	}
	fw, err := w.CreateFormFile("chunk", "chunk")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/upload/chunk", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

// itoaChunkIdx 是本文件的极小工具，避免为一次转换引入 strconv（与既有测试文件的写法一致）。
func itoaChunkIdx(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// TestService_Complete_RejectsChunksWhileMerging 见文件头注释。
func TestService_Complete_RejectsChunksWhileMerging(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	content := []byte("01234567")
	fileCS := sha256Hex(content)
	const uploadID = "barrier-1"
	const filename = "barrier1.bin"

	// init + 两个分块（此时临时文件内容 == content，complete 的校验会通过）
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": 4, "total_chunks": 2, "file_checksum": fileCS, "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	for i := range 2 {
		cr := httptest.NewRecorder()
		h.UploadChunk(cr, newChunkRequest(t, uploadID, i, content[i*4:(i+1)*4]))
		if cr.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, cr.Code, cr.Body.String())
		}
	}

	// 模拟「一个 chunk 已在途」：它已通过检查并持住 IO 读锁（complete 的 merge 写锁会等它）。
	ioLockReleased := false
	unlockIO := env.us.LockChunkIO(uploadID)
	t.Cleanup(func() {
		if !ioLockReleased {
			unlockIO()
		}
	})

	// 启动 complete：它会先置「合并中」，然后在校验的 merge 写锁处等待上面的在途 chunk。
	completeDone := make(chan *httptest.ResponseRecorder, 1)
	completeReq := newCompleteRequest(t, uploadID)
	go func() {
		rr := httptest.NewRecorder()
		h.UploadComplete(rr, completeReq)
		completeDone <- rr
	}()

	// 「合并中」标记必须先于校验落地（这是本片新增的屏障）。
	testutil.WaitFor(t, 5*time.Second, func() bool {
		s := env.us.GetSession(uploadID)
		return s != nil && s.Completing
	}, "complete 进入合并阶段后应置「合并中」标记（否则合并期间的 chunk 会排队等待并在 rename 之后改写临时文件）")

	// 合并中到达的 chunk：必须**立刻**被拒绝。修复前它不会被拒（会在锁上排队，或直接写盘）。
	chunkDone := make(chan *httptest.ResponseRecorder, 1)
	chunkReq := newChunkRequest(t, uploadID, 0, []byte("XXXX"))
	go func() {
		rr := httptest.NewRecorder()
		h.UploadChunk(rr, chunkReq)
		chunkDone <- rr
	}()
	select {
	case rr := <-chunkDone:
		if rr.Code != http.StatusConflict {
			t.Fatalf("合并中的 chunk 应被拒绝（409），实际=%d body=%s", rr.Code, rr.Body.String())
		}
		// 复核 S3：合并是**瞬态**窗口（毫秒级），必须置 should_retry=true 让 SDK 退避重试；
		// 否则 SDK（pkg/client/chunked.go 仅在 should_retry=true 时重试）会把整个上传判死。
		// 多进程共用同一会话时可达：SDK 的 uploadID 是 filename|size|mtime|checksum 的确定性散列。
		var cur ChunkUploadResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &cur); err != nil {
			t.Fatalf("解析 chunk 响应失败: %v body=%s", err, rr.Body.String())
		}
		if !cur.ShouldRetry {
			t.Fatalf("合并中的 409 必须置 should_retry=true（瞬态窗口），否则 SDK 会直接判死上传: body=%s", rr.Body.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("合并中的 chunk 请求未被拒绝（仍在排队）：这正是 C-3 窗口——rename 之后它仍会改写临时文件")
	}

	// 放行在途 chunk / complete，等 complete 结束。
	unlockIO()
	ioLockReleased = true
	rec = <-completeDone
	if rec.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var cr ChunkCompleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("解析 complete 响应: %v", err)
	}
	if !cr.Success || cr.FileChecksum != fileCS {
		t.Fatalf("complete 响应异常: %+v", cr)
	}

	// 落盘内容必须等于**校验过**的内容（合并期间那次 chunk 不得生效）。
	rel, _ := env.tnt.UserRel(filename)
	abs, ok := env.tnt.Root().Abs(rel)
	if !ok {
		t.Fatal("落盘路径派生失败")
	}
	got, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("落盘内容 != 校验内容（合并窗口内的 chunk 生效了）: got=%q want=%q", got, content)
	}
	if csVal, ok := env.cs.Get(rel); !ok || csVal != fileCS {
		t.Fatalf("checksum 台账未记录: ok=%v val=%q", ok, csVal)
	}
}

// TestService_Complete_ConcurrentSecondIsRejected 钉住「同一会话不得同时合并两次」：
// 第一个 complete 已进入合并阶段（持有 merge 写锁）时，第二个 complete 应立刻 409，
// 而不是排队等待后重做一遍 verify→rename（那会与第一个的 rename 交错）。
func TestService_Complete_ConcurrentSecondIsRejected(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	content := []byte("01234567")
	fileCS := sha256Hex(content)
	const uploadID = "barrier-2"
	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": "barrier2.bin", "total_size": len(content),
		"chunk_size": 4, "total_chunks": 2, "file_checksum": fileCS, "file_mod_time": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	for i := range 2 {
		cr := httptest.NewRecorder()
		h.UploadChunk(cr, newChunkRequest(t, uploadID, i, content[i*4:(i+1)*4]))
		if cr.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, cr.Code, cr.Body.String())
		}
	}

	// 持住 merge 写锁，使第一个 complete 停在「合并中」（已置位）而不会立刻完成。
	unlockMerge := env.us.LockChunkMerge(uploadID)
	mergeHeld := true
	t.Cleanup(func() {
		if mergeHeld {
			unlockMerge()
		}
	})
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	// 请求在**主 goroutine** 构造（newCompleteRequest 内含 t.Fatal；在 goroutine 内失败会
	// Goexit 该 goroutine，主 goroutine 将永远阻塞在 <-firstDone 上而非快速失败——复核 R3）。
	firstReq := newCompleteRequest(t, uploadID)
	go func() {
		rr := httptest.NewRecorder()
		h.UploadComplete(rr, firstReq)
		firstDone <- rr
	}()
	testutil.WaitFor(t, 5*time.Second, func() bool {
		s := env.us.GetSession(uploadID)
		return s != nil && s.Completing
	}, "第一个 complete 应立即进入合并阶段")

	// 第二个 complete：必须立刻 409（不得排队后重做一遍）。
	second := httptest.NewRecorder()
	h.UploadComplete(second, newCompleteRequest(t, uploadID))
	if second.Code != http.StatusConflict {
		t.Fatalf("并发第二个 complete 应 409，实际=%d body=%s", second.Code, second.Body.String())
	}

	unlockMerge()
	mergeHeld = false
	if rec = <-firstDone; rec.Code != http.StatusOK {
		t.Fatalf("第一个 complete 应成功，实际=%d body=%s", rec.Code, rec.Body.String())
	}
}

// newCompleteRequest 构造 complete 请求（JSON）。
func newCompleteRequest(t *testing.T, uploadID string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"upload_id": uploadID})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/upload/complete", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// TestStore_CompletingFlag_BeginEnd 钉住「合并中」标记的语义（幂等/可回退）：
// 置位成功后并发第二次 BeginComplete 必须失败（防两个 complete 同时合并），
// 失败路径 EndComplete 后必须可再次置位（客户端重传后能重试 complete）。
func TestStore_CompletingFlag_BeginEnd(t *testing.T) {
	t.Parallel()
	env := newChunkedTestEnv(t)
	us := env.us

	sess := newSession("flag-1", "flag1.bin", 8, 4, 2, sha256Hex([]byte("01234567")), 0, time.Hour)
	us.mu.Lock()
	us.sessions[sess.UploadID] = sess
	us.mu.Unlock()

	if !us.BeginComplete(sess.UploadID) {
		t.Fatal("首次 BeginComplete 应成功")
	}
	if got := us.GetSession(sess.UploadID); got == nil || !got.Completing {
		t.Fatalf("BeginComplete 后 Completing 应为 true: %+v", got)
	}
	if us.BeginComplete(sess.UploadID) {
		t.Fatal("并发第二次 BeginComplete 应失败（同一会话不得同时合并两次）")
	}
	us.EndComplete(sess.UploadID)
	if got := us.GetSession(sess.UploadID); got == nil || got.Completing {
		t.Fatalf("EndComplete 后 Completing 应为 false: %+v", got)
	}
	if !us.BeginComplete(sess.UploadID) {
		t.Fatal("EndComplete 后应可再次 BeginComplete（客户端重传后重试 complete）")
	}
	if err := us.CompleteSession(sess.UploadID); err != nil {
		t.Fatalf("CompleteSession: %v", err)
	}
	if got := us.GetSession(sess.UploadID); got == nil || !got.Completed {
		t.Fatalf("CompleteSession 后 Completed 应为 true: %+v", got)
	}
	if us.BeginComplete(sess.UploadID) {
		t.Fatal("已完成会话不得再 BeginComplete")
	}
}
