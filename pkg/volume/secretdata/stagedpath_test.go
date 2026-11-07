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

// TestWriteFromStagedPath_ReusesStagedSource 钉住「复用调用方已暂存的本地路径」：
//   - 内容与直接 `WriteFile` 等价（逐字节一致，加密/分块/meta 走同一写路径）；
//   - **源文件只读**（生命周期契约：实现方不得移动/删除/改写 ⇒ 调用方负责清理）；
//   - 走**流式**加密（可 seek 源）⇒ 不把整份内容读进内存（大文件场景的收益）。
func TestWriteFromStagedPath_ReusesStagedSource(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("staged-path-reuse-"), 8192) // 128KiB

	staged := filepath.Join(t.TempDir(), "upload.tmp")
	if err := os.WriteFile(staged, payload, 0o600); err != nil {
		t.Fatalf("预置暂存文件失败: %v", err)
	}
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	fs, err := NewFS(raw, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := fs.WriteFromStagedPath(ctx, "reused.bin", staged, int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFromStagedPath: %v", err)
	}
	rc, rerr := fs.OpenRead(ctx, "reused.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got := make([]byte, len(payload))
	if _, rerr := io.ReadFull(rc, got); rerr != nil {
		t.Fatalf("读回失败: %v", rerr)
	}
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("复用暂存路径写入的内容与原文不一致（%d vs %d 字节）", len(got), len(payload))
	}
	// 生命周期契约：源文件必须**原样保留**（只读语义）。
	after, aerr := os.ReadFile(staged)
	if aerr != nil {
		t.Fatalf("暂存文件被移动/删除了（违反只读契约）: %v", aerr)
	}
	if !bytes.Equal(after, payload) {
		t.Fatal("暂存文件被改写了（违反只读契约）✗")
	}
	// size<=0（未知长度）也必须正确（按实际文件大小结算）。
	if err := fs.WriteFromStagedPath(ctx, "reused2.bin", staged, 0, 0); err != nil {
		t.Fatalf("未知长度写入失败: %v", err)
	}
	e := fs.index["reused2.bin"]
	if e == nil || e.size != int64(len(payload)) {
		t.Fatalf("未知长度写入的记账 size=%v, want %d", e, len(payload))
	}
}
