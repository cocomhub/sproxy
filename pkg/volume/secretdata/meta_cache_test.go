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

// metaCacheCountingFS 统计底层 OpenRead 次数（验证缓存命中不重复下载）。
type metaCacheCountingFS struct {
	syncpkg.FS
	reads int
	data  []byte
}

func (c *metaCacheCountingFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	c.reads++
	return io.NopCloser(bytes.NewReader(c.data)), nil
}

// TestMetaCache_HitSkipsDownload 缓存命中必须不再读底层（外部卷 ⇒ 不再起下载进程）。
// 变异：readBlobCached 忽略缓存 ⇒ reads 变 2 ⇒ 红。
func TestMetaCache_HitSkipsDownload(t *testing.T) {
	t.Parallel()
	inner := &metaCacheCountingFS{data: []byte("cipher-blob")}
	fs := &SecretdataFS{inner: inner, opts: Options{MetaCacheDir: t.TempDir()}}
	ctx := context.Background()
	for range 2 {
		if _, err := fs.readBlobCached(ctx, "c/b/meta1"); err != nil {
			t.Fatalf("readBlobCached: %v", err)
		}
	}
	if inner.reads != 1 {
		t.Fatalf("第 2 次应命中缓存（底层 reads 应为 1），实际 %d", inner.reads)
	}
}

// TestMetaCache_DropForcesRedownload 删除缓存（解密失败自愈）后必须重新下载。
func TestMetaCache_DropForcesRedownload(t *testing.T) {
	t.Parallel()
	inner := &metaCacheCountingFS{data: []byte("cipher-blob")}
	fs := &SecretdataFS{inner: inner, opts: Options{MetaCacheDir: t.TempDir()}}
	ctx := context.Background()
	if _, err := fs.readBlobCached(ctx, "c/b/meta2"); err != nil {
		t.Fatal(err)
	}
	fs.dropBlobCache("c/b/meta2")
	if _, err := fs.readBlobCached(ctx, "c/b/meta2"); err != nil {
		t.Fatal(err)
	}
	if inner.reads != 2 {
		t.Fatalf("删缓存后应重下（reads=2），实际 %d", inner.reads)
	}
	// 禁止残留 .part 文件
	ents, _ := os.ReadDir(filepath.Dir(fs.metaCachePath("c/b/meta2")))
	for _, e := range ents {
		if filepath.Ext(e.Name()) != "" && e.Name() != filepath.Base(fs.metaCachePath("c/b/meta2")) {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}

// TestMetaCache_PopulatedByUpload 上传写路径必须把 meta blob 写入密文缓存
// （否则「写后重建索引」仍要下载这些 meta）。变异：uploadBlob 不调 putBlobCache ⇒ 红。
func TestMetaCache_PopulatedByUpload(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "metacache")
	fs, err := NewFS(syncpkg.NewLocalFS(filepath.Join(dir, "inner"), nil), Options{
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
		MetaCacheDir: cacheDir,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if werr := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, "a/b.txt", bytes.NewReader([]byte("hello")), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	n := 0
	_ = filepath.Walk(cacheDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	if n < 2 { // 至少 file-meta + dir-meta 各一份
		t.Fatalf("写路径应缓存 meta blob（期望 ≥2 个文件），实际 %d", n)
	}
}

// TestMetaCache_ManualDeleteRefetch 人工删除缓存目录后，新实例装配必须自动重新拉取并重建
// 精简缓存（缓存永远可从权威 blob 重建）。变异：readFileMetaFast 不写缓存 ⇒ 重建后仍为 0。
func TestMetaCache_ManualDeleteRefetch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, "mc")
	secret := []byte("0123456789abcdef0123456789abcdef")
	mk := func() *SecretdataFS {
		fs, err := NewFS(syncpkg.NewLocalFS(filepath.Join(dir, "inner"), nil), Options{
			Secret: secret, MetaCacheDir: cacheDir,
		})
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		return fs
	}
	ctx := context.Background()
	fs1 := mk()
	if werr := fs1.WriteFile(ctx, syncpkg.LogicalKey{}, "a/f.txt", bytes.NewReader([]byte("hello")), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if n := countFiles(t, cacheDir); n < 2 {
		t.Fatalf("写后缓存应 ≥2 个文件，实际 %d", n)
	}
	if rerr := os.RemoveAll(cacheDir); rerr != nil {
		t.Fatalf("人工删除缓存: %v", rerr)
	}
	// 模拟重启：新实例 loadIndex 走权威 blob（缓存已空）并回填。
	fs2 := mk()
	if n := countFiles(t, cacheDir); n < 2 {
		t.Fatalf("重启后应自动重新拉取并重建缓存（≥2），实际 %d", n)
	}
	// 访问文件仍正确（内容完整）。
	e, ok := fs2.index["a/f.txt"]
	if !ok {
		t.Fatal("重启后索引应含该文件")
	}
	if e.meta == nil {
		t.Fatal("重启后条目应带 meta")
	}
}

// TestEnsureEntryMeta_SelfHeal 索引条目 meta 缺失（旧条目/降级）时，读路径必须回源自愈。
// 变异：ensureEntryMeta 直接返回 ⇒ e.meta 仍为 nil ⇒ 红。
func TestEnsureEntryMeta_SelfHeal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs, err := NewFS(syncpkg.NewLocalFS(filepath.Join(dir, "inner"), nil), Options{
		Secret: secret, MetaCacheDir: filepath.Join(dir, "mc"),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if werr := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "a/g.txt", bytes.NewReader([]byte("world")), 5, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	real, ok := fs.index["a/g.txt"]
	if !ok || real.meta == nil {
		t.Fatal("写后条目应带 meta")
	}
	// 模拟「索引里的 meta 丢失」（只保留定位信息）。
	bare := &metaEntry{dirSeg: real.dirSeg, metaName: real.metaName}
	if bare.meta != nil {
		t.Fatal("前置条件")
	}
	if merr := fs.ensureEntryMeta(ctx, bare); merr != nil {
		t.Fatalf("ensureEntryMeta: %v", merr)
	}
	if bare.meta == nil || bare.meta.Original.SHA256 != real.meta.Original.SHA256 {
		t.Fatal("自愈后应恢复等价 meta")
	}
}

func countFiles(t *testing.T, root string) int {
	t.Helper()
	n := 0
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			n++
		}
		return nil
	})
	return n
}
