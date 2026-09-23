// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// trash_test.go 验证回收站 HTTP 端点（roadmap P2 回收站）：
//  1. 软删（POST /delete?soft=true）→ 文件消失 + trash 列表含条目。
//  2. 恢复（POST /api/trash/restore）→ 文件回 user/ 原路径。

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// TestTrash_Endpoints 软删 → 列表 → 恢复 全流程。
func TestTrash_Endpoints(t *testing.T) {
	t.Parallel()
	url, _, _ := newTestServerCreds(t, nil)
	cl := &http.Client{Transport: netutil.IsolatedTransport()}

	// 上传文件。
	body := []byte("trash endpoint content")
	st := uploadFileSigned(t, url, "trash.txt", body)
	if st != 200 {
		t.Fatalf("upload = %d", st)
	}

	// 软删（POST /delete?filename=trash.txt&soft=true）。
	req, _ := http.NewRequest(http.MethodPost, url+"/delete?filename=trash.txt&soft=true", nil)
	req.Header.Set("X-File-Checksum", sha256hex(body))
	signRequest(req, testAccessKey, testAccessSecret)
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatalf("soft delete: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("soft delete = %d", resp.StatusCode)
	}

	// 列表含条目。
	req2, _ := http.NewRequest(http.MethodGet, url+"/api/trash", nil)
	signRequest(req2, testAccessKey, testAccessSecret)
	resp2, err := cl.Do(req2)
	if err != nil {
		t.Fatalf("list trash: %v", err)
	}
	body2, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 {
		t.Fatalf("list = %d", resp2.StatusCode)
	}
	var listed struct {
		Entries []struct {
			TrashRel string `json:"trash_rel"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(body2, &listed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(listed.Entries) != 1 {
		t.Fatalf("trash 应 1 条, got %d: %s", len(listed.Entries), body2)
	}
	trashRel := listed.Entries[0].TrashRel

	// 恢复。
	req3, _ := http.NewRequest(http.MethodPost, url+"/api/trash/restore?file="+trashRel, nil)
	signRequest(req3, testAccessKey, testAccessSecret)
	resp3, rerr := cl.Do(req3)
	if rerr != nil {
		t.Fatalf("restore: %v", rerr)
	}
	io.Copy(io.Discard, resp3.Body)
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("restore = %d", resp3.StatusCode)
	}

	// 原路径可下载。
	req4, _ := http.NewRequest(http.MethodGet, url+"/download?filename=trash.txt", nil)
	signRequest(req4, testAccessKey, testAccessSecret)
	resp4, derr := cl.Do(req4)
	if derr != nil {
		t.Fatalf("download: %v", derr)
	}
	body4, _ := io.ReadAll(resp4.Body)
	resp4.Body.Close()
	if resp4.StatusCode != 200 || string(body4) != "trash endpoint content" {
		t.Fatalf("恢复后下载: status=%d body=%q", resp4.StatusCode, body4)
	}
}

var _ = strings.NewReader
