// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestWriteFile_SeekableStreamsToEncryptShardsReader 锁定：可 seek 输入（bytes.Reader）
// 走流式加密路径（EncryptShardsReader）——通过 size 严格按声明截断验证（内存变体会
// io.ReadAll 读全；流式按 size 读块）。断言用「声明 size 少于实际内容」：
//
//	内存路径读全 21B → 加密成功，写后读回全 21B；
//	流式路径严格按 size=18 读 → 只加密 18B → 读回 18B。
//
// 变异（去掉 canStream 分叉回 io.ReadAll）→ 读回 21B ≠ 18B → 红。
func TestWriteFile_SeekableSizeHonored(t *testing.T) {
	t.Parallel()
	fs := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(fs, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("encrypted-content-123") // 21B
	declared := int64(18)                      // 声明 18B（故意少于实际）
	if err := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "pikpak/t1/movie.mp4", bytes.NewReader(payload), declared, 0); err != nil {
		t.Fatalf("WriteFile(seekable): %v", err)
	}
	rc, rerr := sfs.OpenRead(context.Background(), "pikpak/t1/movie.mp4")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	got, gerr := io.ReadAll(rc)
	if gerr != nil {
		t.Fatal(gerr)
	}
	// 流式按声明 size 加密 → 只存 18B。
	if int64(len(got)) != declared {
		t.Fatalf("流式应严格按声明 size=%d 截断，读回 %d 字节", declared, len(got))
	}
}

// TestWriteFile_NonSeekableReadsAll 锁定：纯流（无 Seeker）仍 io.ReadAll 读全（功能不变）。
func TestWriteFile_NonSeekableReadsAll(t *testing.T) {
	t.Parallel()
	fs := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(fs, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte("encrypted-content-123")             // 21B
	plain := struct{ io.Reader }{bytes.NewReader(payload)} // 丢弃 Seeker
	if err := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "nonseek.bin", plain, 18, 0); err != nil {
		t.Fatalf("WriteFile(non-seek): %v", err)
	}
	rc, rerr := sfs.OpenRead(context.Background(), "nonseek.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	defer rc.Close()
	got, gerr := io.ReadAll(rc)
	if gerr != nil {
		t.Fatal(gerr)
	}
	// 非 seek 回退 io.ReadAll → 读全 21B 加密。
	if string(got) != string(payload) {
		t.Fatalf("非 seek 应 io.ReadAll 读全（%d 字节），读回 %d 字节", len(payload), len(got))
	}
}

// TestWriteFile_LocalFSSourceStreams 锁定 sync 引擎 / 备份恢复 的写目标路径：
// 二者均为 `src.OpenRead(...) → dst.WriteFile(rc, ...)`（pkg/sync/engine.go、pkg/backup/backup.go），
// 本地源 LocalFS.OpenRead 返回 *os.File（可 seek/ReaderAt）→ secretdata 写路径 canStream
// 命中 → 走流式（EncryptShardsReader）。本测试直接复刻该组合，断言来源 reader 可流式 +
// 端到端还原一致。
func TestWriteFile_LocalFSSourceStreams(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	src := syncpkg.NewLocalFS(srcDir, nil)
	payload := make([]byte, 3<<20) // 3MiB（跨多块，验证流式多块）
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "src.bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	// 复刻 sync/backup：源 OpenRead → 目标 WriteFile。
	rc, oerr := src.OpenRead(context.Background(), "src.bin")
	if oerr != nil {
		t.Fatalf("OpenRead: %v", oerr)
	}
	defer rc.Close()
	if !canStream(rc) {
		t.Fatalf("LocalFS.OpenRead 来源 reader 不可流式（%T）——sync/备份流式未生效", rc)
	}

	dstFS := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(dstFS, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	if werr := sfs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "restored/src.bin", rc, int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile(本地源流式): %v", werr)
	}
	// 还原一致。
	back, rerr := sfs.OpenRead(context.Background(), "restored/src.bin")
	if rerr != nil {
		t.Fatalf("OpenRead(还原): %v", rerr)
	}
	defer back.Close()
	got, gerr := io.ReadAll(back)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("还原不一致：got %d bytes want %d", len(got), len(payload))
	}
}
