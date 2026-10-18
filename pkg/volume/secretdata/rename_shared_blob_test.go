// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// rename_shared_blob_test.go 钉住「共享分块集合搬不动，只能复制一份」（独立审核 P1，2026-10-18）。
//
// 缺陷：同容器复制（`copy.go`）让两份 meta **引用同一批分块 blob**（存在即引用）；而
// `relocateRenamedBlobsLocked` 跨容器时用底层 mv 把**内容分块+parity 物理搬走** ⇒ 兄弟条目的
// meta 仍指向**原容器**里的分块名 ⇒ 其 `OpenRead` 在 `readChunkBlob`/`readChunkRangeBytes` 处
// 报「读底层分块失败」。对用户表现为**静默数据丢失**：`ListDir`/`Stat` 仍可见（size 正常）、
// 但内容读不出来（幽灵条目）。
//
// 修法：搬移前判定 `chunkSetStillReferenced` ⇒ 被兄弟引用时改**字节拷贝**（内容零改动、不重加密），
// 原分块留给兄弟；且**不得** `deleteFileBlobs`（那会删掉兄弟的分块）。

import (
	"bytes"
	"context"
	"io"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// TestRename_SharedChunksAreCopiedNotMoved 共享分块被 mv 后，兄弟文件必须仍可读。
func TestRename_SharedChunksAreCopiedNotMoved(t *testing.T) {
	t.Parallel()
	fs, _ := newCountingFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "a"); err != nil {
		t.Fatalf("MakeDir(a): %v", err)
	}
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir(b): %v", err)
	}
	data := bytes.Repeat([]byte("shared-chunk-payload"), 512)
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f1.bin", bytes.NewReader(data), int64(len(data)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 同容器复制 ⇒ 分块**原地共享**（两份 meta 引用同一批 blob）。
	if err := fs.CopyFile(ctx, "a/f1.bin", "a/f2.bin"); err != nil {
		t.Fatalf("CopyFile(同容器): %v", err)
	}
	// 前置：确认确实共享（同 dataSeg + 同分块名）。
	src := fs.index["a/f1.bin"]
	sib := fs.index["a/f2.bin"]
	if src == nil || sib == nil {
		t.Fatal("源/兄弟条目缺失")
	}
	if !sameChunkNames(src.meta, sib.meta) || fs.dataSeg(src) != fs.dataSeg(sib) {
		t.Fatalf("前置失败：复制未产生共享分块（src.dataSeg=%q sib.dataSeg=%q）", fs.dataSeg(src), fs.dataSeg(sib))
	}

	// 跨容器 mv：a/f1.bin → b/f1.bin（会触发 relocateRenamedBlobsLocked）。
	if err := fs.Rename(ctx, "a/f1.bin", "b/f1.bin"); err != nil {
		t.Fatalf("Rename(跨容器): %v", err)
	}

	// ① 被移动的文件必须可读。
	assertReadable(t, fs, ctx, "b/f1.bin", data, "被移动的文件")
	// ② **兄弟文件必须仍可读**（未修必红：分块被物理搬走 ⇒ 读底层分块失败）。
	assertReadable(t, fs, ctx, "a/f2.bin", data, "兄弟文件（共享分块被搬走 ⇒ 幽灵条目）")
}

// assertReadable 断言 path 可读且内容等于 want。
func assertReadable(t *testing.T, fs *SecretdataFS, ctx context.Context, path string, want []byte, what string) {
	t.Helper()
	rc, err := fs.OpenRead(ctx, path)
	if err != nil {
		t.Fatalf("%s %q OpenRead 失败（未修必红）：%v", what, path, err)
	}
	defer func() { _ = rc.Close() }()
	got, rerr := io.ReadAll(rc)
	if rerr != nil {
		t.Fatalf("%s %q 读取失败（未修必红）：%v", what, path, rerr)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s %q 内容不符：got %d 字节, want %d 字节", what, path, len(got), len(want))
	}
}
