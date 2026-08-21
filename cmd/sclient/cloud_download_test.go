// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/cmd/sclient/internal/clientfactory"
	"github.com/cocomhub/sproxy/cmd/sclient/internal/state"
	"github.com/cocomhub/sproxy/pkg/cli"
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// sha256Hex 计算数据的 SHA-256 十六进制字符串。
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

func TestCloudDownloadCmd_UseAndArgs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)
	if cmd.Use != "cloud-download <url> [url...]" {
		t.Fatalf("expected Use 'cloud-download <url> [url...]', got %q", cmd.Use)
	}
	if cmd.Args == nil {
		t.Fatal("expected Args to be set")
	}
}

// newChainMockServer 创建一个模拟完整链式操作的服务端。
// tasks 是要返回的任务列表，archiveFile 是归档文件内容。
func newChainMockServer(t *testing.T, tasks []map[string]any, archiveFile []byte) *httptest.Server {
	t.Helper()
	// 按任务 ID 独立计数轮询次数（避免多任务并发轮询时共享单个计数器导致
	// 某个任务的早期轮询提前把所有任务置为 completed）。用 mutex 保护：
	// pollAllTasks 对多个任务并发 GET，直接 map 读写是数据竞争。
	var pollMu sync.Mutex
	pollCounts := make(map[string]int)

	// 计算归档文件的真实 SHA-256 校验和
	archiveChecksum := sha256Hex(archiveFile)

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost:
			resp := map[string]any{"tasks": tasks}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)

		case strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") && r.Method == http.MethodGet:
			// 提取任务 ID
			taskID := strings.TrimPrefix(r.URL.Path, "/api/cloud/tasks/")
			// 找到对应的任务
			for _, t := range tasks {
				if t["id"] == taskID {
					// 从原始 map 读字段（只读，不修改 tasks——并发 GET 下写会 data race）
					status := t["status"].(string)
					totalSize := int64(100)
					if s, ok := t["total_size"].(int64); ok {
						totalSize = s
					}
					pollMu.Lock()
					pollCounts[taskID]++
					if (status == "pending" || status == "downloading") && pollCounts[taskID] >= 2 {
						status = "completed"
					}
					pollMu.Unlock()
					resp := map[string]any{
						"id":         taskID,
						"url":        t["url"],
						"filename":   t["filename"],
						"status":     status,
						"total_size": totalSize,
					}
					w.Header().Set("Content-Type", "application/json")
					json.NewEncoder(w).Encode(resp)
					return
				}
			}
			w.WriteHeader(http.StatusNotFound)

		case r.URL.Path == "/api/cloud/archive" && r.Method == http.MethodPost:
			result := map[string]any{
				"success": true,
				"file":    "archive.tar.gz",
				"size":    len(archiveFile),
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result)

		case strings.HasPrefix(r.URL.Path, "/api/files/stat") && r.Method == http.MethodHead:
			w.Header().Set("X-File-Size", fmt.Sprintf("%d", len(archiveFile)))
			w.Header().Set("X-File-Checksum", archiveChecksum)
			w.WriteHeader(http.StatusOK)

		case strings.HasPrefix(r.URL.Path, "/download") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write(archiveFile)

		case strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCloudDownloadCmd_ChainOperation(t *testing.T) {
	tasks := []map[string]any{
		{
			"id":         "chain-1",
			"url":        "https://example.com/file.zip",
			"filename":   "file.zip",
			"status":     "completed",
			"total_size": int64(100),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("archive content"))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s",
		"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message in output, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "本地路径") {
		t.Fatalf("expected local path in output, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_KeepFiles(t *testing.T) {
	tasks := []map[string]any{
		{
			"id":         "keep-1",
			"url":        "https://example.com/file.zip",
			"filename":   "file.zip",
			"status":     "completed",
			"total_size": int64(100),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("keep content"))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--output-dir", outDir, "--keep-files", "--poll-interval", "100ms", "--timeout", "30s",
		"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message, got: %s", buf.String())
	}
	if strings.Contains(buf.String(), "远端文件") {
		t.Error("expected no remote cleanup message when keep-files is set")
	}
}

func TestCloudDownloadCmd_NoURLs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no URLs provided")
	}
}

