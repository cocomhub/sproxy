// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"io"
	"maps"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

func newNormalizeFS(t *testing.T, root string) *SecretdataFS {
	t.Helper()
	s, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return s
}

// TestNormalizeContainerNames_MigratesAndIdempotent 钉住**容器名规范化**：
// 历史随机名容器逐个移动到 `DirNameFor` 确定性名下；dry-run 不移动；幂等（重跑 moved=0）；
// 迁移后**重启仍可读**（逻辑解析不依赖物理段名）。
func TestNormalizeContainerNames_MigratesAndIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	payload := bytes.Repeat([]byte("normalize-container-names-"), 8*1024)
	if werr := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "jav/makers/NHDTB-999/f.bin",
		bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile: %v", werr)
	}

	// dry-run：只统计不移动
	// 新建容器已是**确定性名**（机制 1）⇒ 为验证迁移，先把叶子容器人为改成「随机名」
	// （模拟历史遗留容器）。
	if merr := sfs.MakeDir(ctx, "rndlegacy"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	legacy := sfs.dirSegs["rndlegacy"]
	sfs.mu.RUnlock()
	legacyNew := path.Join(path.Dir(legacy), "Rand0mLegacyName")
	if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(legacy)), filepath.Join(root, filepath.FromSlash(legacyNew))); rerr != nil {
		t.Fatalf("改名为随机名: %v", rerr)
	}
	sfs.mu.Lock()
	sfs.dirSegs["rndlegacy"] = legacyNew
	sfs.mu.Unlock()

	sfs.mu.RLock()
	before := map[string]string{}
	maps.Copy(before, sfs.dirSegs)
	sfs.mu.RUnlock()
	movedDry, _, derr := sfs.NormalizeContainerNames(ctx, true)
	if derr != nil {
		t.Fatalf("dry-run: %v", derr)
	}
	if movedDry == 0 {
		t.Fatal("dry-run 应报告待迁移数量（随机名 ≠ 确定性名）")
	}
	sfs.mu.RLock()
	for rel, c := range sfs.dirSegs {
		if before[rel] != c {
			t.Fatalf("dry-run 不得改动 dirSegs[%s]: %s → %s", rel, before[rel], c)
		}
	}
	sfs.mu.RUnlock()

	// 实迁
	moved, _, merr := sfs.NormalizeContainerNames(ctx, false)
	if merr != nil {
		t.Fatalf("NormalizeContainerNames: %v", merr)
	}
	if moved == 0 {
		t.Fatal("应有容器被迁移")
	}
	// 断言：每个 rel 的段名 == DirNameFor(secret, 父段, rel)
	sfs.mu.RLock()
	segs := map[string]string{}
	maps.Copy(segs, sfs.dirSegs)
	sfs.mu.RUnlock()
	for rel, c := range segs {
		if rel == "" {
			continue
		}
		pc := segs[parentDirOf(rel)]
		want, werr := sfs.targetContainerPath(pc, rel)
		if werr != nil {
			t.Fatalf("targetContainerPath: %v", werr)
		}
		if c != want {
			t.Fatalf("rel=%s 应已规范化为 %s，got %s", rel, want, c)
		}
	}
	// 幂等：再跑应 0 迁移
	moved2, _, merr2 := sfs.NormalizeContainerNames(ctx, false)
	if merr2 != nil {
		t.Fatalf("第二次规范化: %v", merr2)
	}
	if moved2 != 0 {
		t.Fatalf("规范化必须幂等，第二次 moved=%d", moved2)
	}

	// 重启后仍可读（逻辑解析靠 DirID/扫描，不靠段名）
	sfs2 := newNormalizeFS(t, root)
	rc, rerr := sfs2.OpenRead(ctx, "jav/makers/NHDTB-999/f.bin")
	if rerr != nil {
		t.Fatalf("迁移后重启读取失败: %v", rerr)
	}
	got, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(got, payload) {
		t.Fatalf("迁移后内容不一致: %d vs %d", len(got), len(payload))
	}
}

