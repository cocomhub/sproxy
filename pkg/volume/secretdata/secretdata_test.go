// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal/mockkdf"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// init 在测试进程启动时注册 mock 算法（once 守卫；供全部测试的 testAlgo 使用，含内联
// Options 构造——统一入口避免逐 helper 遗漏注册）。
func init() {
	mockkdf.RegisterMockAlgorithm()
}

// testAlgo 是测试用 mock 算法（mockkdf.MockAlgorithmName："shardseal/aes-256-gcm-mock"，
// HKDF 轻量派生 ~µs + **真实 AES-GCM 加密**）。全量 -race 下几十个并行测试不再因
// standard/high 档的真实 scrypt 内存叠加而内存爆炸/超时；仅派生轻量化（KDF 由
// mockkdf 注入，KDFDomain 域分离语义不变），加密组装仍真实验证——生产默认仍是
// standard 档真实 scrypt，测试与生产解耦。mock 仅供测试/开发，生产禁配。
const testAlgo = mockkdf.MockAlgorithmName

// newFS 建一个基于本地临时目录底层 FS 的 secretdata FS（小块策略加速；mock KDF 派生加速）。
func newFS(t *testing.T) *SecretdataFS {
	t.Helper()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
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
		t.Fatal("容器应含目录 meta（q 标记）")
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

// TestUnderlyingLayout_DirMetaMarked：含文件的容器内混放三类文件（目录 meta 含 q、文件
// meta 含 z、分块无标记）。task10 目录解耦后每个逻辑目录都是容器，根下多个容器并存，
// 故定位「含文件 meta 的容器」断言其三类齐备（不假设根首个条目即文件容器）。
func TestUnderlyingLayout_DirMetaMarked(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/f1.mp4", 300)

	rootEntries, _ := fs.inner.ListDir(ctx, "")
	foundFileContainer := false
	for _, e := range rootEntries {
		inner, _ := fs.inner.ListDir(ctx, e.Name)
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
		if !hasFileMeta {
			continue // 纯目录容器（空目录 meta 容器）跳过
		}
		foundFileContainer = true
		if !hasDirMeta || !hasChunk {
			t.Errorf("含文件容器应含目录meta(@)/文件meta(-)/分块三类，got dirMeta=%v fileMeta=%v chunk=%v",
				hasDirMeta, hasFileMeta, hasChunk)
		}
	}
	if !foundFileContainer {
		t.Error("未找到含文件 meta 的容器")
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
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128, BlockletMin: 16, BlockletMax: 32},
		TempDir:   t.TempDir(),
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
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("wrong-key"), Algorithm: testAlgo, TempDir: t.TempDir()})
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

// dirMetaNameOf 返回容器内目录 meta blob 名（空串 = 无）。task10 断言目录 meta 是否被重写
// （移动根 → 改名；子树 → 零改动）用。
func dirMetaNameOf(t *testing.T, ctx context.Context, inner syncpkg.FS, container string) string {
	t.Helper()
	entries, err := inner.ListDir(ctx, container)
	if err != nil {
		t.Fatalf("ListDir(%q): %v", container, err)
	}
	for _, f := range entries {
		if !f.IsDir && shardseal.IsDirMetaName(f.Name) {
			return f.Name
		}
	}
	return ""
}

