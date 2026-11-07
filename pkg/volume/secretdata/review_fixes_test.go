// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// review_fixes_test.go —— 批次 16 对抗审查发现的缺陷的**红灯复现 + 回归钉住**测试。
// 每个缺陷先有失败用例（复现），再修实现，再以变异确认真实性。

import (
	"bytes"
	"context"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

const reviewTestSecret = "test-secret-key-000"

// mountReviewFS 以指定快照路径装配一个卷（用于「重挂载识别重复容器组」）。
func mountReviewFS(t *testing.T, root, snapPath string) *SecretdataFS {
	t.Helper()
	sfs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte(reviewTestSecret), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		IndexSnapshotPath: snapPath,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return sfs
}

// legacyRenameContainer 把指定逻辑目录的容器**物理改名为随机名**（模拟历史遗留容器），
// 并同步内存映射。返回改名后的容器路径。
func legacyRenameContainer(t *testing.T, sfs *SecretdataFS, root, rel string) string {
	t.Helper()
	sfs.mu.RLock()
	cur, ok := sfs.dirSegs[rel]
	sfs.mu.RUnlock()
	if !ok || cur == "" {
		t.Fatalf("dirSegs[%s] 不存在", rel)
	}
	next := cur + "LegacyRand"
	if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(cur)),
		filepath.Join(root, filepath.FromSlash(next))); rerr != nil {
		t.Fatalf("夹具改名失败(%s): %v", rel, rerr)
	}
	// **必须同步索引条目**：真实历史态里 `e.dirSeg` 就是那个随机名（blob 定位依据），
	// 否则夹具会掩盖 P1-1（条目恰好指向迁移目标名 ⇒ 看起来"已同步"）。
	sfs.mu.Lock()
	sfs.rebaseContainerPathsLocked(cur, next)
	sfs.mu.Unlock()
	return next
}

// copyFile 复制单个文件（夹具用）。
func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o600)
}

// cloneDirMetaContainer 复制**容器目录 meta blob**到同父下的新目录 `suffix` 后缀名 ⇒
// 形成「第二个容器声明同一逻辑路径」（重复容器），且**不含子容器**（避免触发
// 中断移动修复）。返回新容器相对路径。
func cloneDirMetaContainer(t *testing.T, root, containerRel, suffix string, withFiles bool) string {
	t.Helper()
	srcDir := filepath.Join(root, filepath.FromSlash(containerRel))
	dupRel := containerRel + suffix
	dupDir := filepath.Join(root, filepath.FromSlash(dupRel))
	if err := os.MkdirAll(dupDir, 0o755); err != nil {
		t.Fatalf("建落选容器目录: %v", err)
	}
	ents, err := os.ReadDir(srcDir)
	if err != nil {
		t.Fatalf("读权威容器: %v", err)
	}
	copied := 0
	for _, e := range ents {
		if e.IsDir() {
			continue // 只复制本容器条目（不含子容器 ⇒ 不触发中断移动修复）
		}
		if !withFiles && shardseal.ClassifyName(e.Name()) != shardseal.KindDirMeta {
			continue // 只复制目录 meta（落选容器为空）
		}
		if cerr := copyFile(filepath.Join(srcDir, e.Name()), filepath.Join(dupDir, e.Name())); cerr != nil {
			t.Fatalf("复制条目 %s: %v", e.Name(), cerr)
		}
		copied++
	}
	if copied == 0 {
		t.Fatal("未复制到任何条目（夹具失效）")
	}
	return dupRel
}

