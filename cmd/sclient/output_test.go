// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

func TestPrintFileList(t *testing.T) {
	tests := []struct {
		name     string
		files    []client.FileInfo
		contains []string // substrings that must appear in output
		not      []string // substrings that must NOT appear
	}{
		{
			name: "regular_files",
			files: []client.FileInfo{
				{Name: "report.pdf", Size: 1024, Checksum: "abc123def456"},
				{Name: "notes.txt", Size: 512, Checksum: "xyz789"},
			},
			contains: []string{"report.pdf", "notes.txt", "1.0 KB", "512 B"},
			not:      []string{"[DIR]"},
		},
		{
			name: "directory_entry",
			files: []client.FileInfo{
				{Name: "mydir", IsDir: true, Size: 0, Checksum: ""},
			},
			contains: []string{"[DIR]", "mydir/"},
		},
		{
			name: "empty_checksum",
			files: []client.FileInfo{
				{Name: "no_checksum.bin", Size: 0, Checksum: ""},
			},
			contains: []string{"no_checksum.bin", "-"},
		},
		{
			name: "truncated_long_checksum",
			files: []client.FileInfo{
				{Name: "long_hash.bin", Size: 999, Checksum: "abcdef1234567890extra"},
			},
			contains: []string{"long_hash.bin", "abcdef1234567890"},
			not:      []string{"extra"},
		},
		{
			name:     "empty_file_list",
			files:    []client.FileInfo{},
			contains: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf strings.Builder
			printFileList(tt.files, &buf)
			output := buf.String()

			for _, want := range tt.contains {
				if !strings.Contains(output, want) {
					t.Errorf("expected output to contain %q, got:\n%s", want, output)
				}
			}
			for _, not := range tt.not {
				if strings.Contains(output, not) {
					t.Errorf("expected output NOT to contain %q, got:\n%s", not, output)
				}
			}
		})
	}
}

// ---- JSONFormatter tests ----

func TestJSONFormatter_PrintFileList(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintFileList(nil)
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, `"total": 0`) {
		t.Fatalf("expected total=0, got %q", output)
	}
}

func TestJSONFormatter_PrintFileList_WithFiles(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintFileList([]client.FileInfo{
		{Name: "test.txt", Size: 100, Checksum: "abc"},
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "test.txt") {
		t.Fatalf("expected output to contain 'test.txt', got %q", output)
	}
	if !strings.Contains(output, "abc") {
		t.Fatalf("expected output to contain 'abc', got %q", output)
	}
}

func TestJSONFormatter_PrintShareList(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintShareList(nil)
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "shares") {
		t.Fatalf("expected output to contain 'shares', got %q", output)
	}
}

func TestJSONFormatter_PrintShareList_WithShares(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintShareList([]*client.ShareLink{
		{Token: "tok123", Filename: "file.txt", Downloads: 5, MaxDownloads: 10},
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
	if !strings.Contains(output, "file.txt") {
		t.Fatalf("expected output to contain 'file.txt', got %q", output)
	}
}

func TestJSONFormatter_PrintStat(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "file.txt", Size: 42, Checksum: "deadbeef"}, "file.txt")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "file.txt") {
		t.Fatalf("expected output to contain 'file.txt', got %q", output)
	}
	if !strings.Contains(output, "deadbeef") {
		t.Fatalf("expected output to contain 'deadbeef', got %q", output)
	}
}

func TestJSONFormatter_PrintStat_Directory(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "mydir", IsDir: true, Size: 0, Checksum: ""}, "mydir")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "directory") {
		t.Fatalf("expected output to contain 'directory', got %q", output)
	}
}

func TestJSONFormatter_PrintShareCreated(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintShareCreated(&client.ShareLink{
		Token: "tok123", Filename: "f.txt", ExpiresAt: "2027-01-01", MaxDownloads: 5, OneTime: false,
	}, "https://example.com/s/tok123")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
	if !strings.Contains(output, "https://example.com/s/tok123") {
		t.Fatalf("expected output to contain share URL, got %q", output)
	}
}

