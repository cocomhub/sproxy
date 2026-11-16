// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁定设计 docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md
// §9 第 8 项（卷级去重容器名改确定性派生）：随机容器名 ⇒ 每次重启/缓存丢失都会新建去重
// 容器、旧容器成孤儿（与 2026-11-06「重复容器」事故同型）。

func newDedupFSOn(t *testing.T, inner syncpkg.FS, secret []byte) *SecretdataFS {
	t.Helper()
	fs, err := NewFS(inner, Options{
		Secret:    secret,
		Algorithm: testAlgo,
		Block:     shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:   t.TempDir(),
		Dedup:     true,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs
}

func writeDedupFile(t *testing.T, fs *SecretdataFS, content []byte) {
	t.Helper()
	ctx := context.Background()
	if err := fs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func dedupDirOf(t *testing.T, fs *SecretdataFS) string {
	t.Helper()
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.dedupDir
}

// TestDedupDir_DeterministicAcrossInstances：同一卷两次装配（重启/缓存丢失）必须得到**同一个**
// 去重容器名 —— 随机名时第二个实例会新建容器、旧容器成孤儿（本用例红）。
func TestDedupDir_DeterministicAcrossInstances(t *testing.T) {
	t.Parallel()
	inner := syncpkg.NewLocalFS(t.TempDir(), nil)
	secret := []byte("test-secret-key-000")
	want, err := shardseal.DirNameFor(secret, "", dedupDirLogicalPath)
	if err != nil {
		t.Fatalf("DirNameFor: %v", err)
	}
	content := bytes.Repeat([]byte("dedup-deterministic-"), 8)

	fsA := newDedupFSOn(t, inner, secret)
	writeDedupFile(t, fsA, content)
	if got := dedupDirOf(t, fsA); got != want {
		t.Fatalf("去重容器名必须确定性派生: got %q want %q", got, want)
	}
	// 第二个实例（模拟重启）必须复用同一容器。
	fsB := newDedupFSOn(t, inner, secret)
	writeDedupFile(t, fsB, content)
	if got := dedupDirOf(t, fsB); got != want {
		t.Fatalf("重启后去重容器名必须与首次一致（否则旧容器成孤儿）: got %q want %q", got, want)
	}
}

// TestDedupDir_CrossVolumeDiffers（硬验收 §10.2）：不同卷密钥 ⇒ 去重容器名必不相同。
func TestDedupDir_CrossVolumeDiffers(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("cross-volume-"), 8)
	fsA := newDedupFSOn(t, syncpkg.NewLocalFS(t.TempDir(), nil), []byte("volume-key-A-0123456789"))
	fsB := newDedupFSOn(t, syncpkg.NewLocalFS(t.TempDir(), nil), []byte("volume-key-B-0123456789"))
	writeDedupFile(t, fsA, content)
	writeDedupFile(t, fsB, content)
	if dedupDirOf(t, fsA) == dedupDirOf(t, fsB) {
		t.Fatalf("不同卷密钥必须派生不同去重容器名（否则跨卷可关联）: %q", dedupDirOf(t, fsA))
	}
}

// TestDedupDir_LegacyRandomNameAdopted：老版本的**随机名**去重容器若存在，必须被沿用（不静默丢弃）。
// 随机名新建逻辑下，新实例会建一个新容器、把遗留容器成孤儿（本用例红）。
func TestDedupDir_LegacyRandomNameAdopted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	inner := syncpkg.NewLocalFS(root, nil)
	// 遗留去重容器：卷根下、无目录 meta 的目录（老实现用随机名）。
	const legacy = "abcdefgh1234"
	if err := os.MkdirAll(filepath.Join(root, legacy), 0o700); err != nil {
		t.Fatalf("MkdirAll(legacy): %v", err)
	}
	fs := newDedupFSOn(t, inner, []byte("test-secret-key-000"))
	writeDedupFile(t, fs, bytes.Repeat([]byte("legacy-pool-"), 8))
	if got := dedupDirOf(t, fs); got != legacy {
		t.Fatalf("既有随机名去重容器必须被沿用（不得静默丢弃）: got %q want %q", got, legacy)
	}
	// 池 blob 必须落在遗留容器内，而非新建的空目录。
	ctx := context.Background()
	entries, err := inner.ListDir(ctx, legacy)
	if err != nil {
		t.Fatalf("ListDir(legacy): %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("遗留去重容器未被使用（池 blob 未落入其中）")
	}
}
