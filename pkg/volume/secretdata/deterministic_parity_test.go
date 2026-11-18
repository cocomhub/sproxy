// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"testing"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// 本文件锁定「**同一 secretdata 密钥 ⇒ 派生完全相同**」这一核心契约（用户 2026-11-18 要求）：
// 两个**独立卷**（不同目录/不同后端），只要密钥相同，写入相同内容后
// **整个目录树 + 所有 blob 字节**必须逐字节一致。
//
// 为什么能在**本地**验证（无需真实远端）：派生全部发生在 secretdata 加密层
// （容器名 `DirNameFor`、分块名、meta 名），**与存储后端无关** ⇒ 本地两个卷即可完整证明；
// 真实远端（百度）只影响「怎么把同一串字节存出去」，不影响「生成什么字节/名字」。

const paritySecret = "test-secret-key-32bytes-abcdefgh"

// parityMTime 是固定 mtime：meta 里记录原始时间，两次写入必须给同一值才可字节比对。
const parityMTime int64 = 1700000000

// snapshotTree 递归快照目录：相对路径 → 文件内容 sha256（含空目录，用 "<dir>" 标记）。
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			out[rel+"/"] = "<dir>"
			return nil
		}
		f, oerr := os.Open(p)
		if oerr != nil {
			return oerr
		}
		defer f.Close()
		h := sha256.New()
		if _, cerr := io.Copy(h, f); cerr != nil {
			return cerr
		}
		out[rel] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	if err != nil {
		t.Fatalf("遍历 %s: %v", root, err)
	}
	return out
}

// newParityVolume 在指定目录上建一个 secretdata 卷（同一 secret）。返回卷与其目录。
func newParityVolume(t *testing.T, dir string, secret string) *SecretdataFS {
	t.Helper()
	fs, err := NewFS(syncpkg.NewLocalFS(dir, nil), Options{Secret: []byte(secret)})
	if err != nil {
		t.Fatalf("NewFS(%s): %v", dir, err)
	}
	return fs
}

// parityCase 是一个写入用例（路径 + 内容）。
type parityCase struct {
	rel     string
	content []byte
}

// parityMatrix 覆盖**尺寸边界**与**目录层级**：
// 0/1B/blocklet 边界(256KiB±1)/chunk 下界(1MiB)/多 MiB/跨 chunk 大文件；
// 以及新建深层目录（会额外产生目录 meta）与同名覆盖写（第二个卷也要写入一次）。
func parityMatrix() []parityCase {
	pattern := func(n int) []byte {
		b := make([]byte, n)
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
		return b
	}
	return []parityCase{
		{"one.bin", []byte("x")},                  // 1 字节
		{"dir/a-255k.bin", pattern(255 << 10)},    // blocklet 下界
		{"dir/a-256k.bin", pattern(256 << 10)},    // blocklet 边界
		{"dir/a-256k1.bin", pattern(256<<10 + 1)}, // blocklet 边界 +1
		{"dir/sub/b-1m.bin", pattern(1 << 20)},    // chunk 下界
		{"dir/sub/b-1m1.bin", pattern(1<<20 + 1)},
		{"deep/x/y/z/c-4m.bin", pattern(4 << 20)}, // 多 chunk
		{"deep/x/y/z/c-4m1.bin", pattern(4<<20 + 1)},
		{"big.bin", pattern(9<<20 + 123)}, // 跨多个 chunk
		{"same-content-a.bin", pattern(300 << 10)},
		{"same-content-b.bin", pattern(300 << 10)}, // 同内容不同名
	}
}

// writeParity 用固定 mtime 写入一份内容。
func writeParity(t *testing.T, fs *SecretdataFS, rel string, content []byte) {
	t.Helper()
	if err := fs.WriteFile(context.Background(), syncpkg.LogicalKey{}, rel,
		bytes.NewReader(content), int64(len(content)), parityMTime); err != nil {
		t.Fatalf("WriteFile(%s, %d B): %v", rel, len(content), err)
	}
}