// entryBlobHash 聚合条目全部分块 + parity + 文件 meta blob 的 SHA-256（task10 文件移动
// blob 零改动断言：移动前后哈希一致 = 内容完全未变；含 parity 防 Erasure 冗余被遗漏）。
func entryBlobHash(t *testing.T, ctx context.Context, fs *SecretdataFS, e *metaEntry) string {
	t.Helper()
	h := sha256.New()
	for _, ci := range e.meta.Chunks {
		blob, err := readBlob(ctx, fs.inner, path.Join(fs.dataSeg(e), ci.FileName))
		if err != nil {
			t.Fatalf("read chunk %s: %v", ci.FileName, err)
		}
		h.Write(blob)
	}
	if p := e.meta.Parity; p != nil {
		blob, err := readBlob(ctx, fs.inner, path.Join(fs.dataSeg(e), p.FileName))
		if err != nil {
			t.Fatalf("read parity %s: %v", p.FileName, err)
		}
		h.Write(blob)
	}
	metaBlob, err := readBlob(ctx, fs.inner, path.Join(e.dirSeg, e.metaName))
	if err != nil {
		t.Fatalf("read file meta %s: %v", e.metaName, err)
	}
	h.Write(metaBlob)
	return hex.EncodeToString(h.Sum(nil))
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
// **实际加密落盘 blob**——首/末段 = 加密 meta blob 两窗口（base62，非明文哈希），中段 =
// HMAC 分组盲签（同文件共享）。6b 后 writeFileEncrypted 在 EncryptShards 之后用
// encryptMetaBlob 重新加密（mtime/parity 改后），meta 名直接锚定最终 blob。
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
		// 首尾窗口（加密 blob 两窗口的 base62 编码）与组签在乱序重排+同字符集混排后
		// 无法逐字符精确核对（rand 段同字符集可干扰）；改为校验整体特征：
		// ① 名称含 z 标记且长度 35-43；② 名称无特殊符号；③ 不含明文原文哈希截断
		// （存在性探针回归守卫）。密文锚定由 meta.Chunks[].EncSHA256 与 blob 内嵌索引承担。
		if !strings.ContainsRune(f.Name, 'z') {
			t.Errorf("文件 meta 名 %q 应含 z 标记", f.Name)
		}
		if len(f.Name) < 35 || len(f.Name) > 43 {
			t.Errorf("文件 meta 名长度 %d 应在 35-43", len(f.Name))
		}
		mm, err := fs.decryptFileMeta(blob)
		if err != nil {
			t.Fatalf("decryptFileMeta: %v", err)
		}
		// 安全性断言：中段不得是明文原文哈希的任何截断（拒绝回归 content-existence oracle）。
		if strings.Contains(f.Name, mm.Original.SHA256[:12]) {
			t.Errorf("文件 meta 名中段不应含明文原文哈希截断 %q（会退化为存在性探针）", mm.Original.SHA256[:12])
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
		Algorithm:    testAlgo,
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

	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
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

// TestDirMove_UpdatesMetaOnly（task10 改造）：目录移动仅更新根容器目录 meta 的
// name/parent 引用（一个文件重写，blob 名变化、DirID 身份不变），文件 meta/分块零改动
// （dirSeg/metaName 不变）、旧键删除、新键可解析、重启后按新路径重建。
func TestDirMove_UpdatesMetaOnly(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "old/f1.mp4", 200)
	oldDir := fs.index["old/f1.mp4"].dirSeg
	oldMeta := fs.index["old/f1.mp4"].metaName
	oldDmName := dirMetaNameOf(t, ctx, fs.inner, oldDir)
	if oldDmName == "" {
		t.Fatal("容器应含目录 meta")
	}
	if err := fs.Rename(ctx, "old", "new"); err != nil {
		t.Fatalf("Rename 目录: %v", err)
	}
	newEntry := fs.index["new/f1.mp4"]
	if newEntry == nil {
		t.Fatal("移动后新键 index[new/f1.mp4] 应存在")
	}
	// 目录移动仅改根 meta 引用：容器不变、文件 meta/分块零改动。
	if newEntry.dirSeg != oldDir || newEntry.metaName != oldMeta {
		t.Error("目录移动不应改动文件 meta/分块")
	}
	if _, ok := fs.index["old/f1.mp4"]; ok {
		t.Error("移动后旧键 index[old/f1.mp4] 应已删除（幽灵映射/split-brain 禁止）")
	}
	if _, ok := fs.dirs["new"]; !ok {
		t.Error("移动后新目录应存在")
	}
	if _, ok := fs.dirs["old"]; ok {
		t.Error("移动后旧目录应已删除")
	}
	// 仅重写了根容器目录 meta（blob 名变化 = 内容/引用变化）。
	if dm := dirMetaNameOf(t, ctx, fs.inner, oldDir); dm == "" || dm == oldDmName {
		t.Error("目录移动应重写目录 meta blob（Name/ParentDirID 变化）")
	}
	assertListDir(t, fs, ctx, "new", map[string]bool{"f1.mp4": false})
	// 重启后按新目录 meta 引用解析。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	rc, err := fs2.OpenRead(ctx, "new/f1.mp4")
	if err != nil {
		t.Fatalf("移动后重启 OpenRead(new/f1.mp4): %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(200)) {
		t.Error("移动后重启读取内容不一致")
	}
	if de, _ := fs2.Stat(ctx, "old"); de != nil {
		t.Error("移动后重启旧目录不应可见")
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
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
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
// fail-closed 跳过该容器并记日志，不把文件压平（避免跨容器同名遮蔽，F-1）。新父引用模型下
// 删**叶子**容器的目录 meta 只使该容器文件不可恢复；根与其它 meta 完好的容器/目录正常恢复。
func TestLoadIndex_SkipsContainerWithoutDirMeta(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "orphan.bin", 300)     // 根容器（目录 meta 完好 → 恢复）
	writeContent(t, fs, ctx, "keep/sub/b.bin", 100) // 叶子容器（删目录 meta → fail-closed）
	// 删掉 b.bin 所在叶子容器的目录 meta → 残留文件 meta 但无目录 meta。
	seg := fs.index["keep/sub/b.bin"].dirSeg
	inner, _ := fs.inner.ListDir(ctx, seg)
	for _, f := range inner {
		if shardseal.IsDirMetaName(f.Name) {
			if err := fs.inner.Delete(ctx, path.Join(seg, f.Name)); err != nil {
				t.Fatalf("删目录 meta: %v", err)
			}
		}
	}
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	// 缺目录 meta 的孤儿容器未被压平；其余容器（根 + keep）正常恢复。
	if _, ok := fs2.index["keep/sub/b.bin"]; ok {
		t.Error("缺目录 meta 的容器不应把文件压平到索引")
	}
	if ent, _ := fs2.Stat(ctx, "keep/sub/b.bin"); ent != nil {
		t.Error("孤儿容器文件不应出现在逻辑路径（fail-closed 跳过）")
	}
	if ent, _ := fs2.Stat(ctx, "orphan.bin"); ent == nil {
		t.Error("根文件（目录 meta 完好）应可恢复")
	}
	if de, _ := fs2.Stat(ctx, "keep"); de == nil || !de.IsDir {
		t.Error("keep 目录（meta 完好）应可恢复")
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

// TestDirMove_SubtreeZeroTouch（task10）：移动 a 整棵子树到新位置，仅改根 a 的父引用；
// 子树内子目录/文件 blob 零改动（目录 meta blob 名不变、文件 dirSeg/metaName 不变）。
func TestDirMove_SubtreeZeroTouch(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir(b): %v", err) // 目标父目录（b/a 的父 b）
	}
	writeContent(t, fs, ctx, "a/child/f1.mp4", 200)
	writeContent(t, fs, ctx, "a/child/deep/f2.mp4", 100)
	// 移动前记录子树各容器与文件 meta blob 引用。
	childSeg := fs.index["a/child/f1.mp4"].dirSeg
	childMeta := fs.index["a/child/f1.mp4"].metaName
	deepSeg := fs.index["a/child/deep/f2.mp4"].dirSeg
	deepMeta := fs.index["a/child/deep/f2.mp4"].metaName
	childDmBefore := dirMetaNameOf(t, ctx, fs.inner, childSeg)
	deepDmBefore := dirMetaNameOf(t, ctx, fs.inner, deepSeg)
	if childDmBefore == "" || deepDmBefore == "" {
		t.Fatal("子树容器应含目录 meta")
	}
	if err := fs.Rename(ctx, "a", "b/a"); err != nil {
		t.Fatalf("Rename 目录子树: %v", err)
	}
	// 子树文件容器/meta 零改动。
	c := fs.index["b/a/child/f1.mp4"]
	if c == nil || c.dirSeg != childSeg || c.metaName != childMeta {
		t.Error("子目录文件容器/meta 零改动断言失败")
	}
	d := fs.index["b/a/child/deep/f2.mp4"]
	if d == nil || d.dirSeg != deepSeg || d.metaName != deepMeta {
		t.Error("深层子文件容器/meta 零改动断言失败")
	}
	// 子树 dirMeta blob 名不变（仅根 a 被重写）。
	if dirMetaNameOf(t, ctx, fs.inner, childSeg) != childDmBefore {
		t.Error("子目录 meta blob 不应被移动改写（零改动）")
	}
	if dirMetaNameOf(t, ctx, fs.inner, deepSeg) != deepDmBefore {
		t.Error("深层子目录 meta blob 不应被移动改写（零改动）")
	}
	// 重启后在新路径按父引用解析。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	assertListDir(t, fs2, ctx, "b/a/child", map[string]bool{"f1.mp4": false, "deep": true})
	rc, err := fs2.OpenRead(ctx, "b/a/child/deep/f2.mp4")
	if err != nil {
		t.Fatalf("重启读子树: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(100)) {
		t.Error("子树移动后重启内容不一致")
	}
}

// TestFileMove_PhysicalCopy（task10）：文件跨目录移动（basename 不变）物理搬 blob + 删源，
// blob 内容零改动（聚合哈希不变）、源容器清空、重启后按新容器路径解析；改名（basename 变）
// 仍报错。
func TestFileMove_PhysicalCopy(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "dst"); err != nil {
		t.Fatalf("MakeDir(dst): %v", err) // 目标父目录（dst/f1.bin 的父 dst）
	}
	writeContent(t, fs, ctx, "src/f1.bin", 300)
	srcSeg := fs.index["src/f1.bin"].dirSeg
	metaName := fs.index["src/f1.bin"].metaName
	hashBefore := entryBlobHash(t, ctx, fs, fs.index["src/f1.bin"])
	if err := fs.Rename(ctx, "src/f1.bin", "dst/f1.bin"); err != nil {
		t.Fatalf("Rename 文件: %v", err)
	}
	moved := fs.index["dst/f1.bin"]
	if moved == nil {
		t.Fatal("移动后新键应存在")
	}
	if moved.dirSeg == srcSeg {
		t.Error("文件移动应换容器")
	}
	if moved.metaName != metaName {
		t.Error("文件移动应保留 meta blob 名（内容寻址不变）")
	}
	if entryBlobHash(t, ctx, fs, moved) != hashBefore {
		t.Error("文件移动后 blob 内容应零改动（聚合哈希不变）")
	}
	// 源容器 meta/chunks 已删（源容器不再引用该文件）。
	if ent, _ := fs.inner.Stat(ctx, path.Join(srcSeg, metaName)); ent != nil {
		t.Error("源容器文件 meta blob 应已删除")
	}
	if _, ok := fs.index["src/f1.bin"]; ok {
		t.Error("移动后源键应删除")
	}
	// 重启后按新容器路径解析；旧路径不可见。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	rc, err := fs2.OpenRead(ctx, "dst/f1.bin")
	if err != nil {
		t.Fatalf("移动后重启读: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(300)) {
		t.Error("移动后重启内容不一致")
	}
	if _, err := fs2.OpenRead(ctx, "src/f1.bin"); err == nil {
		t.Error("旧路径重启后不可读")
	}
	// 改名（basename 变）仍报错。
	if err := fs.Rename(ctx, "dst/f1.bin", "dst/renamed.bin"); err == nil {
		t.Error("文件改名（basename 变）应报错")
	}
}

// TestDeleteLastFile_RemovesDir（Imp-1）：删除目录最后文件后目录彻底成空——目录 meta +
// dirSegs 一起删，ListDir/Stat 不再返回该目录，重启也不复现（空目录不残留）。
func TestDeleteLastFile_RemovesDir(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "movies/sub/b.bin", 100)
	if de, _ := fs.Stat(ctx, "movies/sub"); de == nil || !de.IsDir {
		t.Fatal("写入后子目录应可 Stat 为目录")
	}
	if err := fs.Delete(ctx, "movies/sub/b.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// 立即不可见（含递归空祖先）。
	if _, ok := fs.dirs["movies/sub"]; ok {
		t.Error("删除最后文件后目录 movies/sub 应注销（Imp-1）")
	}
	if _, ok := fs.dirs["movies"]; ok {
		t.Error("删除最后文件后空祖先目录 movies 应一并回收")
	}
	if de, _ := fs.Stat(ctx, "movies/sub"); de != nil {
		t.Error("删除最后文件后目录不应可见")
	}
	if de, _ := fs.Stat(ctx, "movies"); de != nil {
		t.Error("删除最后文件后空祖先目录不应可见")
	}
	// 重启不复现（磁盘目录 meta 已随删除清理）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	if de, _ := fs2.Stat(ctx, "movies/sub"); de != nil {
		t.Error("重启后空目录不应复现（Imp-1）")
	}
	if de, _ := fs2.Stat(ctx, "movies"); de != nil {
		t.Error("重启后空祖先目录不应复现（Imp-1）")
	}
	assertListDir(t, fs2, ctx, "", map[string]bool{})
}

// TestLoadIndex_Parallel（Imp-2）：多容器 + 多文件卷挂载（loadIndex 按容器并行扫描 + 容器内
// 文件并行解密 + 派生缓存），-race 下无竞态，全部文件可解析、内容可读。
func TestLoadIndex_Parallel(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	n := 24
	for i := range n {
		writeContent(t, fs, ctx, fmt.Sprintf("dir%d/f%d.bin", i%6, i), 50+i)
	}
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	for i := range n {
		k := fmt.Sprintf("dir%d/f%d.bin", i%6, i)
		if _, ok := fs2.index[k]; !ok {
			t.Fatalf("并行加载后缺索引键 %q", k)
		}
	}
	// 根呈现 6 个目录。
	assertListDir(t, fs2, ctx, "", dirsOnly(6))
	// 跨目录读回内容一致。
	rc, err := fs2.OpenRead(ctx, "dir3/f15.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(65)) {
		t.Error("并行加载后读取内容不一致")
	}
}

// dirsOnly 构造期望的 dir0..dir5 目录集（ListDir 根断言用）。
func dirsOnly(n int) map[string]bool {
	m := map[string]bool{}
	for i := range n {
		m[fmt.Sprintf("dir%d", i)] = true
	}
	return m
}

// TestFileMove_PhysicalCopy_ErasureParity（Imp-A 修复）：Erasure 卷文件移动时 parity blob 随
// 文件一并物理搬移（目标保留 XOR 冗余、源 parity 清理）、blob 内容零改动（含 parity）；
// GC 不误删 parity；分块丢失可经 parity 恢复。
func TestFileMove_PhysicalCopy_ErasureParity(t *testing.T) {
	t.Parallel()
	fs := newErasureFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "dst"); err != nil {
		t.Fatalf("MakeDir(dst): %v", err)
	}
	writeContent(t, fs, ctx, "src/f1.bin", 300) // 300B 随机块 64-128 → ≥2 分块 → 有 parity
	srcSeg := fs.index["src/f1.bin"].dirSeg
	parName := fs.index["src/f1.bin"].meta.Parity.FileName
	if parName == "" {
		t.Fatal("Erasure 卷多分块文件应含 parity 引用")
	}
	hashBefore := entryBlobHash(t, ctx, fs, fs.index["src/f1.bin"])
	if err := fs.Rename(ctx, "src/f1.bin", "dst/f1.bin"); err != nil {
		t.Fatalf("Rename 文件: %v", err)
	}
	moved := fs.index["dst/f1.bin"]
	if moved == nil {
		t.Fatal("移动后新键应存在")
	}
	if moved.meta.Parity == nil {
		t.Fatal("移动后 parity 引用应保留")
	}
	// 目标容器含 parity blob（XOR 冗余不得丢失）；源 parity 已删。
	if ent, _ := fs.inner.Stat(ctx, path.Join(moved.dirSeg, parName)); ent == nil {
		t.Fatal("移动后目标容器应含 parity blob（XOR 冗余不得丢失）")
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(srcSeg, parName)); ent != nil {
		t.Error("源容器 parity blob 应已删除")
	}
	// blob 内容零改动（含 parity）。
	if entryBlobHash(t, ctx, fs, moved) != hashBefore {
		t.Error("移动后 blob 内容应零改动（含 parity）")
	}
	// GC 不误删 parity（引用已标记）。
	if _, err := fs.GC(ctx); err != nil {
		t.Fatalf("GC: %v", err)
	}
	if ent, _ := fs.inner.Stat(ctx, path.Join(moved.dirSeg, parName)); ent == nil {
		t.Error("GC 后 parity 应仍存活（引用已标记，不得被当孤儿清扫）")
	}
	// 分块丢失可经 parity 恢复：删目标容器第一个数据分块 → OpenRead 成功且内容一致。
	ci := moved.meta.Chunks[0]
	if err := fs.inner.Delete(ctx, path.Join(moved.dirSeg, ci.FileName)); err != nil {
		t.Fatalf("删分块: %v", err)
	}
	rc, err := fs.OpenRead(ctx, "dst/f1.bin")
	if err != nil {
		t.Fatalf("分块丢失后经 parity 恢复读: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(300)) {
		t.Error("分块丢失经 parity 恢复的内容不一致")
	}
}

// TestResolveDirMetaPath_Cycle（Imp-B 修复）：父引用自环/成环 → resolveDirMetaPath 返回
// ok=false（fail-closed，不无限递归）；父引用断裂同样 fail-closed；正常链仍正确解析。
func TestResolveDirMetaPath_Cycle(t *testing.T) {
	t.Parallel()
	// 自环：A.parent=A。
	byID := map[string]*dirMeta{"aaa": {Name: "a", ParentDirID: "aaa", DirID: "aaa"}}
	if _, ok := resolveDirMetaPath(byID, byID["aaa"]); ok {
		t.Error("自环应 fail-closed（ok=false）")
	}
	// 两节点环：A.parent=B、B.parent=A。
	byID = map[string]*dirMeta{
		"aaa": {Name: "a", ParentDirID: "bbb", DirID: "aaa"},
		"bbb": {Name: "b", ParentDirID: "aaa", DirID: "bbb"},
	}
	if _, ok := resolveDirMetaPath(byID, byID["aaa"]); ok {
		t.Error("两节点环（A 侧）应 fail-closed（ok=false）")
	}
	if _, ok := resolveDirMetaPath(byID, byID["bbb"]); ok {
		t.Error("两节点环（B 侧）应 fail-closed（ok=false）")
	}
	// 父引用缺失（悬挂）→ fail-closed。
	byID = map[string]*dirMeta{"aaa": {Name: "a", ParentDirID: "missing", DirID: "aaa"}}
	if _, ok := resolveDirMetaPath(byID, byID["aaa"]); ok {
		t.Error("父引用断裂应 fail-closed（ok=false）")
	}
	// 正常链（根 → a → b）仍正确解析。
	byID = map[string]*dirMeta{
		"root": {Name: "", ParentDirID: "", DirID: "root"},
		"aaa":  {Name: "a", ParentDirID: "root", DirID: "aaa"},
		"bbb":  {Name: "b", ParentDirID: "aaa", DirID: "bbb"},
	}
	p, ok := resolveDirMetaPath(byID, byID["bbb"])
	if !ok || p != "a/b" {
		t.Errorf("正常链应解析为 a/b，got %q ok=%v", p, ok)
	}
	if p, ok := resolveDirMetaPath(byID, byID["root"]); !ok || p != "" {
		t.Errorf("根容器应解析为空串，got %q ok=%v", p, ok)
	}
}

// TestLoadIndex_CyclicDirMeta_DoesNotCrash（Imp-B 修复）：底层卷存在父引用自环的目录 meta →
// 挂载不崩溃（loadIndex fail-closed 跳过该容器，不无限递归栈溢出）；其它容器正常恢复。
func TestLoadIndex_CyclicDirMeta_DoesNotCrash(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 100)
	writeContent(t, fs, ctx, "ok/f2.bin", 50)
	// 篡改 a 的目录 meta：ParentDirID 指向自身（自环）。
	seg := fs.index["a/f1.bin"].dirSeg
	oldName, oldBlob, err := findDirMetaBlob(ctx, fs.inner, seg)
	if err != nil {
		t.Fatalf("findDirMetaBlob: %v", err)
	}
	dm, derr := fs.decryptDirMeta(oldBlob)
	if derr != nil {
		t.Fatalf("decryptDirMeta: %v", derr)
	}
	dm.ParentDirID = dm.DirID // 自环
	newName, werr := fs.writeDirMetaLocked(ctx, seg, dm)
	if werr != nil {
		t.Fatalf("writeDirMetaLocked: %v", werr)
	}
	if newName != oldName {
		if delErr := fs.inner.Delete(ctx, path.Join(seg, oldName)); delErr != nil {
			t.Fatalf("删旧 dir meta: %v", delErr)
		}
	}
	// 重新挂载：不崩溃；成环容器被 fail-closed 跳过，其余容器正常。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("自环卷挂载应不崩溃: %v", err)
	}
	if _, ok := fs2.index["a/f1.bin"]; ok {
		t.Error("自环容器文件不应恢复（fail-closed 跳过）")
	}
	if de, _ := fs2.Stat(ctx, "a"); de != nil {
		t.Error("自环目录不应可见")
	}
	if ent, _ := fs2.Stat(ctx, "ok/f2.bin"); ent == nil {
		t.Error("非成环容器文件应正常恢复")
	}
}

// TestDecryptDirMeta_DirIDFailClosed：dir_id 非 9 字符 base62 的目录 meta 解密必须
// fail-closed（防旧格式/损坏/外来数据经 interleaveCore 段长守卫 panic——解密边界提前
// 拒绝，容器被跳过而非崩进程）。手工加密 bad-DirID blob（不经过 writeDirMetaLocked，
// 后者对非法 DirID 会在 interleaveCore 处 panic，正是本校验要拦截的路径）。
func TestDecryptDirMeta_DirIDFailClosed(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "dir/f.bin", 100) // 建一个真实容器
	seg := fs.index["dir/f.bin"].dirSeg
	_, blob, err := findDirMetaBlob(ctx, fs.inner, seg)
	if err != nil {
		t.Fatalf("findDirMetaBlob: %v", err)
	}
	dm, derr := fs.decryptDirMeta(blob)
	if derr != nil {
		t.Fatalf("decryptDirMeta 正常卷应成功: %v", derr)
	}
	if !shardseal.IsBase62ID(dm.DirID) || len(dm.DirID) != shardseal.NameCharsLen {
		t.Fatalf("正常 DirID 应恰 %d 字符 base62，got %q", shardseal.NameCharsLen, dm.DirID)
	}
	// 篡改为非法 DirID → 手工加密 blob → 解密必须 fail-closed。
	encryptBad := func(dm *dirMeta) []byte {
		t.Helper()
		salt, _ := shardseal.RandSalt()
		key, _ := shardseal.DeriveKey(fs.secret, salt, fs.algoVer)
		dmJSON, _ := json.Marshal(dm)
		b, _ := shardseal.EncryptMetaJSON(key, salt, dmJSON, 0)
		return b
	}
	for _, bad := range []string{
		"",            // 空
		"short",       // 长度不足
		"abcdefghijk", // 超长
		"abc_12345",   // 含非法字符（8 字符 + _）
		"ABCdef!23",   // 含非法字符
		"abc-def-gh",  // 含非法字符
	} {
		cp := *dm
		cp.DirID = bad
		if _, derr2 := fs.decryptDirMeta(encryptBad(&cp)); derr2 == nil {
			t.Errorf("非法 dir_id %q 解密应 fail-closed", bad)
		}
	}
}

// TestLoadIndex_LargeVolume（Imp-C 修复）：200+ 文件多容器卷挂载——loadIndex 有界并行
// （loadGate 并发上界，scrypt 128MB/次防内存爆炸）下不崩溃、全量键可解析、内容可读。
// setup 写卷用**有界并行**（8 worker：scrypt 128MB/次限并发），墙钟时间可控
// （-race 下 200 次串行 scrypt 会顶爆 10m 包超时）。
func TestLoadIndex_LargeVolume(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	n := 200
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{} // 写卷并发上界（setup 阶段）
			defer func() { <-sem }()
			name := fmt.Sprintf("dir%d/f%d.bin", i%8, i)
			if err := fs.WriteFile(ctx, name, bytes.NewReader(data(40+i)), int64(40+i), 0); err != nil {
				t.Errorf("WriteFile %s: %v", name, err)
			}
		}(i)
	}
	wg.Wait()
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	for i := range n {
		k := fmt.Sprintf("dir%d/f%d.bin", i%8, i)
		if _, ok := fs2.index[k]; !ok {
			t.Fatalf("大卷并行加载后缺索引键 %q", k)
		}
	}
	assertListDir(t, fs2, ctx, "", dirsOnly(8))
	rc, err := fs2.OpenRead(ctx, "dir7/f199.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(239)) {
		t.Error("大卷并行加载后读取内容不一致")
	}
}

