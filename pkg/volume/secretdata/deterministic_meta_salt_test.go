// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁定设计 docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md
// §9 第 7 项（目录 meta 的盐改确定性派生）：盐的派生源 = meta 明文内容标识 + **卷密钥**，
// 使「同内容 ⇒ 同盐」且「不同卷 ⇒ 不同盐」。

// dirMetaFixture 建一个已存在容器 "a" 的 FS，并给出固定的目录 meta 明文。
func dirMetaFixture(t *testing.T, secret []byte) (*SecretdataFS, string, *dirMeta) {
	t.Helper()
	fs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
		Secret:    secret,
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	ctx := context.Background()
	if merr := fs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir(a): %v", merr)
	}
	fs.mu.RLock()
	seg := fs.dirSegs["a"]
	fs.mu.RUnlock()
	if seg == "" {
		t.Fatal("夹具异常：容器 a 未建立")
	}
	dm := &dirMeta{Version: 1, Algorithm: testAlgo, KDF: "scrypt", Type: "dir",
		Name: "a", DirID: "EXAMPLEID", MTime: "2026-01-01T00:00:00Z"}
	return fs, seg, dm
}

// writeDirMetaBlob 落一次目录 meta 并读回 blob 字节。
func writeDirMetaBlob(t *testing.T, fs *SecretdataFS, container string, dm *dirMeta) []byte {
	t.Helper()
	ctx := context.Background()
	name, err := fs.writeDirMetaLocked(ctx, container, dm)
	if err != nil {
		t.Fatalf("writeDirMetaLocked: %v", err)
	}
	blob, rerr := readBlob(ctx, fs.inner, path.Join(container, name))
	if rerr != nil {
		t.Fatalf("readBlob(%s): %v", name, rerr)
	}
	return blob
}

// assertDirMetaSaltDerived 断言 blob 内嵌盐 == DeriveBytes(卷密钥, dirMetaSaltLabel, meta 明文)
// ——即盐必须由「meta 明文内容 + 卷密钥」确定性派生，而**不是随机**。
func assertDirMetaSaltDerived(t *testing.T, fs *SecretdataFS, blob []byte) {
	t.Helper()
	salt, err := shardseal.MetaBlobSalt(blob)
	if err != nil {
		t.Fatalf("MetaBlobSalt: %v", err)
	}
	key, kerr := shardseal.DeriveKey(fs.secret, salt, fs.algoVer)
	if kerr != nil {
		t.Fatalf("DeriveKey: %v", kerr)
	}
	plain, derr := shardseal.DecryptMetaJSON(key, blob)
	if derr != nil {
		t.Fatalf("DecryptMetaJSON: %v", derr)
	}
	want, werr := shardseal.DeriveBytes(fs.secret, dirMetaSaltLabel, plain, shardseal.SaltLen)
	if werr != nil {
		t.Fatalf("DeriveBytes: %v", werr)
	}
	if !bytes.Equal(salt, want) {
		t.Fatalf("目录 meta 盐必须确定性派生自 meta 明文（非随机）: got %x want %x", salt, want)
	}
}

// TestDirMetaSalt_DeterministicAcrossWrites：**同明文**两次落 meta ⇒ 内嵌盐必须相同。
// 随机盐时两次不同 ⇒ 同一容器的 meta blob 名随之不同 ⇒ 重复 meta（本用例红）。
func TestDirMetaSalt_DeterministicAcrossWrites(t *testing.T) {
	t.Parallel()
	fs, seg, dm := dirMetaFixture(t, []byte("test-secret-key-000"))
	s1, err := shardseal.MetaBlobSalt(writeDirMetaBlob(t, fs, seg, dm))
	if err != nil {
		t.Fatalf("MetaBlobSalt(1): %v", err)
	}
	s2, err := shardseal.MetaBlobSalt(writeDirMetaBlob(t, fs, seg, dm))
	if err != nil {
		t.Fatalf("MetaBlobSalt(2): %v", err)
	}
	if !bytes.Equal(s1, s2) {
		t.Fatalf("同明文两次落 meta 的盐必须相同（否则 meta blob 字节/名不同 ⇒ 重复 meta）: %x vs %x", s1, s2)
	}
}

// TestDirMetaSalt_DerivedFromPlaintext：盐必须恰为「meta 明文 + 卷密钥」的确定性派生。
func TestDirMetaSalt_DerivedFromPlaintext(t *testing.T) {
	t.Parallel()
	fs, seg, dm := dirMetaFixture(t, []byte("test-secret-key-000"))
	assertDirMetaSaltDerived(t, fs, writeDirMetaBlob(t, fs, seg, dm))
}

