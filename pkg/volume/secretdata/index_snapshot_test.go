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

// TestIndexSnapshot_InvalidatedByWrite 写操作后快照必须被删除（否则重启载入陈旧索引：
// 删除的文件"复活"、新文件"不可见"）。变异：invalidateIndexSnapshot 不删文件 ⇒ 红。
func TestIndexSnapshot_InvalidatedByWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	// 直接构造（跳过装配）验证失效行为
	sd := &SecretdataFS{opts: Options{IndexSnapshotPath: path}, secret: []byte("k")}
	if werr := os.WriteFile(path, []byte("snapshot"), 0o600); werr != nil {
		t.Fatal(werr)
	}
	sd.invalidateIndexSnapshot()
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Fatalf("写后快照应被删除，stat err=%v", serr)
	}
}

// TestIndexSnapshot_CachedAcrossCalls 锁定**进程内快照缓存**（P0 内存优化，2026-11-12）。
//
// 回归背景：快照实测 241.8 MB，`loadIndexSnapshot` 每次要 ReadFile(242MB) +
// gcm.Open(242MB) + Unmarshal(放大 2-3×)，pprof 实测占 inuse_space 的 **56.9%**
// （790MB/1388MB）；而 `tryLoadIndexSnapshotFast` 与 `tryLoadIndexSnapshot`
// **各自独立调用** ⇒ 两条快速路径都走到就重复展开两次（RSS 冲 15.3GB 被 OOM）。
//
// 判据（决定性）：第一次加载成功后**删掉快照文件**，第二次仍必须成功返回同一结果
// —— 只有"真的缓存了"才可能通过（未缓存则第二次数不到文件 ⇒ ok=false）。
// 变异：去掉 snapDone 短路 ⇒ 本用例红。
func TestIndexSnapshot_CachedAcrossCalls(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	fs.writeIndexSnapshot(&indexSnapshot{
		Version: indexSnapshotVersion, Algorithm: "shardseal/aes-256-gcm",
		SecretFP: snapshotSecretFP(secret), Containers: []string{"a/b"},
		Entries: []indexSnapshotEntry{{Rel: "x.mp4", Size: 10, DirSeg: "a/b", MetaName: "zzz",
			Meta: &shardseal.Meta{Version: 1}}},
	})

	first, ok := fs.loadIndexSnapshot()
	if !ok || first == nil {
		t.Fatalf("首次加载应成功: ok=%v", ok)
	}
	// 删掉磁盘快照：第二次若还去读盘 ⇒ 必失败（证明未缓存）。
	if rerr := os.Remove(path); rerr != nil {
		t.Fatalf("删除快照失败: %v", rerr)
	}
	second, ok2 := fs.loadIndexSnapshot()
	if !ok2 || second == nil {
		t.Fatal("缓存未生效：第二次不应再读盘（现已删除文件）")
	}
	if second != first {
		t.Fatal("第二次应返回缓存的同一快照实例，而不是重新解析")
	}
	if len(second.Entries) != 1 || second.Entries[0].Rel != "x.mp4" {
		t.Fatalf("缓存内容不符: %#v", second.Entries)
	}
}

// TestIndexSnapshot_CachesFailure 失败结果也缓存（避免反复读 242MB 磁盘 + 解密失败）。
func TestIndexSnapshot_CachesFailure(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	// 文件不存在 ⇒ 首次 false；随后**写入**一个合法快照，第二次仍应 false（已缓存失败）。
	if _, ok := fs.loadIndexSnapshot(); ok {
		t.Fatal("快照不存在时应 false")
	}
	fs.writeIndexSnapshot(&indexSnapshot{
		Version: indexSnapshotVersion, Algorithm: "shardseal/aes-256-gcm",
		SecretFP: snapshotSecretFP(secret), Containers: []string{"a"}})
	if _, ok := fs.loadIndexSnapshot(); ok {
		t.Fatal("失败结果应被缓存（不重试）；如需重试应由调用方新建 FS")
	}
}