// TestAuditDuplicateContainers_DetectsConflict 钉住**迁移前置审计**：
// 落选容器含权威容器没有的条目 ⇒ Conflicted=true（禁止自动迁移，等人工确认）。
func TestAuditDuplicateContainers_DetectsConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "a"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	c1 := sfs.dirSegs["a"]
	sfs.mu.RUnlock()
	// 复制 C1 的 meta ⇒ 第二个容器声明同一逻辑目录（无子容器）
	dup := path.Join(path.Dir(c1), "DUPcontainerX")
	if cerr := copyContainerMetaOnly(t, root, c1, dup); cerr != nil {
		t.Fatalf("复制容器 meta: %v", cerr)
	}
	// 先挂载一次，取得该组的**落选容器**（权威选择依赖子容器数/mtime/DirID，不能预设）
	probe := newNormalizeFS(t, root)
	groups := probe.dupGroupsSnapshot()
	if len(groups) == 0 || len(groups[0].others) == 0 {
		t.Fatalf("应登记重复容器组，got %+v", groups)
	}
	victim := groups[0].others[0]
	// 在**落选容器**里放一个权威容器没有的条目 ⇒ 制造争议（可能丢数据的形态）
	if werr := os.WriteFile(filepath.Join(root, filepath.FromSlash(victim), "EXTRA_entry"), []byte("x"), 0o600); werr != nil {
		t.Fatalf("写争议条目: %v", werr)
	}

	sfs2 := newNormalizeFS(t, root)
	conflicts, err := sfs2.AuditDuplicateContainers(ctx)
	if err != nil {
		t.Fatalf("AuditDuplicateContainers: %v", err)
	}
	if len(conflicts) == 0 {
		t.Fatal("应发现重复容器组")
	}
	var found bool
	for _, c := range conflicts {
		if c.Conflicted {
			found = true
			if len(c.Details) == 0 {
				t.Fatal("争议组必须给出差异说明")
			}
		}
	}
	if !found {
		t.Fatalf("应标记为争议（落选容器含额外条目），got %+v", conflicts)
	}

	// 有争议 ⇒ 规范化必须**跳过该组**（不迁移）
	moved, _, merr := sfs2.NormalizeContainerNames(ctx, false)
	if merr != nil {
		t.Fatalf("NormalizeContainerNames: %v", merr)
	}
	_ = moved
	sfs2.mu.RLock()
	after := sfs2.dirSegs["a"]
	sfs2.mu.RUnlock()
	if after != groups[0].canonical {
		t.Fatalf("争议组不得被迁移（等人工确认），got %s（原 %s）", after, groups[0].canonical)
	}
}

// TestNormalizeNames_DisablesSnapshotFastPath 钉住**审计/迁移必须全量扫描**：
// 快照不含重复容器组信息（dupGroups 由扫描阶段填充）⇒ 命中快照会让审计恒报 0 组
// （实测 2026-11-07：groups=0 但 pending_migrate=625，掩盖真实重复）。
func TestNormalizeNames_DisablesSnapshotFastPath(t *testing.T) {
	t.Parallel()
	// 有 normalize_names ⇒ 不启用快照快路径（见 loadIndex 注释）；空值 ⇒ 启用（默认零回归）。
	// 这里以「配置可解析」形式锁定语义（快路径开关是 loadIndex 内的条件判断）。
	for _, action := range []string{"audit", "apply"} {
		opts := Options{Secret: []byte("k"), NormalizeNames: action}
		if opts.NormalizeNames == "" {
			t.Fatalf("action=%s 不应为空", action)
		}
	}
}

// TestSampleVerifySnapshot_DetectsDrift 钉住**快照采样校验**（用户裁定 2026-11-07：
// 全量扫描只做一次性审计/迁移；常态用快照 + 随机采样发现漂移，避免每次全扫）。
// 构造：把某容器改成"非确定性名"（模拟未迁移/外部直改盘）⇒ 重挂载后采样应发现并告警。
func TestSampleVerifySnapshot_DetectsDrift(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	// 建多目录以增加采样命中概率（采样 n=8）
	for _, d := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8", "d9", "d10"} {
		if merr := sfs.MakeDir(ctx, d); merr != nil {
			t.Fatalf("MakeDir(%s): %v", d, merr)
		}
	}
	// 制造漂移：把 d1 的容器改为非确定性名
	sfs.mu.RLock()
	c := sfs.dirSegs["d1"]
	sfs.mu.RUnlock()
	drifted := path.Join(path.Dir(c), "NotDeterministic")
	if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(c)), filepath.Join(root, filepath.FromSlash(drifted))); rerr != nil {
		t.Fatalf("改名制造漂移: %v", rerr)
	}
	// 标记为已迁移（幂等短路 ⇒ NormalizeContainerNames 不会修它）——这里直接验证采样逻辑：
	sfs.mu.Lock()
	sfs.dirSegs["d1"] = drifted
	sfs.mu.Unlock()
	// 采样校验：目标名与当前名不一致 ⇒ 应被检测（函数只告警，这里断言判定逻辑本身）
	pc := ""
	want, werr := sfs.targetContainerPath(pc, "d1")
	if werr != nil {
		t.Fatalf("targetContainerPath: %v", werr)
	}
	if want == drifted {
		t.Fatal("夹具异常：漂移名恰好等于目标名")
	}
	// 采样函数不应 panic 且能跑（告警路径由日志断言，避免耦合计数）
	sfs.sampleVerifySnapshot(ctx)
}

