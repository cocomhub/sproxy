// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// inspect_test.go 锁「真实存储审查」的映射准确性（设计
// docs/designs/2026-11-04-secretdata-storage-inspection.md）。
//
// 每个用例都从**真实写入后的落盘状态**出发（不是构造假索引），因为本能力的价值就是
// 「相信磁盘事实」，用假索引测等于自证。

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"path"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

func writeFileT(t *testing.T, fsys *SecretdataFS, rel string, content []byte) {
	t.Helper()
	if err := fsys.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel,
		bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile(%s): %v", rel, err)
	}
}

// TestInspect_TraceFile 正向：分块/meta 落点与写入一致，disk 模式全绿。
func TestInspect_TraceFile(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	content := data(600) // 64-128B 随机块 ⇒ 多块
	writeFileT(t, fsys, "dir/movie.mp4", content)

	tr, err := fsys.Trace(ctx, "dir/movie.mp4", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Kind != "file" || tr.Size != int64(len(content)) {
		t.Fatalf("kind/size = %s/%d, want file/%d", tr.Kind, tr.Size, len(content))
	}
	if tr.Meta == nil || tr.Meta.RealPath == "" {
		t.Fatal("文件 meta 落点缺失")
	}
	if len(tr.Chunks) < 2 {
		t.Fatalf("分块数 %d，期望多块（内容 600B / 块 ≤128B）", len(tr.Chunks))
	}
	for i, c := range tr.Chunks {
		if c.Blob.RealPath == "" || !strings.HasPrefix(c.Blob.RealPath, tr.Meta.RealPath[:strings.Index(tr.Meta.RealPath, "/")+1]) {
			t.Fatalf("分块 %d 落点 %q 不在同一容器链上（meta=%q）", i, c.Blob.RealPath, tr.Meta.RealPath)
		}
		if c.Blob.Bytes <= 0 || c.Blocklets < 1 {
			t.Fatalf("分块 %d 尺寸/块数异常: %+v", i, c)
		}
	}

	// disk 模式：所有声明 blob 必须真实存在且大小一致。
	trDisk, derr := fsys.Trace(ctx, "dir/movie.mp4", volume.InspectDisk)
	if derr != nil {
		t.Fatalf("Trace(disk): %v", derr)
	}
	if len(trDisk.Anomalies) != 0 {
		t.Fatalf("disk 模式不应有差异，got %+v", trDisk.Anomalies)
	}
	for _, ref := range append([]*volume.BlobRef{trDisk.Meta, trDisk.Parity}, blobRefsOf(trDisk.Chunks)...) {
		if ref == nil {
			continue
		}
		if ref.OnDisk == nil || !*ref.OnDisk || ref.DiskBytes == nil || *ref.DiskBytes != ref.Bytes {
			t.Fatalf("blob %s 磁盘状态不符: %+v", ref.RealPath, ref)
		}
	}
}

func blobRefsOf(chunks []volume.ChunkRef) []*volume.BlobRef {
	out := make([]*volume.BlobRef, 0, len(chunks))
	for i := range chunks {
		out = append(out, &chunks[i].Blob)
	}
	return out
}

// TestInspect_TraceDir 正向：目录输出容器 + dir-meta。
func TestInspect_TraceDir(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	if err := fsys.MakeDir(ctx, "photos"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	writeFileT(t, fsys, "photos/a.jpg", data(50))

	tr, err := fsys.Trace(ctx, "photos", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace(dir): %v", err)
	}
	if tr.Kind != "dir" || tr.Meta == nil || tr.Meta.RealPath == "" {
		t.Fatalf("目录落点异常: kind=%s meta=%+v", tr.Kind, tr.Meta)
	}
	if shardseal.ClassifyName(path.Base(tr.Meta.RealPath)) != shardseal.KindDirMeta {
		t.Fatalf("目录 meta 文件名 %q 不是 dir-meta（q 标记）", path.Base(tr.Meta.RealPath))
	}

	// 卷根同样可 trace（根容器）。
	root, rerr := fsys.Trace(ctx, "", volume.InspectIndex)
	if rerr != nil || root.Kind != "dir" || root.Meta == nil {
		t.Fatalf("Trace(root) = %+v, %v", root, rerr)
	}
}

// TestInspect_ExplainContainer 反向：容器内逐条用途与逻辑归属。
func TestInspect_ExplainContainer(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	writeFileT(t, fsys, "docs/a.txt", data(300))

	tr, err := fsys.Trace(ctx, "docs/a.txt", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	container := path.Dir(tr.Meta.RealPath)

	ex, err := fsys.Explain(ctx, container, volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(container): %v", err)
	}
	if ex.Kind != "container" {
		t.Fatalf("kind = %s, want container", ex.Kind)
	}
	if ex.Container == nil || ex.Container.LogicalDir != "docs" {
		t.Fatalf("容器逻辑目录 = %+v, want docs", ex.Container)
	}
	var sawDirMeta, sawFileMeta, sawChunk int
	for _, e := range ex.Entries {
		switch e.Kind {
		case "dir_meta":
			sawDirMeta++
		case "file_meta":
			sawFileMeta++
			if e.Owner == nil || e.Owner.Rel != "docs/a.txt" {
				t.Fatalf("file_meta 归属 = %+v, want docs/a.txt", e.Owner)
			}
		case "chunk":
			sawChunk++
			if e.Owner == nil || e.Owner.Rel != "docs/a.txt" || e.Owner.ChunkIndex == nil {
				t.Fatalf("chunk 归属 = %+v, want docs/a.txt + chunk_index", e.Owner)
			}
		}
	}
	if sawDirMeta != 1 || sawFileMeta != 1 || sawChunk == 0 {
		t.Fatalf("条目分类计数 dir=%d file=%d chunk=%d", sawDirMeta, sawFileMeta, sawChunk)
	}
}

// TestInspect_ExplainContainerSubdirIsContainer 锁「子目录 = 容器，不得误报为孤儿分块」。
//
// 真实数据实测发现：容器内既有 meta/分块，也有**子容器目录**；若把目录当 blob 判定，
// 根容器里的每个子目录都会被报成 blob_orphan（且大小 0 B），审查结论完全失真。
func TestInspect_ExplainContainerSubdirIsContainer(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	if err := fsys.MakeDir(ctx, "parent"); err != nil {
		t.Fatalf("MakeDir parent: %v", err)
	}
	if err := fsys.MakeDir(ctx, "parent/child"); err != nil {
		t.Fatalf("MakeDir child: %v", err)
	}
	writeFileT(t, fsys, "parent/child/f.txt", data(40))

	root, err := fsys.Trace(ctx, "", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace(root): %v", err)
	}
	rootContainer := path.Dir(root.Meta.RealPath)
	ex, err := fsys.Explain(ctx, rootContainer, volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(root container): %v", err)
	}
	var containers, orphans int
	for _, e := range ex.Entries {
		if e.Kind == "container" {
			containers++
			if e.Container == nil || e.Container.LogicalDir != "parent" {
				t.Fatalf("子容器归属 = %+v, want parent", e.Container)
			}
			if e.Note == "" || strings.Contains(e.Note, "孤儿") {
				t.Fatalf("子容器文案不应含孤儿: %q", e.Note)
			}
		}
	}
	for _, a := range ex.Anomalies {
		if a.Code == volume.AnomalyBlobOrphan {
			orphans++
		}
	}
	if containers != 1 {
		t.Fatalf("子容器数 = %d, want 1（entries=%+v）", containers, ex.Entries)
	}
	if orphans != 0 {
		t.Fatalf("子目录被误报为孤儿 %d 次: %+v", orphans, ex.Anomalies)
	}
}

// TestInspect_ExplainChunkVsParity 是**关键**用例：parity 与普通分块命名同形，
// 只能靠 meta 判定（变异：按名字/最后一个分块猜 ⇒ 本测红）。
func TestInspect_ExplainChunkVsParity(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := syncpkg.NewLocalFS(root, nil)
	fsys, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Erasure:   true,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	writeFileT(t, fsys, "v.mp4", data(600))

	tr, err := fsys.Trace(ctx, "v.mp4", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	if tr.Parity == nil {
		t.Fatal("Erasure 写入应产生 parity 落点")
	}
	if tr.Parity.RealPath == tr.Chunks[0].Blob.RealPath {
		t.Fatal("parity 与分块落点不应相同")
	}

	ex, err := fsys.Explain(ctx, tr.Parity.RealPath, volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(parity): %v", err)
	}
	if ex.Kind != "parity" {
		t.Fatalf("kind = %s, want parity（不能按名字猜测）", ex.Kind)
	}
	if ex.Owner == nil || ex.Owner.Rel != "v.mp4" || !ex.Owner.Parity {
		t.Fatalf("parity 归属 = %+v, want v.mp4 + Parity", ex.Owner)
	}

	// 普通分块必须判为 chunk（不能误判为 parity）。
	ex2, err := fsys.Explain(ctx, tr.Chunks[0].Blob.RealPath, volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(chunk): %v", err)
	}
	if ex2.Kind != "chunk" || ex2.Owner == nil || ex2.Owner.Parity {
		t.Fatalf("chunk 解释 = kind=%s owner=%+v", ex2.Kind, ex2.Owner)
	}
}

// TestInspect_ExplainOrphan 反向：无索引引用的 blob ⇒ blob_orphan。
func TestInspect_ExplainOrphan(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	writeFileT(t, fsys, "a/b.txt", data(80))
	tr, err := fsys.Trace(ctx, "a/b.txt", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Trace: %v", err)
	}
	container := path.Dir(tr.Meta.RealPath)

	// 造一个「形状合法但无引用」的分块名（不含 q/z 标记）。
	orphan := strings.Repeat("a1b2c3d4e5", 3)[:30]
	if werr := fsys.inner.WriteFile(ctx, syncpkg.LogicalKey{}, path.Join(container, orphan), bytes.NewReader(data(32)), 32, 0); werr != nil {
		t.Fatalf("写孤儿 blob: %v", werr)
	}
	ex, err := fsys.Explain(ctx, path.Join(container, orphan), volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(orphan): %v", err)
	}
	if ex.Owner != nil {
		t.Fatalf("孤儿不应有归属: %+v", ex.Owner)
	}
	if !hasAnomaly(ex.Anomalies, volume.AnomalyBlobOrphan) {
		t.Fatalf("缺少 blob_orphan 差异码: %+v", ex.Anomalies)
	}
	if !strings.Contains(anomalyDetail(ex.Anomalies, volume.AnomalyBlobOrphan), "GC") {
		t.Fatal("孤儿文案必须指向 GC（不得暗示可直接删除）")
	}
}

// TestInspect_ExplainRejectsEscape 安全：越界路径必须拒绝。
func TestInspect_ExplainRejectsEscape(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	for _, p := range []string{"../other", "a/../../b", "/etc/passwd", `C:/Windows`, `a\..\..\b`} {
		_, err := fsys.Explain(context.Background(), p, volume.InspectIndex)
		if !errors.Is(err, volume.ErrOutsideVolume) {
			t.Fatalf("Explain(%q) err = %v, want ErrOutsideVolume", p, err)
		}
	}
}

// TestInspect_ExplainNotFound 审查工具必须解释「这里为什么是空的」（不是错误）。
func TestInspect_ExplainNotFound(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ex, err := fsys.Explain(context.Background(), "no/such/blob", volume.InspectIndex)
	if err != nil {
		t.Fatalf("Explain(notfound) 应返回解释而不是错误: %v", err)
	}
	if ex.Kind != "unknown" || !hasAnomaly(ex.Anomalies, volume.AnomalyNotFound) {
		t.Fatalf("not_found 解释异常: kind=%s anomalies=%+v", ex.Kind, ex.Anomalies)
	}
}

// TestInspect_DiskDetectsMissingAndMismatch disk 模式必须暴露真实差异。
func TestInspect_DiskDetectsMissingAndMismatch(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	ctx := context.Background()
	writeFileT(t, fsys, "x.bin", data(400))
	tr, err := fsys.Trace(ctx, "x.bin", volume.InspectIndex)
	if err != nil || len(tr.Chunks) < 2 {
		t.Fatalf("前置失败: %v (%d chunks)", err, len(tr.Chunks))
	}

	// ① 截断一个分块 ⇒ size_mismatch。
	victim := tr.Chunks[0].Blob.RealPath
	short := data(8)
	if err := fsys.inner.WriteFile(ctx, syncpkg.LogicalKey{}, victim, bytes.NewReader(short), int64(len(short)), 0); err != nil {
		t.Fatalf("截断 blob: %v", err)
	}
	// ② 删掉另一个分块 ⇒ blob_missing。
	if err := fsys.inner.Delete(ctx, tr.Chunks[1].Blob.RealPath); err != nil {
		t.Fatalf("删除 blob: %v", err)
	}

	trDisk, derr := fsys.Trace(ctx, "x.bin", volume.InspectDisk)
	if derr != nil {
		t.Fatalf("Trace(disk): %v", derr)
	}
	if !hasAnomaly(trDisk.Anomalies, volume.AnomalySizeMismatch) {
		t.Fatalf("缺少 size_mismatch: %+v", trDisk.Anomalies)
	}
	if !hasAnomaly(trDisk.Anomalies, volume.AnomalyBlobMissing) {
		t.Fatalf("缺少 blob_missing: %+v", trDisk.Anomalies)
	}
}

// TestInspect_TraceMissing 不存在的逻辑路径 ⇒ fs.ErrNotExist（不是空结果）。
func TestInspect_TraceMissing(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	if _, err := fsys.Trace(context.Background(), "nope/x", volume.InspectIndex); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

// TestAssertStorageInspector 能力探测：实现者 true，其它 false。
func TestAssertStorageInspector(t *testing.T) {
	t.Parallel()
	fsys := newFS(t)
	if _, ok := volume.AssertStorageInspector(fsys); !ok {
		t.Fatal("SecretdataFS 必须实现 StorageInspector")
	}
	if _, ok := volume.AssertStorageInspector(struct{}{}); ok {
		t.Fatal("非实现者不应被判定为支持")
	}
}

func hasAnomaly(list []volume.Anomaly, code string) bool {
	for _, a := range list {
		if a.Code == code {
			return true
		}
	}
	return false
}

func anomalyDetail(list []volume.Anomaly, code string) string {
	for _, a := range list {
		if a.Code == code {
			return a.Detail
		}
	}
	return ""
}