// TestDeterministic_TwoVolumesSameSecretAreByteIdentical 是本文件的核心用例：
// **两个独立卷 + 相同密钥 + 相同输入 ⇒ 整棵目录树与所有 blob 字节完全一致**。
func TestDeterministic_TwoVolumesSameSecretAreByteIdentical(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "volA"), filepath.Join(t.TempDir(), "volB")
	volA := newParityVolume(t, dirA, paritySecret)
	volB := newParityVolume(t, dirB, paritySecret)

	for _, c := range parityMatrix() {
		writeParity(t, volA, c.rel, c.content)
		writeParity(t, volB, c.rel, c.content)
	}

	treeA := snapshotTree(t, dirA)
	treeB := snapshotTree(t, dirB)
	if len(treeA) == 0 {
		t.Fatal("夹具异常：卷目录为空")
	}
	diffParity(t, treeA, treeB)
}

// diffParity 逐路径比对两棵树，报告缺失/多余/字节不同（附可读摘要）。
func diffParity(t *testing.T, a, b map[string]string) {
	t.Helper()
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	all := make([]string, 0, len(keys))
	for k := range keys {
		all = append(all, k)
	}
	sort.Strings(all)
	for _, k := range all {
		va, oka := a[k]
		vb, okb := b[k]
		switch {
		case !oka:
			t.Errorf("卷B 多出条目: %s", k)
		case !okb:
			t.Errorf("卷B 缺少条目: %s", k)
		case va != vb:
			t.Errorf("条目内容不同: %s (%s vs %s)", k, shortHash(va), shortHash(vb))
		}
	}
}

func shortHash(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

// TestDeterministic_DifferentSecretDiffers 锁定「派生**绑定卷密钥**」（用户硬规则）：
// 换密钥 ⇒ 容器名/分块名/全部字节都必须不同（否则跨卷可比对内容，违背确定性体系的安全前提）。
func TestDeterministic_DifferentSecretDiffers(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "k1"), filepath.Join(t.TempDir(), "k2")
	volA := newParityVolume(t, dirA, paritySecret)
	volB := newParityVolume(t, dirB, "another-secret-key-32bytes-zzzz")

	content := bytes.Repeat([]byte("k"), 300<<10)
	writeParity(t, volA, "same.bin", content)
	writeParity(t, volB, "same.bin", content)

	treeA, treeB := snapshotTree(t, dirA), snapshotTree(t, dirB)
	if len(treeA) == 0 || len(treeB) == 0 {
		t.Fatal("夹具异常：卷目录为空")
	}
	// 容器目录名必须不同（密钥绑定）。
	shared := 0
	for k := range treeA {
		if _, ok := treeB[k]; ok {
			shared++
		}
	}
	if shared != 0 {
		t.Fatalf("换密钥后不应有同名条目（共享 %d 条）⇒ 派生未绑定卷密钥 ✗", shared)
	}
}

// TestDeterministic_SameContentDifferentPathsShareChunk 锁定分层语义：
// **chunk blob 只由内容决定**（跨路径共享，秒传/去重前提）；
// **file meta 由内容+原始名决定**（不同名 ⇒ 不同 meta，这是设计使然，meta 里存着原始文件名）。
func TestDeterministic_SameContentDifferentPathsShareChunk(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "v")
	vol := newParityVolume(t, dir, paritySecret)
	content := bytes.Repeat([]byte("share"), 200<<10)
	writeParity(t, vol, "a/one.bin", content)
	writeParity(t, vol, "b/two.bin", content) // 不同目录 + 不同名

	// 从两个目录 meta 里读出各自分块名，断言**同一个** blob 名。
	chunkOf := func(rel string) string {
		vol.mu.RLock()
		e := vol.index[rel]
		vol.mu.RUnlock()
		if e == nil {
			t.Fatalf("索引无 %s", rel)
		}
		if e.meta == nil {
			t.Fatalf("%s 的 meta 未加载", rel)
		}
		if len(e.meta.Chunks) == 0 {
			t.Fatalf("%s 无分块", rel)
		}
		return e.meta.Chunks[0].FileName
	}
	c1, c2 := chunkOf("a/one.bin"), chunkOf("b/two.bin")
	if c1 != c2 {
		t.Fatalf("同内容跨路径的分块 blob 名必须相同（内容寻址），got %q vs %q", c1, c2)
	}
	// 索引里两个条目必须各自存在（逻辑层是两份），但共享同一物理 blob。
	vol.mu.RLock()
	n := len(vol.index)
	vol.mu.RUnlock()
	if n < 2 {
		t.Fatalf("两个逻辑文件应各自有条目，实际 %d", n)
	}
}

