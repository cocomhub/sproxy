// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁定设计 docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md
// §9.1（索引快照 nonce 确定性派生，消除本地重复存储）+ §10（派生绑定卷密钥）。

func snapshotFS(t *testing.T, secret []byte) (*SecretdataFS, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "state", "snap.bin")
	fs, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), Options{
		Secret:            secret,
		Algorithm:         testAlgo,
		Block:             shardseal.BlockPolicy{Mode: "random", Min: 64, Max: 128},
		TempDir:           t.TempDir(),
		IndexSnapshotPath: p,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return fs, p
}

// fixedSnapshot 构造字段固定的快照（无 wall-clock / 映射迭代顺序噪声 ⇒ 明文可逐字节复现）。
func fixedSnapshot() *indexSnapshot {
	return &indexSnapshot{
		Version: indexSnapshotVersion, Algorithm: testAlgo, SecretFP: "FIXEDFP",
		CreatedAt:  "2026-01-01T00:00:00Z",
		Containers: []string{"c1"},
		Dirs:       []string{"d1"},
		DirSegs:    map[string]string{"d1": "c1"},
		DirIDs:     map[string]string{"d1": "ID1"},
		Entries:    []indexSnapshotEntry{{Rel: "d1/f.bin", Size: 3, MTime: 1, DirSeg: "c1", MetaName: "m1"}},
	}
}

func writeSnapshot(t *testing.T, fs *SecretdataFS, snap *indexSnapshot) []byte {
	t.Helper()
	fs.writeIndexSnapshot(snap)
	raw, err := os.ReadFile(fs.opts.IndexSnapshotPath)
	if err != nil {
		t.Fatalf("ReadFile(snapshot): %v", err)
	}
	return raw
}

// snapshotPlainNonce 解密快照文件，返回 gzip 明文与 GCM nonce（格式 = magic(5) || nonce(12) || ct）。
func snapshotPlainNonce(t *testing.T, fs *SecretdataFS, raw []byte) (plain, nonce []byte) {
	t.Helper()
	key := snapshotKey(fs.secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatalf("NewGCM: %v", err)
	}
	ct := raw[len(snapshotGzipMagic):]
	if len(ct) <= gcm.NonceSize() {
		t.Fatalf("快照密文过短: %d", len(ct))
	}
	nonce = append([]byte(nil), ct[:gcm.NonceSize()]...)
	plain, derr := gcm.Open(nil, nonce, ct[gcm.NonceSize():], nil)
	if derr != nil {
		t.Fatalf("gcm.Open: %v", derr)
	}
	return plain, nonce
}

// TestIndexSnapshotNonce_DeterministicAcrossWrites：同明文两次落快照 ⇒ 字节逐字节相同。
// 随机 nonce 时两次字节不同 ⇒ 本地重复存储、无法跳过重写（本用例红）。
func TestIndexSnapshotNonce_DeterministicAcrossWrites(t *testing.T) {
	t.Parallel()
	fs, _ := snapshotFS(t, []byte("test-secret-key-000"))
	snap := fixedSnapshot()
	raw1 := writeSnapshot(t, fs, snap)
	raw2 := writeSnapshot(t, fs, snap)
	if !bytes.Equal(raw1, raw2) {
		t.Fatalf("同明文两次快照必须逐字节相同（随机 nonce ⇒ 本地重复存储）: len %d vs %d", len(raw1), len(raw2))
	}
	_, n1 := snapshotPlainNonce(t, fs, raw1)
	_, n2 := snapshotPlainNonce(t, fs, raw2)
	if !bytes.Equal(n1, n2) {
		t.Fatalf("同明文两次快照的 nonce 必须相同: %x vs %x", n1, n2)
	}
}

// TestIndexSnapshotNonce_DerivedFromPlaintextAndKey：nonce 必为
// DeriveBytes(卷密钥, snapshotNonceLabel, 快照明文, NonceSize)（直接锁定派生公式）。
func TestIndexSnapshotNonce_DerivedFromPlaintextAndKey(t *testing.T) {
	t.Parallel()
	fs, _ := snapshotFS(t, []byte("test-secret-key-000"))
	plain, nonce := snapshotPlainNonce(t, fs, writeSnapshot(t, fs, fixedSnapshot()))
	want, werr := shardseal.DeriveBytes(fs.secret, snapshotNonceLabel, plain, 12)
	if werr != nil {
		t.Fatalf("DeriveBytes: %v", werr)
	}
	if !bytes.Equal(nonce, want) {
		t.Fatalf("快照 nonce 必须确定性派生自「明文 + 卷密钥」（非随机）: got %x want %x", nonce, want)
	}
}

// TestIndexSnapshotNonce_DifferentVolumeKeysDiffer（硬验收 §10.2）：同明文、不同卷密钥 ⇒ nonce 必不同。
func TestIndexSnapshotNonce_DifferentVolumeKeysDiffer(t *testing.T) {
	t.Parallel()
	fsA, _ := snapshotFS(t, []byte("volume-key-A-0123456789"))
	fsB, _ := snapshotFS(t, []byte("volume-key-B-0123456789"))
	plainA, nonceA := snapshotPlainNonce(t, fsA, writeSnapshot(t, fsA, fixedSnapshot()))
	plainB, nonceB := snapshotPlainNonce(t, fsB, writeSnapshot(t, fsB, fixedSnapshot()))
	if !bytes.Equal(plainA, plainB) {
		t.Fatal("夹具异常：两卷快照明文应相同（只有密钥不同）")
	}
	if bytes.Equal(nonceA, nonceB) {
		t.Fatalf("不同卷密钥必须派生不同 nonce（否则跨卷可关联）: %x", nonceA)
	}
}

// TestIndexSnapshot_SkipRewriteWhenUnchanged：目标已存在且内容相同 ⇒ 不重写（mtime 不变）；
// 内容变化 ⇒ 必须重写。随机 nonce 下"内容相同"永远不成立 ⇒ 恒重写（本用例红）。
func TestIndexSnapshot_SkipRewriteWhenUnchanged(t *testing.T) {
	t.Parallel()
	fs, p := snapshotFS(t, []byte("test-secret-key-000"))
	snap := fixedSnapshot()
	writeSnapshot(t, fs, snap)
	stamp := time.Unix(1000000000, 0)
	if err := os.Chtimes(p, stamp, stamp); err != nil {
		t.Fatalf("Chtimes: %v", err)
	}
	writeSnapshot(t, fs, snap) // 内容同上 ⇒ 应跳过重写
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !fi.ModTime().Equal(stamp) {
		t.Fatalf("内容未变应跳过重写（mtime 不应变化）: got %v want %v", fi.ModTime(), stamp)
	}
	// 反向：内容变化 ⇒ 必须重写（mtime 更新）。
	changed := fixedSnapshot()
	changed.Entries[0].Size = 99
	writeSnapshot(t, fs, changed)
	fi2, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi2.ModTime().Equal(stamp) {
		t.Fatal("内容变化必须重写快照（mtime 应更新）")
	}
}