func TestJSONFormatter_PrintShareRevoked(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintShareRevoked("tok123")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "revoked") {
		t.Fatalf("expected output to contain 'revoked', got %q", output)
	}
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
}

func TestJSONFormatter_PrintUpdateResult(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintUpdateResult("log_level", "debug")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "updated") {
		t.Fatalf("expected output to contain 'updated', got %q", output)
	}
	if !strings.Contains(output, "log_level") {
		t.Fatalf("expected output to contain 'log_level', got %q", output)
	}
}

func TestJSONFormatter_PrintStats(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintStats(&client.StatsResponse{
		FilesUploaded:   10,
		FilesDownloaded: 5,
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "files_uploaded") {
		t.Fatalf("expected output to contain 'files_uploaded', got %q", output)
	}
}

func TestJSONFormatter_PrintConfig(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintConfig(&client.ConfigResponse{
		LogLevel:  "info",
		LogFormat: "text",
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "log_level") {
		t.Fatalf("expected output to contain 'log_level', got %q", output)
	}
}

func TestJSONFormatter_PrintCloudTaskList(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintCloudTaskList([]cloudTaskInfo{
		{ID: "task-1", URL: "https://example.com/f.zip", Filename: "f.zip", Status: "completed"},
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "task-1") {
		t.Fatalf("expected output to contain 'task-1', got %q", output)
	}
	if !strings.Contains(output, "tasks") {
		t.Fatalf("expected output to contain 'tasks', got %q", output)
	}
}

func TestJSONFormatter_PrintCloudTaskCancelResult(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintCloudTaskCancelResult("task-1", true, "")
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "task-1") {
		t.Fatalf("expected output to contain 'task-1', got %q", output)
	}
	if !strings.Contains(output, "true") {
		t.Fatalf("expected output to contain 'true', got %q", output)
	}
}

func TestJSONFormatter_PrintVersionList(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintVersionList("test.txt", []client.VersionInfo{
		{VersionID: 1, Size: 100, Filename: "test.txt", CreatedAt: "2026-01-01"},
	})
	output := strings.TrimSpace(buf.String())
	if !strings.Contains(output, "test.txt") {
		t.Fatalf("expected output to contain 'test.txt', got %q", output)
	}
	if !strings.Contains(output, "versions") {
		t.Fatalf("expected output to contain 'versions', got %q", output)
	}
}

func TestJSONFormatter_PrintfAndPrintln(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	// These are no-ops in JSON mode
	fm.Printf("should not appear")
	fm.Println("should not appear too")
	if buf.Len() > 0 {
		t.Fatalf("expected empty output, got %q", buf.String())
	}
}

// ---- TextFormatter tests ----

func TestTextFormatter_PrintShareList(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList(nil)
	output := buf.String()
	if !strings.Contains(output, "暂无分享链接") {
		t.Fatalf("expected '暂无分享链接', got %q", output)
	}
}

func TestTextFormatter_PrintShareList_WithShares(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList([]*client.ShareLink{
		{Token: "tok123", Filename: "file.txt", Downloads: 5, MaxDownloads: 10},
	})
	output := buf.String()
	if !strings.Contains(output, "TOKEN") {
		t.Fatalf("expected header 'TOKEN', got %q", output)
	}
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
}

func TestTextFormatter_PrintShareList_Expired(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList([]*client.ShareLink{
		{Token: "tok-expired", Filename: "old.txt", Downloads: 5, MaxDownloads: 5, Expired: true},
	})
	output := buf.String()
	if !strings.Contains(output, "已过期") {
		t.Fatalf("expected '已过期', got %q", output)
	}
}

func TestTextFormatter_PrintShareList_UnlimitedDownloads(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareList([]*client.ShareLink{
		{Token: "tok-unlim", Filename: "f.txt", Downloads: 3, MaxDownloads: 0},
	})
	output := buf.String()
	if !strings.Contains(output, "∞") {
		t.Fatalf("expected '∞' for unlimited max downloads, got %q", output)
	}
}

func TestTextFormatter_PrintShareList_LongToken(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	longToken := "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF"
	fm.PrintShareList([]*client.ShareLink{
		{Token: longToken, Filename: "f.txt", Downloads: 0, MaxDownloads: 0},
	})
	output := buf.String()
	if strings.Contains(output, longToken) {
		t.Fatalf("expected long token to be truncated, got full token in output")
	}
	if !strings.Contains(output, "...") {
		t.Fatalf("expected truncated token to contain '...', got %q", output)
	}
}

func TestTextFormatter_PrintShareCreated(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareCreated(&client.ShareLink{
		Token: "tok123", Filename: "f.txt", ExpiresAt: "2027-01-01", MaxDownloads: 5, OneTime: true,
	}, "https://example.com/s/tok123")
	output := buf.String()
	if !strings.Contains(output, "分享链接") {
		t.Fatalf("expected '分享链接', got %q", output)
	}
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
}

func TestTextFormatter_PrintShareRevoked(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintShareRevoked("tok123")
	output := buf.String()
	if !strings.Contains(output, "已撤销分享") {
		t.Fatalf("expected '已撤销分享', got %q", output)
	}
	if !strings.Contains(output, "tok123") {
		t.Fatalf("expected output to contain 'tok123', got %q", output)
	}
}

func TestTextFormatter_PrintUpdateResult(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintUpdateResult("log_level", "debug")
	output := buf.String()
	if !strings.Contains(output, "远程配置已更新") {
		t.Fatalf("expected '远程配置已更新', got %q", output)
	}
	if !strings.Contains(output, "log_level") {
		t.Fatalf("expected output to contain 'log_level', got %q", output)
	}
}

func TestTextFormatter_PrintCloudTaskList(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintCloudTaskList(nil)
	output := buf.String()
	if !strings.Contains(output, "暂无云端下载任务") {
		t.Fatalf("expected '暂无云端下载任务', got %q", output)
	}
}

func TestTextFormatter_PrintCloudTaskList_WithTasks(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintCloudTaskList([]cloudTaskInfo{
		{ID: "task-1", URL: "https://example.com/a.zip", Filename: "a.zip", Status: "completed", TotalSize: 1000},
		{ID: "task-2", URL: "https://example.com/b.zip", Filename: "b.zip", Status: "downloading", TotalSize: 5000, Downloaded: 2000},
	})
	output := buf.String()
	if !strings.Contains(output, "任务ID") {
		t.Fatalf("expected header '任务ID', got %q", output)
	}
	if !strings.Contains(output, "task-1") {
		t.Fatalf("expected output to contain 'task-1', got %q", output)
	}
	if !strings.Contains(output, "40%") {
		t.Fatalf("expected output to contain '40%%' progress, got %q", output)
	}
}

func TestTextFormatter_PrintCloudTaskCancelResult(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintCloudTaskCancelResult("task-1", true, "")
	output := buf.String()
	if !strings.Contains(output, "已取消云端下载任务") {
		t.Fatalf("expected '已取消云端下载任务', got %q", output)
	}
}

func TestTextFormatter_PrintCloudTaskCancelResult_Failed(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintCloudTaskCancelResult("task-1", false, "already completed")
	output := buf.String()
	if !strings.Contains(output, "取消云端下载任务失败") {
		t.Fatalf("expected '取消云端下载任务失败', got %q", output)
	}
	if !strings.Contains(output, "already completed") {
		t.Fatalf("expected message 'already completed', got %q", output)
	}
}

func TestTextFormatter_PrintVersionList(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintVersionList("test.txt", nil)
	output := buf.String()
	if !strings.Contains(output, "没有历史版本") {
		t.Fatalf("expected '没有历史版本', got %q", output)
	}
}

func TestTextFormatter_PrintVersionList_WithVersions(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintVersionList("test.txt", []client.VersionInfo{
		{VersionID: 1, Size: 100, Filename: "test.txt", CreatedAt: "2026-01-01T00:00:00Z", Checksum: "abc123def456"},
	})
	output := buf.String()
	if !strings.Contains(output, "版本历史") {
		t.Fatalf("expected '版本历史', got %q", output)
	}
	if !strings.Contains(output, "abc123def456") {
		t.Fatalf("expected checksum 'abc123def456', got %q", output)
	}
}

func TestTextFormatter_PrintStat(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "test.txt", Size: 42, Checksum: "abc123"}, "test.txt")
	output := buf.String()
	if !strings.Contains(output, "test.txt") {
		t.Fatalf("expected 'test.txt', got %q", output)
	}
	if !strings.Contains(output, "42") {
		t.Fatalf("expected size '42', got %q", output)
	}
	if !strings.Contains(output, "abc123") {
		t.Fatalf("expected checksum 'abc123', got %q", output)
	}
}

