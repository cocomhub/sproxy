// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// TestIndexSnapshot_RoundTrip 快照加解密往返 + 指纹/算法校验（换密钥或换算法必须作废）。
// 变异：loadIndexSnapshot 不校验 SecretFP ⇒ 换密钥用例红。
func TestIndexSnapshot_RoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	snap := &indexSnapshot{
		Version: indexSnapshotVersion, Algorithm: "shardseal/aes-256-gcm",
		SecretFP: snapshotSecretFP(secret), Containers: []string{"a/b"},
		Entries: []indexSnapshotEntry{{Rel: "x.mp4", Size: 10, DirSeg: "a/b", MetaName: "zzz",
			Meta: &shardseal.Meta{Version: 1}}},
	}
	fs.writeIndexSnapshot(snap)
	if _, serr := os.Stat(path); serr != nil {
		t.Fatalf("快照应已落盘: %v", serr)
	}
	got, ok := fs.loadIndexSnapshot()
	if !ok || got == nil || len(got.Entries) != 1 || got.Entries[0].Rel != "x.mp4" {
		t.Fatalf("快照载入失败: ok=%v", ok)
	}

	// 换密钥 ⇒ 解密/指纹校验失败 ⇒ 作废（回退全量）。
	other := &SecretdataFS{secret: []byte("ffffffffffffffffffffffffffffffff"), opts: Options{
		Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	if _, ok := other.loadIndexSnapshot(); ok {
		t.Fatal("换密钥后快照必须作废")
	}
}

// TestIndexSnapshot_ContainersMatch 容器集合变化（外部直改盘新增/删除）必须判为不匹配。
func TestIndexSnapshot_ContainersMatch(t *testing.T) {
	t.Parallel()
	snap := &indexSnapshot{Containers: []string{"a", "b"}}
	if !snapshotContainersMatch(snap, []string{"b", "a"}) {
		t.Fatal("集合相同（顺序无关）应匹配")
	}
	if snapshotContainersMatch(snap, []string{"a"}) {
		t.Fatal("数量不同应不匹配")
	}
	if snapshotContainersMatch(snap, []string{"a", "c"}) {
		t.Fatal("成员不同应不匹配")
	}
}