// TestP1_1_ReadAfterMigration_SameProcess 复现 **P1-1**：迁移把容器改名后，只有 `s.dirSegs`
// 被更新，**索引条目的 `e.dirSeg` 未同步** ⇒ 同进程内既有文件的读/删仍指向**已不存在的旧路径**
// ⇒ 读失败（要到重启全量重扫才恢复）。仓库既有 `rebaseContainerPathsLocked` 注释已记录该模式。
func TestP1_1_ReadAfterMigration_SameProcess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	const rel = "jav/makers/NHDTB-888/P1-1.bin"
	payload := bytes.Repeat([]byte("dirseg-sync-needed-"), 4*1024)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	if rc, rerr := sfs.OpenRead(ctx, rel); rerr != nil {
		t.Fatalf("基线读取失败: %v", rerr)
	} else {
		_ = rc.Close()
	}
	dirRel := path.Dir(rel)
	legacyRenameContainer(t, sfs, root, dirRel)

	sfs.mu.RLock()
	entryBefore := sfs.index[rel]
	sfs.mu.RUnlock()
	if entryBefore == nil {
		t.Fatal("索引条目不存在")
	}
	oldDirSeg := entryBefore.dirSeg

	moved, _, merr := sfs.NormalizeContainerNames(ctx, false)
	if merr != nil {
		t.Fatalf("NormalizeContainerNames: %v", merr)
	}
	if moved == 0 {
		t.Fatal("应至少迁移 1 个容器")
	}
	sfs.mu.RLock()
	entryAfter := sfs.index[rel]
	sfs.mu.RUnlock()
	if entryAfter == nil {
		t.Fatal("索引条目丢失")
	}
	if entryAfter.dirSeg == oldDirSeg {
		t.Fatalf("迁移后 e.dirSeg 仍是旧路径 %q（未同步）", oldDirSeg)
	}
	// 断言 2（GC 面）：迁移后 GC 标记的引用必须全部落在**真实存在**的物理路径上
	marked := map[string]struct{}{}
	sfs.mu.RLock()
	sfs.gcMarkIndex(ctx, marked)
	sfs.mu.RUnlock()
	for name := range marked {
		if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(name))); serr != nil {
			t.Fatalf("GC 引用指向不存在的物理路径 %q（dirSeg 陈旧）: %v", name, serr)
		}
	}
	// 断言 3：同进程内必须仍能读到该文件
	rc, rerr := sfs.OpenRead(ctx, rel)
	if rerr != nil {
		t.Fatalf("迁移后同进程读取失败（e.dirSeg 未同步）: %v", rerr)
	}
	got, cerr := io.ReadAll(rc)
	_ = rc.Close()
	if cerr != nil {
		t.Fatalf("读取: %v", cerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("迁移后读取内容不一致: got %d bytes, want %d", len(got), len(payload))
	}
	// 断言 4：删除也必须成功（删除路径同样用 e.dirSeg 定位 meta blob）
	if derr := sfs.Delete(ctx, rel); derr != nil {
		t.Fatalf("迁移后同进程删除失败: %v", derr)
	}
}

// TestP1_2_SnapshotFinalizeAbortsWhenInvalidated 复现 **P1-2**：`writeIndexSnapshot` 只在入口
// 检查抑制标记，入口到 `os.Rename` 之间是 marshal+gzip+GCM+写盘（秒级窗口）⇒ 窗口内发生的
// 写/迁移（已删快照）**无法阻止**陈旧快照被 rename 回原路径。
func TestP1_2_SnapshotFinalizeAbortsWhenInvalidated(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapPath := filepath.Join(t.TempDir(), "snap.enc")
	sfs := mountReviewFS(t, root, snapPath)
	// 模拟：快照字节已落 tmp，随后（窗口内）发生写操作 ⇒ 快照被失效
	gen := sfs.snapshotGen.Load()
	tmp := snapPath + ".tmp"
	if werr := os.WriteFile(tmp, []byte("stale-snapshot"), 0o600); werr != nil {
		t.Fatalf("写 tmp: %v", werr)
	}
	sfs.invalidateIndexSnapshot() // 窗口内：删除快照 + 代际 +1

	if ok := sfs.finalizeSnapshotFile(tmp, snapPath, gen); ok {
		t.Fatal("代际已变化却仍提交了陈旧快照（P1-2）")
	}
	if _, serr := os.Stat(snapPath); !os.IsNotExist(serr) {
		t.Fatalf("陈旧快照被写回（应不存在）: %v", serr)
	}
	if _, serr := os.Stat(tmp); !os.IsNotExist(serr) {
		t.Fatalf("tmp 未清理: %v", serr)
	}
}

