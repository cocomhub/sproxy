// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"testing"
)

func TestDownloadChunk_FullFileViaChunks(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	// 准备 16 KiB 文件
	fileData := bytes.Repeat([]byte("ChunkDownloadTest!"), 1024) // 16 KiB
	fileChecksum := sha256hex(fileData)
	chunkSize := int64(4096)

	// 通过分块上传创建文件
	totalChunks := (len(fileData) + int(chunkSize) - 1) / int(chunkSize)
	uploadID := initSessionEx(t, url, "full-dl-chunked.bin", int64(len(fileData)), chunkSize,
		totalChunks, fileChecksum)

	for i := range totalChunks {
		start := i * int(chunkSize)
		end := min(start+int(chunkSize), len(fileData))
		chunkData := fileData[start:end]
		uploadChunk(t, url, uploadID, i, sha256hex(chunkData), chunkData)
	}
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

	// 用分块下载重组
	var assembled []byte
	for offset := 0; offset < len(fileData); offset += int(chunkSize) {
		length := min(int(chunkSize), len(fileData)-offset)
		resp, err := http.Get(fmt.Sprintf("%s/download/chunk?filename=full-dl-chunked.bin&offset=%d&length=%d",
			url, offset, length))
		if err != nil {
			t.Fatalf("download chunk at offset %d: %v", offset, err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			t.Fatalf("expected 200 at offset %d, got %d", offset, resp.StatusCode)
		}
		chunkBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		assembled = append(assembled, chunkBody...)
	}

	if !bytes.Equal(assembled, fileData) {
		t.Fatal("reassembled file content mismatch")
	}

	// 验证完整 file checksum
	gotCS := sha256hex(assembled)
	if gotCS != fileChecksum {
		t.Fatalf("reassembled checksum mismatch: got %s, want %s", gotCS, fileChecksum)
	}
}

func TestDownloadChunk_OffsetBeyondFile(t *testing.T) {
	url, _, cleanup := newTestServerWithChunked(t, nil)
	defer cleanup()

	fileData := []byte("small")
	fileChecksum := sha256hex(fileData)
	uploadID := initSession(t, url, "small-dl.bin", int64(len(fileData)), fileChecksum)
	uploadChunk(t, url, uploadID, 0, fileChecksum, fileData)
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))

	// offset 超过 file size
	resp, err := http.Get(url + "/download/chunk?filename=small-dl.bin&offset=100&length=10")
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("expected 416, got %d", resp.StatusCode)
	}
}