func TestCloudDownloadCmd_ReadEntriesFromFile(t *testing.T) {
	dir := t.TempDir()

	f1 := filepath.Join(dir, "urls.txt")
	os.WriteFile(f1, []byte("https://example.com/a.zip\nhttps://example.com/b.zip\n"), 0644)
	entries, err := readEntriesFromFile(f1)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}
	if entries[0].URL != "https://example.com/a.zip" {
		t.Fatalf("expected first URL, got %q", entries[0].URL)
	}

	f2 := filepath.Join(dir, "with-comments.txt")
	os.WriteFile(f2, []byte("# comment\n\nhttps://example.com/valid.zip\n  # another comment\n"), 0644)
	entries, err = readEntriesFromFile(f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	f3 := filepath.Join(dir, "empty.txt")
	os.WriteFile(f3, []byte(""), 0644)
	entries, err = readEntriesFromFile(f3)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 entries, got %d", len(entries))
	}

	_, err = readEntriesFromFile(filepath.Join(dir, "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCloudDownloadCmd_BatchFileFlag(t *testing.T) {
	// --batch 已随 readURLsFromFile 一同移除，改用 --url-file 验证等价行为。
	tasks := []map[string]any{
		{
			"id":         "batch-chain-1",
			"url":        "https://example.com/batch-file.zip",
			"filename":   "batch-file.zip",
			"status":     "completed",
			"total_size": int64(100),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("batch content"))
	defer mock.Close()

	urlFile := filepath.Join(t.TempDir(), "batch-urls.txt")
	os.WriteFile(urlFile, []byte("https://example.com/batch-file.zip\n"), 0644)

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--url-file", urlFile, "--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_NewFlags(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	oldFlags := []string{"wait", "archive", "download", "extract", "no-cleanup", "no-cache"}
	for _, name := range oldFlags {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("old flag --%s should have been removed", name)
		}
	}

	newFlags := []string{"keep-files", "timeout", "archive-name", "output-dir", "poll-interval", "url-file"}
	for _, name := range newFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected new flag --%s to exist", name)
		}
	}
	if cmd.Flags().Lookup("batch") != nil {
		t.Errorf("old flag --batch should have been removed")
	}
}

func TestCloudDownloadCmd_Subcommands(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	subcommands := []string{"submit", "wait", "archive", "download", "download-archive", "delete", "resume-chain", "resume-download", "list", "cancel"}
	for _, name := range subcommands {
		sub := findSubCommand(cmd, name)
		if sub == nil {
			t.Errorf("expected subcommand %q to be registered", name)
		}
	}
}

func TestCloudDownloadCmd_MigratedGroupStubs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	// 迁移 stub 应存在
	for _, name := range []string{"group", "group-list", "group-archive", "group-cancel", "group-resume"} {
		sub := findSubCommand(cmd, name)
		if sub == nil {
			t.Errorf("expected migrated stub %q to be registered", name)
		}
	}

	// 通过父命令执行 stub：应返回错误并提示迁移
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"group", "g1", "https://example.com/a.zip"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected migrated stub to return error")
	}
	if !strings.Contains(err.Error(), "cloud-download-group") {
		t.Errorf("expected error to mention cloud-download-group, got: %v", err)
	}
}

func findSubCommand(cmd *cobra.Command, name string) *cobra.Command {
	for _, sub := range cmd.Commands() {
		if sub.Name() == name {
			return sub
		}
	}
	return nil
}

