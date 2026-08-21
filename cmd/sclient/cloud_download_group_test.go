// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
)

func TestCloudDownloadGroupCmd_Use(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{}, nil)
	if cmd.Use != "cloud-download-group <name> <url> [url...]" {
		t.Fatalf("expected Use 'cloud-download-group <name> <url> [url...]', got %q", cmd.Use)
	}
}

func TestCloudDownloadGroupCmd_Subcommands(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{}, nil)

	subcommands := []string{"submit", "wait", "download", "download-archive", "list", "archive", "cancel", "delete", "resume-chain", "resume-download"}
	for _, name := range subcommands {
		sub := findSubCommand(cmd, name)
		if sub == nil {
			t.Errorf("expected subcommand %q to be registered under cloud-download-group", name)
		}
	}
}

func TestCloudDownloadGroupCmd_Submit(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups" && r.Method == http.MethodPost {
			var req struct {
				Name string              `json:"name"`
				URLs []map[string]string `json:"urls"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			group := map[string]any{
				"id":          "group-1",
				"name":        req.Name,
				"status":      "pending",
				"total_tasks": len(req.URLs),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(group)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"submit", "g1", "https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "group-1") {
		t.Fatalf("expected group ID in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "-> a.zip") || !strings.Contains(buf.String(), "-> b.zip") {
		t.Fatalf("expected plan output to show auto filenames, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_SubmitWithUrlFile(t *testing.T) {
	var gotURLs []map[string]string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups" && r.Method == http.MethodPost {
			var req struct {
				Name string              `json:"name"`
				URLs []map[string]string `json:"urls"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			gotURLs = req.URLs
			group := map[string]any{"id": "group-1", "name": req.Name, "status": "pending", "total_tasks": len(req.URLs)}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(group)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	dir := t.TempDir()
	urlFile := filepath.Join(dir, "entries.txt")
	if err := os.WriteFile(urlFile, []byte("https://example.com/a.zip\tcustom.zip\nhttps://example.com/b/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"submit", "g1", "--url-file", urlFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit subcommand with url-file failed: %v", err)
	}
	if len(gotURLs) != 2 {
		t.Fatalf("want 2 URLs sent, got %d", len(gotURLs))
	}
	if gotURLs[0]["filename"] != "custom.zip" {
		t.Fatalf("want first entry filename custom.zip, got %q", gotURLs[0]["filename"])
	}
	// 第二条自动生成 index.html（目录结尾）
	if gotURLs[1]["filename"] != "" {
		t.Fatalf("want second entry empty filename (server auto-generates), got %q", gotURLs[1]["filename"])
	}
	if !strings.Contains(buf.String(), "-> index.html") {
		t.Fatalf("expected plan output to show index.html, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_PreflightConflict(t *testing.T) {
	// 两个目录结尾 URL 自动生成相同文件名 index.html → 客户端预校验应拦截，服务端不应被调用。
	serverHit := false
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serverHit = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"submit", "g1", "https://example.com/a/", "https://example.com/b/"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected conflict error from preflight, got nil")
	}
	if !strings.Contains(err.Error(), "冲突") {
		t.Fatalf("expected conflict error, got: %v", err)
	}
	if serverHit {
		t.Fatal("server should not be called when preflight detects filename conflict")
	}
	if !strings.Contains(buf.String(), "-> index.html") {
		t.Fatalf("expected plan output to show index.html filenames, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_List(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups" && r.Method == http.MethodGet {
			groups := []map[string]any{
				{"id": "group-1", "name": "g1", "status": "completed", "total_tasks": 2, "completed": 2},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"groups": groups, "total": len(groups)})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "group-1") {
		t.Fatalf("expected group-1 in output, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_ListEmpty(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"groups": []map[string]any{}, "total": 0})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("list subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "暂无下载组") {
		t.Fatalf("expected '暂无下载组' in output, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_Cancel(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/groups/") && strings.HasSuffix(r.URL.Path, "/cancel") && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "cancelled"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"cancel", "group-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cancel subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Fatalf("expected cancelled message, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_Delete(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/groups/") && r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "deleted"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"delete", "group-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "已删除") {
		t.Fatalf("expected deleted message, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_Archive(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/groups/") && strings.HasSuffix(r.URL.Path, "/archive") && r.Method == http.MethodPost {
			result := map[string]any{
				"success": true,
				"file":    "group-archive.tar.gz",
				"size":    200,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"archive", "group-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "group-archive.tar.gz") {
		t.Fatalf("expected archive name in output, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_Resume(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/groups/") && strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPost {
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"status": "resumed"})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)

	resume := findSubCommand(cmd, "resume-download")
	if resume == nil {
		t.Fatal("expected resume-download subcommand")
	}
	if resume.Use != "resume-download <group-id>" {
		t.Fatalf("expected Use 'resume-download <group-id>', got %q", resume.Use)
	}
	// 验证命令能实际执行
	var buf strings.Builder
	cmdResume := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmdResume.SetArgs([]string{"resume-download", "group-1"})
	if err := cmdResume.Execute(); err != nil {
		t.Fatalf("resume-download subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "恢复成功") {
		t.Fatalf("expected resume success message, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_DownloadSubcommand(t *testing.T) {
	// mock group detail + download endpoint
	var calledGroup, calledDownload bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups/group-dl" && r.Method == http.MethodGet {
			calledGroup = true
			detail := map[string]any{
				"group": map[string]any{
					"id":          "group-dl",
					"name":        "dl-group",
					"status":      "completed",
					"total_tasks": 1,
				},
				"tasks": []map[string]any{
					{"id": "task-1", "status": "completed", "filename": "file.zip"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
			return
		}
		if r.URL.Path == "/download" && r.Method == http.MethodGet {
			calledDownload = true
			if r.URL.Query().Get("filename") != ".__cloud__/task-1/file.zip" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Write([]byte("file content"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"download", "group-dl", "task-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("download subcommand failed: %v", err)
	}
	if !calledGroup {
		t.Fatal("expected CloudGetGroup call")
	}
	if !calledDownload {
		t.Fatal("expected Download call")
	}
}

func TestCloudDownloadGroupCmd_DownloadSubcommandNotCompleted(t *testing.T) {
	// mock group detail 返回 downloading 状态子任务，download 不应被调用
	var calledDownload bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups/group-dl-pending" && r.Method == http.MethodGet {
			detail := map[string]any{
				"group": map[string]any{
					"id":          "group-dl-pending",
					"name":        "pending-group",
					"status":      "downloading",
					"total_tasks": 1,
				},
				"tasks": []map[string]any{
					{"id": "task-pending", "status": "downloading", "filename": "file.zip"},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
			return
		}
		if r.URL.Path == "/download" && r.Method == http.MethodGet {
			calledDownload = true
			w.Write([]byte("should not be called"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"download", "group-dl-pending", "task-pending"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for not-completed subtask")
	}
	if !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("expected '未完成' in error, got: %v", err)
	}
	if calledDownload {
		t.Fatal("download should not be called for not-completed subtask")
	}
}

func TestCloudDownloadGroupCmd_DownloadArchiveSubcommand(t *testing.T) {
	var calledDownload bool
	archiveContent := []byte("archive content")
	archiveChecksum := sha256Hex(archiveContent)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" && r.Method == http.MethodGet {
			calledDownload = true
			if r.URL.Query().Get("filename") != "archive.tar.gz" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-File-Checksum", archiveChecksum)
			w.Write(archiveContent)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"download-archive", "archive.tar.gz"})
	// 切到临时目录为工作目录：下载命令默认输出到当前目录，避免落到真实工作目录。
	// 必须在 Execute 之前切换（Execute 内部的 DownloadItems 默认写到 cwd）。
	outDir := t.TempDir()
	origWd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(origWd) }()
	if err = os.Chdir(outDir); err != nil {
		t.Fatal(err)
	}
	if err = cmd.Execute(); err != nil {
		t.Fatalf("download-archive subcommand failed: %v", err)
	}
	if !calledDownload {
		t.Fatal("expected Download call")
	}
	got, err := os.ReadFile(filepath.Join(outDir, "archive.tar.gz"))
	if err != nil {
		t.Fatalf("expected downloaded archive on disk: %v", err)
	}
	if !bytes.Equal(got, archiveContent) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", got, archiveContent)
	}
}

func TestCloudDownloadGroupCmd_ResumeChainSubcommand(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)

	resumeChain := findSubCommand(cmd, "resume-chain")
	if resumeChain == nil {
		t.Fatal("expected resume-chain subcommand")
	}
	if resumeChain.Use != "resume-chain <chain-id>" {
		t.Fatalf("expected Use 'resume-chain <chain-id>', got %q", resumeChain.Use)
	}
	if resumeChain.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

func TestCloudDownloadGroupCmd_Wait(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/groups/group-wait" && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "completed"
			}
			detail := map[string]any{
				"group": map[string]any{
					"id":          "group-wait",
					"name":        "wait-group",
					"status":      status,
					"total_tasks": 2,
					"completed":   2,
				},
				"tasks": []map[string]any{
					{"id": "t-1", "status": status},
					{"id": "t-2", "status": status},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(detail)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"wait", "--poll-interval", "50ms", "--timeout", "5s", "group-wait"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wait subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "completed") {
		t.Fatalf("expected completed in output, got: %s", buf.String())
	}
}

func TestCloudDownloadGroupCmd_NoURLs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownloadGroup(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, nil)
	cmd.SetArgs([]string{"submit", "g1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when no URLs provided")
	}
}