// TestP1_2_SnapshotFinalizeCommitsWhenUnchanged 正向对照：代际未变时必须正常提交。
func TestP1_2_SnapshotFinalizeCommitsWhenUnchanged(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapPath := filepath.Join(t.TempDir(), "snap.enc")
	sfs := mountReviewFS(t, root, snapPath)
	gen := sfs.snapshotGen.Load()
	tmp := snapPath + ".tmp"
	if werr := os.WriteFile(tmp, []byte("fresh-snapshot"), 0o600); werr != nil {
		t.Fatalf("写 tmp: %v", werr)
	}
	if ok := sfs.finalizeSnapshotFile(tmp, snapPath, gen); !ok {
		t.Fatal("代际未变却拒绝提交（正向路径失效）")
	}
	if _, serr := os.Stat(snapPath); serr != nil {
		t.Fatalf("快照未提交: %v", serr)
	}
}

// TestP1_3_CleanupInvalidatesSnapshot 复现 **P1-3**：空落选容器清理**删除了物理目录**，
// 却没有失效索引快照 ⇒ `snapshotRootMatches` 只比顶层集合（深层删除拦不住）⇒ 陈旧快照
// 可命中并指向已删容器。
func TestP1_3_CleanupInvalidatesSnapshot(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	snapPath := filepath.Join(t.TempDir(), "snap.enc")
	seed := mountReviewFS(t, root, filepath.Join(t.TempDir(), "seed.enc"))
	if merr := seed.MakeDir(ctx, "dup/y"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	seed.mu.RLock()
	canonical := seed.dirSegs["dup/y"]
	seed.mu.RUnlock()
	cloneDirMetaContainer(t, root, canonical, "Rnd", false) // 落选容器（仅目录 meta ⇒ 空）

	sfs := mountReviewFS(t, root, snapPath) // 重挂载识别重复容器组
	if len(sfs.dupGroupsSnapshot()) == 0 {
		t.Fatal("应识别出重复容器组（夹具失效）")
	}
	// 模拟「上次装配留下的快照」
	if werr := os.WriteFile(snapPath, []byte("stale"), 0o600); werr != nil {
		t.Fatalf("写占位快照: %v", werr)
	}
	removed, _, cerr := sfs.CleanupEmptyDuplicateContainers(ctx, "", false)
	if cerr != nil {
		t.Fatalf("CleanupEmptyDuplicateContainers: %v", cerr)
	}
	if removed == 0 {
		t.Fatal("应清理至少 1 个空落选容器")
	}
	if _, serr := os.Stat(snapPath); !os.IsNotExist(serr) {
		t.Fatalf("清理删除了物理容器 ⇒ 快照必须失效，stat err=%v", serr)
	}
}

// TestP1_4_ExportDoesNotLeakState 复现 **P1-4**：导出（`exportOneContainer`）借道索引临时注册，
// `registerLoadedEntryLocked` 的副作用（usage 累加 / 祖先目录键 / volVersion）**未被回滚**
// ⇒ 用量虚增、`ListDir("/")` 出现幻影目录 `__dup_export__`。
func TestP1_4_ExportDoesNotLeakState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	seed := mountReviewFS(t, root, filepath.Join(t.TempDir(), "seed.enc"))
	const rel = "dup/x/f.bin"
	payload := bytes.Repeat([]byte("export-leak-"), 4*1024)
	if werr := seed.WriteFile(ctx, syncpkg.LogicalKey{}, rel, bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	seed.mu.RLock()
	canonical := seed.dirSegs["dup/x"]
	seed.mu.RUnlock()
	cloneDirMetaContainer(t, root, canonical, "Rnd", true) // 落选容器含文件 meta + 分块

	sfs := mountReviewFS(t, root, filepath.Join(t.TempDir(), "s.enc"))
	if len(sfs.dupGroupsSnapshot()) == 0 {
		t.Fatal("应识别出重复容器组（夹具失效）")
	}
	sfs.mu.RLock()
	usageBefore, versionBefore := sfs.usage, sfs.volVersion
	dirsBefore := map[string]struct{}{}
	for k := range sfs.dirs {
		dirsBefore[k] = struct{}{}
	}
	if len(dirsBefore) == 0 {
		t.Fatal("夹具应有非空目录集合")
	}
	if usageBefore == 0 {
		t.Fatal("夹具应有非零 usage（否则用量断言无意义）")
	}
	sfs.mu.RUnlock()

	files, exported, eerr := sfs.ExportDuplicateContainers(ctx, t.TempDir())
	if eerr != nil {
		t.Fatalf("ExportDuplicateContainers: %v", eerr)
	}
	if files == 0 || exported == 0 {
		t.Fatalf("应导出至少 1 个文件，got files=%d bytes=%d", files, exported)
	}

	sfs.mu.RLock()
	usageAfter, versionAfter := sfs.usage, sfs.volVersion
	leakedDirs := []string{}
	for k := range sfs.dirs {
		if _, ok := dirsBefore[k]; !ok {
			leakedDirs = append(leakedDirs, k)
		}
	}
	sfs.mu.RUnlock()

	if usageAfter != usageBefore {
		t.Fatalf("导出后 usage 虚增: %d → %d（P1-4）", usageBefore, usageAfter)
	}
	if versionAfter != versionBefore {
		t.Fatalf("导出后 volVersion 被污染: %d → %d", versionBefore, versionAfter)
	}
	if len(leakedDirs) != 0 {
		t.Fatalf("导出泄漏幻影目录: %v", leakedDirs)
	}
	ents, lerr := sfs.ListDir(ctx, "")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name, "__dup_export__") {
			t.Fatalf("ListDir(\"/\") 出现幻影目录 %q", e.Name)
		}
	}
}

