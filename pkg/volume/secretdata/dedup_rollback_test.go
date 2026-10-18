// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeThenFailFS 模拟内层「写成功但复核失败」：内容真正落盘后返回错误（baidupcs 上传后
// ETag 复核不匹配即此形态）。用于断言失败回滚不留下远端 blob 孤儿。
type writeThenFailFS struct {
	syncpkg.FS
	failN int // 前 N 次 WriteFile 落盘后返回错误
	seen  int
}

func (f *writeThenFailFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	if err := f.FS.WriteFile(ctx, key, p, r, size, mtime); err != nil {
		return err
	}
	f.seen++
	if f.seen <= f.failN {
		return errors.New("write-then-fail: 内层写成功但复核失败（模拟 baidupcs ETag 不匹配）")
	}
	return nil
}

var _ syncpkg.FS = (*writeThenFailFS)(nil)

// TestDedup_MissUploadFailure_NoPoolBlobOrphan（P2 修复，2026-10-19）：去重 miss 路径的池
// blob 名含随机 salt/nonce（不复用）——内层「写成功但复核失败」返回错误时 blob 已落远端，
// 若不在回滚集合里就成**永久孤儿**。此前 encryptAndUploadDedupChunk 的局部 uploaded 切片
// 被丢弃、writeFileDedupMiss 失败分支直接 return uerr ⇒ 无任何清理。
//
// 夹具：先 MakeDir("d") 使容器已存在（否则第一次写是目录 meta，会先失败）；随后令池分块上传
// 落盘后返错 ⇒ 精确命中该失败分支。
//
// 变异验证：把 writeFileDedupMiss 的失败回滚去掉（改回直接 return uerr）⇒ 本用例必红。
func TestDedup_MissUploadFailure_NoPoolBlobOrphan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inner := &writeThenFailFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil)}
	sfs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Dedup:     true,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	payload := bytes.Repeat([]byte("dedup-orphan-"), 64)
	inner.seen = 0 // 仅统计本次写路径的写次数（MakeDir 已消耗若干次）
	inner.failN = 1
	werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("内层写失败 ⇒ WriteFile 必须失败")
	}
	if !strings.Contains(werr.Error(), "上传去重分块失败") {
		t.Fatalf("夹具未命中池分块上传失败分支: %v", werr)
	}
	sfs.mu.RLock()
	poolDir := sfs.dedupDir
	sfs.mu.RUnlock()
	if poolDir == "" {
		t.Fatal("夹具异常：去重池容器未建立")
	}
	entries, lerr := sfs.inner.ListDir(ctx, poolDir)
	if lerr != nil && !errors.Is(lerr, os.ErrNotExist) {
		t.Fatalf("ListDir(池容器): %v", lerr)
	}
	if len(entries) != 0 {
		t.Fatalf("去重 miss 上传失败后池容器残留 %d 个 blob 孤儿（blob 名随机不复用）✗: %+v", len(entries), entries)
	}
	// 回滚后重写同内容必须成功（证明池状态未被半成品污染）。
	inner.failN = 0
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("回滚后重写应成功: %v", werr)
	}
}

// TestDedup_MissMetaUploadFail_NoMetaOrphan（P2 修复，2026-10-19）：去重 miss 路径的 **meta 上传**
// write-then-fail——此前 metaPath **不在回滚集合内**（只有池分块在）⇒ 内层「写成功但复核失败」
// 时 meta blob 永久孤儿（与池分块同类，且与本 PR「先登记后上传」口径不一致）。
// 修：metaPath 在上传前登记进 uploaded，两处显式 Delete(metaPath) 由 rollbackWrite 统一承担。
//
// 变异：去掉 `uploaded = append(uploaded, metaPath)` ⇒ 底层残留 file meta blob ⇒ 本用例必红。
func TestDedup_MissMetaUploadFail_NoMetaOrphan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	inner := &failAfterWriteFS{FS: syncpkg.NewLocalFS(root, nil), pred: func(p string) bool {
		return shardseal.IsMetaName(path.Base(p)) // 仅 **file meta**（dir meta 标记为 q，不受影响）
	}}
	sfs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Dedup:     true,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	payload := bytes.Repeat([]byte("dedup-meta-orphan-"), 64)
	werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("meta 上传 write-then-fail ⇒ WriteFile 必须失败 ✗")
	}
	if !strings.Contains(werr.Error(), "上传去重 meta 失败") {
		t.Fatalf("夹具未命中去重 meta 上传失败分支: %v", werr)
	}
	if !inner.fired {
		t.Fatal("夹具失效：未观测到 file meta 的内层写")
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e != nil {
		t.Fatal("meta 上传失败后不得留下索引条目（半态）✗")
	}
	if orphans := residualDataBlobs(t, root); len(orphans) != 0 {
		t.Fatalf("去重 meta 上传失败回滚后底层残留 %d 个非目录 blob（远端孤儿）✗: %v", len(orphans), orphans)
	}
}