func TestCloudDownloadCmd_SubmitSubcommand(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "submit-1",
						"url":        "https://example.com/file.zip",
						"filename":   "file.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"submit", "https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "submit-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitSubcommand(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/wait-1") && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "completed"
			}
			task := map[string]any{
				"id":         "wait-1",
				"url":        "https://example.com/large.zip",
				"filename":   "large.zip",
				"status":     status,
				"total_size": 50 * 1024 * 1024,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"wait", "--poll-interval", "100ms", "--timeout", "30s", "wait-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wait subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "wait-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "完成") {
		t.Fatalf("expected completion message in output, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_FetchSubcommandRemoved(t *testing.T) {
	// fetch 子命令已移除：注册表中不应存在名为 fetch 的子命令
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)

	if sub := findSubCommand(cmd, "fetch"); sub != nil {
		t.Fatalf("expected fetch subcommand to be removed, found it registered with Use %q", sub.Use)
	}
	// 帮助文本也不应再列出 fetch
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("help execution failed: %v", err)
	}
	if strings.Contains(buf.String(), "fetch") {
		t.Fatalf("help text should not mention fetch, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_ArchiveSubcommand(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/archive" && r.Method == http.MethodPost {
			result := map[string]any{
				"success": true,
				"file":    "archive.tar.gz",
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
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"archive", "task-1", "task-2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "archive.tar.gz") {
		t.Fatalf("expected output to contain archive name, got: %s", buf.String())
	}
}

func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	content := []byte("hello world")
	hdr := &tar.Header{
		Name:     "testfile.txt",
		Size:     int64(len(content)),
		Mode:     0644,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	src := filepath.Join(dir, "test.tar.gz")
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, dir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "testfile.txt"))
	if err != nil {
		t.Fatalf("expected extracted file: %v", err)
	}
	if string(data) != "hello world" {
		t.Fatalf("expected 'hello world', got %q", string(data))
	}
}

func TestExtractTarGz_PathTraversal(t *testing.T) {
	dir := t.TempDir()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	hdr := &tar.Header{
		Name:     "../../../etc/passwd",
		Size:     4,
		Mode:     0644,
		Typeflag: tar.TypeReg,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("data")); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	src := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, dir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() == "evil.tar.gz" {
			continue
		}
		t.Fatalf("unexpected file: %s (path traversal may have succeeded)", e.Name())
	}
}

func TestCloudDownloadCmd_ResumeChainSubcommand(t *testing.T) {
	// resume-chain 需要 cache_dir 或 KVStore，直接测试会失败，只验证命令存在性与 Use
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)

	// resume 的迁移 stub 应存在（旧命令名 stub 提示用户使用 resume-chain）
	resumeStub := findSubCommand(cmd, "resume")
	if resumeStub == nil {
		t.Fatal("expected resume migration stub subcommand")
	}
	if resumeStub.Use != "resume <chain-id>" {
		t.Fatalf("expected Use 'resume <chain-id>', got %q", resumeStub.Use)
	}
	// 执行 stub 应返回迁移提示错误
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"resume", "chain-123"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected migration stub to return error")
	}
	if !strings.Contains(err.Error(), "resume-chain") {
		t.Errorf("expected error to mention resume-chain, got: %v", err)
	}
	// resume-chain 是实际实现
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

func TestCloudDownloadCmd_ListSubcommand(t *testing.T) {
	// list 命令已通过 cloud_list 实现，验证子命令存在即可
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	list := findSubCommand(cmd, "list")
	if list == nil {
		t.Fatal("expected list subcommand")
	}
}