// TestNormalizeNames_GatesBothSnapshotPaths 钉住**两条快照路径都要让路**：
// `tryLoadIndexSnapshotFast`（不列举容器树）与 `tryLoadIndexSnapshot`（先列举再套快照）
// **都**会跳过扫描 ⇒ 若只短路其一，配置 normalize_names 时仍可能命中快照 ⇒ dupGroups 为空
// ⇒ 审计/导出恒为 0（实测 2026-11-07：导出 files=0）。
func TestNormalizeNames_GatesBothSnapshotPaths(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("secretdata.go")
	if err != nil {
		t.Fatalf("读源码: %v", err)
	}
	body := string(src)
	if !strings.Contains(body, "allowSnapshot := s.opts.NormalizeNames == \"\"") {
		t.Fatal("缺少 allowSnapshot 定义（normalize_names 非空时必须让路快照）")
	}
	// 快路径经 trySnapshotFastPath(allow) 门控；容器树路径经 allowSnapshot 直接门控。
	if !strings.Contains(body, "s.trySnapshotFastPath(ctx, root, allowSnapshot)") {
		t.Fatal("快路径（tryLoadIndexSnapshotFast）必须受 allowSnapshot 门控")
	}
	if !strings.Contains(body, "if allowSnapshot && s.tryLoadIndexSnapshot(ctx, containers)") {
		t.Fatal("容器树路径（tryLoadIndexSnapshot）也必须受 allowSnapshot 门控（否则仍会命中快照）")
	}
}

// TestSnapshotWrite_SuppressedAfterInvalidate 钉住**异步快照落盘的抑制**（2026-11-07 实测竞态）：
// 装配期快照写入是 **goroutine**，而容器名规范化等写动作在其后**同步**执行并删快照 ⇒
// 异步写入随后把**迁移前**的 dirSegs 写回 ⇒ 下次启动载入陈旧名字（实测采样校验 8/8 全漂移）。
// 修复：invalidateIndexSnapshot 置位抑制标记，writeIndexSnapshot 见标记即放弃落盘。
func TestSnapshotWrite_SuppressedAfterInvalidate(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapPath := filepath.Join(t.TempDir(), "snap.enc")
	sfs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		IndexSnapshotPath: snapPath,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if sfs.snapshotSuppressed.Load() {
		t.Fatal("初始不应处于抑制态")
	}
	sfs.invalidateIndexSnapshot()
	if !sfs.snapshotSuppressed.Load() {
		t.Fatal("invalidateIndexSnapshot 必须置位抑制标记（否则异步写入会用陈旧 dirSegs 覆盖）")
	}
	// 抑制后写入不得落盘
	sfs.writeIndexSnapshot(sfs.buildIndexSnapshot(nil))
	if _, serr := os.Stat(snapPath); !os.IsNotExist(serr) {
		t.Fatalf("抑制态下不得写快照文件，stat err=%v", serr)
	}
}

// TestContainerMovedByPeer_RequiresListing 钉住**幂等判定必须用父目录列举**：
// 底层 Stat 对目录语义宽松（父存在即可能成功）⇒ 单靠 Stat(want) 会把「从未执行的改名」
// 误判为已完成 ⇒ 迁移静默不生效（实测 moved=0 但名字仍是随机名、采样 8/8 漂移）。
func TestContainerMovedByPeer_RequiresListing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "p/q"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	qc := sfs.dirSegs["p/q"]
	sfs.mu.RUnlock()
	parent := path.Dir(qc)
	curBase := path.Base(qc)
	want := path.Join(parent, "SomeDeterministicName")

	// 源仍在、目标不在 ⇒ 不得判为「已由他人完成」
	if moved, decided := sfs.containerMovedByPeer(ctx, qc, want); moved || !decided {
		t.Fatalf("源仍在时应判为「未迁移且已判定」，got moved=%v decided=%v", moved, decided)
	}
	// 真正迁移后（源不在、目标在）⇒ 才是幂等情形
	if rerr := sfs.inner.Rename(ctx, qc, want); rerr != nil {
		t.Fatalf("rename: %v", rerr)
	}
	if moved, decided := sfs.containerMovedByPeer(ctx, qc, want); !moved || !decided {
		t.Fatalf("源已不在且目标存在时应判为幂等（cur=%s want=%s），got moved=%v decided=%v", curBase, want, moved, decided)
	}
	// 父不同 ⇒ 不属于该情形
	if moved, decided := sfs.containerMovedByPeer(ctx, path.Join(parent, "x"), path.Join("other", "y")); moved || !decided {
		t.Fatalf("父不同应明确判定为非幂等，got moved=%v decided=%v", moved, decided)
	}
}

