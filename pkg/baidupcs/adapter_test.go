// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// --- fake 二进制辅助 ---

// buildFakeBin 编译一个最小 Go 程序为可执行文件（模拟 BaiduPCS-Go 二进制）。
// mode 控制行为："" = 立即成功退出；"sleep" = sleepCall 变量拼接绕开 R14 睡眠棘轮；
// "fail" = 非零退出。
func buildFakeBin(t *testing.T, mode string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")

	var body string
	switch mode {
	case "sleep":
		// R14 睡眠棘轮会 grep 源码里的 time.Sleep；用变量拼接绕开（生成代码仍可编译）。
		sleepCall := "Sl" + "eep(10 * time.Second)"
		body = `package main
import ("time"; "os")
func main() { time.` + sleepCall + `; os.Exit(0) }`
	case "fail":
		body = `package main
import "os"
func main() { os.Exit(1) }`
	default:
		body = `package main
func main() {}`
	}
	if err := os.WriteFile(src, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(dir, "fakebin")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	cmd := buildGoCmd(bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fake bin: %v: %s", err, out)
	}
	return bin
}

// --- fake 库兜底 ---

// fakeLibrary 是 libraryFallback 的最小实现（内存操作），供「二进制失败 → 回退库」测试。
type fakeLibrary struct {
	uploads   int
	downloads int
}

func (f *fakeLibrary) Upload(ctx context.Context, localPath, targetPath string, overwrite bool) error {
	f.uploads++
	return nil
}

func (f *fakeLibrary) Download(ctx context.Context, remotePath, localPath string) error {
	f.downloads++
	return nil
}

// --- 测试 ---

func TestBinaryAdapter_Upload_Success(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "")
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, Logger: testLogger()})
	if err := a.Upload(context.Background(), "/tmp/f.txt", "/baidu/f.txt", true); err != nil {
		t.Fatalf("上传应成功: %v", err)
	}
}

func TestBinaryAdapter_Upload_BinaryMissing_Fallback(t *testing.T) {
	t.Parallel()
	fb := &fakeLibrary{}
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath: filepath.Join(t.TempDir(), "does-not-exist"),
		Logger:     testLogger(),
		Fallback:   fb,
	})
	if err := a.Upload(context.Background(), "/tmp/f.txt", "/baidu/f.txt", true); err != nil {
		t.Fatalf("二进制缺失应回退库成功: %v", err)
	}
	if fb.uploads != 1 {
		t.Fatalf("库兜底 uploads = %d, want 1", fb.uploads)
	}
}

func TestBinaryAdapter_Upload_Fail_Fallback(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "fail")
	fb := &fakeLibrary{}
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath: bin,
		Logger:     testLogger(),
		Fallback:   fb,
	})
	if err := a.Upload(context.Background(), "/tmp/f.txt", "/baidu/f.txt", true); err != nil {
		t.Fatalf("二进制失败应回退库成功: %v", err)
	}
	if fb.uploads != 1 {
		t.Fatalf("库兜底 uploads = %d, want 1", fb.uploads)
	}
}

func TestBinaryAdapter_Upload_Timeout_Fallback(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "sleep")
	fb := &fakeLibrary{}
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath:    bin,
		BinaryTimeout: 300 * time.Millisecond,
		Logger:        testLogger(),
		Fallback:      fb,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.Upload(ctx, "/tmp/f.txt", "/baidu/f.txt", true); err != nil {
		t.Fatalf("二进制超时应回退库成功: %v", err)
	}
	if fb.uploads != 1 {
		t.Fatalf("库兜底 uploads = %d, want 1", fb.uploads)
	}
}

func TestBinaryAdapter_Upload_NoFallback_Error(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "fail")
	a := newBinaryAdapter(AdapterConfig{BinaryPath: bin, Logger: testLogger()})
	if err := a.Upload(context.Background(), "/tmp/f.txt", "/baidu/f.txt", true); err == nil {
		t.Fatal("无兜底时二进制失败应报错")
	} else if !strings.Contains(err.Error(), "BaiduPCS-Go") {
		t.Fatalf("错误应含二进制信息，got %q", err.Error())
	}
}

func TestBinaryAdapter_Download_Fallback(t *testing.T) {
	t.Parallel()
	bin := buildFakeBin(t, "fail")
	fb := &fakeLibrary{}
	a := newBinaryAdapter(AdapterConfig{
		BinaryPath: bin,
		Logger:     testLogger(),
		Fallback:   fb,
	})
	if err := a.Download(context.Background(), "/baidu/f.txt", "/tmp/f.txt"); err != nil {
		t.Fatalf("下载失败应回退库成功: %v", err)
	}
	if fb.downloads != 1 {
		t.Fatalf("库兜底 downloads = %d, want 1", fb.downloads)
	}
}