func TestCloudDownloadCmd_DownloadSubcommand(t *testing.T) {
	fileContent := []byte("original file content")
	fileChecksum := sha256Hex(fileContent)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/cloud/tasks/task-dl-1" && r.Method == http.MethodGet:
			task := map[string]any{
				"id":       "task-dl-1",
				"url":      "https://example.com/original.zip",
				"filename": "original.zip",
				"status":   "completed",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
		case strings.HasPrefix(r.URL.Path, "/download") && r.Method == http.MethodGet:
			// 校验远端路径为 .__cloud__/<taskID>/<filename>
			got := r.URL.Query().Get("filename")
			if got != ".__cloud__/task-dl-1/original.zip" {
				t.Errorf("expected download filename '.__cloud__/task-dl-1/original.zip', got %q", got)
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("X-File-Checksum", fileChecksum)
			w.Write(fileContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	// t.Chdir 自动恢复原工作目录（Go 1.24+）
	t.Chdir(outDir)

	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"download", "task-dl-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("download subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "全部下载完成") {
		t.Fatalf("expected output to contain completion message, got: %s", buf.String())
	}
	got, err := os.ReadFile(filepath.Join(outDir, "original.zip"))
	if err != nil {
		t.Fatalf("expected downloaded file on disk: %v", err)
	}
	if !bytes.Equal(got, fileContent) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", got, fileContent)
	}
}

func TestCloudDownloadCmd_DownloadSubcommandNotCompleted(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/tasks/task-pending-1" && r.Method == http.MethodGet {
			task := map[string]any{
				"id":       "task-pending-1",
				"url":      "https://example.com/pending.zip",
				"filename": "pending.zip",
				"status":   "downloading",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"download", "task-pending-1"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when task not completed")
	}
	if !strings.Contains(err.Error(), "未完成") {
		t.Fatalf("expected error to mention not completed, got: %v", err)
	}
}

func TestCloudDownloadCmd_DownloadArchiveSubcommand(t *testing.T) {
	archiveContent := []byte("archive content")
	archiveChecksum := sha256Hex(archiveContent)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/download") && r.Method == http.MethodGet {
			got := r.URL.Query().Get("filename")
			if got != ".__cloud_archives__/my-archive.tar.gz" {
				t.Errorf("expected download filename '.__cloud_archives__/my-archive.tar.gz', got %q", got)
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
	outDir := t.TempDir()
	// t.Chdir 自动恢复原工作目录（Go 1.24+）
	t.Chdir(outDir)

	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"download-archive", ".__cloud_archives__/my-archive.tar.gz"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("download-archive subcommand failed: %v", err)
	}
	// 本地保存名为 filepath.Base(archiveFile)
	got, err := os.ReadFile(filepath.Join(outDir, "my-archive.tar.gz"))
	if err != nil {
		t.Fatalf("expected downloaded archive on disk: %v", err)
	}
	if !bytes.Equal(got, archiveContent) {
		t.Fatalf("downloaded content mismatch: got %q, want %q", got, archiveContent)
	}
}

func TestCloudDownloadCmd_DeleteSubcommand(t *testing.T) {
	var gotPath string
	var gotMethod string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") && r.Method == http.MethodDelete {
			gotPath = r.URL.Path
			gotMethod = r.Method
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"delete", "task-del-1", "--yes"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("delete subcommand failed: %v", err)
	}
	if gotPath != "/api/cloud/tasks/task-del-1" {
		t.Fatalf("expected DELETE /api/cloud/tasks/task-del-1, got %s %s", gotMethod, gotPath)
	}
	if !strings.Contains(buf.String(), "task-del-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

// TestCloudDownloadCmd_DeleteRequiresYes 验证未传 --yes 时 delete 拒绝执行且返回非零。
func TestCloudDownloadCmd_DeleteRequiresYes(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var out, errBuf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &out, ErrOut: &errBuf}, &state.State{}, nil)
	cmd.SetArgs([]string{"delete", "task-del-1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("expected error when --yes not provided")
	}
	if !strings.Contains(errBuf.String(), "--yes") {
		t.Fatalf("expected stderr to mention --yes, got: %s", errBuf.String())
	}
}

func TestCloudDownloadCmd_ResumeDownloadSubcommand(t *testing.T) {
	var gotPath string
	var gotBody map[string]bool
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") && strings.HasSuffix(r.URL.Path, "/resume") && r.Method == http.MethodPost {
			gotPath = r.URL.Path
			if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
				t.Fatalf("decode resume body: %v", err)
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"resume-download", "--force", "task-resume-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("resume-download subcommand failed: %v", err)
	}
	if gotPath != "/api/cloud/tasks/task-resume-1/resume" {
		t.Fatalf("expected POST /api/cloud/tasks/task-resume-1/resume, got %s", gotPath)
	}
	if gotBody == nil || !gotBody["force"] {
		t.Fatalf("expected force=true in resume body, got %v", gotBody)
	}
	if !strings.Contains(buf.String(), "task-resume-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_CancelSubcommand(t *testing.T) {
	// cancel 命令已通过 cloud_cancel 实现，验证子命令存在即可
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	cancel := findSubCommand(cmd, "cancel")
	if cancel == nil {
		t.Fatal("expected cancel subcommand")
	}
}

func TestCloudDownloadCmd_SubmitBatchCommand(t *testing.T) {
	urlFile := filepath.Join(t.TempDir(), "batch-submit.txt")
	os.WriteFile(urlFile, []byte("https://example.com/batch-submit.zip\n"), 0644)

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "batch-submit-1",
						"url":        "https://example.com/batch-submit.zip",
						"filename":   "batch-submit.zip",
						"status":     "completed",
						"total_size": 100,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"submit", "--url-file", urlFile})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "batch-submit-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitNoPending(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/done-1") && r.Method == http.MethodGet {
			task := map[string]any{
				"id":         "done-1",
				"url":        "https://example.com/done.zip",
				"filename":   "done.zip",
				"status":     "completed",
				"total_size": 100,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"wait", "done-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wait subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "done-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitTimeout(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/timeout-1") && r.Method == http.MethodGet {
			pollCount++
			task := map[string]any{
				"id":         "timeout-1",
				"url":        "https://example.com/slow.zip",
				"filename":   "slow.zip",
				"status":     "pending",
				"total_size": 50 * 1024 * 1024,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	// 设置 timeout 小于 poll-interval，确保 timeout 先触发
	cmd.SetArgs([]string{"wait", "--timeout", "50ms", "--poll-interval", "1s", "timeout-1"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error for timeout")
	}
}

func TestCloudDownloadCmd_SubmitNoURLs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"submit"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no URLs provided")
	}
}

func TestCloudDownloadCmd_WaitNoArgs(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"wait"})
	err := cmd.Execute()
	if err == nil {
		t.Error("expected error when no task IDs provided")
	}
}

