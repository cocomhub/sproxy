// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
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
	if err := sfs.WriteFile(context.Background(), "pikpak/t1/movie.mp4", bytes.NewReader(payload), declared, 0); err != nil {
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
	if err := sfs.WriteFile(context.Background(), "nonseek.bin", plain, 18, 0); err != nil {
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
