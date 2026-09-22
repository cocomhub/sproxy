// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package remote_test

// blockaccess_test.go 验证 remoteFS 的 BlockAccessor（roadmap 4.3 P2 跨 FS 块级）：
//  1. OpenReaderAt：经 /download/chunk 读旧块（offset 定位 + 内容校验）。
//  2. OpenWriterAt：open→write→close 会话（预分配 + 偏移写 + 完成 mtime）。

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/remote"
	"github.com/cocomhub/sproxy/pkg/server"
	"github.com/cocomhub/sproxy/pkg/tunnel"
)

// TestRemoteFS_BlockReaderAt 远端 OpenReaderAt 读指定 offset 块。
func TestRemoteFS_BlockReaderAt(t *testing.T) {
	t.Parallel()
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b := startBEnd(t, aID.Fingerprint())
	content := bytes.Repeat([]byte("block-read"), 4096) // 32 KiB
	writeBFile(t, b.cfg, "docs/blob.bin", content)

	c := newAClient(t, b, aID)
	fs := c.FS(remote.Ref{Node: testNodeA, Volume: testVol, Path: "docs"})

	ba, ok := fs.(interface {
		OpenReaderAt(context.Context, string) (io.ReaderAt, io.Closer, error)
	})
	if !ok {
		t.Fatal("remoteFS 应实现 BlockAccessor（OpenReaderAt）")
	}
	ra, closer, err := ba.OpenReaderAt(context.Background(), "docs/blob.bin")
	if err != nil {
		t.Fatalf("OpenReaderAt: %v", err)
	}
	if closer != nil {
		defer closer.Close()
	}
	// 读 offset 0 起 16 字节 = content[0:16]。
	buf := make([]byte, 16)
	if n, err := ra.ReadAt(buf, 0); err != nil || n != 16 {
		t.Fatalf("ReadAt(0) = %d err=%v, want 16", n, err)
	}
	if !bytes.Equal(buf, content[:16]) {
		t.Fatalf("offset 0 内容不符")
	}
	// 读中部 offset 4096 起 16 字节。
	if n, err := ra.ReadAt(buf, 4096); err != nil || n != 16 {
		t.Fatalf("ReadAt(4096) = %d err=%v", n, err)
	}
	if !bytes.Equal(buf, content[4096:4112]) {
		t.Fatalf("offset 4096 内容不符")
	}
}

// TestRemoteFS_BlockWriterAt 远端 OpenWriterAt 会话（open→write→close）。
func TestRemoteFS_BlockWriterAt(t *testing.T) {
	t.Parallel()
	aID, err := tunnel.GenerateIdentity()
	if err != nil {
		t.Fatal(err)
	}
	readAddr, writeAddr, bFP, cfg := startBEndWrite(t, aID.Fingerprint())
	// B 端已有旧文件（覆盖场景）。
	writeBFile(t, cfg, "docs/target.bin", bytes.Repeat([]byte("a"), 8192))

	dialTo := func(addr string) remote.Dialer {
		return remote.DialerFunc(func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", addr)
		})
	}
	c := remote.New(dialTo(readAddr),
		remote.WithIdentity(aID),
		remote.WithPeerPin(testNodeA, bFP),
		remote.WithWriteDialer(dialTo(writeAddr)),
	)
	t.Cleanup(func() { _ = c.Close() })
	fs := c.FS(remote.Ref{Node: testNodeA, Volume: testVol, Path: "docs"})

	ba, ok := fs.(interface {
		OpenWriterAt(context.Context, string, int64, int64) (io.WriterAt, io.Closer, error)
	})
	if !ok {
		t.Fatal("remoteFS 应实现 BlockAccessor（OpenWriterAt）")
	}
	wa, closer, err := ba.OpenWriterAt(context.Background(), "docs/target.bin", 8192, 0)
	if err != nil {
		t.Fatalf("OpenWriterAt: %v", err)
	}
	if closer == nil {
		t.Fatal("closer 不应为 nil（close 完成会话）")
	}
	// 模拟引擎写全部块：差异块（0/4096）写新内容；相同块（中间）读旧内容写入
	// （OpenWriterAt 是预分配稀疏文件——未写块保持 0，引擎负责逐块写全）。
	oldContent := bytes.Repeat([]byte("a"), 8192)
	if _, err := wa.WriteAt([]byte("BBBB"), 0); err != nil {
		t.Fatalf("WriteAt(0): %v", err)
	}
	if _, err := wa.WriteAt(oldContent[4:4096], 4); err != nil {
		t.Fatalf("WriteAt(4): %v", err)
	}
	if _, err := wa.WriteAt([]byte("CCCC"), 4096); err != nil {
		t.Fatalf("WriteAt(4096): %v", err)
	}
	if _, err := wa.WriteAt(oldContent[4100:], 4100); err != nil {
		t.Fatalf("WriteAt(4100): %v", err)
	}
	if err := closer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// 校验远端文件内容（读回）。
	data := readBFile(t, cfg, "docs/target.bin")
	if len(data) != 8192 {
		t.Fatalf("远端文件长度 = %d, want 8192", len(data))
	}
	want := append([]byte("BBBB"), oldContent[4:]...)
	want = append(want[:4096], append([]byte("CCCC"), oldContent[4100:]...)...)
	if !bytes.Equal(data, want) {
		t.Fatalf("偏移写内容不符: got %q want %q", data[:8], want[:8])
	}
}

// readBFile 读 B 端卷内文件（写回校验用）。
func readBFile(t *testing.T, cfg *server.Config, rel string) []byte {
	t.Helper()
	abs := filepath.Join(cfg.StorageRoot, testOwner, "user", filepath.FromSlash(rel))
	data, err := os.ReadFile(abs)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return data
}