func TestCloudDownloadCmd_ArchiveSubcommandSingle(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/") && strings.HasSuffix(r.URL.Path, "/archive") && r.Method == http.MethodPost {
			result := map[string]any{
				"success": true,
				"file":    "single-archive.tar.gz",
				"size":    100,
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
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"archive", "task-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "single-archive.tar.gz") {
		t.Fatalf("expected output to contain archive name, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_ArchiveSubcommandNoName(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/archive" && r.Method == http.MethodPost {
			result := map[string]any{
				"success": true,
				"file":    "auto-archive.tar.gz",
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
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"archive", "task-1", "task-2"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("archive subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "auto-archive.tar.gz") {
		t.Fatalf("expected output to contain archive name, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_MultipleURLs(t *testing.T) {
	tasks := []map[string]any{
		{
			"id":         "multi-1",
			"url":        "https://example.com/a.zip",
			"filename":   "a.zip",
			"status":     "completed",
			"total_size": int64(100),
		},
		{
			"id":         "multi-2",
			"url":        "https://example.com/b.zip",
			"filename":   "b.zip",
			"status":     "completed",
			"total_size": int64(200),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("multi content"))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s",
		"https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_ChainOperationWithPolling(t *testing.T) {
	tasks := []map[string]any{
		{
			"id":         "poll-chain-1",
			"url":        "https://example.com/large.zip",
			"filename":   "large.zip",
			"status":     "pending",
			"total_size": int64(50 * 1024 * 1024),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("polled content"))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s",
		"https://example.com/large.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("cloud-download command failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_SubmitMultipleURLs(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/cloud/download/batch" && r.Method == http.MethodPost {
			resp := map[string]any{
				"tasks": []map[string]any{
					{
						"id":         "submit-multi-1",
						"url":        "https://example.com/a.zip",
						"filename":   "a.zip",
						"status":     "completed",
						"total_size": 100,
					},
					{
						"id":         "submit-multi-2",
						"url":        "https://example.com/b.zip",
						"filename":   "b.zip",
						"status":     "completed",
						"total_size": 200,
					},
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"submit", "https://example.com/a.zip", "https://example.com/b.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("submit subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "submit-multi-1") {
		t.Fatalf("expected output to contain first task ID, got: %s", buf.String())
	}
	if !strings.Contains(buf.String(), "submit-multi-2") {
		t.Fatalf("expected output to contain second task ID, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitTaskFailed(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/fail-1") && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "failed"
			}
			task := map[string]any{
				"id":       "fail-1",
				"url":      "https://example.com/fail.zip",
				"filename": "fail.zip",
				"status":   status,
				"error":    "connection refused",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"wait", "--poll-interval", "100ms", "--timeout", "30s", "fail-1"})
	err := cmd.Execute()
	// 任务失败时 wait 必须返回非零（与链式 waitForTasks 语义一致），避免脚本误判成功
	if err == nil {
		t.Fatalf("wait subcommand should fail when a task fails, got no error")
	}
	if !strings.Contains(err.Error(), "fail-1") {
		t.Fatalf("expected error to mention task ID, got: %v", err)
	}
	if !strings.Contains(buf.String(), "失败") {
		t.Fatalf("expected failure message in output, got: %s", buf.String())
	}
}

func TestCloudDownloadCmd_WaitTaskCancelled(t *testing.T) {
	pollCount := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/cloud/tasks/cancel-1") && r.Method == http.MethodGet {
			pollCount++
			status := "downloading"
			if pollCount >= 2 {
				status = "cancelled"
			}
			task := map[string]any{
				"id":       "cancel-1",
				"url":      "https://example.com/cancel.zip",
				"filename": "cancel.zip",
				"status":   status,
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(task)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"wait", "--poll-interval", "100ms", "--timeout", "30s", "cancel-1"})
	// cancelled 计入失败（用户确认 cancelled=失败），wait 必须返回非零
	err := cmd.Execute()
	if err == nil {
		t.Fatalf("wait subcommand should fail on cancelled task")
	}
	if !strings.Contains(err.Error(), "cancel-1") {
		t.Fatalf("expected error to mention task ID, got: %v", err)
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Fatalf("expected cancelled message in output, got: %s", buf.String())
	}
}

func TestReadEntriesFromFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "urls.txt")
	content := "# comment line\n" +
		"https://example.com/a.zip\tcustom-a.zip\n" +
		"\n" +
		"https://example.com/b.zip\n" +
		"https://example.com/my%20file.txt\t我的文件.txt\n"
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	entries, err := readEntriesFromFile(path)
	if err != nil {
		t.Fatalf("readEntriesFromFile: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(entries))
	}
	if entries[0].URL != "https://example.com/a.zip" || entries[0].Filename != "custom-a.zip" {
		t.Fatalf("entry[0] = %+v, want url a.zip + filename custom-a.zip", entries[0])
	}
	if entries[1].URL != "https://example.com/b.zip" || entries[1].Filename != "" {
		t.Fatalf("entry[1] = %+v, want url b.zip + empty filename", entries[1])
	}
	if entries[2].Filename != "我的文件.txt" {
		t.Fatalf("entry[2].Filename = %q, want 我的文件.txt", entries[2].Filename)
	}
}

// TestExtractTarGz_PathTraversalPrevented 回归测试：extractTarGz 必须阻止 tar 内
// header.Name 为 ../../ 的路径穿越。旧代码仅对未 Clean 的 Join 结果做前缀检查，
// ../../evil 的 Join 结果仍以 destDir 为前缀，会写出 destDir（任意文件写）。
func TestExtractTarGz_PathTraversalPrevented(t *testing.T) {
	dir := t.TempDir()
	destDir := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(destDir, 0755); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	content := []byte("evil")
	hdr := &tar.Header{Name: "../../evil.txt", Mode: 0644, Size: int64(len(content)), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()

	src := filepath.Join(dir, "evil.tar.gz")
	if err := os.WriteFile(src, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}

	if err := extractTarGz(src, destDir); err != nil {
		t.Fatalf("extractTarGz failed: %v", err)
	}

	// ../../evil.txt 从 destDir 逃逸的目标是 dir/evil.txt，必须被拦截
	if _, err := os.Stat(filepath.Join(dir, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("path traversal escaped destDir")
	}
}
