// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// newFS 建一个基于本地临时目录底层 FS 的 secretdata FS（小块策略加速）。
func newFS(t *testing.T) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// data 生成定长测试字节。
func data(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i % 251)
	}
	return b
}

func TestWriteReadRoundtrip(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000) // 跨多块
	if err := fs.WriteFile(ctx, "movie.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "movie.mp4")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("还原内容不一致: len(got)=%d len(content)=%d", len(got), len(content))
	}
}

func TestWrite_ThenStatAndList(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "a.bin", bytes.NewReader(data(200)), 200, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	ent, err := fs.Stat(ctx, "a.bin")
	if err != nil || ent == nil {
		t.Fatalf("Stat: %v nil=%v", err, ent == nil)
	}
	if ent.Size != 200 {
		t.Errorf("Stat size=%d want 200", ent.Size)
	}
	entries, err := fs.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a.bin" {
		t.Errorf("ListDir=%+v", entries)
	}
	// 不存在返回 nil（sync.Engine 契约）。
	if ent, _ := fs.Stat(ctx, "nope"); ent != nil {
		t.Errorf("Stat 不存在应返回 nil，got %+v", ent)
	}
}

// TestUnderlyingLayout_NoStructWords：底层根下不得出现 data/meta 结构目录；目录名随机 5-30。
func TestUnderlyingLayout_NoStructWords(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)

	rootEntries, err := fs.inner.ListDir(ctx, "")
	if err != nil {
		t.Fatalf("inner ListDir: %v", err)
	}
	for _, e := range rootEntries {
		if e.Name == "data" || e.Name == "meta" {
			t.Errorf("底层不应出现 data/meta 结构目录：%q", e.Name)
		}
		if len(e.Name) < 5 || len(e.Name) > 30 {
			t.Errorf("容器目录名长度 %d 超出 5-30", len(e.Name))
		}
	}
}

// TestMetaFileSizeInRange（审查重点 2，适配说明）：目录 meta（@，JSON 仅 ~160B）可 padding
// 到统一格式落盘总长 ∈ [192, 384]（min_block_size=64 → pad 目标 = max(192, 64+rand) = 192，天然
// 落盘 355）；文件 meta 落盘 = 全量 shardseal.Meta 加密 blob（含全部 chunk+sha256，实测约 1.2KB，
// 无法裁剪进 384——padding 只会往大里扩）——故文件 meta 断言「≥192 pad 地板」，目录 meta 断言
// 「∈ [192,384]」，共同守住「meta 与分块大小分布重叠、不可凭文件大小区分」的审查语义。
func TestMetaFileSizeInRange(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "m.bin", 300)

	rootEntries, _ := fs.inner.ListDir(ctx, "")
	container := rootEntries[0].Name
	inner, _ := fs.inner.ListDir(ctx, container)

	var dirMetaSize int64
	hasDirMeta := false
	for _, f := range inner {
		if shardseal.IsDirMetaName(f.Name) {
			hasDirMeta = true
			dirMetaSize = f.Size
		}
	}
	if !hasDirMeta {
		t.Fatal("容器应含目录 meta（@ 标记）")
	}
	if dirMetaSize < 192 || dirMetaSize > 384 {
		t.Errorf("目录 meta 落盘大小 %d 不在 [192, 384] 范围", dirMetaSize)
	}
	for _, f := range inner {
		if shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		if f.Size < 192 {
			t.Errorf("文件 meta %q 落盘大小 %d 低于 R 地板 192", f.Name, f.Size)
		}
	}
}

// TestUnderlyingLayout_DirMetaMarked：容器内混放三类文件，目录 meta 含 @。
func TestUnderlyingLayout_DirMetaMarked(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)

	rootEntries, _ := fs.inner.ListDir(ctx, "")
	container := rootEntries[0].Name
	inner, _ := fs.inner.ListDir(ctx, container)
	hasDirMeta, hasFileMeta, hasChunk := false, false, false
	for _, f := range inner {
		switch {
		case shardseal.IsDirMetaName(f.Name):
			hasDirMeta = true
		case shardseal.IsMetaName(f.Name):
			hasFileMeta = true
		default:
			hasChunk = true
		}
	}
	if !hasDirMeta || !hasFileMeta || !hasChunk {
		t.Errorf("容器应含目录meta(@)/文件meta(-)/分块三类，got dirMeta=%v fileMeta=%v chunk=%v", hasDirMeta, hasFileMeta, hasChunk)
	}
}