// TestDeterministic_BackendIndependent 锁定「**与存储后端无关**」：
// 同样的 (密钥, 内容, 路径) 写在**不同类型的 FS**（LocalFS vs PrefixFS 包装）上，
// 派生出的 blob 名必须相同 —— 这正是「本地卷与远端卷产物一致」的本地可证形式。
func TestDeterministic_BackendIndependent(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "plain"), filepath.Join(t.TempDir(), "prefixed")
	volA, err := NewFS(syncpkg.NewLocalFS(dirA, nil), Options{Secret: []byte(paritySecret)})
	if err != nil {
		t.Fatalf("NewFS A: %v", err)
	}
	// 不同**类型**的底层 FS（PrefixFS 包一层；透明装饰器语义）。
	prefixed := syncpkg.NewPrefixFS(syncpkg.NewLocalFS(dirB, nil), "bucket/")
	volB, err := NewFS(prefixed, Options{Secret: []byte(paritySecret)})
	if err != nil {
		t.Fatalf("NewFS B: %v", err)
	}

	content := bytes.Repeat([]byte("backend"), 128<<10)
	writeParity(t, volA, "same/name.bin", content)
	writeParity(t, volB, "same/name.bin", content)

	names := func(v *SecretdataFS, rel string) (chunk, meta string) {
		v.mu.RLock()
		e := v.index[rel]
		v.mu.RUnlock()
		if e == nil || e.meta == nil || len(e.meta.Chunks) == 0 {
			t.Fatalf("索引/meta 缺失: %s", rel)
		}
		return e.meta.Chunks[0].FileName, e.metaName
	}
	c1, m1 := names(volA, "same/name.bin")
	c2, m2 := names(volB, "same/name.bin")
	if c1 != c2 || m1 != m2 {
		t.Fatalf("派生必须与存储后端无关：chunk %q/%q, meta %q/%q", c1, c2, m1, m2)
	}
	// 容器名同样必须一致。
	volA.mu.RLock()
	segA := volA.index["same/name.bin"].dirSeg
	volA.mu.RUnlock()
	volB.mu.RLock()
	segB := volB.index["same/name.bin"].dirSeg
	volB.mu.RUnlock()
	if segA != segB {
		t.Fatalf("容器名必须与后端无关：%q vs %q", segA, segB)
	}
}

