// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"encoding/json"
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
// 到统一格式落盘总长 ∈ [196, 384]（min_block_size=64 → pad 目标 = max(196, 64+rand) = 196，天然
// 落盘 360）；文件 meta 落盘 = 全量 shardseal.Meta 加密 blob（含全部 chunk+sha256，实测约 1.2KB，
// 无法裁剪进 384——padding 只会往大里扩）——故文件 meta 断言「≥196 pad 地板」，目录 meta 断言
// 「∈ [196,384]」，共同守住「meta 与分块大小分布重叠、不可凭文件大小区分」的审查语义。
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
	if dirMetaSize < 196 || dirMetaSize > 384 {
		t.Errorf("目录 meta 落盘大小 %d 不在 [196, 384] 范围", dirMetaSize)
	}
	for _, f := range inner {
		if shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		if f.Size < 196 {
			t.Errorf("文件 meta %q 落盘大小 %d 低于 R 地板 196", f.Name, f.Size)
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

// TestOpenRangeRead 验证随机读取：OpenRangeRead(rel, offset, size) 只返回目标区间明文
// 内容正确（含跨 blocklet 与跨块区间）。仅覆盖 register 密文的 blocklet 段被解密还原。
func TestOpenRangeRead(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:  []byte("test-secret-key-000"),
		Block:   shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMin: 16, BlockletMax: 32},
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	content := data(300)
	if err := fs.WriteFile(ctx, "r.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cases := []struct {
		name                string
		offset, size        int64
		wantOffset, wantLen int
	}{
		{"块首区间", 0, 50, 0, 50},
		{"跨 blocklet 区间", 20, 100, 20, 100},
		{"跨块区间", 0, 300, 0, 300},
		{"块尾区间", 250, 50, 250, 50},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			rc, rerr := fs.OpenRangeRead(ctx, "r.bin", c.offset, c.size)
			if rerr != nil {
				t.Fatalf("OpenRangeRead: %v", rerr)
			}
			got, rerr2 := io.ReadAll(rc)
			rc.Close()
			if rerr2 != nil {
				t.Fatalf("ReadAll: %v", rerr2)
			}
			want := content[c.wantOffset : c.wantOffset+c.wantLen]
			if !bytes.Equal(got, want) {
				t.Errorf("区间 [%d,%d) 内容不一致：len(got)=%d", c.wantOffset, c.wantOffset+c.wantLen, len(got))
			}
		})
	}

	// 越界区间 / 大小非正 → 错误（fail-closed）。
	if _, err := fs.OpenRangeRead(ctx, "r.bin", 0, -1); err == nil {
		t.Error("size<0 应报错")
	}
	if _, err := fs.OpenRangeRead(ctx, "r.bin", 0, 1000); err == nil {
		t.Error("区间越出文件应报错")
	}
	if _, err := fs.OpenRangeRead(ctx, "missing.bin", 0, 10); err == nil {
		t.Error("不存在文件应报错")
	}
}