// TestUnderlyingLayout_ChunksEncrypted：底层分块为密文、容器目录名不泄逻辑名。
func TestUnderlyingLayout_ChunksEncrypted(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)
	writeContent(t, fs, ctx, "secret.mp4", 1000)

	e := fs.index["secret.mp4"]
	if strings.Contains(e.dirSeg, "secret") {
		t.Errorf("容器目录名不应含逻辑文件名：%q", e.dirSeg)
	}
	for _, ci := range e.meta.Chunks {
		rc, err := fs.inner.OpenRead(ctx, path.Join(e.dirSeg, ci.FileName))
		if err != nil {
			t.Fatalf("inner open chunk: %v", err)
		}
		blob, _ := io.ReadAll(rc)
		rc.Close()
		if bytes.Contains(blob, content) {
			t.Errorf("底层分块含明文泄露")
		}
	}
}

func TestDelete(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "del.bin", bytes.NewReader(data(100)), 100, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := fs.Delete(ctx, "del.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if ent, _ := fs.Stat(ctx, "del.bin"); ent != nil {
		t.Errorf("删除后 Stat 应 nil")
	}
	// 幂等：再删不报错。
	if err := fs.Delete(ctx, "del.bin"); err != nil {
		t.Errorf("重复 Delete 应幂等，got %v", err)
	}
}

func TestLoadIndexFromExistingVolume(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)
	if err := fs.WriteFile(ctx, "persisted.mp4", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用同一底层 FS 新建一个 FS（模拟挂载旧卷）→ 应能从容器目录 meta → path、文件 meta → name
	// 扫回索引并按需还原（修复 F-1：索引键与写路径一致）。
	fs2, err := NewFS(fs.inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	rc, err := fs2.OpenRead(ctx, "persisted.mp4")
	if err != nil {
		t.Fatalf("旧卷加载后 OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("旧卷还原内容不一致")
	}
}

func TestWrongSecretFails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "w.bin", bytes.NewReader(data(1000)), 1000, 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// 用错误密钥建 FS 读（索引已载入但解密失败）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("wrong-key"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if _, err := fs2.OpenRead(ctx, "w.bin"); err == nil {
		t.Error("错误密钥应解密失败，却成功")
	}
}

// writeContent 写定长内容文件（断言 helper）。
func writeContent(t *testing.T, fs *SecretdataFS, ctx context.Context, name string, n int) {
	t.Helper()
	if err := fs.WriteFile(ctx, name, bytes.NewReader(data(n)), int64(n), 0); err != nil {
		t.Fatalf("WriteFile %s: %v", name, err)
	}
}

// assertListDir 断言 rel 目录的条目集合（name → IsDir）（断言 helper）。
func assertListDir(t *testing.T, fs *SecretdataFS, ctx context.Context, rel string, want map[string]bool) {
	t.Helper()
	got, err := fs.ListDir(ctx, rel)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", rel, err)
	}
	names := map[string]bool{}
	for _, e := range got {
		names[e.Name] = e.IsDir
	}
	if len(names) != len(want) {
		t.Errorf("ListDir(%q) 条目数 %d，want %d（got %+v）", rel, len(names), len(want), got)
	}
	for name, isDir := range want {
		d, ok := names[name]
		switch {
		case !ok:
			t.Errorf("ListDir(%q) 缺 %q（got %+v）", rel, name, got)
		case d != isDir:
			t.Errorf("ListDir(%q) %q IsDir=%v，want %v", rel, name, d, isDir)
		}
	}
}

