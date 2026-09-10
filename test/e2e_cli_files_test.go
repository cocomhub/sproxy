// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

// e2e_cli_files_test.go 覆盖基础文件面与元信息 CLI（真二进制 + 子进程）：
// upload / list / download / delete / stat server / stats / search / mv / batch-rename。
// 每条正例 CLI 调用都断言真实副作用（磁盘文件内容/checksum 或签名 HTTP API 响应）。
package sproxy_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/client"
)

// TestE2E_CLI_UploadDownloadListDelete 覆盖 CLI 最小闭环：
// upload → 磁盘 + /api/files + list --json → download → /api/stats → delete → 磁盘 + 404。
func TestE2E_CLI_UploadDownloadListDelete(t *testing.T) {
	env := startCLIEnv(t, "")

	content := []byte("cli e2e content")
	checksum := sha256hex(content)
	if err := os.WriteFile(filepath.Join(env.TmpDir, "hello.txt"), content, 0644); err != nil {
		t.Fatalf("写本地文件失败: %v", err)
	}

	// 1) upload
	env.sclient(t, env.TmpDir, "upload", "hello.txt")

	// 2) 磁盘副作用：恰好 1 个 hello.txt，内容/checksum 一致
	found := findFilesNamed(t, env.StorageRoot, "hello.txt")
	if len(found) != 1 {
		t.Fatalf("upload 后应恰有 1 个 hello.txt，got %d: %v", len(found), found)
	}
	onDisk, err := os.ReadFile(found[0])
	if err != nil {
		t.Fatalf("读取落盘文件失败: %v", err)
	}
	if string(onDisk) != string(content) {
		t.Fatalf("落盘内容不一致: got %q, want %q", onDisk, content)
	}
	if got := sha256hex(onDisk); got != checksum {
		t.Fatalf("落盘 checksum 不一致: got %s, want %s", got, checksum)
	}

	// 3) 签名 API：GET /api/files 含该文件且 checksum 一致
	var listAPI struct {
		Files []client.FileInfo `json:"files"`
	}
	getJSON(t, env.BaseURL+"/api/files", &listAPI)
	assertFileInfo(t, listAPI.Files, "hello.txt", checksum)

	// 4) CLI list --json
	var listResp struct {
		Files []client.FileInfo `json:"files"`
	}
	env.sclientJSON(t, env.TmpDir, &listResp, "list")
	assertFileInfo(t, listResp.Files, "hello.txt", checksum)

	// 5) download 到本地，内容字节全等
	outPath := filepath.Join(env.TmpDir, "out.txt")
	env.sclient(t, env.TmpDir, "download", "hello.txt", outPath)
	downloaded, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读取下载文件失败: %v", err)
	}
	if string(downloaded) != string(content) {
		t.Fatalf("下载内容不一致: got %q, want %q", downloaded, content)
	}

	// 6) 签名 API：/api/stats 计数递增
	var stats client.StatsResponse
	getJSON(t, env.BaseURL+"/api/stats", &stats)
	if stats.FilesUploaded < 1 {
		t.Fatalf("stats.FilesUploaded 应 >= 1, got %d", stats.FilesUploaded)
	}

	// 7) delete
	env.sclient(t, env.TmpDir, "delete", "hello.txt")

	// 8) 磁盘 + 接口副作用：文件消失
	if got := findFilesNamed(t, env.StorageRoot, "hello.txt"); len(got) != 0 {
		t.Fatalf("delete 后磁盘不应有 hello.txt, got %v", got)
	}
	if status, _ := statFile(t, env.BaseURL, "hello.txt"); status != http.StatusNotFound {
		t.Fatalf("delete 后 stat 应 404, got %d", status)
	}
}