// ─── 金值锁定（golden，**整棵树**）──────────────────────────────────────────────
//
// 用户要求「**锁定固定全套派生行为**」：固定输入 ⇒ 固定产物（容器目录、目录 meta、分块、file meta，
// 连**密文字节都固定**）。任何**有意**的派生变更（新算法档/新域分隔前缀/改名映射/padding 策略）
// 必须**显式更新此表**，并在 PR 说明迁移影响（生产卷已存在的名字会与新建产物分叉）。
//
// 输入：secret=paritySecret、rel=`golden/f.bin`、内容=pattern(300KiB)、mtime=parityMTime。
var goldenTree = map[string]string{
	// 注（2026-11-18 有意变更）：容器名与 dir_id 改为**只依赖自身名** ⇒ 容器目录名与目录 meta
	// 的密文/名字都变了；**chunk 与 file meta 未变**（它们本就只依赖内容/自身文件名）。
	"d6QA13lG1Mh0QE6T/": "<dir>",
	"d6QA13lG1Mh0QE6T/A0AYrNZ8zbt9oBLPYUKnbuQgGUPqcWfQhzKEDt":                   "4e2c72245f46dceed70e093bfbe7a0da8c61084c4b1550ac28bdb784679ff00e",
	"d6QA13lG1Mh0QE6T/IGDljGcAUnkzxNjF/":                                        "<dir>",
	"d6QA13lG1Mh0QE6T/IGDljGcAUnkzxNjF/AvA2p3MGcRyKsdqOsCJEswEDn0UaN0ro0yPAu5v": "ad9cddb2de7af92b640067b0c627a36884c444801cef17dacaea40ea53ce82e2",
	"d6QA13lG1Mh0QE6T/IGDljGcAUnkzxNjF/BAAAGoptbndYOZPzKIyMB4ongrGDLj7Nfrcx":    "c8b5b85f8d4108104df6c373be08418314a79e53206eb02bf47e38430ff14cd1",
	"d6QA13lG1Mh0QE6T/IGDljGcAUnkzxNjF/BABEOogtUnDlD6FZ9w6vy2BD6DS7azTojZdf3Ox": "d5186f72763508a63bfe7e671bbd370cf47e52563138bbdbfa232bcf915b0dcd",
}

// TestDeterministic_GoldenTree 锁定整棵产物树（任一派生或密文变化 ⇒ 红）。
func TestDeterministic_GoldenTree(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "g")
	vol := newParityVolume(t, dir, paritySecret)
	content := make([]byte, 300<<10)
	for i := range content {
		content[i] = byte(i*31 + 7)
	}
	writeParity(t, vol, "golden/f.bin", content)

	got := snapshotTree(t, dir)
	if len(got) != len(goldenTree) {
		t.Errorf("产物条目数变了：got %d want %d（有意变更请同步更新 goldenTree 并说明迁移影响）",
			len(got), len(goldenTree))
	}
	diffParity(t, goldenTree, got)
}

// TestDeterministic_OperationSequenceParity 锁定「**相同操作序列 ⇒ 相同产物**」：
// 覆盖写（BaseVersion 自增）、改名、删除、删除后重建 —— 这些会改 meta 内容/版本/目录 meta，
// 是两个独立卷最容易分叉的地方（版本号、名称重写、dir_id 重写）。
func TestDeterministic_OperationSequenceParity(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "seqA"), filepath.Join(t.TempDir(), "seqB")
	volA := newParityVolume(t, dirA, paritySecret)
	volB := newParityVolume(t, dirB, paritySecret)

	step := func(name string, fn func(v *SecretdataFS)) {
		fn(volA)
		fn(volB)
		diffParity(t, snapshotTree(t, dirA), snapshotTree(t, dirB)) // 每一步后都必须一致
		t.Logf("步骤 %s 后两卷一致 ✓", name)
	}
	ctx := context.Background()
	c1 := bytes.Repeat([]byte("one"), 200<<10)
	c2 := bytes.Repeat([]byte("two"), 300<<10)

	step("写入 v1", func(v *SecretdataFS) { writeParity(t, v, "ops/f.bin", c1) })
	step("覆盖写 v2（内容变，BaseVersion+1）", func(v *SecretdataFS) {
		if err := v.WriteFile(ctx, syncpkg.LogicalKey{}, "ops/f.bin", bytes.NewReader(c2), int64(len(c2)), parityMTime); err != nil {
			t.Fatalf("覆盖写: %v", err)
		}
	})
	step("改名", func(v *SecretdataFS) {
		if err := v.Rename(ctx, "ops/f.bin", "ops/g.bin"); err != nil {
			t.Fatalf("Rename: %v", err)
		}
	})
	step("删除", func(v *SecretdataFS) {
		if err := v.Delete(ctx, "ops/g.bin"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})
	step("删除后重建同内容", func(v *SecretdataFS) { writeParity(t, v, "ops/h.bin", c1) })
}

