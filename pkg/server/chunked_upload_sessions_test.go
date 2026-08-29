// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

// getUploadSessions 请求 GET /upload/sessions 并解码响应。
func getUploadSessions(t *testing.T, baseURL string) ChunkSessionsResponse {
	t.Helper()
	resp, err := http.Get(baseURL + "/upload/sessions")
	if err != nil {
		t.Fatalf("GET /upload/sessions 请求失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("预期 200，实际 %d", resp.StatusCode)
	}
	var body ChunkSessionsResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解码响应失败: %v", err)
	}
	if !body.Success {
		t.Fatalf("预期 success=true，实际: %+v", body)
	}
	return body
}

func TestUploadSessions_ListAndAfterComplete(t *testing.T) {
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()

	// 空列表
	if got := getUploadSessions(t, url); len(got.Sessions) != 0 {
		t.Fatalf("预期空列表，实际: %+v", got.Sessions)
	}

	// init 一个 2 分块会话
	fileData := bytes.Repeat([]byte("T"), 8192)
	fileChecksum := sha256hex(fileData)
	uploadID := initSessionEx(t, url, "sessions-list.txt", int64(len(fileData)), 4096, 2, fileChecksum)

	// 传 1 块
	chunk0 := fileData[:4096]
	chunk0CS := sha256hex(chunk0)
	uploadChunk(t, url, uploadID, 0, chunk0CS, chunk0)

	// 列出应包含该会话
	sessions := getUploadSessions(t, url).Sessions
	if len(sessions) != 1 {
		t.Fatalf("预期 1 个会话，实际 %d 个: %+v", len(sessions), sessions)
	}
	got := sessions[0]
	if got.UploadID != uploadID {
		t.Errorf("upload_id 不匹配: got=%q want=%q", got.UploadID, uploadID)
	}
	if got.Filename != "sessions-list.txt" {
		t.Errorf("filename 不匹配: got=%q", got.Filename)
	}
	if got.TotalSize != int64(len(fileData)) {
		t.Errorf("total_size 不匹配: got=%d want=%d", got.TotalSize, len(fileData))
	}
	if got.ReceivedCount != 1 {
		t.Errorf("received_count 不匹配: got=%d want=1", got.ReceivedCount)
	}
	if got.TotalChunks != 2 {
		t.Errorf("total_chunks 不匹配: got=%d want=2", got.TotalChunks)
	}
	if got.FileChecksum != fileChecksum {
		t.Errorf("file_checksum 不匹配: got=%q want=%q", got.FileChecksum, fileChecksum)
	}
	if got.Status != "uploading" {
		t.Errorf("status 不匹配: got=%q want=%q", got.Status, "uploading")
	}

	// 传剩余块并完成，列表应清空
	chunk1 := fileData[4096:]
	chunk1CS := sha256hex(chunk1)
	uploadChunk(t, url, uploadID, 1, chunk1CS, chunk1)
	completeBody, _ := json.Marshal(map[string]string{"upload_id": uploadID})
	cresp, err := http.Post(url+"/upload/complete", "application/json", bytes.NewReader(completeBody))
	if err != nil {
		t.Fatalf("complete 请求失败: %v", err)
	}
	cresp.Body.Close()

	if got := getUploadSessions(t, url); len(got.Sessions) != 0 {
		t.Fatalf("complete 后预期空列表，实际: %+v", got.Sessions)
	}
}
