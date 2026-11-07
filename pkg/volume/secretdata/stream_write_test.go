// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/units/sizex"
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

// TestWriteFile_UnknownSizeBoundedByMaxFileBytes（P2 修复，2026-10-19）：配置 MaxFileBytes 时，
// **未知长度（size<0）+ 非 seek 源**（chunked 上传形态）此前走内存路径的裸 io.ReadAll ⇒
// 「按声明 size 预检」失效 ⇒ 任意大小内容整读进内存（OOM）。修后按「上限 +1」读并超限
// fail-closed（与流式路径对未知长度的拒绝同一口径）；未配置上限（0=不限）时保持既有「读全」。
//
// 变异：把 readAllBounded 改回裸 io.ReadAll ⇒ ① 超限用例必红（写成功而非报错）。
func TestWriteFile_UnknownSizeBoundedByMaxFileBytes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	newFS := func(t *testing.T, max int64) *SecretdataFS {
		t.Helper()
		sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
			Secret: []byte("test-secret-key-32bytes-abcdefgh"), MaxFileBytes: sizex.ByteSize(max),
		})
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		return sfs
	}
	// ① 超限 + size=-1 ⇒ 必须 fail-closed（ErrMaxFileBytes，与写路径预检同哨兵）
	sfs := newFS(t, 64)
	big := struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("x"), 200))} // 丢弃 Seeker
	err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "big.bin", big, -1, 0)
	if err == nil {
		t.Fatal("未知长度源超过 MaxFileBytes 必须失败（否则无界整读内存）✗")
	}
	if !errors.Is(err, ErrMaxFileBytes) {
		t.Fatalf("应为 ErrMaxFileBytes（与写路径预检同哨兵），实测: %v", err)
	}
	// ② 未超限 + size=-1 ⇒ 仍须成功（上限不得过度收紧）
	small := struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("y"), 32))}
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "small.bin", small, -1, 0); werr != nil {
		t.Fatalf("未超限的未知长度源应成功: %v", werr)
	}
	// ③ 未配置上限（0=不限）⇒ 保持既有「读全」语义（不回归 TestWriteFile_NonSeekableReadsAll）
	sfs2 := newFS(t, 0)
	big2 := struct{ io.Reader }{bytes.NewReader(bytes.Repeat([]byte("z"), 200))}
	if werr := sfs2.WriteFile(ctx, syncpkg.LogicalKey{}, "unbounded.bin", big2, -1, 0); werr != nil {
		t.Fatalf("未配置上限时未知长度源应读全成功: %v", werr)
	}
}