// TestP1_5_ConflictCriterionIgnoresSubContainerPhysicalNames 复现 **P1-5**：争议判据用
// **物理条目名**比对（`containerEntries` + `missingFrom`），而历史子容器名是随机的 ⇒
// 含子容器的重复组必然被判 `Conflicted=true` ⇒ 最需要迁移的嵌套重复组被**永久跳过**。
//
// 夹具：同一 rel 两个容器，落选容器比权威容器多一个**随机名的空子目录**（子容器名天然不同），
// 两者**文件内容完全一致** ⇒ 应判「无争议」。
func TestP1_5_ConflictCriterionIgnoresSubContainerPhysicalNames(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "dup/n"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "dup/n/f.bin",
		bytes.NewReader([]byte("nested-content")), 14, 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}
	sfs.mu.RLock()
	canonical := sfs.dirSegs["dup/n"]
	sfs.mu.RUnlock()
	dupRel := cloneDirMetaContainer(t, root, canonical, "Rnd", true)
	// 落选容器内塞一个**随机名**子目录（模拟历史随机子容器名；权威侧没有这个物理名）
	randSub := filepath.Join(root, filepath.FromSlash(dupRel), "zzRandomSubContainer")
	if merr := os.MkdirAll(randSub, 0o755); merr != nil {
		t.Fatalf("建随机子目录: %v", merr)
	}
	c, aerr := sfs.auditOneGroup(ctx, dupContainerGroup{
		rel: "dup/n", canonical: canonical, others: []string{dupRel},
	})
	if aerr != nil {
		t.Fatalf("auditOneGroup: %v", aerr)
	}
	if c.Conflicted {
		t.Fatalf("内容等价（仅子容器随机名不同）不得判为争议：%v", c.Details)
	}
}

// TestP2_3_SampleDriftClassifiesUnmigratedVsAnomalous 钉住**采样告警降噪**（批次 16 P2-3）：
// 父未迁移 ⇒ 子名不同属**预期态**（Debug，不计入 anomalous）；父符合机制而自身不符合 ⇒ 真异常。
func TestP2_3_SampleDriftClassifiesUnmigratedVsAnomalous(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	// 构造：顶层 a 规范；a/b 规范；a/c 名为随机（父规范 ⇒ 异常）；x 为随机（顶层，无父）
	segs := map[string]string{}
	for _, rel := range []string{"a", "a/b", "a/c", "x"} {
		p := parentDirOf(rel)
		pc := ""
		if p != "" {
			pc = segs[p]
		}
		want, err := sfs.targetContainerPath(pc, rel)
		if err != nil {
			t.Fatalf("targetContainerPath(%s): %v", rel, err)
		}
		segs[rel] = want
	}
	if got := sfs.classifySampleDrift(segs, "a/b"); got != sampleDriftNone {
		t.Fatalf("规范命名应判 None，got %v", got)
	}
	segs["a/c"] += "Rand" // 父规范、自身被改 ⇒ 异常
	if got := sfs.classifySampleDrift(segs, "a/c"); got != sampleDriftAnomalous {
		t.Fatalf("父规范而自身不合规应判 Anomalous，got %v", got)
	}
	// 父（a）改随机 ⇒ a/b 子名必然不同 ⇒ 预期态（未迁移）
	segs["a"] += "Rand"
	if got := sfs.classifySampleDrift(segs, "a/b"); got != sampleDriftUnmigrated {
		t.Fatalf("父未迁移时子命名差异应判 Unmigrated（降噪），got %v", got)
	}
}