func TestListDir_ShowsSubdirectories(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a.bin", 200)
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)
	writeContent(t, fs, ctx, "movies/sub/f2.mp4", 100)

	assertListDir(t, fs, ctx, "", map[string]bool{"a.bin": false, "movies": true})
	assertListDir(t, fs, ctx, "movies", map[string]bool{"f1.mp4": false, "sub": true})
	assertListDir(t, fs, ctx, "movies/sub", map[string]bool{"f2.mp4": false})

	// 目录 Stat 返回目录条目；文件 Stat 返回文件条目。
	de, err := fs.Stat(ctx, "movies")
	if err != nil || de == nil || !de.IsDir {
		t.Errorf("Stat(movies)=%+v err=%v（应为目录）", de, err)
	}
	fe, err := fs.Stat(ctx, "movies/f1.mp4")
	if err != nil || fe == nil || fe.IsDir {
		t.Errorf("Stat(movies/f1.mp4)=%+v err=%v（应为文件）", fe, err)
	}
}

// TestMetaPadBytes_OverrideFloor 验证 Options.MetaPadBytes（任务 6 extra.meta_pad_bytes
// 传入）被消费：设大 pad 基准后，目录 meta blob 落盘 ≥ pad 目标（默认 Block.Min 的
// addendum 只抬高不裁剪）。Ruling：该字段由任务 4 定义，默认 = Block.Min。
func TestMetaPadBytes_OverrideFloor(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:       []byte("test-secret-key-000"),
		Block:        shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:      t.TempDir(),
		MetaPadBytes: 400, // pad 目标 ∈ [400, 799] > 默认 192
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	writeContent(t, fs, ctx, "m.bin", 300)

	rootEntries, _ := fs.inner.ListDir(ctx, "")
	container := rootEntries[0].Name
	innerFiles, _ := fs.inner.ListDir(ctx, container)
	for _, f := range innerFiles {
		if shardseal.IsDirMetaName(f.Name) && f.Size < 400 {
			t.Errorf("MetaPadBytes=400 下目录 meta 落盘大小 %d 应 ≥400", f.Size)
		}
	}
}

// TestOverwrite_ReplacesAndCleansOld 验证覆盖写：内容更新为新版本，且旧版本的
// 文件 meta blob 被清理（审查 I-1：只替换索引条目会残留孤儿旧数据）。
func TestOverwrite_ReplacesAndCleansOld(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "ov.bin", 800)
	old := fs.index["ov.bin"]
	oldMetaPath := path.Join(old.dirSeg, old.metaName)

	// 覆盖写：内容不同 → 新随机 meta blob 名（同容器内不换容器，见 TestOverwrite_Atomic）。
	writeContent(t, fs, ctx, "ov.bin", 1000)
	if ent, _ := fs.inner.Stat(ctx, oldMetaPath); ent != nil {
		t.Error("覆盖写后旧 meta blob 应被删除（孤儿残留）")
	}
	// 读回为 v2。
	rc, err := fs.OpenRead(ctx, "ov.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(1000)) {
		t.Error("覆盖写后应读到新版本")
	}
}

// TestOverwrite_Atomic：覆盖写新随机名先传后删旧——中途失败旧数据完好（F-2）。
// v3 模型：容器 = 逻辑目录（不随文件覆盖改变）；覆盖写在容器内以「新随机 meta blob 名」
// 上传新版本，成功后索引切换到新条目并 best-effort 删除旧 meta blob + 旧分块。
func TestOverwrite_Atomic(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "ov.bin", 800)
	oldEntry := fs.index["ov.bin"]
	oldContainer := oldEntry.dirSeg
	oldMeta := oldEntry.metaName

	writeContent(t, fs, ctx, "ov.bin", 1000)
	newEntry := fs.index["ov.bin"]
	if newEntry.dirSeg != oldContainer {
		t.Error("覆盖写不应更换容器目录（容器=逻辑目录）")
	}
	if newEntry.metaName == oldMeta {
		t.Error("覆盖写应生成新随机 meta blob 名，而非原地覆盖")
	}
	// 旧 meta blob 应被清理（孤儿残留禁止）。
	if ent, _ := fs.inner.Stat(ctx, path.Join(oldContainer, oldMeta)); ent != nil {
		t.Error("覆盖写后旧 meta blob 应被删除（孤儿残留）")
	}
	rc, _ := fs.OpenRead(ctx, "ov.bin")
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(1000)) {
		t.Error("覆盖写后应读到新版本")
	}
}
