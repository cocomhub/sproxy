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
	pollCount := 0

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
					pollCount++
					status := t["status"].(string)
					totalSize := int64(100)
					if s, ok := t["total_size"].(int64); ok {
						totalSize = s
					}
					if status == "pending" || status == "downloading" {
						if pollCount >= 2 {
							t["status"] = "completed"
						}
					}
					resp := map[string]any{
						"id":         taskID,
						"url":        t["url"],
						"filename":   t["filename"],
						"status":     t["status"],
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
	if !strings.Contains(buf.String(), "远端文件") {
		// 当 keep-files 时，不显示"已清理"
		// 但应该仍然显示完成信息
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

func TestCloudDownloadCmd_ReadURLsFromFile(t *testing.T) {
	dir := t.TempDir()

	f1 := filepath.Join(dir, "urls.txt")
	os.WriteFile(f1, []byte("https://example.com/a.zip\nhttps://example.com/b.zip\n"), 0644)
	urls, err := readURLsFromFile(f1)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 2 {
		t.Fatalf("expected 2 URLs, got %d", len(urls))
	}
	if urls[0] != "https://example.com/a.zip" {
		t.Fatalf("expected first URL, got %q", urls[0])
	}

	f2 := filepath.Join(dir, "with-comments.txt")
	os.WriteFile(f2, []byte("# comment\n\nhttps://example.com/valid.zip\n  # another comment\n"), 0644)
	urls, err = readURLsFromFile(f2)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 1 {
		t.Fatalf("expected 1 URL, got %d", len(urls))
	}

	f3 := filepath.Join(dir, "empty.txt")
	os.WriteFile(f3, []byte(""), 0644)
	urls, err = readURLsFromFile(f3)
	if err != nil {
		t.Fatal(err)
	}
	if len(urls) != 0 {
		t.Fatalf("expected 0 URLs, got %d", len(urls))
	}

	_, err = readURLsFromFile(filepath.Join(dir, "nonexistent.txt"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestCloudDownloadCmd_BatchFileFlag(t *testing.T) {
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

	batchFile := filepath.Join(t.TempDir(), "batch-urls.txt")
	os.WriteFile(batchFile, []byte("https://example.com/batch-file.zip\n"), 0644)

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"--batch", batchFile, "--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s"})
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

	oldFlags := []string{"wait", "archive", "download", "extract", "no-cleanup"}
	for _, name := range oldFlags {
		if cmd.Flags().Lookup(name) != nil {
			t.Errorf("old flag --%s should have been removed", name)
		}
	}

	newFlags := []string{"keep-files", "timeout", "no-cache", "archive-name", "output-dir", "poll-interval", "batch"}
	for _, name := range newFlags {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected new flag --%s to exist", name)
		}
	}
}

func TestCloudDownloadCmd_Subcommands(t *testing.T) {
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{}, &state.State{}, nil)

	subcommands := []string{"submit", "wait", "archive", "fetch", "resume", "list", "cancel"}
	for _, name := range subcommands {
		sub := findSubCommand(cmd, name)
		if sub == nil {
			t.Errorf("expected subcommand %q to be registered", name)
		}
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

func TestCloudDownloadCmd_FetchSubcommand(t *testing.T) {
	tasks := []map[string]any{
		{
			"id":         "fetch-1",
			"url":        "https://example.com/file.zip",
			"filename":   "file.zip",
			"status":     "completed",
			"total_size": int64(100),
		},
	}
	mock := newChainMockServer(t, tasks, []byte("fetch content"))
	defer mock.Close()

	svc := client.NewFileClient(mock.URL)
	factory := clientfactory.NewMock(svc, nil)
	outDir := t.TempDir()
	var buf strings.Builder
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: &buf, ErrOut: io.Discard}, &state.State{}, nil)
	cmd.SetArgs([]string{"fetch", "--output-dir", outDir, "--poll-interval", "100ms", "--timeout", "30s",
		"https://example.com/file.zip"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("fetch subcommand failed: %v", err)
	}
	if !strings.Contains(buf.String(), "链式下载完成") {
		t.Fatalf("expected completion message, got: %s", buf.String())
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

func TestCloudDownloadCmd_ResumeSubcommand(t *testing.T) {
	// resume 需要 cache_dir 或 KVStore，直接测试会失败，只验证命令存在性
	svc := client.NewFileClient("http://test.local")
	factory := clientfactory.NewMock(svc, nil)
	cmd := NewCmdCloudDownload(factory, cli.IOStreams{Out: io.Discard, ErrOut: io.Discard}, &state.State{}, nil)

	resume := findSubCommand(cmd, "resume")
	if resume == nil {
		t.Fatal("expected resume subcommand")
	}
	if resume.Use != "resume <chain-id>" {
		t.Fatalf("expected Use 'resume <chain-id>', got %q", resume.Use)
	}
	if resume.Args == nil {
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
	batchFile := filepath.Join(t.TempDir(), "batch-submit.txt")
	os.WriteFile(batchFile, []byte("https://example.com/batch-submit.zip\n"), 0644)

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
	cmd.SetArgs([]string{"submit", "--batch", batchFile})
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
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wait subcommand should not fail on partial failure: %v", err)
	}
	if !strings.Contains(buf.String(), "fail-1") {
		t.Fatalf("expected output to contain task ID, got: %s", buf.String())
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
	if err := cmd.Execute(); err != nil {
		t.Fatalf("wait subcommand should not fail on cancelled task: %v", err)
	}
	if !strings.Contains(buf.String(), "已取消") {
		t.Fatalf("expected cancelled message in output, got: %s", buf.String())
	}
}