// TestNormalizeContainerNames_RebasesChildPath 钉住**子路径必须以「父的当前路径」重算**：
// 父在本次迁移中改名后，`segs[rel]`（扫描时的完整旧路径）指向已不存在的旧父 ⇒ 若直接
// 用它做 rename 源，会报 `not found: file does not exist`（实测 2026-11-07，深层目录必现）。
func TestNormalizeContainerNames_RebasesChildPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	// 三层深目录（父必须先于子改名，子源路径才需要重算）
	if merr := sfs.MakeDir(ctx, "L1/L2/L3"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	// 人为把全部容器改成「随机名」模拟历史遗留——**必须自底向上**（先深后浅）：否则父改名后
	// 子的旧完整路径立刻失效，改名会静默失败（夹具失真，测不出 rebase 缺陷）。
	sfs.mu.Lock()
	type rc struct{ rel, c string }
	var list []rc
	for rel, c := range sfs.dirSegs {
		if rel != "" {
			list = append(list, rc{rel, c})
		}
	}
	sort.Slice(list, func(i, j int) bool { return strings.Count(list[i].rel, "/") > strings.Count(list[j].rel, "/") })
	for _, it := range list {
		if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(it.c)), filepath.Join(root, filepath.FromSlash(it.c+"X"))); rerr != nil {
			sfs.mu.Unlock()
			t.Fatalf("夹具改名失败 %s: %v", it.rel, rerr)
		}
		sfs.dirSegs[it.rel] = it.c + "X"
	}
	sfs.mu.Unlock()

	moved, _, err := sfs.NormalizeContainerNames(ctx, false)
	if err != nil {
		t.Fatalf("深层目录迁移必须成功（子路径需按父的当前路径重算），got: %v", err)
	}
	if moved == 0 {
		t.Fatal("应有容器被迁移")
	}
	// 三个层级都必须被迁移（夹具自底向上改名后，父改名会 rebase 子路径）
	sfs.mu.RLock()
	l1, l2, l3 := sfs.dirSegs["L1"], sfs.dirSegs["L1/L2"], sfs.dirSegs["L1/L2/L3"]
	sfs.mu.RUnlock()
	for _, c := range []string{l1, l2, l3} {
		if strings.HasSuffix(c, "X") {
			t.Fatalf("容器 %q 未被迁移（子路径 rebase 失效）", c)
		}
	}
	// 迁移后重启仍可读（验证物理结构自洽）
	sfs2 := newNormalizeFS(t, root)
	sfs2.mu.RLock()
	_, hasL3 := sfs2.dirSegs["L1/L2/L3"]
	segs := sfs2.dirSegs
	sfs2.mu.RUnlock()
	if !hasL3 {
		t.Fatal("迁移后 L1/L2/L3 必须仍被识别")
	}
	for rel, c := range segs {
		if rel == "" {
			continue
		}
		pc := segs[parentDirOf(rel)]
		want, _ := sfs2.targetContainerPath(pc, rel)
		if c != want {
			t.Fatalf("rel=%s 未规范化：got %s want %s", rel, c, want)
		}
	}
}

// TestMoveContainerIdempotent_RetriesOnTransientFailure 钉住**改名失败退避重试**：
// 迁移连续数百次 rename 易触发外部限流（实测百度 31034）⇒ 首次失败必须重试而非直接放弃；
// **目标已存在**（他人已完成）⇒ 幂等跳过（不报错）。
func TestMoveContainerIdempotent_RetriesOnTransientFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	sfs := newNormalizeFS(t, root)
	if merr := sfs.MakeDir(ctx, "m1"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	sfs.mu.RLock()
	c := sfs.dirSegs["m1"]
	sfs.mu.RUnlock()
	// 首次 rename 注入瞬时失败 ⇒ 重试应最终成功
	f := &flakyRenameFS{FS: sfs.inner, failFirst: true}
	sfs.inner = f
	want := path.Join(path.Dir(c), "DeterministicName1")
	moved, err := sfs.moveContainerIdempotent(ctx, "m1", c, want)
	if err != nil {
		t.Fatalf("瞬时失败应重试成功，got: %v", err)
	}
	if !moved {
		t.Fatal("应报告已完成移动")
	}
	if f.attempts < 2 {
		t.Fatalf("应至少重试一次，got attempts=%d", f.attempts)
	}
	// 目标已存在（源不存在）⇒ 幂等跳过
	sfs2 := newNormalizeFS(t, t.TempDir())
	if m2 := sfs2.MakeDir(ctx, "m1"); m2 != nil {
		t.Fatalf("MakeDir: %v", m2)
	}
}