func TestTextFormatter_PrintStat_Directory(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "mydir", IsDir: true, Size: 0, Checksum: ""}, "mydir")
	output := buf.String()
	if !strings.Contains(output, "directory") {
		t.Fatalf("expected 'directory', got %q", output)
	}
}

func TestTextFormatter_PrintStat_WithModTime(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStat(&client.FileInfo{Name: "f.txt", Size: 10, Checksum: "x", ModTime: 1700000000000000000}, "f.txt")
	output := buf.String()
	if !strings.Contains(output, "mtime") {
		t.Fatalf("expected 'mtime' in output, got %q", output)
	}
}

func TestTextFormatter_Printf(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.Printf("hello %s", "world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("expected 'hello world', got %q", buf.String())
	}
}

func TestTextFormatter_Println(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.Println("hello world")
	if !strings.Contains(buf.String(), "hello world") {
		t.Fatalf("expected 'hello world', got %q", buf.String())
	}
}

// ---- Utility functions ----

func TestBoolStr(t *testing.T) {
	if got := boolStr(true); got != "已设置" {
		t.Fatalf("expected '已设置', got %q", got)
	}
	if got := boolStr(false); got != "未设置" {
		t.Fatalf("expected '未设置', got %q", got)
	}
}

func TestBuildFormatter_Default(t *testing.T) {
	var buf strings.Builder
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	fm := buildFormatterWithWriter(&buf, cmd)
	if _, ok := fm.(*TextFormatter); !ok {
		t.Fatalf("expected TextFormatter, got %T", fm)
	}
}