// TestP2_4_RetriesUseBoundedExponentialBackoff 钉住**退避重试次数**（批次 16 P2-4）：
// 限流下应重试 5 次（而非 3 次），且退避指数放大但封顶。
func TestP2_4_RetriesUseBoundedExponentialBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "r1"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	cur := sfs.dirSegs["r1"]
	sfs.mu.RUnlock()
	legacyRenameContainer(t, sfs, root, "r1")
	flaky := &countingFailRenameFS{FS: sfs.inner}
	sfs.inner = flaky
	_, err := sfs.moveContainerIdempotent(ctx, "r1", sfs.dirSegs["r1"], cur)
	if err == nil {
		t.Fatal("始终失败应返回错误")
	}
	if flaky.attempts != 5 {
		t.Fatalf("应重试 5 次（maxAttempts），got %d", flaky.attempts)
	}
	if maxContainerMoveBackoff != 30*time.Second {
		t.Fatalf("退避上限应封顶 30s，got %v", maxContainerMoveBackoff)
	}
}

// countingFailRenameFS 让所有 Rename 失败并计数（限流桩）。
type countingFailRenameFS struct {
	syncpkg.FS
	attempts int
}

func (f *countingFailRenameFS) Rename(_ context.Context, _, _ string) error {
	f.attempts++
	return os.ErrDeadlineExceeded
}

// TestP2_5_ApplyConvergesInOneRun 钉住**分批迁移在一次装配内收敛**（批次 16 P2-5）：
// `NormalizeMaxMoves` 是每轮上限，原先只跑一轮 ⇒ 需人工多次重启；现应循环到 moved==0。
func TestP2_5_ApplyConvergesInOneRun(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	rels := []string{"c1", "c2", "c3", "c4"}
	for _, rel := range rels {
		if merr := sfs.MakeDir(ctx, rel); merr != nil {
			t.Fatalf("MakeDir(%s): %v", rel, merr)
		}
		legacyRenameContainer(t, sfs, root, rel)
	}
	sfs.opts.NormalizeMaxMoves = 1 // 每轮只迁 1 个 ⇒ 必须靠循环收敛
	sfs.opts.NormalizeMoveInterval = time.Millisecond
	sfs.runApplyAction(ctx)
	for _, rel := range rels {
		sfs.mu.RLock()
		got := sfs.dirSegs[rel]
		pc := sfs.dirSegs[parentDirOf(rel)]
		sfs.mu.RUnlock()
		want, err := sfs.targetContainerPath(pc, rel)
		if err != nil {
			t.Fatalf("targetContainerPath: %v", err)
		}
		if got != want {
			t.Fatalf("一次 apply 应迁移全部容器：%s = %s，期望 %s", rel, got, want)
		}
	}
}

// TestP2_7_DupGroupsSnapshotDeepCopies 钉住 **P2-7**：快照的 `others` 必须是深拷贝
// （否则调用方误改会污染内部状态）。
func TestP2_7_DupGroupsSnapshotDeepCopies(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	sfs.mu.Lock()
	sfs.dupGroups = []dupContainerGroup{{rel: "r", canonical: "C", others: []string{"O1", "O2"}}}
	sfs.mu.Unlock()
	snap := sfs.dupGroupsSnapshot()
	snap[0].others[0] = "MUTATED"
	sfs.mu.RLock()
	internal := sfs.dupGroups[0].others[0]
	sfs.mu.RUnlock()
	if internal != "O1" {
		t.Fatalf("快照未深拷贝 ⇒ 内部状态被污染: %s", internal)
	}
}
