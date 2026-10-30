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
