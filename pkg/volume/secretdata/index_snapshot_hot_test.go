// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// bigMeta 构造一个"大 Meta"：含大量 blocklets（真实卷里单文件可达 43 万个）。
// 用于验证快照**不再承载**这些冷结构。
func bigMeta(blocklets int) *shardseal.Meta {
	bl := make([]shardseal.BlockletInfo, blocklets)
	for i := range bl {
		bl[i] = shardseal.BlockletInfo{
			Offset: int64(i) * 65536, Size: 65536, EncSize: 65600,
			EncOffset:  int64(i) * 65600,
			OrigSHA256: "0123456789abcdef0123456789abcdef", OrigSHA256Full: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		}
	}
	return &shardseal.Meta{
		Version: 1, Algorithm: "shardseal/aes-256-gcm",
		Original: shardseal.OriginalInfo{Name: "x.mp4", Size: int64(blocklets) * 65536, SHA256: "deadbeef"},
		Chunks:   []shardseal.ChunkInfo{{Index: 0, FileName: "c0", Blocklets: bl}},
	}
}

// TestIndexSnapshot_OmitsMetaAndBlocklets 锁定 P1 核心：
// 快照**不得**承载 `Meta`（含 blocklets）—— 这些是冷数据，只在读文件内容时按需加载。
//
// 判据有决定性：结构层已经没有 `Meta` 字段（编译期保证），再断言**落盘体积**极小
// （含 meta 时会大几个数量级，只看结构断言会漏掉“别的字段悄悄变大”）。
func TestIndexSnapshot_OmitsMetaAndBlocklets(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}

	// 20 万个 blocklets ⇒ 若快照仍含 Meta，明文 JSON 会是几百 MB 量级。
	big := bigMeta(200000)
	fs.index = map[string]*metaEntry{
		"a/b/x.mp4": {size: 100, mtime: 1700000000, dirSeg: "a/b", metaName: "zzz",
			meta: big, origSHA256: "deadbeef", baseVersion: 3, phys: 4096},
	}
	snap := fs.buildIndexSnapshot([]string{"a/b"})
	if len(snap.Entries) != 1 {
		t.Fatalf("应有 1 条，得到 %d", len(snap.Entries))
	}
	// 结构层：`indexSnapshotEntry` 已**没有** Meta 字段（编译期即保证）；
	// 这里再断言热字段被正确提升。
	if snap.Entries[0].OrigSHA256 != "deadbeef" {
		t.Fatalf("热字段 origSHA256 必须被提升进快照，得到 %q", snap.Entries[0].OrigSHA256)
	}
	if snap.Entries[0].BaseVersion != 3 {
		t.Fatalf("baseVersion 应保留，得到 %d", snap.Entries[0].BaseVersion)
	}

	fs.writeIndexSnapshot(snap)
	st, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("快照应已落盘: %v", serr)
	}
	// 含 20 万 blocklets 时快照会是几十 MB；不含则应远小于 64 KiB。
	if st.Size() > 64<<10 {
		t.Fatalf("快照体积应远小于 64 KiB（不含 blocklets），实际 %d 字节", st.Size())
	}
}

// TestIndexSnapshot_LoadLeavesMetaNil 锁定：加载后 `meta` 必须为 nil（交给 ensureEntryMeta 惰性加载），
// 且热字段（origSHA256/baseVersion/phys）被正确恢复。
func TestIndexSnapshot_LoadLeavesMetaNil(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "vol.idx")
	secret := []byte("0123456789abcdef0123456789abcdef")
	fs := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}

	fs.index = map[string]*metaEntry{
		"a/b/x.mp4": {size: 100, mtime: 1700000000, dirSeg: "a/b", metaName: "zzz",
			meta: bigMeta(1000), origSHA256: "cafebabe", baseVersion: 7, phys: 8192, dataDir: "a/b"},
	}
	fs.writeIndexSnapshot(fs.buildIndexSnapshot([]string{"a/b"}))

	snap, ok := fs.loadIndexSnapshot()
	if !ok {
		t.Fatal("快照应能载入")
	}
	// 在**新实例**上应用，模拟重启。
	fresh := &SecretdataFS{secret: secret, opts: Options{
		Secret: secret, Algorithm: "shardseal/aes-256-gcm", IndexSnapshotPath: path}}
	fresh.applyIndexSnapshot(snap)

	e, hit := fresh.index["a/b/x.mp4"]
	if !hit {
		t.Fatal("条目应被恢复到索引")
	}
	if e.meta != nil {
		t.Fatal("加载后 meta 必须为 nil（惰性加载），否则 OOM 根因未除")
	}
	if e.origSHA256 != "cafebabe" {
		t.Fatalf("origSHA256 应被恢复，得到 %q", e.origSHA256)
	}
	if e.baseVersion != 7 || e.phys != 8192 || e.size != 100 || e.dataDir != "a/b" {
		t.Fatalf("热字段恢复不完整: %+v", e)
	}
}

// TestHotOrigSHA256_PrefersFlatField 锁定「热路径不依赖冷结构」的核心保证：
// checksum 必须能在 **meta 为 nil**（快照加载后的常态）时给出；
// meta 已加载时才回退到它。
func TestHotOrigSHA256_PrefersFlatField(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		e    *metaEntry
		want string
	}{
		{"只有扁平热字段（快照加载后的常态）", &metaEntry{origSHA256: "flat"}, "flat"},
		{"只有已加载的 meta（全量扫描路径）", &metaEntry{meta: &shardseal.Meta{Original: shardseal.OriginalInfo{SHA256: "frommeta"}}}, "frommeta"},
		{"两者都有 ⇒ 热字段优先", &metaEntry{origSHA256: "flat", meta: &shardseal.Meta{Original: shardseal.OriginalInfo{SHA256: "frommeta"}}}, "flat"},
		{"都无 ⇒ 空（调用方 stat 兜底）", &metaEntry{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.e.hotOrigSHA256(); got != tc.want {
				t.Fatalf("hotOrigSHA256()=%q, want %q", got, tc.want)
			}
		})
	}
}