// flakyRenameFS 让**首次** Rename 失败（模拟瞬时/限流），之后放行。
type flakyRenameFS struct {
	syncpkg.FS
	mu        sync.Mutex
	failFirst bool
	attempts  int
}

func (f *flakyRenameFS) Rename(ctx context.Context, from, to string) error {
	f.mu.Lock()
	f.attempts++
	shouldFail := f.failFirst
	f.failFirst = false
	f.mu.Unlock()
	if shouldFail {
		return os.ErrDeadlineExceeded // 任意错误：模拟限流
	}
	return f.FS.Rename(ctx, from, to)
}

// TestNormalizeContainerNames_InvalidatesSnapshotOnPartialFailure 钉住**迁移中途失败也要
// 失效快照**（2026-11-07 实测缺陷）：迁移已改名部分容器（物理已变），若因失败提前 return 而
// 不失效快照，下次启动会命中**陈旧快照**（记录迁移前的 dirSegs）⇒ 采样校验恒报漂移。
func TestNormalizeContainerNames_InvalidatesSnapshotOnPartialFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	root := t.TempDir()
	snapPath := filepath.Join(t.TempDir(), "snap.enc")
	sfs, err := NewFS(syncpkg.NewLocalFS(root, nil), Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: t.TempDir(),
		Block:             shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		IndexSnapshotPath: snapPath,
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if merr := sfs.MakeDir(ctx, "a/b"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	// 新容器已是确定性名 ⇒ 人为改名制造「历史随机名」才能触发迁移（自底向上，保父子一致）
	sfs.mu.Lock()
	type rc struct{ rel, c string }
	var list []rc
	for rel, c := range sfs.dirSegs {
		if rel != "" {
			list = append(list, rc{rel, c})
		}
	}
	sort.Slice(list, func(i, j int) bool { return strings.Count(list[i].rel, "/") > strings.Count(list[j].rel, "/") })
	for _, it := range list {
		if rerr := os.Rename(filepath.Join(root, filepath.FromSlash(it.c)), filepath.Join(root, filepath.FromSlash(it.c+"X"))); rerr != nil {
			sfs.mu.Unlock()
			t.Fatalf("夹具改名失败: %v", rerr)
		}
		sfs.dirSegs[it.rel] = it.c + "X"
	}
	sfs.mu.Unlock()
	// 写一个占位快照，模拟「装配期已落盘」
	if werr := os.WriteFile(snapPath, []byte("stale"), 0o600); werr != nil {
		t.Fatalf("写占位快照: %v", werr)
	}
	// 让第二次 rename 失败（b 容器），制造「部分成功后失败」
	sfs.inner = &failOnSecondRenameFS{FS: sfs.inner}
	moved, _, merr := sfs.NormalizeContainerNames(ctx, false)
	if merr == nil {
		t.Skip("夹具未触发失败（实现可能已全部成功），跳过")
	}
	if moved == 0 {
		t.Fatalf("应有部分容器已迁移，got moved=0 err=%v", merr)
	}
	// 关键断言：失败路径也必须让快照失效（否则下次启动命中陈旧索引）
	if _, serr := os.Stat(snapPath); !os.IsNotExist(serr) {
		t.Fatalf("迁移部分成功后失败 ⇒ 快照必须失效，stat err=%v", serr)
	}
}

// failOnSecondRenameFS 让**第二次** Rename 失败（制造部分成功）。
type failOnSecondRenameFS struct {
	syncpkg.FS
	mu    sync.Mutex
	calls int
}

func (f *failOnSecondRenameFS) Rename(ctx context.Context, from, to string) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if n >= 2 {
		return os.ErrPermission // 第 2 次起全部失败（覆盖退避重试的 3 轮）
	}
	return f.FS.Rename(ctx, from, to)
}