// TestDeterministic_RestartParity 锁定「**重启后继续写 ⇒ 与从未重启的卷一致**」：
// 重启要经 loadIndex 从磁盘恢复 dirSegs/dirIDs/版本；恢复若与内存派生不一致（如 dir_id 仍随机/
// 版本回退），两卷会分叉 ✗。
func TestDeterministic_RestartParity(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "rA"), filepath.Join(t.TempDir(), "rB")
	content := bytes.Repeat([]byte("restart"), 100<<10)
	writeParity(t, newParityVolume(t, dirA, paritySecret), "r/f.bin", content)
	writeParity(t, newParityVolume(t, dirB, paritySecret), "r/f.bin", content)

	// A 重启（重新 NewFS，走 loadIndex），B 不重启；随后两卷继续同样的写入。
	volA := newParityVolume(t, dirA, paritySecret)
	volB := newParityVolume(t, dirB, paritySecret)
	more := bytes.Repeat([]byte("more"), 120<<10)
	writeParity(t, volA, "r/g.bin", more)
	writeParity(t, volB, "r/g.bin", more)

	diffParity(t, snapshotTree(t, dirA), snapshotTree(t, dirB))
}

// TestDeterministic_ErasureParity 锁定 Erasure（parity 段）也确定性：
// parity blob 名/字节同样由内容派生，不能引入随机。
func TestDeterministic_ErasureParity(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "eA"), filepath.Join(t.TempDir(), "eB")
	opts := Options{Secret: []byte(paritySecret), Erasure: true}
	volA, err := NewFS(syncpkg.NewLocalFS(dirA, nil), opts)
	if err != nil {
		t.Fatalf("NewFS A: %v", err)
	}
	volB, err := NewFS(syncpkg.NewLocalFS(dirB, nil), opts)
	if err != nil {
		t.Fatalf("NewFS B: %v", err)
	}
	content := bytes.Repeat([]byte("erasure"), 3<<20) // 多分块才生成 parity
	writeParity(t, volA, "e/f.bin", content)
	writeParity(t, volB, "e/f.bin", content)
	treeA, treeB := snapshotTree(t, dirA), snapshotTree(t, dirB)
	if len(treeA) < 3 {
		t.Fatalf("夹具异常：erasure 产物过少（%d）", len(treeA))
	}
	diffParity(t, treeA, treeB)
}

// TestDeterministic_DedupPoolParity 锁定去重池（Dedup=true）的派生确定性：
// 池目录名（既有 `TestDedupDir_*` 覆盖）之外，**池 blob 与引用 meta** 也必须两卷一致。
func TestDeterministic_DedupPoolParity(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "dA"), filepath.Join(t.TempDir(), "dB")
	opts := Options{Secret: []byte(paritySecret), Dedup: true}
	volA, err := NewFS(syncpkg.NewLocalFS(dirA, nil), opts)
	if err != nil {
		t.Fatalf("NewFS A: %v", err)
	}
	volB, err := NewFS(syncpkg.NewLocalFS(dirB, nil), opts)
	if err != nil {
		t.Fatalf("NewFS B: %v", err)
	}
	content := bytes.Repeat([]byte("dedup"), 250<<10)
	writeParity(t, volA, "d/one.bin", content)
	writeParity(t, volB, "d/one.bin", content)
	// 同内容第二个文件：应命中池（引用同一 blob），两卷同样如此。
	writeParity(t, volA, "d/two.bin", content)
	writeParity(t, volB, "d/two.bin", content)
	diffParity(t, snapshotTree(t, dirA), snapshotTree(t, dirB))
}