// TestE2E_CLI_StatSearchStats 覆盖元信息命令：
// 多文件上传 → stats / stat server（同字段）→ search --json（唯一命中）→ search 文本输出。
func TestE2E_CLI_StatSearchStats(t *testing.T) {
	env := startCLIEnv(t, "")

	files := map[string][]byte{
		"alpha.txt": []byte("alpha cli content"),
		"beta.txt":  []byte("beta cli content"),
		"gamma.txt": []byte("gamma cli content"),
	}
	checksums := make(map[string]string, len(files))
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(env.TmpDir, name), content, 0644); err != nil {
			t.Fatalf("写本地文件 %s 失败: %v", name, err)
		}
		env.sclient(t, env.TmpDir, "upload", name)
		checksums[name] = sha256hex(content)
	}

	// 磁盘副作用：三者各命中 1 个且 checksum 一致
	for name, content := range files {
		found := findFilesNamed(t, env.StorageRoot, name)
		if len(found) != 1 {
			t.Fatalf("upload 后应恰有 1 个 %s，got %d: %v", name, len(found), found)
		}
		onDisk, err := os.ReadFile(found[0])
		if err != nil {
			t.Fatalf("读取落盘文件 %s 失败: %v", name, err)
		}
		if string(onDisk) != string(content) {
			t.Fatalf("%s 落盘内容不一致", name)
		}
		if got := sha256hex(onDisk); got != checksums[name] {
			t.Fatalf("%s 落盘 checksum 不一致: got %s, want %s", name, got, checksums[name])
		}
	}

	// stats 与 stat server 应给出相同字段
	var stats, stats2 client.StatsResponse
	env.sclientJSON(t, env.TmpDir, &stats, "stats")
	env.sclientJSON(t, env.TmpDir, &stats2, "stat", "server")
	if stats.FilesUploaded < 3 {
		t.Fatalf("stats.FilesUploaded 应 >= 3, got %d", stats.FilesUploaded)
	}
	if stats.DiskUsage.TotalFiles < 3 {
		t.Fatalf("stats.DiskUsage.TotalFiles 应 >= 3, got %d", stats.DiskUsage.TotalFiles)
	}
	if stats2.FilesUploaded != stats.FilesUploaded {
		t.Fatalf("stat server 与 stats 的 FilesUploaded 应一致: %d vs %d", stats2.FilesUploaded, stats.FilesUploaded)
	}
	if stats2.DiskUsage.TotalFiles != stats.DiskUsage.TotalFiles {
		t.Fatalf("stat server 与 stats 的 TotalFiles 应一致: %d vs %d",
			stats2.DiskUsage.TotalFiles, stats.DiskUsage.TotalFiles)
	}

	// search --json 唯一命中 beta.txt
	var searchResp struct {
		Files []client.FileInfo `json:"files"`
	}
	env.sclientJSON(t, env.TmpDir, &searchResp, "search", "beta")
	if len(searchResp.Files) != 1 {
		t.Fatalf("search beta 应恰命中 1 个, got %d: %+v", len(searchResp.Files), searchResp.Files)
	}
	if searchResp.Files[0].Name != "beta.txt" {
		t.Fatalf("search beta 命中名应为 beta.txt, got %q", searchResp.Files[0].Name)
	}
	if searchResp.Files[0].Checksum != checksums["beta.txt"] {
		t.Fatalf("search beta checksum 不一致: got %s, want %s",
			searchResp.Files[0].Checksum, checksums["beta.txt"])
	}

	// 文本输出包含命中文件名
	if out := env.sclient(t, env.TmpDir, "search", "alpha"); !strings.Contains(out, "alpha.txt") {
		t.Fatalf("search alpha 文本输出应含 alpha.txt, got:\n%s", out)
	}
}

