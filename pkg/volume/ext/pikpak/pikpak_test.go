// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// --- CLI 替身 ---

// fakeCLIBin 创建假 pikpak 可执行：把 args 原样回显到 stdout（-F json 时输出指定 JSON）。
// 用 sh 脚本：$1 $2... 打印；测试里通过 env FAKE_OUT 指定 auth status 的返回。
func fakeCLIBin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pikpak")
	script := `#!/bin/sh
if [ "$1" = "auth" ] && [ "$2" = "status" ]; then
  echo '{"logged_in":true,"user_id":"u1","name":"t","email":""}'
  exit 0
fi
if [ "$1" = "auth" ] && [ "$2" = "token" ]; then
  exit 0
fi
echo "unexpected: $*"
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// --- API fake：httptest 模拟 drive/v1 ---

// TestPikpakDownloader_Download 端到端：分享 → 转存 → 定位 → CLI 下载。
func TestPikpakDownloader_Download(t *testing.T) {
	t.Parallel()
	share := []FileMeta{
		{ID: "share-img-1", Name: "cover.jpg", Kind: "drive#file", Size: 1024, MimeType: "image/jpeg"},
		{ID: "share-vid-1", Name: "SAMPLE-123-full.mp4", Kind: "drive#file", Size: 1000000, MimeType: "video/mp4"},
	}
	fsrv := newFakeServer(share, "https://dl.example.com/download?fid=x")
	defer fsrv.Close()

	cli, err := NewCli(CliConfig{BinaryPath: fakeCLIBin(t), HTTPClient: fsrv.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	api := NewAPI(APIConfig{Host: fsrv.srv.URL, HTTPClient: fsrv.srv.Client()}, cli)

	dl, err := NewPikpakDownloader(DownloaderConfig{
		Cli: cli, API: api, DownloadDir: t.TempDir(), Timeout: 5 * time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}

	if !dl.Supports("https://mypikpak.com/s/abc123/xyz") {
		t.Fatal("expected Supports true for mypikpak share")
	}
	if dl.Supports("https://example.com/file.txt") {
		t.Fatal("expected Supports false for non-pikpak")
	}

	// Download 会调 CLI 下载（fake bin 无法真正下载，我们只验证链路到「调用 CLI」这步）
	dest := filepath.Join(t.TempDir(), "out.mp4")
	// 注：fake CLI 对 download 命令返回非零 → Download 报错；用 err != nil 验证走到了 CLI 调用
	_, err = dl.Download(context.Background(), "https://mypikpak.com/s/abc123/xyz", dest, nil)
	if err == nil {
		t.Fatal("expected error from fake cli download (fake bin returns non-zero)")
	}
	// 但在此之前应已完成：share 解析 + restore（网盘里有转存文件）
	if len(fsrv.driveFiles) != 1 {
		t.Fatalf("expected 1 drive file after restore, got %d", len(fsrv.driveFiles))
	}
	if fsrv.driveFiles[0].ID != "share-vid-1" {
		t.Fatalf("expected restored file share-vid-1, got %q", fsrv.driveFiles[0].ID)
	}
}

// fakeServer 模拟 PikPak drive/v1 API（分享详情/转存/列表/删除/直链）。
type fakeServer struct {
	shareFiles  []FileMeta
	driveFiles  []FileMeta
	downloadURL string
	deleted     []string
	srv         *httptest.Server
}

func newFakeServer(share []FileMeta, dlURL string) *fakeServer {
	fs := &fakeServer{shareFiles: share, downloadURL: dlURL}
	fs.srv = httptest.NewServer(http.HandlerFunc(fs.handle))
	return fs
}

func (f *fakeServer) Close() { f.srv.Close() }

func (f *fakeServer) handle(w http.ResponseWriter, r *http.Request) {
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	dec := func(v any) error { return json.NewDecoder(r.Body).Decode(v) }

	switch {
	case r.URL.Path == "/drive/v1/share/detail":
		writeJSON(map[string]any{"files": f.shareFiles, "next_page_token": ""})
	case r.URL.Path == "/drive/v1/share/restore" && r.Method == http.MethodPost:
		var body struct {
			FileIDs []string `json:"file_ids"`
		}
		_ = dec(&body)
		for _, fid := range body.FileIDs {
			for _, sf := range f.shareFiles {
				if sf.ID == fid {
					df := sf
					df.ParentID = ""
					f.driveFiles = append(f.driveFiles, df)
				}
			}
		}
		writeJSON(map[string]any{"restore_status": "RESTORE_START", "file_id": "restored-1"})
	case r.URL.Path == "/drive/v1/files" && r.Method == http.MethodGet:
		writeJSON(map[string]any{"files": f.driveFiles})
	case strings.HasPrefix(r.URL.Path, "/drive/v1/files/") && r.Method == http.MethodGet:
		writeJSON(map[string]any{"web_content_link": f.downloadURL})
	case r.URL.Path == "/drive/v1/files:batchTrash" && r.Method == http.MethodPost:
		var body struct {
			IDs []string `json:"ids"`
		}
		_ = dec(&body)
		f.deleted = append(f.deleted, body.IDs...)
		writeJSON(map[string]any{"ok": true})
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}