// TestOpenRangeRead_DirFails：对目录路径随机读应报错。
func TestOpenRangeRead_DirFails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "d/f.bin", 100)
	if _, err := fs.OpenRangeRead(ctx, "d", 0, 10); err == nil {
		t.Error("目录随机读应报错")
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

// TestFileMetaNameHashSegmentsAlignBlob（I1 回归）：文件 meta 名三段哈希必须锚定
// **实际加密落盘 blob**——首段 = meta 明文 JSON 哈希前 16，中段 = 原始总校验和前 16，
// 末段 = 实际（含 padding）密文 blob 哈希前 16。不得复用 EncryptShards 任务 3 的
// out.MetaName（其末段对应未 padding 的旧 blob，会破坏名称完整性锚定）。
func TestFileMetaNameHashSegmentsAlignBlob(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "m.bin", 300)

	root, _ := fs.inner.ListDir(ctx, "")
	inner, _ := fs.inner.ListDir(ctx, root[0].Name)
	found := 0
	for _, f := range inner {
		if shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		found++
		blob, err := readBlob(ctx, fs.inner, path.Join(root[0].Name, f.Name))
		if err != nil {
			t.Fatalf("read meta blob: %v", err)
		}
		// 末段 = 实际 blob 哈希前 16。
		wantEnc, _ := shardseal.Hash16(blob)
		if got := f.Name[len(f.Name)-16:]; got != wantEnc {
			t.Errorf("文件 meta 名末段 %q 与实际上传 blob 哈希 %q 不符（名称完整性锚定被破坏）", got, wantEnc)
		}
		// 首段 = meta 明文 JSON 哈希前 16。
		mm, err := fs.decryptFileMeta(blob)
		if err != nil {
			t.Fatalf("decryptFileMeta: %v", err)
		}
		metaJSON, _ := json.Marshal(mm)
		wantOrig, _ := shardseal.Hash16(metaJSON)
		if got := f.Name[:16]; got != wantOrig {
			t.Errorf("文件 meta 名首段 %q 与明文 JSON 哈希 %q 不符", got, wantOrig)
		}
		// 中段 = 原始总校验和前 16（embedded run）。
		if len(mm.Original.SHA256) >= 16 && !strings.Contains(f.Name, mm.Original.SHA256[:16]) {
			t.Errorf("文件 meta 名未包含原始总校验和前 16 %q", mm.Original.SHA256[:16])
		}
	}
	if found == 0 {
		t.Fatal("容器内未找到文件 meta 条目")
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
		MetaPadBytes: 400, // pad 目标 ∈ [400, 799] > 默认 196
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

// TestLoadIndex_RestoresDirTree：重启后从目录meta.path+文件meta.basename重建完整路径树
// （修复 F-1：子目录不丢失、不同目录同名文件不冲突）。
func TestLoadIndex_RestoresDirTree(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a.bin", 200)
	writeContent(t, fs, ctx, "movies/sub/f1.mp4", 300)
	writeContent(t, fs, ctx, "docs/sub/f1.mp4", 100) // 同名 basename 不同目录

	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	assertListDir(t, fs2, ctx, "", map[string]bool{"a.bin": false, "movies": true, "docs": true})
	assertListDir(t, fs2, ctx, "movies/sub", map[string]bool{"f1.mp4": false})
	assertListDir(t, fs2, ctx, "docs/sub", map[string]bool{"f1.mp4": false})
	// 两个同名文件都能读回各自内容
	rc, _ := fs2.OpenRead(ctx, "movies/sub/f1.mp4")
	m1, _ := io.ReadAll(rc)
	rc.Close()
	rc2, _ := fs2.OpenRead(ctx, "docs/sub/f1.mp4")
	m2, _ := io.ReadAll(rc2)
	rc2.Close()
	if bytes.Equal(m1, m2) {
		t.Error("不同目录同名文件应读出不同内容")
	}
}

// TestDirMove_UpdatesMetaOnly：目录移动仅更新目录meta.path，文件可解析（审查重点5）。
// 判定（设计 §6.2）：索引键 = 完整逻辑 rel —— 移动后旧键删除、新键存在且指向同一
// 物理条目（dirSeg/metaName 不变，文件 meta/分块零改动）。
func TestDirMove_UpdatesMetaOnly(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "old/f1.mp4", 200)
	oldDir := fs.index["old/f1.mp4"].dirSeg
	oldMeta := fs.index["old/f1.mp4"].metaName
	if err := fs.Rename(ctx, "old", "new"); err != nil {
		t.Fatalf("Rename 目录: %v", err)
	}
	newEntry := fs.index["new/f1.mp4"]
	if newEntry == nil {
		t.Fatal("移动后新键 index[new/f1.mp4] 应存在")
	}
	if newEntry.dirSeg != oldDir || newEntry.metaName != oldMeta {
		t.Error("目录移动不应改动文件 meta/分块")
	}
	if _, ok := fs.index["old/f1.mp4"]; ok {
		t.Error("移动后旧键 index[old/f1.mp4] 应已删除（幽灵映射/split-brain 禁止）")
	}
	if _, ok := fs.dirs["new"]; !ok {
		t.Error("移动后新目录应存在")
	}
	assertListDir(t, fs, ctx, "new", map[string]bool{"f1.mp4": false})
	_, oldStillDir := fs.dirs["old"]
	if _, err := fs.Stat(ctx, "old"); err != nil || oldStillDir {
		t.Error("移动后旧目录不应存在")
	}
}

// TestDirMove_SurvivesReload：目录移动后重新挂载旧卷，loadIndex 按新目录 meta.path
// 重建——文件在新路径可读、旧路径不可见（目录 meta 改写已持久化的验证）。
func TestDirMove_SurvivesReload(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "old/f1.mp4", 200)
	if err := fs.Rename(ctx, "old", "new"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	assertListDir(t, fs2, ctx, "", map[string]bool{"new": true})
	assertListDir(t, fs2, ctx, "new", map[string]bool{"f1.mp4": false})
	rc, err := fs2.OpenRead(ctx, "new/f1.mp4")
	if err != nil {
		t.Fatalf("移动后重启 OpenRead(new/f1.mp4): %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(200)) {
		t.Error("移动后重启读取内容不一致")
	}
}

// TestLoadIndex_SkipsContainerWithoutDirMeta：容器目录 meta 缺失但残留文件 meta →
// fail-closed 跳过该容器并记日志，不把文件压平到根（避免跨容器同名遮蔽，F-1）。
func TestLoadIndex_SkipsContainerWithoutDirMeta(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "orphan.bin", 300)     // 根容器（将被删目录 meta）
	writeContent(t, fs, ctx, "keep/sub/a.bin", 100) // 独立容器，目录 meta 完好
	// 删掉 orphan.bin 所在容器（根容器）的目录 meta → 残留文件 meta 但无目录 meta。
	seg := fs.index["orphan.bin"].dirSeg
	inner, _ := fs.inner.ListDir(ctx, seg)
	for _, f := range inner {
		if shardseal.IsDirMetaName(f.Name) {
			if err := fs.inner.Delete(ctx, path.Join(seg, f.Name)); err != nil {
				t.Fatalf("删目录 meta: %v", err)
			}
		}
	}
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"),
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	// 缺目录 meta 的孤儿容器未被压平到根；目录 meta 完好的容器正常恢复。
	if _, ok := fs2.index["orphan.bin"]; ok {
		t.Error("缺目录 meta 的容器不应把文件压平到根索引")
	}
	if ent, _ := fs2.Stat(ctx, "orphan.bin"); ent != nil {
		t.Error("孤儿容器文件不应出现在根（fail-closed 跳过）")
	}
	if ent, _ := fs2.Stat(ctx, "keep/sub/a.bin"); ent == nil {
		t.Error("目录 meta 完好容器文件应可恢复")
	}
}

// TestNewFS_AlgorithmResolutionFailFast：NewFS 对 Options.Algorithm 做 fail-fast 解析——
// 未注册算法立刻报错（不静默回落默认）；空默认 v1（shardseal/aes-256-gcm）；显式已知算法成功。
func TestNewFS_AlgorithmResolutionFailFast(t *testing.T) {
	t.Parallel()
	mkInner := func(t *testing.T) syncpkg.FS {
		t.Helper()
		root := filepath.Join(t.TempDir(), "backing")
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		return syncpkg.NewLocalFS(root, nil)
	}

	// 未注册算法 → fail-fast（不静默回落默认）。
	if _, err := NewFS(mkInner(t), Options{Secret: []byte("s"), Algorithm: "shardseal/aes-256-cbc", TempDir: t.TempDir()}); err == nil {
		t.Fatal("未注册算法应 fail-fast 报错，却成功")
	}

	// 空算法 → 默认 v1（shardseal/aes-256-gcm）成功。
	fs, err := NewFS(mkInner(t), Options{Secret: []byte("s"), TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("空算法应默认 v1 成功: %v", err)
	}
	if fs.algoVer != shardseal.AlgoV1GCM {
		t.Errorf("空算法默认 version=%d，应为 v1", fs.algoVer)
	}

	// 显式已知算法 → 成功。
	fs2, err := NewFS(mkInner(t), Options{Secret: []byte("s"), Algorithm: shardseal.AlgorithmName, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("已知算法应成功: %v", err)
	}
	if fs2.algoVer != shardseal.AlgoV1GCM {
		t.Errorf("已知算法解析 version=%d，应为 v1", fs2.algoVer)
	}
}
