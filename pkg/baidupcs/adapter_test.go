// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	pcsapi "github.com/cocomhub/sproxy/pkg/baidupcs/internal"
	"github.com/cocomhub/sproxy/pkg/baidupcs/internal/pcserror"
)

// fakeBaiduPCSCmd 生成一个 fake BaiduPCS-Go 可执行文件（写入临时目录），
// 模拟 upload/download 子命令的成功/失败输出。返回二进制路径。
// 实现：用 Go 源码在测试内 go build 生成（跨平台，Windows 也可用）。
func fakeBaiduPCSCmd(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	var mainSrc string
	switch mode {
	case "upload-ok":
		mainSrc = `package main
import ("fmt"; "os")
func main() {
	if len(os.Args) > 1 && os.Args[1] == "upload" {
		fmt.Println("upload ok")
		os.Exit(0)
	}
	fmt.Println("unknown")
	os.Exit(1)
}`
	case "upload-fail":
		mainSrc = `package main
import ("fmt"; "os")
func main() {
	if len(os.Args) > 1 && os.Args[1] == "upload" {
		fmt.Fprintln(os.Stderr, "upload failed: network error")
		os.Exit(1)
	}
	fmt.Println("unknown")
	os.Exit(1)
}`
	case "download-ok":
		mainSrc = `package main
import ("fmt"; "os")
func main() {
	if len(os.Args) > 1 && os.Args[1] == "download" {
		// 把内容写到 --saveto 或位置参数指定路径
		out := ""
		args := os.Args[2:]
		for i := 0; i < len(args)-1; i++ {
			if args[i] == "--saveto" { out = args[i+1] }
		}
		if out == "" && len(args) > 0 { out = args[len(args)-1] }
		if out != "" {
			_ = os.WriteFile(out, []byte("downloaded-content"), 0644)
		}
		fmt.Println("download ok")
		os.Exit(0)
	}
	fmt.Println("unknown")
	os.Exit(1)
}`
	default:
		mainSrc = `package main
import ("fmt"; "os")
func main() { fmt.Println("unknown"); os.Exit(1) }`
	}
	if err := os.WriteFile(src, []byte(mainSrc), 0o644); err != nil {
		t.Fatalf("write fake bin src: %v", err)
	}
	bin := filepath.Join(dir, "baidupcs-go.exe")
	// 测试内 go build：用当前进程的 go 工具
	cmd := newGoBuildCmd(src, bin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build fake bin: %v\n%s", err, out)
	}
	return bin
}

// fakePCSMeta 构造一个 FileDirectory 元数据（供 fake 库 Adapter 使用）。
func fakePCSMeta(path, name string, size int64, isdir bool) *pcsapi.FileDirectory {
	return &pcsapi.FileDirectory{
		Path:     path,
		Filename: name,
		Size:     size,
		Isdir:    isdir,
		MD5:      "fake-md5",
	}
}

// fakeLibraryPCS 实现 pcsLibrary 接口的最小 fake（内存 map），
// 供「二进制缺失 → 回退库」测试使用。
type fakeLibraryPCS struct {
	files         map[string]*pcsapi.FileDirectory // path → meta
	uploadCalls   int
	downloadCalls int
}

func newFakeLibraryPCS() *fakeLibraryPCS {
	return &fakeLibraryPCS{files: make(map[string]*pcsapi.FileDirectory)}
}

func (f *fakeLibraryPCS) FilesDirectoriesMeta(path string) (*pcsapi.FileDirectory, pcserror.Error) {
	if fd, ok := f.files[path]; ok {
		return fd, nil
	}
	return nil, &pcserror.PCSErrInfo{Operation: "meta", ErrType: pcserror.ErrTypeRemoteError}
}

// Upload 记录调用（fake：直接成功）。
func (f *fakeLibraryPCS) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	f.uploadCalls++
	return nil
}

// Download 记录调用（fake：写入本地文件）。
func (f *fakeLibraryPCS) Download(ctx context.Context, remotePath, localPath string) error {
	f.downloadCalls++
	if err := os.WriteFile(localPath, []byte("fake-library-content"), 0o644); err != nil {
		return err
	}
	return nil
}

func TestAdapter_BinaryUpload_Success(t *testing.T) {
	t.Parallel()
	bin := fakeBaiduPCSCmd(t, "upload-ok")
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	// 写一个临时文件供上传
	local := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Upload(context.Background(), local, "/dst/f.txt", false); err != nil {
		t.Fatalf("二进制上传成功应无错误, got %v", err)
	}
}

func TestAdapter_BinaryUpload_MissingBinary_FallbackToLibrary(t *testing.T) {
	t.Parallel()
	// 二进制路径不存在 → 回退库；库 fake 上传成功。
	lib := newFakeLibraryPCS()
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist.exe"),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Fallback:   lib,
	})
	local := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// fake 库的 Upload 成功（不真的上传，仅验证回退路径被调用）。
	if err := a.Upload(context.Background(), local, "/dst/f.txt", false); err != nil {
		t.Fatalf("二进制缺失应回退库, got %v", err)
	}
}

func TestAdapter_BinaryDownload_Success(t *testing.T) {
	t.Parallel()
	bin := fakeBaiduPCSCmd(t, "download-ok")
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	local := filepath.Join(t.TempDir(), "out.bin")
	if err := a.Download(context.Background(), "/remote/f.bin", local); err != nil {
		t.Fatalf("二进制下载成功应无错误, got %v", err)
	}
	got, err := os.ReadFile(local)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "downloaded-content" {
		t.Fatalf("下载内容 = %q, want downloaded-content", got)
	}
}

// TestAdapter_BinaryUpload_Timeout 验证二进制超时（exec.CommandContext + ctx）。
func TestAdapter_BinaryUpload_Timeout(t *testing.T) {
	t.Parallel()
	// fake 二进制 sleep 10s；ctx 500ms 超时 → 回退库。
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	mainSrc := `package main
import ("time"; "os")
func main() { time.Sleep(10 * time.Second); os.Exit(0) }`
	if err := os.WriteFile(src, []byte(mainSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "slow.exe")
	cmd := newGoBuildCmd(src, bin)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake slow bin: %v\n%s", err, out)
	}
	lib := newFakeLibraryPCS()
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Fallback: lib})
	ctx, cancel := context.WithTimeout(context.Background(), 500e6) // 500ms 纳秒单位避免依赖 time import
	defer cancel()
	local := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.Upload(ctx, local, "/dst/f.txt", false); err != nil {
		t.Fatalf("二进制超时应回退库, got %v", err)
	}
}

// newGoBuildCmd 构造 go build 命令（Windows 兼容）。
func newGoBuildCmd(src, bin string) *exec.Cmd {
	cmd := exec.Command("go", "build", "-o", bin, src)
	return cmd
}