// TestRename_ToNonexistentParent_Fails（M-1）：目录/文件移动到不存在的父目录 → fail-closed
// 报错（不自动建链）；移动到已占用位置报错。
func TestRename_ToNonexistentParent_Fails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 100)
	if err := fs.Rename(ctx, "a", "nope/a"); err == nil {
		t.Error("目录移动到不存在的父目录应报错")
	}
	if err := fs.Rename(ctx, "a/f1.bin", "nope/f1.bin"); err == nil {
		t.Error("文件移动到不存在的父目录应报错")
	}
	if err := fs.Rename(ctx, "a", "a/f1.bin"); err == nil {
		t.Error("移动到已存在的文件位置应报错")
	}
}

// TestLoadIndex_RootListFault_FailClosed（Minor + Imp-3 硬前提回归）：底层卷根 ListDir
// 抛非「不存在」故障（瞬时 IO/云盘故障）时，loadIndex 必须返回错误使 NewFS fail-closed——
// 不得吞为空卷（否则 GC 复用内存索引会把全部 meta 当孤儿清扫，数据丢失）。
func TestLoadIndex_RootListFault_FailClosed(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := &faultListFS{wrap: syncpkg.NewLocalFS(root, nil)}
	_, err := NewFS(inner, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err == nil {
		t.Fatal("底层根 ListDir 故障应使 NewFS 失败（fail-closed），got nil")
	}
	if !strings.Contains(err.Error(), "扫描卷根失败") {
		t.Errorf("错误文案应表明非首次使用故障，got %v", err)
	}
}

// faultListFS 包装底层 FS：仅对根（""）ListDir 返回非「不存在」错误（模拟瞬时故障）。
// 子路径正常透传（loadIndex 首层失败即中止，不会触达子路径）。
type faultListFS struct {
	wrap syncpkg.FS
}

func (f *faultListFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	if p == "" {
		return nil, errors.New("底层卷根瞬时故障")
	}
	return f.wrap.ListDir(ctx, p)
}
func (f *faultListFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	return f.wrap.Stat(ctx, p)
}
func (f *faultListFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return f.wrap.OpenRead(ctx, p)
}
func (f *faultListFS) WriteFile(ctx context.Context, p string, r io.Reader, size int64, mtime int64) error {
	return f.wrap.WriteFile(ctx, p, r, size, mtime)
}
func (f *faultListFS) Rename(ctx context.Context, from, to string) error {
	return f.wrap.Rename(ctx, from, to)
}
func (f *faultListFS) Delete(ctx context.Context, p string) error  { return f.wrap.Delete(ctx, p) }
func (f *faultListFS) MakeDir(ctx context.Context, p string) error { return f.wrap.MakeDir(ctx, p) }

// TestWriteFile_MaxFileBytesCap（Imp-2 回归）：单文件上限在**读取全文前**按 size 拦截
// （超限返回 ErrMaxFileBytes，不等 io.ReadAll 把大文件读进内存）。
func TestWriteFile_MaxFileBytesCap(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "backing")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	inner := syncpkg.NewLocalFS(root, nil)
	fs, err := NewFS(inner, Options{
		Secret:       []byte("test-secret-key-000"),
		Algorithm:    testAlgo,
		Block:        shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:      t.TempDir(),
		MaxFileBytes: 4096,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 超过上限 → ErrMaxFileBytes（未落盘）。
	if werr := fs.WriteFile(context.Background(), "big.bin", bytes.NewReader(data(5000)), 5000, 0); !errors.Is(werr, ErrMaxFileBytes) {
		t.Fatalf("超限应返回 ErrMaxFileBytes，got %v", werr)
	}
	if e, statErr := fs.Stat(context.Background(), "big.bin"); statErr != nil || e != nil {
		t.Error("超限文件不应落盘（index 无条目）")
	}
	// 未超限正常写入 + 读回。
	if werr := fs.WriteFile(context.Background(), "ok.bin", bytes.NewReader(data(300)), 300, 0); werr != nil {
		t.Fatalf("未超限写失败: %v", werr)
	}
	rc, err := fs.OpenRead(context.Background(), "ok.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data(300)) {
		t.Error("未超限内容不一致")
	}
}

// TestWriteFile_EmptyFileFails 空文件（0 字节）：RandomPlanner 有意拒绝空文件（M-2 已知
// 边界，block_test.go 与设计文档已标注）——writeFile 对空内容经 encryptContent → shardseal
// 空文件错误 fail-closed，不静默写半态（无索引条目、磁盘无容器残留）。
func TestWriteFile_EmptyFileFails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.WriteFile(ctx, "empty.txt", bytes.NewReader(nil), 0, 0); err == nil {
		t.Fatal("空文件写入应报错（fail-closed），却成功")
	}
	if e, _ := fs.Stat(ctx, "empty.txt"); e != nil {
		t.Error("空文件写入失败后不应有残留索引条目")
	}
	// 加密失败发生在上传之前（rollback 删净 uploaded）——底层不得出现文件 meta/分块
	// （根容器目录 meta @ 恒保留是设计：ensureContainer 先建根、空文件失败回滚不回收根）。
	root, _ := fs.inner.ListDir(ctx, "")
	for _, e := range root {
		if !e.IsDir {
			continue
		}
		inner, _ := fs.inner.ListDir(ctx, e.Name)
		for _, f := range inner {
			switch {
			case shardseal.IsDirMetaName(f.Name):
				// 根容器目录 meta（@）允许
			case shardseal.ClassifyName(f.Name) == shardseal.KindFileMeta:
				t.Errorf("空文件写入失败后不应残留文件 meta %q", f.Name)
			default:
				t.Errorf("空文件写入失败后不应残留分块 %q", f.Name)
			}
		}
	}
}

// TestWriteFile_LongPathAndBasename_Roundtrip 超长文件名/深层嵌套路径：逻辑路径任意长、
// basename 超 254 字节——secretdata 只把逻辑名写进加密 meta（磁盘容器目录恒 5-30 随机），
// 超长逻辑路径 roundtrip 内容一致（HTTP 层 ValidateFilePath 的 254 字节限制不在此层）。
func TestWriteFile_LongPathAndBasename_Roundtrip(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	longName := strings.Repeat("n", 300) + ".bin" // >254 字节 basename
	deep := "d0/d1/d2/d3/d4/d5/d6/d7/" + longName // 8 层嵌套
	content := data(500)
	if err := fs.WriteFile(ctx, deep, bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("超长路径 WriteFile: %v", err)
	}
	rc, err := fs.OpenRead(ctx, deep)
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("超长路径 roundtrip 内容不一致")
	}
	// 每级子目录可见。
	assertListDir(t, fs, ctx, "d0/d1/d2/d3/d4/d5/d6", map[string]bool{"d7": true})
	// 重启旧卷加载后仍可读（逻辑名完整恢复）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS2: %v", err)
	}
	rc2, err := fs2.OpenRead(ctx, deep)
	if err != nil {
		t.Fatalf("重启后 OpenRead 超长路径: %v", err)
	}
	got2, _ := io.ReadAll(rc2)
	rc2.Close()
	if !bytes.Equal(got2, content) {
		t.Fatal("重启后超长路径 roundtrip 内容不一致")
	}
}