func TestBuildFormatter_JSON(t *testing.T) {
	var buf strings.Builder
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Set("json", "true")
	fm := buildFormatterWithWriter(&buf, cmd)
	if _, ok := fm.(*JSONFormatter); !ok {
		t.Fatalf("expected JSONFormatter, got %T", fm)
	}
}

func TestBuildFormatterWithWriter_Default(t *testing.T) {
	var buf strings.Builder
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	fm := buildFormatterWithWriter(&buf, cmd)
	if _, ok := fm.(*TextFormatter); !ok {
		t.Fatalf("expected TextFormatter, got %T", fm)
	}
}

func TestBuildFormatterWithWriter_JSON(t *testing.T) {
	var buf strings.Builder
	cmd := &cobra.Command{}
	cmd.Flags().Bool("json", false, "")
	cmd.Flags().Set("json", "true")
	fm := buildFormatterWithWriter(&buf, cmd)
	if _, ok := fm.(*JSONFormatter); !ok {
		t.Fatalf("expected JSONFormatter, got %T", fm)
	}
}

func TestJsonOutputIsValid(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintShareCreated(&client.ShareLink{
		Token: "tok123", Filename: "f.txt", ExpiresAt: "2027-01-01", MaxDownloads: 5, OneTime: false,
	}, "https://example.com/s/tok123")
	output := buf.String()
	if !json.Valid([]byte(output)) {
		t.Fatalf("expected valid JSON, got %q", output)
	}
}

