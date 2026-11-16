// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"path"
	"sort"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁定 secretdata 卷写路径的**收敛确定性**（设计
// docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md §6 §9 S3）：
// 同一明文在同一卷上两次写入 ⇒ **分块 blob 名与密文逐字节相同**（秒传/跨路径去重的前提）。
//
// 范围说明（有意为之）：确定性收益落在**内容分块**（大对象，秒传/零重复的关键）。blob 名不再
// 确定的两处元数据均属**每卷/每写可变语义**：
//   - 文件 meta：含 `Original.CTime`（每次写入的创建时间）与 `BaseVersion`（乐观锁序号）；
//   - 目录 meta：中段用目录 `DirID`（卷内随机生成、卷内稳定，跨卷不同）。
// 两者都是小型对象，且卷内可复用；不影响大对象去重/秒传。

// convTestSecret 收敛卷测试密钥（通用假数据，非真实凭据）。
var convTestSecret = []byte("conv-test-volume-key-0001")

// newConvFSOn 在给定底层目录上建一个**收敛档**（确定性密文）secretdata 卷。
func newConvFSOn(t *testing.T, dir string) *SecretdataFS {
	t.Helper()
	inner := syncpkg.NewLocalFS(dir, nil)
	fs, err := NewFS(inner, Options{
		Secret:    convTestSecret,
		Algorithm: shardseal.AlgorithmNameConvergent,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

// containerOf 返回 rel 的分块容器（去重引用指向 dataDir，否则 dirSeg）。
func containerOf(t *testing.T, fs *SecretdataFS, rel string) string {
	t.Helper()
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	e, ok := fs.index[rel]
	if !ok {
		t.Fatalf("索引无条目 %q", rel)
	}
	if e.dataDir != "" {
		return e.dataDir
	}
	return e.dirSeg
}

// collectBlobs 读取 container 内匹配 kind 的 blob（名→字节，按名排序）。
func collectBlobs(t *testing.T, ctx context.Context, fs *SecretdataFS, container string, kind shardseal.NameKind) map[string][]byte {
	t.Helper()
	entries, err := fs.inner.ListDir(ctx, container)
	if err != nil {
		t.Fatalf("ListDir(%s): %v", container, err)
	}
	out := map[string][]byte{}
	for _, e := range entries {
		if e.IsDir || shardseal.ClassifyName(e.Name) != kind {
			continue
		}
		rc, oerr := fs.inner.OpenRead(ctx, path.Join(container, e.Name))
		if oerr != nil {
			t.Fatalf("OpenRead %s: %v", e.Name, oerr)
		}
		b, rerr := io.ReadAll(rc)
		rc.Close()
		if rerr != nil {
			t.Fatalf("ReadAll %s: %v", e.Name, rerr)
		}
		out[e.Name] = b
	}
	return out
}

func sortedNames(m map[string][]byte) []string {
	names := make([]string, 0, len(m))
	for n := range m {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func assertSameBlobs(t *testing.T, what string, a, b map[string][]byte) {
	t.Helper()
	na, nb := sortedNames(a), sortedNames(b)
	if len(na) != len(nb) {
		t.Fatalf("%s：blob 数不同 %d vs %d（a=%v b=%v）", what, len(na), len(nb), na, nb)
	}
	for i := range na {
		if na[i] != nb[i] {
			t.Fatalf("%s：blob 名不同 %q vs %q", what, na[i], nb[i])
		}
		if !bytes.Equal(a[na[i]], b[nb[i]]) {
			t.Fatalf("%s：blob %s 密文不同", what, na[i])
		}
	}
}

// TestConvergentVolume_ChunksDeterministicAcrossInstances 同内容 + 同逻辑路径 + 同 mtime，
// 在**两个相同密钥的卷实例**上写入 ⇒ 分块 blob 名集合与逐字节密文相同（秒传前提）。
func TestConvergentVolume_ChunksDeterministicAcrossInstances(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := bytes.Repeat([]byte("convergent-content-"), 40) // ~720B（多块）
	const mtime = int64(1_700_000_000_000_000_000)

	write := func(dir string) (*SecretdataFS, string) {
		fs := newConvFSOn(t, dir)
		if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "x.bin", bytes.NewReader(content), int64(len(content)), mtime); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return fs, containerOf(t, fs, "x.bin")
	}
	fsA, contA := write(t.TempDir())
	fsB, contB := write(t.TempDir())

	chunksA := collectBlobs(t, ctx, fsA, contA, shardseal.KindChunk)
	chunksB := collectBlobs(t, ctx, fsB, contB, shardseal.KindChunk)
	if len(chunksA) == 0 {
		t.Fatal("未收集到任何分块 blob")
	}
	assertSameBlobs(t, "分块", chunksA, chunksB)

	// 读回校验（确定性密文仍可正常解密）。
	rc, err := fsA.OpenRead(ctx, "x.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, content) {
		t.Fatal("读回内容与原文不一致")
	}
}

// TestConvergentVolume_CrossPathSameChunks 同内容放在两个不同逻辑路径 ⇒ 分块 blob 名与密文
// 相同（派生源不含路径 ⇒ 跨路径去重）。
func TestConvergentVolume_CrossPathSameChunks(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := bytes.Repeat([]byte("cross-path-content-"), 30)
	const mtime = int64(1_700_000_000_000_000_000)
	fs := newConvFSOn(t, t.TempDir())
	for _, rel := range []string{"d1/one.bin", "d2/other.bin"} {
		if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(content), int64(len(content)), mtime); err != nil {
			t.Fatalf("WriteFile(%s): %v", rel, err)
		}
	}
	chunks1 := collectBlobs(t, ctx, fs, containerOf(t, fs, "d1/one.bin"), shardseal.KindChunk)
	chunks2 := collectBlobs(t, ctx, fs, containerOf(t, fs, "d2/other.bin"), shardseal.KindChunk)
	if len(chunks1) == 0 {
		t.Fatal("未收集到分块 blob")
	}
	assertSameBlobs(t, "跨路径分块", chunks1, chunks2)
}

// TestConvergentVolume_DifferentSecretsDiffer 不同卷密钥 ⇒ 同内容分块名/密文必不同。
func TestConvergentVolume_DifferentSecretsDiffer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	content := bytes.Repeat([]byte("secret-bound-content-"), 30)
	const mtime = int64(1_700_000_000_000_000_000)
	mk := func(secret []byte) map[string][]byte {
		inner := syncpkg.NewLocalFS(t.TempDir(), nil)
		fs, err := NewFS(inner, Options{
			Secret: secret, Algorithm: shardseal.AlgorithmNameConvergent,
			Block: shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128}, TempDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "x.bin", bytes.NewReader(content), int64(len(content)), mtime); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		return collectBlobs(t, ctx, fs, containerOf(t, fs, "x.bin"), shardseal.KindChunk)
	}
	a := mk([]byte("conv-test-volume-key-0001"))
	b := mk([]byte("conv-test-volume-key-0002"))
	if len(a) == 0 {
		t.Fatal("未收集到分块 blob")
	}
	if bytes.Equal(a[sortedNames(a)[0]], b[sortedNames(b)[0]]) {
		t.Fatal("不同卷密钥的分块密文不得相同（跨卷可关联）")
	}
}