// TestE2E_CLI_MvBatchRename 覆盖 mv 与 batch-rename：
// 单个 rename、批量 rename、以及源不存在的负例（断言无副作用）。
func TestE2E_CLI_MvBatchRename(t *testing.T) {
	env := startCLIEnv(t, "")

	writeAndUpload := func(name string, content []byte) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(env.TmpDir, name), content, 0644); err != nil {
			t.Fatalf("写本地文件 %s 失败: %v", name, err)
		}
		env.sclient(t, env.TmpDir, "upload", name)
		return sha256hex(content)
	}

	// ---- 单个 mv ----
	contentA := []byte("mv content A")
	checksumA := writeAndUpload("mv_old.txt", contentA)

	env.sclient(t, env.TmpDir, "mv", "mv_old.txt", "mv_new.txt")

	// 接口：新名 200 且 checksum 一致；旧名 404
	status, headers := statFile(t, env.BaseURL, "mv_new.txt")
	if status != http.StatusOK {
		t.Fatalf("mv 后新名 stat 应 200, got %d", status)
	}
	if got := headers.Get("X-File-Checksum"); got != checksumA {
		t.Fatalf("mv 后新名 checksum 不一致: got %s, want %s", got, checksumA)
	}
	if status, _ := statFile(t, env.BaseURL, "mv_old.txt"); status != http.StatusNotFound {
		t.Fatalf("mv 后旧名 stat 应 404, got %d", status)
	}
	// 磁盘：新名存在且内容一致，旧名消失
	found := findFilesNamed(t, env.StorageRoot, "mv_new.txt")
	if len(found) != 1 {
		t.Fatalf("mv 后应有 1 个 mv_new.txt, got %d: %v", len(found), found)
	}
	// err 限定在 if 作用域内：避免后续 for 循环内的 err 声明触发 govet shadow。
	if onDisk, rerr := os.ReadFile(found[0]); rerr != nil {
		t.Fatalf("读取 mv_new.txt 失败: %v", rerr)
	} else if string(onDisk) != string(contentA) {
		t.Fatalf("mv 后内容不一致: got %q, want %q", onDisk, contentA)
	}
	if got := findFilesNamed(t, env.StorageRoot, "mv_old.txt"); len(got) != 0 {
		t.Fatalf("mv 后磁盘不应有 mv_old.txt, got %v", got)
	}

	// ---- batch-rename ----
	checksumB1 := writeAndUpload("br1.txt", []byte("batch rename one"))
	checksumB2 := writeAndUpload("br2.txt", []byte("batch rename two"))

	env.sclient(t, env.TmpDir, "batch-rename", "br1.txt", "br1n.txt", "br2.txt", "br2n.txt")

	for _, tc := range []struct{ newName, checksum string }{
		{"br1n.txt", checksumB1},
		{"br2n.txt", checksumB2},
	} {
		status, headers := statFile(t, env.BaseURL, tc.newName)
		if status != http.StatusOK {
			t.Fatalf("batch-rename 后 %s stat 应 200, got %d", tc.newName, status)
		}
		if got := headers.Get("X-File-Checksum"); got != tc.checksum {
			t.Fatalf("batch-rename 后 %s checksum 不一致: got %s, want %s", tc.newName, got, tc.checksum)
		}
		found := findFilesNamed(t, env.StorageRoot, tc.newName)
		if len(found) != 1 {
			t.Fatalf("batch-rename 后应有 1 个 %s, got %d: %v", tc.newName, len(found), found)
		}
		onDisk, err := os.ReadFile(found[0])
		if err != nil {
			t.Fatalf("读取 %s 失败: %v", tc.newName, err)
		}
		if got := sha256hex(onDisk); got != tc.checksum {
			t.Fatalf("batch-rename 后 %s 内容 checksum 不一致: got %s, want %s", tc.newName, got, tc.checksum)
		}
	}
	for _, oldName := range []string{"br1.txt", "br2.txt"} {
		if status, _ := statFile(t, env.BaseURL, oldName); status != http.StatusNotFound {
			t.Fatalf("batch-rename 后旧名 %s stat 应 404, got %d", oldName, status)
		}
		if got := findFilesNamed(t, env.StorageRoot, oldName); len(got) != 0 {
			t.Fatalf("batch-rename 后磁盘不应有 %s, got %v", oldName, got)
		}
	}

	// ---- 负例：源不存在 → 非零退出且无副作用 ----
	stdout, stderr, err := env.sclientRun(t, env.TmpDir, "mv", "missing.txt", "x.txt")
	if err == nil {
		t.Fatalf("mv 不存在的源应非零退出\nstdout:\n%s\nstderr:\n%s", stdout, stderr)
	}
	if status, _ := statFile(t, env.BaseURL, "x.txt"); status != http.StatusNotFound {
		t.Fatalf("失败的 mv 不应产生目标文件, stat x.txt got %d", status)
	}
	if got := findFilesNamed(t, env.StorageRoot, "x.txt"); len(got) != 0 {
		t.Fatalf("失败的 mv 不应在磁盘产生 x.txt, got %v", got)
	}
}

// ---- 本文件内共享的小工具 ----

// assertFileInfo 断言 files 中存在 name 且 checksum 一致。
func assertFileInfo(t *testing.T, files []client.FileInfo, name, checksum string) {
	t.Helper()
	for _, f := range files {
		if f.Name == name {
			if f.Checksum != checksum {
				t.Fatalf("%s checksum 不一致: got %s, want %s", name, f.Checksum, checksum)
			}
			return
		}
	}
	t.Fatalf("文件列表应含 %s, got %+v", name, files)
}