func TestJsonOutput_PrintFileListIsValid(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintFileList([]client.FileInfo{
		{Name: "test.txt", Size: 100, Checksum: "abc"},
	})
	output := buf.String()
	if !json.Valid([]byte(output)) {
		t.Fatalf("expected valid JSON, got %q", output)
	}
}

func TestJsonOutput_PrintStatsIsValid(t *testing.T) {
	var buf strings.Builder
	fm := NewJSONFormatter(&buf)
	fm.PrintStats(&client.StatsResponse{
		DiskUsage: struct {
			UploadsDir string `json:"uploads_dir"`
			TotalFiles int    `json:"total_files"`
			TotalSize  int64  `json:"total_size"`
		}{UploadsDir: "/data", TotalFiles: 10, TotalSize: 1000},
	})
	output := buf.String()
	if !json.Valid([]byte(output)) {
		t.Fatalf("expected valid JSON, got %q", output)
	}
}

func TestTextFormatter_PrintStats(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintStats(&client.StatsResponse{
		DiskUsage: struct {
			UploadsDir string `json:"uploads_dir"`
			TotalFiles int    `json:"total_files"`
			TotalSize  int64  `json:"total_size"`
		}{UploadsDir: "/data", TotalFiles: 10, TotalSize: 1000},
		RequestCounts: struct {
			Total int64 `json:"total"`
			Xx2   int64 `json:"2xx"`
			Xx4   int64 `json:"4xx"`
			Xx5   int64 `json:"5xx"`
		}{Total: 100, Xx2: 80, Xx4: 15, Xx5: 5},
		ActiveConns:     5,
		FilesUploaded:   10,
		BytesUploaded:   5000,
		FilesDownloaded: 20,
		BytesDownloaded: 10000,
		FilesDeleted:    3,
		DiskTotal:       1000000,
		DiskUsed:        500000,
		MaxStorageBytes: 2000000,
		StorageUsage:    500000,
	})
	output := buf.String()
	if !strings.Contains(output, "服务器统计") {
		t.Fatalf("expected '服务器统计', got %q", output)
	}
	if !strings.Contains(output, "50.0%") {
		t.Fatalf("expected disk usage percentage '50.0%%', got %q", output)
	}
}

func TestTextFormatter_PrintConfig(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintConfig(&client.ConfigResponse{
		LogLevel:  "info",
		LogFormat: "text",
	})
	output := buf.String()
	if !strings.Contains(output, "远程服务器配置") {
		t.Fatalf("expected '远程服务器配置', got %q", output)
	}
	if !strings.Contains(output, "info") {
		t.Fatalf("expected 'info', got %q", output)
	}
}

func TestTextFormatter_PrintCloudTaskList_LongFields(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintCloudTaskList([]cloudTaskInfo{
		{
			ID:       "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF",
			URL:      "https://example.com/very/long/path/that/should/be/truncated/file.zip",
			Filename: "very-long-filename-that-should-be-truncated.zip",
			Status:   "downloading",
		},
	})
	output := buf.String()
	if strings.Contains(output, "abcdefghijklmnopqrstuvwxyz1234567890ABCDEF") {
		t.Fatalf("expected long ID to be truncated")
	}
	if !strings.Contains(output, "...") {
		t.Fatalf("expected truncated fields to contain '...', got %q", output)
	}
}

func TestTextFormatter_PrintVersionList_LongChecksum(t *testing.T) {
	var buf strings.Builder
	fm := NewTextFormatter(&buf)
	fm.PrintVersionList("test.txt", []client.VersionInfo{
		{VersionID: 1, Size: 100, CreatedAt: "2026-01-01T00:00:00Z", Checksum: "abcdef1234567890extra"},
	})
	output := buf.String()
	if !strings.Contains(output, "abcdef1234567890") {
		t.Fatalf("expected truncated checksum, got %q", output)
	}
	if strings.Contains(output, "extra") {
		t.Fatalf("expected checksum to be truncated, got full checksum")
	}
}
