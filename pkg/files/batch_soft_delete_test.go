// Copyright (C) 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil"
)

// countFilesUnder 统计目录下普通文件数（回收站条目 = 文件，不含目录）。
func countFilesUnder(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}

// TestBatchDelete_SupportsSoft 锁「批量删除也能软删」（**实测缺陷 2026-11-26**）：
//
// 单条 `POST /delete?...&soft=true` 会移入回收站；而批量 `POST /api/batch/delete`
// **没有任何软删入口**（请求体只有 filename/checksum）⇒ 同一用户在同一界面点「删除」
// 单条可恢复、批量不可恢复。Web UI 的「删除所选」正是走批量族 ⇒ 违反「删除必须可恢复」。
//
// 修复：批量请求体新增 `soft`（bool，缺省 false 保持零回归），逐条透传给 DeleteFile。
func TestBatchDelete_SupportsSoft(t *testing.T) {
	t.Parallel()
	env := newDirsEnv(t)
	env.enableWriteDefaults()
	// 与既有 TestService_BatchDelete_MixedResults 同法写文件（env.root 布局）。
	writeUserFile(t, env, "alice", "user/a.txt", "aaa")
	writeUserFile(t, env, "alice", "user/b.txt", "bbb")
	sumA := testutil.SHA256Hex([]byte("aaa"))
	sumB := testutil.SHA256Hex([]byte("bbb"))

	// 必须带 actor（X-Test-Actor）：否则 owner 归一为 anonymous ⇒ 看不到 alice 的文件（幂等缺失）。
	softTrue := true
	req := postJSONReq(t, "alice", "/api/batch/delete", BatchDeleteRequest{
		Soft: &softTrue,
		Files: []BatchDeleteFile{
			{Filename: "a.txt", Checksum: sumA},
			{Filename: "b.txt", Checksum: sumB},
		},
	})
	rec := httptest.NewRecorder()
	env.svc.BatchDelete(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d body=%s", rec.Code, rec.Body.String())
	}
	var resp BatchResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("解析响应: %v", err)
	}
	if len(resp.Results) != 2 || !resp.Results[0].Success || !resp.Results[1].Success {
		t.Fatalf("批量结果 = %+v", resp.Results)
	}

	// 关键：文件应进入回收站（trash 桶有条目），而不是被物理删除。
	trashRoots, terr := env.svc.TrashRoots("alice", "")
	if terr != nil {
		t.Fatalf("TrashRoots: %v", terr)
	}
	entries := 0
	for _, vr := range trashRoots {
		abs, ok := vr.Root.Abs("trash")
		if !ok {
			continue
		}
		entries += countFilesUnder(t, abs)
	}
	if entries < 2 {
		// 诊断：列出结果与 trash 目录内容。
		vor, _ := env.svc.TrashRoots("alice", "")
		for _, vr := range vor {
			if abs, ok := vr.Root.Abs("trash"); ok {
				t.Logf("trash dir %s:", abs)
				_ = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
					if err == nil {
						t.Logf("  %s dir=%v", p, d.IsDir())
					}
					return nil
				})
			}
		}
		t.Fatalf("批量软删后回收站条目 = %d，期望 ≥2（未走软删？ results=%+v）", entries, resp.Results)
	}
}