// TestDeterministic_MoveDoesNotRenameContainers 锁定用户核心诉求（2026-11-18）：
// **移动目录不得重算容器名**（v1 把父容器纳入派生 ⇒ 移动要按新父级联改名 ✗）。
// 移动后：容器名不变、子树容器名不变、只有被移动目录自己的 dir meta 可能改写（若改名）。
func TestDeterministic_MoveDoesNotRenameContainers(t *testing.T) {
	t.Parallel()
	vol := newParityVolume(t, filepath.Join(t.TempDir(), "v"), paritySecret)
	ctx := context.Background()
	content := bytes.Repeat([]byte("mv"), 60<<10)
	writeParity(t, vol, "src/a/f.bin", content)
	writeParity(t, vol, "dst/keep.txt", []byte("x"))

	snap := func() map[string]string {
		vol.mu.RLock()
		defer vol.mu.RUnlock()
		out := map[string]string{}
		for rel, c := range vol.dirSegs {
			out[rel] = c
		}
		return out
	}
	before := snap()
	// 移动 src/a → dst/moved（父变、名变）
	if err := vol.Rename(ctx, "src/a", "dst/moved"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	after := snap()
	// 自身容器名（basename）必须不变
	oldSelf := path.Base(before["src/a"])
	newSelf := path.Base(after["dst/moved"])
	if oldSelf != newSelf {
		t.Fatalf("移动后容器名必须不变（只依赖自身名）：%q → %q", oldSelf, newSelf)
	}
	// 父容器不变（dst 的容器没动）
	if before["dst"] != after["dst"] {
		t.Fatalf("父容器不应变化：%q → %q", before["dst"], after["dst"])
	}
	// 文件仍可读（数据零改动）
	if _, err := vol.Stat(ctx, "dst/moved/f.bin"); err != nil {
		t.Fatalf("移动后文件应可读: %v", err)
	}
}

// TestDeterministic_SameNameDifferentParentsIdentical 锁定「同一目录在不同父下**完全一致**」：
// 容器名 + 目录 meta（含 dir_id）都必须相同（去掉 parent_dir_id 后才能成立）。
func TestDeterministic_SameNameDifferentParentsIdentical(t *testing.T) {
	t.Parallel()
	dirA, dirB := filepath.Join(t.TempDir(), "A"), filepath.Join(t.TempDir(), "B")
	volA := newParityVolume(t, dirA, paritySecret)
	volB := newParityVolume(t, dirB, paritySecret)
	// 两个卷里各建「同名目录但在不同父下」
	writeParity(t, volA, "p1/same/f.bin", []byte("data"))
	writeParity(t, volB, "p2/same/f.bin", []byte("data"))

	get := func(v *SecretdataFS, rel string) (string, string) {
		v.mu.RLock()
		defer v.mu.RUnlock()
		return path.Base(v.dirSegs[rel]), v.dirIDs[rel]
	}
	cA, idA := get(volA, "p1/same")
	cB, idB := get(volB, "p2/same")
	if cA != cB {
		t.Fatalf("同名目录在不同父下容器名必须相同：%q vs %q", cA, cB)
	}
	if idA != idB {
		t.Fatalf("同名目录在不同父下 dir_id 必须相同：%q vs %q", idA, idB)
	}
	// 目录 meta 的**密文**也必须一致：比较该容器内 dir-meta blob 的字节
	metaOf := func(v *SecretdataFS, rel string) string {
		v.mu.RLock()
		seg := v.dirSegs[rel]
		v.mu.RUnlock()
		_, blob, err := findDirMetaBlob(context.Background(), v.inner, seg)
		if err != nil {
			t.Fatalf("findDirMetaBlob(%s): %v", rel, err)
		}
		return string(blob)
	}
	if metaOf(volA, "p1/same") != metaOf(volB, "p2/same") {
		t.Fatal("同名目录在不同父下的目录 meta 密文必须完全一致（父无关）")
	}
}
