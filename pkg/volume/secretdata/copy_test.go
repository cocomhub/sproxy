// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// copy_test.go 钉住**同卷内复制**（用户裁定 2026-10-13：不重新加解密，最多调整新文件的 meta）：
//
//   - 同容器复制 = **只写一份新 meta**（零 blob 写、零加解密）⇒ 磁盘 blob 集合完全不变；
//   - 跨容器复制 = **字节拷贝**分块（内容零改动 ⇒ 副本分块字节与源逐字节相同，未重加密）；
//   - 共享引用为**存在即引用**：删源或删副本都不影响另一方；最后一个引用者删除时才清分块；
//   - **重启**后共享关系由索引重建恢复 ⇒ 删除语义仍正确。
//
// 变异：同容器分支改走字节拷贝 → 「blob 集合不变」断言红；删掉存在性判定 → 「删一方另一方仍可读」
// 断言红。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// writeCountingFS 统计底层 WriteFile 调用（用于断言「同容器复制 = 只写一份 meta，零 blob 写」）。
type writeCountingFS struct {
	syncpkg.FS
	mu     sync.Mutex
	writes []string
}

func (w *writeCountingFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	w.mu.Lock()
	w.writes = append(w.writes, p)
	w.mu.Unlock()
	return w.FS.WriteFile(ctx, key, p, r, size, mtime)
}

func (w *writeCountingFS) takeWrites() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := w.writes
	w.writes = nil
	return out
}

// newCountingFS 构造带写统计的 secretdata FS（同 newFS 口径）。
func newCountingFS(t *testing.T) (*SecretdataFS, *writeCountingFS) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	cnt := &writeCountingFS{FS: syncpkg.NewLocalFS(root, nil)}
	fs, err := NewFS(cnt, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs, cnt
}

func TestCopyFile_SameContainerMetaOnly(t *testing.T) {
	t.Parallel()
	fs, cnt := newCountingFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 2048)

	before := snapshotBackingBlobs(t, fs)
	content := readAll(t, fs, ctx, "a/f1.bin")
	_ = cnt.takeWrites() // 清空写统计（只统计复制期间的写）

	if err := fs.CopyFile(ctx, "a/f1.bin", "a/f2.bin"); err != nil {
		t.Fatalf("同容器复制应成功（只写 meta）: %v", err)
	}
	// **零 blob 写**：复制期间底层只应有 1 次写，且是**文件 meta**（z 标记）。
	writes := cnt.takeWrites()
	if len(writes) != 1 {
		t.Fatalf("同容器复制应只有 1 次底层写（新 meta），got %d: %v", len(writes), writes)
	}
	if shardseal.ClassifyName(filepath.Base(writes[0])) != shardseal.KindFileMeta {
		t.Fatalf("唯一写入应是文件 meta blob, got %s", writes[0])
	}
	after := snapshotBackingBlobs(t, fs)
	// 分块 blob（内容）必须**逐个字节不变**：同名同内容、不得新增或消失。
	for name, sum := range before.content {
		got, ok := after.content[name]
		if !ok {
			t.Errorf("复制不得删/改内容分块 %s（同容器应原地共享）", name)
			continue
		}
		if got != sum {
			t.Errorf("复制不得改写内容分块 %s", name)
		}
	}
	for name := range after.content {
		if _, ok := before.content[name]; !ok {
			t.Errorf("同容器复制不应新增内容分块 %s（零 blob 写）", name)
		}
	}
	if len(after.meta) != len(before.meta)+1 {
		t.Errorf("meta blob 数应 +1（新文件一份 meta）: before=%d after=%d", len(before.meta), len(after.meta))
	}
	if got := readAll(t, fs, ctx, "a/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("副本内容不一致: len=%d want=%d", len(got), len(content))
	}

	// 存在即引用：删**源** → 副本仍可读且分块仍在。
	if err := fs.Delete(ctx, "a/f1.bin"); err != nil {
		t.Fatalf("Delete(源): %v", err)
	}
	if got := readAll(t, fs, ctx, "a/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("删除源后副本内容不一致: len=%d", len(got))
	}
	if e, _ := fs.Stat(ctx, "a/f1.bin"); e != nil {
		t.Error("删除后源不应可解析")
	}
	// 删除副本（最后一个引用者）→ 分块清空。
	if err := fs.Delete(ctx, "a/f2.bin"); err != nil {
		t.Fatalf("Delete(副本): %v", err)
	}
	if rest := snapshotBackingBlobs(t, fs); len(rest.content) != 0 {
		t.Errorf("最后一个引用者删除后应无内容分块残留, got %v", rest.content)
	}
}

