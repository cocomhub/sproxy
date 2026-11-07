// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// state_ledger_files_test.go 钉住「集群后端（StateStore）台账经 files.Service 可见」（2026-10-30）：
// `filesRuntime.ChecksumStoreFor` 曾只接受 `*checksum.ChecksumStore`（具体类型），集群后端被断言
// 失败后返回 nil ⇒ list/search 的 checksum **恒空**、`RebuildChecksums` 报未装配。

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/state"
)

// TestFilesService_StateBackedChecksumVisible：集群后端下上传后，files.Service.List 能读到
// checksum（此前恒空）。
//
// 变异：filesRuntime.ChecksumStoreFor 改回「类型断言具体类型、失败返回 nil」⇒ 本用例红。
func TestFilesService_StateBackedChecksumVisible(t *testing.T) {
	t.Parallel()
	h := newAssemblyTestHandlers(t, t.TempDir())
	h.SetStateStore(state.NewLocalStateStore(filepath.Join(t.TempDir(), "state"), testLogger()))
	ts := httptest.NewServer(actorUploadMux(h, "alice"))
	t.Cleanup(ts.Close)

	status, _, body := volumeUpload(t, ts.URL, "a.txt", []byte("payload"), "")
	if status != http.StatusOK {
		t.Fatalf("上传应 200, got %d %s", status, body)
	}
	// 台账写经合并窗口（≤1s）⇒ 断言前显式 flush。
	if f, ok := h.checksumStoreFor("alice").(checksum.Flusher); ok {
		if err := f.Flush(); err != nil {
			t.Fatalf("flush: %v", err)
		}
	}
	svc := h.fileService()
	if svc == nil {
		t.Fatal("files.Service 未装配")
	}
	res, err := svc.List(t.Context(), files.ListQuery{Owner: "alice"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(res.Files) != 1 {
		t.Fatalf("应列出 1 个文件, got %+v", res.Files)
	}
	if res.Files[0].Checksum == "" {
		t.Fatal("集群后端下 list 的 checksum 不应为空（ChecksumStoreFor 曾返回 nil）")
	}
}