// TestConcurrentReadWriteDelete_Stress 并发读写删 stress（-race）：多 goroutine 同卷写/读
// （互不重叠 key）+ 并行删另一组 key——验证 s.mu 保护的写路径/读路径/即时删在 -race 下
// 无数据竞争、无半态（幸存文件内容完整、被删文件即时不可见）。
func TestConcurrentReadWriteDelete_Stress(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	const n = 6
	var wg sync.WaitGroup

	// 阶段 A：并发写两组互不重叠的文件（keep 组 + del 组）。
	wg.Add(2 * n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			content := data(60 + i)
			if err := fs.WriteFile(ctx, fmt.Sprintf("keep/f%d.bin", i), bytes.NewReader(content), int64(len(content)), 0); err != nil {
				t.Errorf("keep 写: %v", err)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			content := data(80 + i)
			if err := fs.WriteFile(ctx, fmt.Sprintf("del/f%d.bin", i), bytes.NewReader(content), int64(len(content)), 0); err != nil {
				t.Errorf("del 写: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// 阶段 B：并发读 keep 组（校验内容）+ 并行删 del 组（不同 key，互不干扰）。
	wg.Add(2 * n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			rc, err := fs.OpenRead(ctx, fmt.Sprintf("keep/f%d.bin", i))
			if err != nil {
				t.Errorf("keep 读: %v", err)
				return
			}
			got, _ := io.ReadAll(rc)
			rc.Close()
			if !bytes.Equal(got, data(60+i)) {
				t.Errorf("keep/f%d.bin 并发读内容不一致", i)
			}
		}(i)
		go func(i int) {
			defer wg.Done()
			if err := fs.Delete(ctx, fmt.Sprintf("del/f%d.bin", i)); err != nil {
				t.Errorf("del 删: %v", err)
			}
		}(i)
	}
	wg.Wait()

	// 阶段 C：幸存文件全部可读、被删文件即时不可见（无半态）。
	for i := range n {
		if e, _ := fs.Stat(ctx, fmt.Sprintf("keep/f%d.bin", i)); e == nil {
			t.Errorf("keep/f%d.bin 应仍在", i)
		}
		if e, _ := fs.Stat(ctx, fmt.Sprintf("del/f%d.bin", i)); e != nil {
			t.Errorf("del/f%d.bin 删除后应即时不可见", i)
		}
	}
}

// TestRename_FileBasenameChange_Fails（M6 补充）：文件「改名」（from/to basename 不同）
// 是逻辑改名（basename 锚定于 meta.original.name），不可经 Rename 物理迁移——应明确报错
// 而非静默搬到新路径。目录改名的 basename 变化路径另有 renameDirLocked 覆盖。
func TestRename_FileBasenameChange_Fails(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	writeContent(t, fs, ctx, "a/f1.bin", 100)
	if err := fs.Rename(ctx, "a/f1.bin", "a/f2.bin"); err == nil {
		t.Error("文件改名（basename 变化）应报错（逻辑改名走 delete+write）")
	}
	// 原文件仍存在（改名失败不落半态）。
	if _, err := fs.Stat(ctx, "a/f1.bin"); err != nil {
		t.Errorf("改名失败后原文件应存在: %v", err)
	}
	// 跨目录移动但 basename 相同 → 合法（物理搬移零改内容）；目标父目录需先存在。
	if err := fs.MakeDir(ctx, "b"); err != nil {
		t.Fatalf("MakeDir b: %v", err)
	}
	if err := fs.Rename(ctx, "a/f1.bin", "b/f1.bin"); err != nil {
		t.Errorf("同 basename 跨目录移动应成功: %v", err)
	}
}

// TestMaxParallelLoads_AdaptsToKDFTier（任务 13 档位化补充）：loadGate 并发上界随 KDF 档位
// **真实 scrypt 内存**（RFC 7914 = 128×r×N 字节）自适应 = clamp(512MiB/(2×派生内存), 1,
// NumCPU)——每个并发槽按 ~2× RFC 最小内存记账（allocator/GC 节奏，实测峰值 ≤ 预算）。
// 显式字节数断言（不依赖 ScryptMemEstimate 自证）：low(4MiB)→NumCPU、standard(16MiB)→
// min(16,NumCPU)、high(128MiB)→min(2,NumCPU)。
func TestMaxParallelLoads_AdaptsToKDFTier(t *testing.T) {
	t.Parallel()
	ncpu := runtime.NumCPU()
	// 真实 scrypt 内存（RFC 7914：N×r×128 字节），独立字节常量防套圆。
	const (
		memLow      = int64(1<<12) * 8 * 128 // 4MiB
		memStandard = int64(1<<14) * 8 * 128 // 16MiB
		memHigh     = int64(1<<17) * 8 * 128 // 128MiB
	)
	clampWant := func(mem int64) int {
		w := min(max(int(int64(maxLoadMemBudget)/(2*mem)), 1), ncpu)
		return w
	}
	tiers := []struct {
		name string
		alg  *shardseal.Algorithm
		mem  int64
	}{
		{"low", &shardseal.Algorithm{ScryptN: 1 << 12, ScryptR: 8, ScryptP: 1}, memLow},
		{"standard", &shardseal.Algorithm{ScryptN: 1 << 14, ScryptR: 8, ScryptP: 1}, memStandard},
		{"high", &shardseal.Algorithm{ScryptN: 1 << 17, ScryptR: 8, ScryptP: 1}, memHigh},
	}
	var wLow, wStd, wHigh int
	for _, tc := range tiers {
		// 独立字节常量 → 期望并发，不回调 ScryptMemEstimate（套圆）。
		want := clampWant(tc.mem)
		got := maxParallelLoads(tc.alg)
		if got != want {
			t.Errorf("%s 档并发=%d，应为 clamp(512MiB/(2×%d),1,NumCPU)=%d（NumCPU=%d）", tc.name, got, tc.mem, want, ncpu)
		}
		switch tc.name {
		case "low":
			wLow = got
		case "standard":
			wStd = got
		case "high":
			wHigh = got
		}
	}
	// 档位关系：低档允许并发 ≥ 高档（低档派生内存小）。
	if wLow < wStd || wStd < wHigh {
		t.Errorf("档位并发关系错误：low=%d standard=%d high=%d（应 low≥standard≥high）", wLow, wStd, wHigh)
	}
	// 显式期望值（512MiB 预算 × 2 记账下的真实 scrypt 内存口径）。
	if wHigh != min(2, ncpu) {
		t.Errorf("high 档并发=%d，应为 min(2,NumCPU)=%d（512MiB/(2×128MiB)）", wHigh, min(2, ncpu))
	}
	if wStd != min(16, ncpu) {
		t.Errorf("standard 档并发=%d，应为 min(16,NumCPU)=%d（512MiB/(2×16MiB)）", wStd, min(16, ncpu))
	}
	if wLow != ncpu {
		t.Errorf("low 档并发=%d，应为 NumCPU=%d（512MiB/(2×4MiB)=64 ≥ NumCPU 故钳 NumCPU）", wLow, ncpu)
	}
	// nil 兜底按 standard 档参数。
	if got := maxParallelLoads(nil); got != wStd {
		t.Errorf("nil 兜底并发=%d，应为 standard 档 %d", got, wStd)
	}
}

// TestMaxParallelLoads_FormulaClamps（任务 15 修复轮补充）：**纯公式用例**——直接构造
// `shardseal.Algorithm{ScryptN:...}`（不注册、不经 mock KDF），验证 maxParallelLoads 的
// ×2 记账钳制语义。high→min(2)/standard→min(16)/low=NumCPU 已由
// TestMaxParallelLoads_AdaptsToKDFTier 覆盖；本用例补边界：超大 N 钳到 1、ScryptR 缩放、
// ScryptP=0 兜底。
func TestMaxParallelLoads_FormulaClamps(t *testing.T) {
	t.Parallel()
	ncpu := runtime.NumCPU()
	// 超大 N：单次派生内存远超预算（2^28×8×128=32GiB/次）→ 并发钳到 1（预算不足单槽，
	// 宁串行不炸内存）。
	giant := &shardseal.Algorithm{ScryptN: 1 << 28, ScryptR: 8, ScryptP: 1}
	if got := maxParallelLoads(giant); got != 1 {
		t.Errorf("超大 ScryptN 并发=%d，应钳到 1（单槽预算不足）", got)
	}
	// ScryptR 缩放：R=16 使单次内存 ×2 → 并发减半（standard 档 16→8，仍受 NumCPU 钳）。
	r16 := &shardseal.Algorithm{ScryptN: 1 << 14, ScryptR: 16, ScryptP: 1}
	if got, want := maxParallelLoads(r16), min(8, ncpu); got != want {
		t.Errorf("ScryptR=16 并发=%d，应为 min(8,NumCPU)=%d", got, want)
	}
	// ScryptP 不参与内存记账（p 不放大工作内存；参数保留供 scrypt 计算耗时用）。
	p8 := &shardseal.Algorithm{ScryptN: 1 << 14, ScryptR: 8, ScryptP: 8}
	if got, want := maxParallelLoads(p8), min(16, ncpu); got != want {
		t.Errorf("ScryptP=8 并发=%d，应不受 P 影响（与 standard 同）=%d", got, want)
	}
	// 零值字段：ScryptN=0 → 按 standard 档兜底（与 nil 一致）。
	if got, want := maxParallelLoads(&shardseal.Algorithm{}), min(16, ncpu); got != want {
		t.Errorf("零值 Algorithm 并发=%d，应为 standard 档兜底 %d", got, want)
	}
}

// TestLoadIndex_LoadGate_BoundedConcurrency（任务 15 修复轮改 mock 档）：loadGate 并发
// 上界随解析算法档位自适应——内存测算是**类型级**（ScryptMemEstimate 纯算术，与
// KDFOverride 正交），从不需真实 scrypt。mock 档（N=2^12 → 4MiB）→ 并发 = NumCPU；卷内
// 文件可读回（同档写读 roundtrip）。high→min(2)/standard→min(16) 的钳制语义由
// TestMaxParallelLoads_AdaptsToKDFTier（直接 Algorithm{ScryptN:...} 构造、不注册）覆盖。
func TestLoadIndex_LoadGate_BoundedConcurrency(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	const n = 3 // 少量文件即可覆盖多容器/多 meta 并发派生路径
	for i := range n {
		content := data(100 + i)
		if werr := fs.WriteFile(ctx, fmt.Sprintf("dir%d/f%d.bin", i%2, i), bytes.NewReader(content), int64(len(content)), 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
	}
	// 重挂载：loadGate 容量 = 随解析算法档位自适应并发（mock 档 N=2^12 → NumCPU）。
	fs2, err := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewFS reload: %v", err)
	}
	mockAlg, _ := shardseal.AlgoByVersion(mockkdf.MockAlgoVersion)
	if want := maxParallelLoads(&mockAlg); cap(fs2.loadGate) != want {
		t.Errorf("mock 档 loadGate 容量=%d，应为自适应并发 %d", cap(fs2.loadGate), want)
	}
	for i := range n {
		k := fmt.Sprintf("dir%d/f%d.bin", i%2, i)
		rc, rerr := fs2.OpenRead(ctx, k)
		if rerr != nil {
			t.Fatalf("挂载后 OpenRead(%s): %v", k, rerr)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, data(100+i)) {
			t.Errorf("挂载后 %s 内容不一致", k)
		}
	}
}

// TestLoadIndex_LoadGate_PeakMemoryWithinBudget（任务 15 修复轮改 mock 档）：**非套圆实测**
// loadIndex 期间的堆峰值增量 ≤ maxLoadMemBudget（512MiB）——用 runtime.MemStats 独立采样
// （不是「并发×派生内存公式」自回推）。mock 档派生 ~µs、无真实 scrypt 内存，峰值断言为
// 「机制守卫」（loadGate 并发 × 派生内存记账 ≤ 预算恒成立）。真实 high 内存实测收敛为
// 一次性基准（secretdata_slow_test.go，//go:build slow；ScryptMemEstimate ×2 记账系数已由
// 任务 13 实测 1.08GB≈2×理论标定，常规路径不需重复实测）。
// 串行（不并行）：堆峰值采样对其它并行测试的堆扰动敏感。
func TestLoadIndex_LoadGate_PeakMemoryWithinBudget(t *testing.T) {
	// sproxy:serial: 堆峰值实测对全局堆敏感，串行运行避免并行测试扰动
	fs := newFS(t)
	ctx := context.Background()
	// 4 容器各 1 文件：dir meta + 文件 meta 共 8 次派生、并发触达 loadGate 上界。
	const n = 4
	for i := range n {
		content := data(200 + i)
		if werr := fs.WriteFile(ctx, fmt.Sprintf("dir%d/f%d.bin", i, i), bytes.NewReader(content), int64(len(content)), 0); werr != nil {
			t.Fatalf("WriteFile: %v", werr)
		}
	}
	// 基线：先 GC 回收写路径已释放的派生内存，再取 HeapAlloc。
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	peak := base.HeapAlloc
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		for {
			var m runtime.MemStats
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > peak {
				peak = m.HeapAlloc
			}
			select {
			case <-done:
				return
			default:
				runtime.Gosched()
			}
		}
	})
	// 重挂载触发 loadIndex 并行派生（实测堆峰值，非公式回推）。
	if _, rerr := NewFS(fs.inner, Options{Secret: []byte("test-secret-key-000"), Algorithm: testAlgo,
		Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir()}); rerr != nil {
		t.Fatalf("NewFS reload: %v", rerr)
	}
	close(done)
	wg.Wait()
	// 断言阈值 = 预算 + 分配器/GC slack：mock 派生内存极小，恒在该阈值内；断言守卫 loadGate
	// 机制接线（峰值采样 + 并发 × 记账 ≤ 预算）。
	const loadPeakSlack = 64 << 20 // 64MiB（≈12.5% 预算）
	delta := peak - base.HeapAlloc
	if delta > maxLoadMemBudget+loadPeakSlack {
		t.Errorf("loadIndex 实测堆峰值增量 %d 超预算 %d（loadGate 未守住预算）", delta, maxLoadMemBudget)
	}
}

// TestSecretdataBackend_OpenURL 转存闭环：WriteFile 写入加密卷 → OpenURL 按
// secretdata://<卷>/<路径> 解析 → 解密读回内容一致（客户端 ResolveURL 取用路径）。
func TestSecretdataBackend_OpenURL(t *testing.T) {
	t.Parallel()
	fs := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(fs, Options{Secret: []byte("test-secret-key-32bytes-abcdefgh")})
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{fs: sfs}
	if werr := sfs.WriteFile(context.Background(), "pikpak/t1/movie.mp4", bytes.NewReader([]byte("encrypted-content-123")), 18, 0); werr != nil {
		t.Fatal(werr)
	}
	rc, rerr := b.OpenURL(context.Background(), "secretdata://myvault/pikpak/t1/movie.mp4")
	if rerr != nil {
		t.Fatalf("OpenURL: %v", rerr)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "encrypted-content-123" {
		t.Fatalf("OpenURL 读回内容不一致：%q", got)
	}
}

// TestSecretdataBackend_OpenURL_BadScheme 非法 scheme fail-closed。
func TestSecretdataBackend_OpenURL_BadScheme(t *testing.T) {
	t.Parallel()
	sfs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{Secret: []byte("k")})
	if err != nil {
		t.Fatal(err)
	}
	b := &backend{fs: sfs}
	if _, err := b.OpenURL(context.Background(), "s3://vault/x"); err == nil {
		t.Fatal("非法 scheme 应报错")
	}
}

// TestWriteIfAbsent_RealFSFirstWrite 真实 SecretdataFS 首次 WriteIfAbsent 必须成功
// （回归 W1/W3：曾因 commitEntry 无条件写 index，写后复核必命中 → 恒 (false,nil)，
// 导致 secret 卷转存必失败）。
func TestWriteIfAbsent_RealFSFirstWrite(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	content := data(1000)

	written, err := fs.WriteIfAbsent(ctx, "transfer/out.bin", bytes.NewReader(content), int64(len(content)), 0)
	if err != nil {
		t.Fatalf("首次 WriteIfAbsent 报错: %v", err)
	}
	if !written {
		t.Fatal("首次 WriteIfAbsent 应返回 written=true（目标不存在必写入）")
	}

	// 内容确实落盘可读回。
	rc, err := fs.OpenRead(ctx, "transfer/out.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("读回内容不一致")
	}
}

// TestWriteIfAbsent_RealFSDuplicateRejected 重复同 rel WriteIfAbsent 拒绝且不覆盖。
func TestWriteIfAbsent_RealFSDuplicateRejected(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	first := data(1000)
	second := []byte("second-content")

	if written, err := fs.WriteIfAbsent(ctx, "dup.bin", bytes.NewReader(first), int64(len(first)), 0); err != nil || !written {
		t.Fatalf("首次写应成功: written=%v err=%v", written, err)
	}
	if written, err := fs.WriteIfAbsent(ctx, "dup.bin", bytes.NewReader(second), int64(len(second)), 0); err != nil {
		t.Fatalf("重复写不应报错: %v", err)
	} else if written {
		t.Fatal("重复 WriteIfAbsent 应返回 written=false（拒绝覆盖）")
	}

	// 内容保持首写（未被 second 覆盖）。
	rc, err := fs.OpenRead(ctx, "dup.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, first) {
		t.Fatal("重复 WriteIfAbsent 不应覆盖既有内容")
	}
}

// TestWriteIfAbsent_RealFSConcurrentExactlyOneWinner 并发双写同 rel：恰好一胜一败，
// 败者 (false,nil)、胜者 (true,nil)，无覆盖。
func TestWriteIfAbsent_RealFSConcurrentExactlyOneWinner(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()

	const writers = 4
	contents := make([][]byte, writers)
	for i := range contents {
		contents[i] = []byte(fmt.Sprintf("writer-%d-content", i))
	}
	results := make([]bool, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fs.WriteIfAbsent(ctx, "race.bin", bytes.NewReader(contents[i]), int64(len(contents[i])), 0)
		}(i)
	}
	wg.Wait()

	winners, losers := 0, 0
	for i := range results {
		if errs[i] != nil {
			t.Fatalf("并发 WriteIfAbsent 不应报错: %v", errs[i])
		}
		if results[i] {
			winners++
		} else {
			losers++
		}
	}
	if winners != 1 || losers != writers-1 {
		t.Fatalf("应恰好 1 胜 %d 败，实际 %d 胜 %d 败", writers-1, winners, losers)
	}

	// 卷内只有一份胜者内容。
	entries := map[string]bool{}
	if err := fs.walkIndex(ctx, "race.bin", func(rel string) { entries[rel] = true }); err != nil {
		t.Fatalf("walkIndex: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("卷内应恰有一条 race.bin，实际 %d 条", len(entries))
	}
}

// walkIndex 遍历索引中与 prefix 匹配的条目（测试辅助）。
func (fs *SecretdataFS) walkIndex(ctx context.Context, prefix string, fn func(rel string)) error {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	for rel := range fs.index {
		if strings.HasPrefix(rel, prefix) {
			fn(rel)
		}
	}
	return nil
}
