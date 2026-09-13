// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// chunked_complete_contract_test.go 是分块上传 `complete` 的**上传成功副作用契约钉住测试**
// （P2-d 配套）：分块路径与单次上传路径必须共用同一份「落盘后副作用」内核
// （`recordUploadSuccess`：mtime + checksum 台账）。
//
// 为什么必须钉住：这条路径的 mtime 语义此前**没有任何用例覆盖**（全仓 grep `ModTime()` 无命中），
// 而 `file_mod_time == 0` 表示「不设置」（不是 epoch）、台账 key 是**租户根相对 rel**——这些约定
// 只在复制粘贴时最容易丢。测试先于重构写就（重构前已绿，作为等价性基线），重构后必须仍绿。

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

// TestChunkedCompleteContract_AppliesMTimeAndChecksumLedger 钉住分块 complete 的两项副作用：
//  1. 客户端在 init 声明的 `file_mod_time` 必须落到**落盘文件**的 ModTime；
//  2. 最终 checksum 必须写入 per-tenant 台账（key = 租户根相对 rel）。
func TestChunkedCompleteContract_AppliesMTimeAndChecksumLedger(t *testing.T) {
	env := newChunkedTestEnv(t)
	h := env.handlers(4)

	content := []byte("0123456789")
	fileCS := sha256Hex(content)
	const uploadID = "contract-mtime-1"
	const filename = "dir/mtime.bin"
	modTimeNano := int64(1_700_000_000) * 1e9

	rec := env.doJSON(t, h, http.MethodPost, "/upload/init", h.UploadInit, map[string]any{
		"upload_id": uploadID, "filename": filename, "total_size": len(content),
		"chunk_size": 4, "total_chunks": 3, "file_checksum": fileCS, "file_mod_time": modTimeNano,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("init 状态=%d body=%s", rec.Code, rec.Body.String())
	}

	for i := range 3 {
		start := i * 4
		end := min(start+4, len(content))
		data := content[start:end]

		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		if err := w.WriteField("upload_id", uploadID); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteField("chunk_index", string(rune('0'+i))); err != nil {
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
		chunkRec := httptest.NewRecorder()
		h.UploadChunk(chunkRec, req)
		if chunkRec.Code != http.StatusOK {
			t.Fatalf("chunk %d 状态=%d body=%s", i, chunkRec.Code, chunkRec.Body.String())
		}
	}

	rec = env.doJSON(t, h, http.MethodPost, "/upload/complete", h.UploadComplete, map[string]any{"upload_id": uploadID})
	if rec.Code != http.StatusOK {
		t.Fatalf("complete 状态=%d body=%s", rec.Code, rec.Body.String())
	}
	var cr ChunkCompleteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &cr); err != nil {
		t.Fatalf("解析 complete: %v", err)
	}
	if !cr.Success || cr.FileChecksum != fileCS {
		t.Fatalf("complete 响应异常: %+v", cr)
	}

	rel, ok := env.tnt.UserRel(filename)
	if !ok {
		t.Fatal("派生 rel 失败")
	}
	abs, ok := env.tnt.Root().Abs(rel)
	if !ok {
		t.Fatal("派生落盘路径失败")
	}
	info, err := os.Stat(abs)
	if err != nil {
		t.Fatalf("stat 落盘文件失败: %v", err)
	}
	if got := info.ModTime().Unix(); got != modTimeNano/1e9 {
		t.Fatalf("落盘 mtime=%d want %d（分块路径必须与单次上传共用 recordUploadSuccess 内核）",
			got, modTimeNano/1e9)
	}
	if csVal, ok := env.cs.Get(rel); !ok || csVal != fileCS {
		t.Fatalf("checksum 台账未记录: ok=%v val=%q want %q", ok, csVal, fileCS)
	}
}