// TestCreateContainer_DirMetaSaltDerived：建容器路径（createContainerLocked）的目录 meta 盐同样派生。
func TestCreateContainer_DirMetaSaltDerived(t *testing.T) {
	t.Parallel()
	fs := newFS(t)
	ctx := context.Background()
	if err := fs.MakeDir(ctx, "deep"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	fs.mu.RLock()
	seg := fs.dirSegs["deep"]
	fs.mu.RUnlock()
	_, blob, err := findDirMetaBlob(ctx, fs.inner, seg)
	if err != nil {
		t.Fatalf("findDirMetaBlob: %v", err)
	}
	assertDirMetaSaltDerived(t, fs, blob)
}

// TestDirMetaSalt_DifferentVolumeKeysDiffer（硬验收 §10.2）：同明文、不同卷密钥 ⇒ 盐必不相等。
func TestDirMetaSalt_DifferentVolumeKeysDiffer(t *testing.T) {
	t.Parallel()
	fsA, segA, dmA := dirMetaFixture(t, []byte("volume-key-A-0123456789"))
	fsB, segB, dmB := dirMetaFixture(t, []byte("volume-key-B-0123456789"))
	blobA := writeDirMetaBlob(t, fsA, segA, dmA)
	blobB := writeDirMetaBlob(t, fsB, segB, dmB)
	saltA, err := shardseal.MetaBlobSalt(blobA)
	if err != nil {
		t.Fatalf("MetaBlobSalt(A): %v", err)
	}
	saltB, err := shardseal.MetaBlobSalt(blobB)
	if err != nil {
		t.Fatalf("MetaBlobSalt(B): %v", err)
	}
	if bytes.Equal(saltA, saltB) {
		t.Fatalf("不同卷密钥必须派生不同盐（否则跨卷可关联）: %x", saltA)
	}
	// 两卷各自的盐都必须满足「由本方密钥派生」。
	assertDirMetaSaltDerived(t, fsA, blobA)
	assertDirMetaSaltDerived(t, fsB, blobB)
}

// compactNonceOf 读回紧凑缓存文件的 GCM nonce。
func compactNonceOf(t *testing.T, fs *SecretdataFS, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(fs.metaCachePath(rel))
	if err != nil {
		t.Fatalf("ReadFile(compact cache): %v", err)
	}
	const magic = "SDMC1"
	const nonceLen = 12
	off := len(magic) + 8
	if len(raw) < off+nonceLen {
		t.Fatalf("紧凑缓存文件过短: %d", len(raw))
	}
	return append([]byte(nil), raw[off:off+nonceLen]...)
}

// TestCompactMetaNonce_DeterministicAcrossWrites：紧凑缓存 nonce 必须确定性派生（同明文同卷 ⇒ 同 nonce）。
// 随机 nonce 时两次不同 ⇒ 本地缓存字节每次不同、无法复用（本用例红）。
func TestCompactMetaNonce_DeterministicAcrossWrites(t *testing.T) {
	t.Parallel()
	fs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
		Secret:       []byte("test-secret-key-000"),
		Algorithm:    testAlgo,
		Block:        shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:      t.TempDir(),
		MetaCacheDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	plain := []byte(`{"original":{"name":"a/b.bin"},"chunks":[1]}`)
	rel := "a/b"
	fs.putCompactMeta(rel, plain, 128)
	n1 := compactNonceOf(t, fs, rel)
	fs.putCompactMeta(rel, plain, 128)
	n2 := compactNonceOf(t, fs, rel)
	if !bytes.Equal(n1, n2) {
		t.Fatalf("同明文两次写紧凑缓存的 nonce 必须相同（确定性）: %x vs %x", n1, n2)
	}
}

// TestCompactMetaNonce_DifferentVolumeKeysDiffer（硬验收 §10.2）：同明文、不同卷密钥 ⇒ nonce 必不相等。
func TestCompactMetaNonce_DifferentVolumeKeysDiffer(t *testing.T) {
	t.Parallel()
	plain := []byte(`{"original":{"name":"a/b.bin"}}`)
	rel := "a/b"
	nonceFor := func(secret string) []byte {
		fs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
			Secret:       []byte(secret),
			Algorithm:    testAlgo,
			Block:        shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
			TempDir:      t.TempDir(),
			MetaCacheDir: t.TempDir(),
		})
		if err != nil {
			t.Fatalf("NewFS: %v", err)
		}
		fs.putCompactMeta(rel, plain, 128)
		return compactNonceOf(t, fs, rel)
	}
	if bytes.Equal(nonceFor("volume-key-A-0123456789"), nonceFor("volume-key-B-0123456789")) {
		t.Fatal("不同卷密钥必须派生不同 nonce（否则跨卷可关联）")
	}
}