func TestCopyFile_CrossContainerCopiesBlobsBytewise(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 1024)
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	src := fs.index["a/f1.bin"]
	before := snapshotBackingBlobs(t, fs)
	content := readAll(t, fs, ctx, "a/f1.bin")

	if err := fs.CopyFile(ctx, "a/f1.bin", "b/f2.bin"); err != nil {
		t.Fatalf("跨容器复制: %v", err)
	}
	if got := readAll(t, fs, ctx, "b/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("副本内容不一致: len=%d", len(got))
	}
	// 副本的分块字节必须与源的**逐字节相同**（内容零改动 ⇒ 未重新加密；重新加密会换盐 ⇒ 字节不同）。
	dst := fs.index["b/f2.bin"]
	if dst.dirSeg == src.dirSeg {
		t.Fatal("跨容器复制应落到目标目录容器")
	}
	for i := range src.meta.Chunks {
		sb, err := readBlob(ctx, fs.inner, src.dirSeg+"/"+src.meta.Chunks[i].FileName)
		if err != nil {
			t.Fatalf("读源分块: %v", err)
		}
		db, derr := readBlob(ctx, fs.inner, dst.dirSeg+"/"+dst.meta.Chunks[i].FileName)
		if derr != nil {
			t.Fatalf("读目标分块: %v", derr)
		}
		if !bytes.Equal(sb, db) {
			t.Fatalf("分块 %d 字节不同（说明重新加密了）", i)
		}
	}
	// 独立性：删源不影响副本（跨容器是各自独立的 blob）。
	for name := range before.content {
		_ = name
	}
	if err := fs.Delete(ctx, "a/f1.bin"); err != nil {
		t.Fatalf("Delete(源): %v", err)
	}
	if got := readAll(t, fs, ctx, "b/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("删除源后副本应独立可读, len=%d", len(got))
	}
	// 删副本 → 其分块清空（源已删，故最终无内容分块）。
	if err := fs.Delete(ctx, "b/f2.bin"); err != nil {
		t.Fatalf("Delete(副本): %v", err)
	}
	if rest := snapshotBackingBlobs(t, fs); len(rest.content) != 0 {
		t.Errorf("全部删除后应无内容分块残留, got %v", rest.content)
	}
}

func TestCopyFile_RestartKeepsSharingSafe(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 512)
	if err := fs.CopyFile(ctx, "a/f1.bin", "a/f2.bin"); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	content := readAll(t, fs, ctx, "a/f1.bin")

	// 重启（重开同一底层）：两份条目与共享关系由索引重建恢复。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS(重开): %v", err)
	}
	for _, rel := range []string{"a/f1.bin", "a/f2.bin"} {
		if got := readAll(t, fs2, ctx, rel); !bytes.Equal(got, content) {
			t.Fatalf("重启后 %s 内容不一致: len=%d", rel, len(got))
		}
	}
	// 重启后删除语义仍正确：删一份，另一份仍可读（存在即引用由索引天然恢复）。
	if derr := fs2.Delete(ctx, "a/f1.bin"); derr != nil {
		t.Fatalf("重启后 Delete: %v", derr)
	}
	if got := readAll(t, fs2, ctx, "a/f2.bin"); !bytes.Equal(got, content) {
		t.Fatalf("重启后删源，副本应仍可读, len=%d", len(got))
	}
}

// TestCopyFile_Conflicts：目标已存在（文件/目录）与源不存在 → 明确报错，不落半态。
func TestCopyFile_Conflicts(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 128)
	writeContent(t, fs, ctx, "a/f3.bin", 128)
	if err := fs.MakeDir(ctx, "a/dir1"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := fs.CopyFile(ctx, "a/f1.bin", "a/f3.bin"); err == nil {
		t.Error("目标已是文件应报错")
	}
	if err := fs.CopyFile(ctx, "a/f1.bin", "a/dir1"); err == nil {
		t.Error("目标已是目录应报错")
	}
	if err := fs.CopyFile(ctx, "a/missing.bin", "a/x.bin"); err == nil {
		t.Error("源不存在应报错")
	}
	if err := fs.CopyFile(ctx, "a/f1.bin", "nope/x.bin"); err == nil {
		t.Error("目标父目录不存在应报错")
	}
	// 失败不留半态：目标不存在、源内容完好。
	if e, _ := fs.Stat(ctx, "a/x.bin"); e != nil {
		t.Error("失败后不应产生目标")
	}
	if got := readAll(t, fs, ctx, "a/f1.bin"); len(got) != 128 {
		t.Errorf("失败后源内容应完好, len=%d", len(got))
	}
}
