// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"crypto/md5" //nolint:gosec // 模拟百度秒传索引的 md5 语义，非安全用途
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// fakeRemoteFS 模拟「外部/远端卷」（如 baidupcs）的底层 FS：内存承载 + **操作/字节计量**。
//
// 为什么要计量：本文件是**差分验证**（用户要求 2026-10-18）——用**本地卷 secretdata 作对照组**，
// 同一操作脚本跑两种 target，断言**功能一致**并**量化表现/性能差异**（内层读写次数与字节、
// Range 读取量、是否发生「内层再落一份 staging」的冗余拷贝）。
//
// 可选能力按需启用（同一 fake 既能模拟「支持 Range 的远端」，也能模拟「不支持 Range」）：
//   - rangeReader=true  ⇒ 实现 syncpkg.RangeReader（远端支持定点读 ⇒ secretdata 走段级快路径）
//   - stagedReuse=true  ⇒ 实现 syncpkg.StagedPathReuse（可复用调用方已暂存路径 ⇒ 零二次拷贝）
type fakeRemoteFS struct {
	mu     sync.Mutex
	files  map[string][]byte
	mtimes map[string]int64

	rangeReader bool
	stagedReuse bool

	// 计量（全部内层操作）
	openReads      int   // 整对象读次数（OpenRead）
	openReadBytes  int64 // 整对象读字节
	rangeReads     int   // 定点读次数（OpenRangeRead）
	rangeBytes     int64 // 定点读字节
	writes         int   // WriteFile 次数
	writeBytes     int64 // WriteFile 字节
	stagedWrites   int   // WriteFromStagedPath 次数（复用调用方暂存 ⇒ 不应发生二次拷贝）
	stagedBytes    int64
	internalCopies int64 // 「内层内部 staging 拷贝」字节（模拟 baidupcs 的 staging-* → Storage.Put）
}

func newFakeRemoteFS(rangeReader, stagedReuse bool) *fakeRemoteFS {
	return &fakeRemoteFS{files: map[string][]byte{}, mtimes: map[string]int64{}, rangeReader: rangeReader, stagedReuse: stagedReuse}
}

var (
	_ syncpkg.FS = (*fakeRemoteFS)(nil)
)

func (f *fakeRemoteFS) ListDir(_ context.Context, p string) ([]syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := strings.Trim(p, "/")
	if prefix != "" {
		prefix += "/"
	}
	seen := map[string]bool{}
	var out []syncpkg.Entry
	for name, b := range f.files {
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		rest := name[len(prefix):]
		seg := rest
		if before, _, ok := strings.Cut(rest, "/"); ok {
			seg = before
			if !seen[seg] {
				seen[seg] = true
				out = append(out, syncpkg.Entry{Name: seg, Path: path.Join(p, seg), IsDir: true})
			}
			continue
		}
		if !seen[seg] {
			seen[seg] = true
			out = append(out, syncpkg.Entry{Name: seg, Path: path.Join(p, seg), Size: int64(len(b)), MTime: f.mtimes[name]})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (f *fakeRemoteFS) Stat(_ context.Context, p string) (*syncpkg.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[strings.Trim(p, "/")]
	if !ok {
		return nil, nil // not-exist 契约：不存在返回 (nil,nil)
	}
	return &syncpkg.Entry{Name: path.Base(p), Path: p, Size: int64(len(b)), MTime: f.mtimes[strings.Trim(p, "/")]}, nil
}

func (f *fakeRemoteFS) OpenRead(_ context.Context, p string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[strings.Trim(p, "/")]
	if !ok {
		return nil, fmt.Errorf("fakeRemoteFS: %s 不存在", p)
	}
	f.openReads++
	f.openReadBytes += int64(len(b))
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (f *fakeRemoteFS) OpenRangeRead(_ context.Context, p string, offset, size int64) (io.ReadCloser, error) {
	if !f.rangeReader {
		return nil, fmt.Errorf("fakeRemoteFS: 未实现定点读: %w", syncpkg.ErrUnsupported)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.files[strings.Trim(p, "/")]
	if !ok {
		return nil, fmt.Errorf("fakeRemoteFS: %s 不存在", p)
	}
	if offset < 0 || size < 0 || offset > int64(len(b)) {
		return nil, fmt.Errorf("fakeRemoteFS: 区间越界")
	}
	end := min(offset+size, int64(len(b)))
	f.rangeReads++
	f.rangeBytes += end - offset
	return io.NopCloser(bytes.NewReader(b[offset:end])), nil
}

func (f *fakeRemoteFS) WriteFile(ctx context.Context, _ syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	// 模拟 baidupcs 的 **ctx 暂存复用分支**（其 WriteFile 自取 ctx 中的已暂存路径 ⇒ 不再落第二份）：
	// 使本差分测试成为外部卷的忠实模型（能力面 + 复用语义一致）。
	if f.stagedReuse {
		if sp, ok := syncpkg.StagedLocalPathFrom(ctx); ok {
			return f.WriteFromStagedPath(ctx, p, sp, size, mtime)
		}
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes++
	f.writeBytes += int64(len(b))
	// 模拟「内层内部再落一份 staging 后上传」的拷贝成本（真实 baidupcs 的 staging-* → Storage.Put）。
	f.internalCopies += int64(len(b))
	key := strings.Trim(p, "/")
	f.files[key] = b
	f.mtimes[key] = mtime
	_ = size
	return nil
}

// WriteFromStagedPath 实现 syncpkg.StagedPathReuse（仅当 stagedReuse=true）：**直接消费调用方
// 已暂存的本地路径** ⇒ 内层不再落第二份（internalCopies 不增加）。
func (f *fakeRemoteFS) WriteFromStagedPath(_ context.Context, relPath, stagedPath string, size, mtime int64) error {
	if !f.stagedReuse {
		return fmt.Errorf("fakeRemoteFS: 未实现暂存复用: %w", syncpkg.ErrUnsupported)
	}
	b, err := os.ReadFile(stagedPath)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stagedWrites++
	f.stagedBytes += int64(len(b))
	key := strings.Trim(relPath, "/")
	f.files[key] = b
	f.mtimes[key] = mtime
	_ = size
	return nil
}

func (f *fakeRemoteFS) Rename(_ context.Context, from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	src, dst := strings.Trim(from, "/"), strings.Trim(to, "/")
	b, ok := f.files[src]
	if !ok {
		return fmt.Errorf("fakeRemoteFS: 源不存在")
	}
	f.files[dst] = b
	f.mtimes[dst] = f.mtimes[src]
	delete(f.files, src)
	delete(f.mtimes, src)
	return nil
}

func (f *fakeRemoteFS) Delete(_ context.Context, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := strings.Trim(p, "/")
	delete(f.files, key)
	delete(f.mtimes, key)
	return nil
}

func (f *fakeRemoteFS) MakeDir(_ context.Context, _ string) error { return nil }

// metric 是计量快照（按**单次操作**取差值用）。
type metric struct {
	openReads, rangeReads, writes, stagedWrites                        int
	openReadBytes, rangeBytes, writeBytes, stagedBytes, internalCopies int64
}

// snapshot 返回当前计量快照。
func (f *fakeRemoteFS) snapshot() metric {
	f.mu.Lock()
	defer f.mu.Unlock()
	return metric{
		openReads: f.openReads, rangeReads: f.rangeReads, writes: f.writes, stagedWrites: f.stagedWrites,
		openReadBytes: f.openReadBytes, rangeBytes: f.rangeBytes, writeBytes: f.writeBytes,
		stagedBytes: f.stagedBytes, internalCopies: f.internalCopies,
	}
}

// counters 返回计量快照（含「内层是否发生冗余 staging 拷贝」）。
func (f *fakeRemoteFS) counters() (openReads int, openReadBytes int64, rangeReads int, rangeBytes int64, writes int, writeBytes int64, stagedWrites int, stagedBytes int64, internalCopies int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openReads, f.openReadBytes, f.rangeReads, f.rangeBytes, f.writes, f.writeBytes, f.stagedWrites, f.stagedBytes, f.internalCopies
}

// differentialScript 是**同一套操作脚本**（线性、对两种 target 完全一致）：写多分块文件 →
// 顺序读回 → Stat/列表 → 单次中段 Range 读 → 改名 → 同卷复制 → 删除；返回可逐项对照的观测值。
// 对远端 target：在**顺序读**与**单次 Range 读**两处按差值计量（避免被脚本自身的多次读取污染 ✗）。
func differentialScript(t *testing.T, sfs *SecretdataFS, ctx context.Context, payload []byte) map[string]string {
	t.Helper()
	obs := map[string]string{}
	snap := func() (metric, bool) {
		if r, ok := sfs.inner.(*fakeRemoteFS); ok {
			return r.snapshot(), true
		}
		return metric{}, false
	}

	if err := sfs.MakeDir(ctx, "d"); err != nil {
		t.Fatalf("MakeDir: %v", err)
	}
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	// ① 顺序读（触发整文件校验）+ 放大计量（≤2× 对象 ⇒ 段级快路径）。
	b0, has := snap()
	rc, err := sfs.OpenRead(ctx, "d/f.bin")
	if err != nil {
		t.Fatalf("OpenRead: %v", err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	if has {
		a0, _ := snap()
		obs["seq_range_bytes_le_2x"] = fmt.Sprint(a0.rangeBytes-b0.rangeBytes <= 2*int64(len(payload)))
	}
	obs["content_equal"] = fmt.Sprint(bytes.Equal(got, payload))
	obs["size"] = fmt.Sprint(len(got))

	// ② Stat / ListDir（逻辑视图一致性）
	st, err := sfs.Stat(ctx, "d/f.bin")
	if err != nil || st == nil {
		t.Fatalf("Stat: %v %v", err, st)
	}
	obs["stat_size"] = fmt.Sprint(st.Size)
	ents, err := sfs.ListDir(ctx, "d")
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, fmt.Sprintf("%s:%d:%v", e.Name, e.Size, e.IsDir))
	}
	sort.Strings(names)
	obs["list"] = strings.Join(names, ",")

	// ③ 单次中段 Range 读（200 字节）+ 读取量计量（< 对象 1/4 ⇒ 段级最小下载）。
	b1, has1 := snap()
	if rc, err := sfs.OpenRangeRead(ctx, "d/f.bin", 1000, 200); err == nil {
		seg, _ := io.ReadAll(rc)
		_ = rc.Close()
		obs["range_mid_equal"] = fmt.Sprint(bytes.Equal(seg, payload[1000:1200]))
	} else {
		obs["range_mid_equal"] = "err:" + err.Error()
	}
	if has1 {
		a1, _ := snap()
		obs["single_range_bytes_lt_quarter"] = fmt.Sprint(a1.rangeBytes-b1.rangeBytes < int64(len(payload))/4)
	}
	// ④ 尾部 Range 读
	if rc, err := sfs.OpenRangeRead(ctx, "d/f.bin", int64(len(payload))-50, 50); err == nil {
		seg, _ := io.ReadAll(rc)
		_ = rc.Close()
		obs["range_tail_equal"] = fmt.Sprint(bytes.Equal(seg, payload[len(payload)-50:]))
	} else {
		obs["range_tail_equal"] = "err:" + err.Error()
	}

	// ⑤ 改名（只重写 meta）· ⑥ 同卷复制 · ⑦ 删除（即时物理删）
	if err := sfs.Rename(ctx, "d/f.bin", "d/g.bin"); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	obs["old_gone"] = fmt.Sprint(mustStatNil(t, sfs, ctx, "d/f.bin"))
	obs["new_ok"] = fmt.Sprint(mustReadEqual(t, sfs, ctx, "d/g.bin", payload))
	if err := sfs.CopyFile(ctx, "d/g.bin", "d/h.bin"); err != nil {
		t.Fatalf("CopyFile: %v", err)
	}
	obs["copy_ok"] = fmt.Sprint(mustReadEqual(t, sfs, ctx, "d/h.bin", payload))
	if err := sfs.Delete(ctx, "d/h.bin"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	obs["deleted"] = fmt.Sprint(mustStatNil(t, sfs, ctx, "d/h.bin"))
	obs["g_survives_delete"] = fmt.Sprint(mustReadEqual(t, sfs, ctx, "d/g.bin", payload))
	return obs
}

func mustStatNil(t *testing.T, sfs *SecretdataFS, ctx context.Context, rel string) bool {
	t.Helper()
	st, err := sfs.Stat(ctx, rel)
	return err == nil && st == nil
}

func mustReadEqual(t *testing.T, sfs *SecretdataFS, ctx context.Context, rel string, want []byte) bool {
	t.Helper()
	rc, err := sfs.OpenRead(ctx, rel)
	if err != nil {
		return false
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	return err == nil && bytes.Equal(got, want)
}

// TestDifferential_LocalVsExternalTarget 是**差分验证**（用户要求）：同一操作脚本分别跑
// 「本地卷 target（对照组）」与「外部/远端 target（fake，模拟 baidupcs）」：
//   - **功能一致性**：全部观测值必须逐项相同（内容/尺寸/列表/改名/复制/删除/Range）；
//   - **重启一致性**：两者在同一底层重挂载后均保持可读；
//   - **表现/性能差异**：量化内层操作与字节，并断言**关键性能性质**——
//     ① 支持 RangeReader 的远端：Range 读**不得**退化为整对象下载（rangeBytes ≪ 对象大小）；
//     ② 不支持 RangeReader 的远端：允许退化为整对象读（记录差异，不视为缺陷）；
//     ③ **冗余 staging 拷贝**：本用例现在断言**已修复的行为**——分块经 `WithStagedLocalPath`
//     复用暂存（stagedWrites==0 → 现已 >0；internalCopies 不再随对象大小线性增长）。
func TestDifferential_LocalVsExternalTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("differential-local-vs-external-"), 16*1024) // 512KiB（多分块）
	opts := func(tempDir string) Options {
		return Options{
			Secret:    []byte("test-secret-key-000"),
			Algorithm: testAlgo,
			TempDir:   tempDir,
			Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
		}
	}

	// 对照组：本地卷（LocalFS，具备 RangeReader + LocalVolume）。
	localDir := t.TempDir()
	localInner := syncpkg.NewLocalFS(localDir, nil)
	localFS, err := NewFS(localInner, opts(t.TempDir()))
	if err != nil {
		t.Fatalf("NewFS(local): %v", err)
	}
	localObs := differentialScript(t, localFS, ctx, payload)

	// 实验组：外部/远端（fake，具备 RangeReader + StagedPathReuse）。
	remote := newFakeRemoteFS(true, true)
	remoteFS, err := NewFS(remote, opts(t.TempDir()))
	if err != nil {
		t.Fatalf("NewFS(remote): %v", err)
	}
	remoteObs := differentialScript(t, remoteFS, ctx, payload)

	// ① 功能一致性：逐项对照。
	for k, want := range localObs {
		if got := remoteObs[k]; got != want {
			t.Errorf("功能不一致 [%s]：本地=%q 外部=%q", k, want, got)
		}
	}
	// 外部 target **额外**的观测项必须恰好是**性能差异项**（功能项集合必须与对照组完全一致）。
	perfKeys := map[string]bool{"seq_range_bytes_le_2x": true, "single_range_bytes_lt_quarter": true}
	for k := range remoteObs {
		if _, inLocal := localObs[k]; !inLocal && !perfKeys[k] {
			t.Errorf("外部 target 出现对照组没有的**非性能**观测项 [%s]（功能面应完全一致）", k)
		}
	}

	// ② 重启一致性（两者在同一底层重挂载后仍可读）。
	localRe, err := NewFS(localInner, opts(t.TempDir()))
	if err != nil {
		t.Fatalf("重启(本地): %v", err)
	}
	if !mustReadEqual(t, localRe, ctx, "d/g.bin", payload) {
		t.Error("本地 target 重启后读回不一致")
	}
	remoteRe, err := NewFS(remote, opts(t.TempDir()))
	if err != nil {
		t.Fatalf("重启(外部): %v", err)
	}
	if !mustReadEqual(t, remoteRe, ctx, "d/g.bin", payload) {
		t.Error("外部 target 重启后读回不一致")
	}

	// ③ 性能差异：量化并断言关键性质。
	lr, lrb, lrr, lrrb, lw, lwb, lsw, lsb, lic := remote.counters()
	t.Logf("外部 target 内层计量：整对象读 %d 次/%d 字节 · 定点读 %d 次/%d 字节 · 写 %d 次/%d 字节 · 暂存复用 %d 次/%d 字节 · 内层内部拷贝 %d 字节",
		lr, lrb, lrr, lrrb, lw, lwb, lsw, lsb, lic)
	t.Logf("对象大小=%d 字节（定点读总量 %d 字节 ⇒ 远小于整对象说明走段级快路径）", len(payload), lrrb)

	// 判据（按**单次操作**，非总量——脚本本身多次读同一对象 ⇒ 总量必然 >1×）：
	//   ① 顺序读（一次全量）放大 ≤2×（段级快路径 ⇒ 每块 salt + 目标段）；
	//   ② 单次 200 字节中段 Range 读 ≪ 对象（< 1/4）⇒ 证明走段级最小下载。
	if remoteObs["seq_range_bytes_le_2x"] != "true" {
		t.Errorf("外部 target 顺序读放大 >2×（实测 rangeBytes=%d，对象=%d）✗", lrrb, len(payload))
	}
	if remoteObs["single_range_bytes_lt_quarter"] != "true" {
		t.Errorf("单次 200 字节 Range 读读取量 ≥ 对象 1/4 ⇒ 未走段级最小下载 ✗（rangeReads=%d rangeBytes=%d）", lrr, lrrb)
	}
	if lrr == 0 || lrrb == 0 {
		t.Error("支持 RangeReader 的远端：Range 读不应退化为整对象下载（定点读为 0）✗")
	}
	// 性能判据（锁定 2026-10-18 修复）：**凡有本地路径的写**（= 加密分块）都必须经既有
	// `StagedPathReuse` 机制复用暂存 ⇒ 内层**不得**再落第二份 staging（峰值 1×）。
	// 口径说明：meta blob 是**内存态**（无本地路径）⇒ 走普通 `WriteFile`、内层按需拷贝，
	// 属**设计如此**（不额外落本地文件）⇒ 判据取「内部拷贝字节 == 普通写字节」（即**没有任何
	// 分块**被内层二次拷贝）。修复前实测：stagedWrites=0 且 internalCopies≈加密后大小 ✗ ⇒ 必红。
	if lsw == 0 {
		t.Errorf("分块上传未复用调用方暂存路径（stagedWrites=0）⇒ 外部 target 会冗余落第二份 ✗")
	}
	if lsb < int64(len(payload)) {
		t.Errorf("经暂存复用上传的字节 %d 少于明文大小 %d（应覆盖全部分块）✗", lsb, len(payload))
	}
	if lic != lwb {
		t.Errorf("内部 staging 拷贝 %d 字节 ≠ 普通写字节 %d ⇒ 有分块被内层二次拷贝（未复用暂存）✗", lic, lwb)
	}
}

// fakeRapidFS 在 fakeRemoteFS 之上模拟**百度网盘 putAttempt 语义**（本轮差分扩展）：
// 多分片上传后远端 md5 **非权威**（`MD5Unreliable`）⇒ ETag 复核不匹配 ⇒ **秒传（RapidUpload）
// 刷新**（读**本地产物**算 md5/分块 md5；命中才把远端 md5 刷新为权威整文件 md5）⇒ 再次复核。
// 这正是 docs/roadmap.md ①-附 的领域约束：checksum 只在秒传命中后收敛。
//
// rapidIndex=false 模拟「秒传未命中」（内容不在秒传索引）⇒ 有界重试耗尽 ⇒ 报错（fail-closed）。
type fakeRapidFS struct {
	*fakeRemoteFS
	rapidIndex bool

	rmu          sync.Mutex
	uploads      int               // 真正发生的传输次数（未命中秒传才 +1）
	uploadBytes  int64             // 真正传输的字节
	refreshHits  int               // 秒传刷新命中次数
	refreshMiss  int               // 秒传刷新未命中次数
	refreshReads int               // 刷新时读**本地产物**的次数
	refreshErr   error             // 刷新时读本地产物失败（产物被提前删除）
	auth         map[string]string // 远端**权威** md5（秒传刷新命中后写入；缺失 = 非权威）
}

func newFakeRapidFS(rapidIndex bool) *fakeRapidFS {
	return &fakeRapidFS{fakeRemoteFS: newFakeRemoteFS(true, true), rapidIndex: rapidIndex}
}

// WriteFile 模拟「存在性检查 → 上传 → ETag 复核 → 不匹配则秒传刷新 → 再复核」，最多 3 轮。
// 每轮都必须能重读调用方交付的本地路径（md5/分块 md5 与刷新都用它）。
func (f *fakeRapidFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, p string, r io.Reader, size, mtime int64) error {
	sp, _ := syncpkg.StagedLocalPathFrom(ctx)
	if sp == "" {
		// 内存态写（文件 meta / 目录 meta 无本地路径，设计如此）：走普通上传，不参与秒传收敛。
		return f.fakeRemoteFS.WriteFile(ctx, key, p, r, size, mtime)
	}
	content, err := os.ReadFile(sp)
	if err != nil {
		f.rmu.Lock()
		if f.refreshErr == nil {
			f.refreshErr = err
		}
		f.rmu.Unlock()
		return fmt.Errorf("fakeRapidFS: 读本地产物失败: %w", err)
	}
	localMD5 := md5Hex(content)
	for attempt := 1; attempt <= 3; attempt++ {
		// ① 存在性检查：秒传/ETag 一致 ⇒ 直接完成（零传输）。
		if ok, e := f.authoritative(p, localMD5); e != nil {
			return e
		} else if ok {
			return nil
		}
		// ② 上传（模拟多分片：远端 md5 非权威）。
		if err := f.WriteFromStagedPath(ctx, p, sp, size, mtime); err != nil {
			return err
		}
		f.rmu.Lock()
		f.uploads++
		f.uploadBytes += int64(len(content))
		f.rmu.Unlock()
		// ③ ETag 复核：非权威 ⇒ 秒传刷新（重读本地产物）。
		f.rmu.Lock()
		f.refreshReads++
		f.rmu.Unlock()
		if _, rerr := os.ReadFile(sp); rerr != nil {
			f.rmu.Lock()
			if f.refreshErr == nil {
				f.refreshErr = rerr
			}
			f.rmu.Unlock()
			return fmt.Errorf("fakeRapidFS: 秒传刷新读本地产物失败（产物已清）: %w", rerr)
		}
		if f.rapidIndex {
			// 内容刚上传 ⇒ 秒传索引可命中 ⇒ 远端 md5 刷新为权威值 ⇒ 下一轮复核一致。
			f.rmu.Lock()
			f.refreshHits++
			f.rmu.Unlock()
			f.setAuthoritative(p, localMD5)
			continue
		}
		f.rmu.Lock()
		f.refreshMiss++
		f.rmu.Unlock()
	}
	return fmt.Errorf("fakeRapidFS: 秒传未命中且复核不匹配超过 3 次（fail-closed）")
}

func (f *fakeRapidFS) authoritative(p, wantMD5 string) (bool, error) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	md5s, ok := f.auth[p]
	if !ok {
		return false, nil
	}
	return md5s == wantMD5, nil
}

func (f *fakeRapidFS) setAuthoritative(p, md5v string) {
	f.rmu.Lock()
	defer f.rmu.Unlock()
	if f.auth == nil {
		f.auth = map[string]string{}
	}
	f.auth[p] = md5v
}

// md5Hex 计算内容 md5（**仅模拟百度秒传索引语义**，非安全用途）。
func md5Hex(b []byte) string {
	h := md5.New() //nolint:gosec // 模拟百度秒传索引的 md5，非安全用途
	_, _ = h.Write(b)
	return hex.EncodeToString(h.Sum(nil))
}

// TestDifferential_RapidUploadConvergence 是**秒传收敛**场景的最小差分扩展（docs/roadmap.md
// ①-附）：同一 512KiB 多分块对象分别写「本地卷 target（对照组）」与「秒传型外部 target
// （fake，模拟百度 putAttempt：多分片上传后 md5 非权威 → ETag 不匹配 → 秒传刷新 → 权威 md5）」。
//
// 断言（同时锁定本轮交错上传的三条性质）：
//
//	① **功能一致**：读回内容/尺寸与对照组逐项一致；
//	② **秒传按块刷新**：每个分块都以**本地产物**为源交付（暂存复用），刷新命中次数 == 传输次数
//	   ⇒ 无整文件重传、无二次落 staging；且刷新读本地产物**从未失败**（产物在内层调用期间在场）；
//	③ **本地临时峰值**：写期间本地分块副本数峰值 ≤ 1（**O(1 块)**，而非 O(整个文件)）。
//
// 变异验证：撤销交错（分块全部先落 temp）⇒ ③ 的峰值断言必红；分块改回内存交付（不交本地路径）
// ⇒ ② 的暂存复用/刷新断言必红。
func TestDifferential_RapidUploadConvergence(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("rapid-upload-convergence-"), 20*1024) // 500KiB（多分块）
	tempDir := t.TempDir()
	rapid := newFakeRapidFS(true)
	probe := &interleaveProbeFS{FS: rapid, tempDir: tempDir, stageReads: 1}
	opts := Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
		Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	}

	// 对照组：本地卷 target。
	localFS, err := NewFS(syncpkg.NewLocalFS(t.TempDir(), nil), opts)
	if err != nil {
		t.Fatalf("NewFS(local): %v", err)
	}
	if werr := localFS.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile(local): %v", werr)
	}
	want := readAll(t, localFS, ctx, "d/f.bin")

	// 实验组：秒传型外部 target。
	rapidFS, err := NewFS(probe, opts)
	if err != nil {
		t.Fatalf("NewFS(rapid): %v", err)
	}
	if werr := rapidFS.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); werr != nil {
		t.Fatalf("WriteFile(rapid): %v", werr)
	}
	got := readAll(t, rapidFS, ctx, "d/f.bin")
	if !bytes.Equal(got, want) || !bytes.Equal(got, payload) {
		t.Fatalf("功能不一致：实验组 %d 字节 vs 对照组 %d 字节", len(got), len(want))
	}

	writes, maxLocalFiles, stagedWrites, _, _, entries, stageReadErr := probe.snapshot()
	rapid.rmu.Lock()
	uploads, uploadBytes, hits, misses, refreshReads, refreshErr := rapid.uploads, rapid.uploadBytes, rapid.refreshHits, rapid.refreshMiss, rapid.refreshReads, rapid.refreshErr
	rapid.rmu.Unlock()
	t.Logf("实验组实测：内层写 %d 次（分块经暂存复用 %d 次）· 本地分块副本峰值 %d（逐次 %v）", writes, stagedWrites, maxLocalFiles, entries)
	t.Logf("秒传收敛实测：真实传输 %d 次/%d 字节 · 刷新命中 %d · 未命中 %d · 刷新读本地产物 %d 次（错误=%v）",
		uploads, uploadBytes, hits, misses, refreshReads, refreshErr)

	if stageReadErr != nil || refreshErr != nil {
		t.Fatalf("内层/秒传刷新读本地产物失败（产物未在内层调用期间保持在场）: %v / %v", stageReadErr, refreshErr)
	}
	if misses != 0 || hits == 0 {
		t.Fatalf("秒传刷新应全部命中（hits=%d misses=%d）⇒ 未收敛 ✗", hits, misses)
	}
	if hits != uploads {
		t.Fatalf("秒传刷新命中 %d != 真实传输 %d ⇒ 存在未刷新/重复传输 ✗", hits, uploads)
	}
	if uploads != len(rapidFS.index["d/f.bin"].meta.Chunks) {
		t.Fatalf("真实传输次数 %d != 分块数 %d ⇒ 有分块未上传/重复传输 ✗",
			uploads, len(rapidFS.index["d/f.bin"].meta.Chunks))
	}
	if maxLocalFiles > 1 {
		t.Fatalf("本地分块副本峰值 %d ⇒ 未交错（应 O(1 块)）✗ 逐次 %v", maxLocalFiles, entries)
	}
}

// TestDifferential_RapidUploadMiss_FailClosed 是**对照组**：秒传**未命中**（内容不在秒传索引）
// ⇒ 复核恒不匹配 ⇒ 有界重试耗尽 ⇒ 必须 fail-closed（明确错误 + 无半态），绝不静默接受
// 「内容未必一致」的写（docs/roadmap.md ①-附 §2 ③ 的同型语义）。
func TestDifferential_RapidUploadMiss_FailClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("rapid-upload-miss-"), 16*1024)
	tempDir := t.TempDir()
	rapid := newFakeRapidFS(false) // 秒传索引不可用 ⇒ 恒未命中
	probe := &interleaveProbeFS{FS: rapid, tempDir: tempDir, stageReads: 1}
	rapidFS, err := NewFS(probe, Options{
		Secret: []byte("test-secret-key-000"), Algorithm: testAlgo, TempDir: tempDir,
		Block: shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	// 先建目录 "d"（其容器已存在）⇒ 失败写的回滚不会走「回收**本次新建**空容器」分支，
	// 于是本用例检测的正是**失败块本身**是否被删除（而不是容器回收的副作用）。
	if merr := rapidFS.MakeDir(ctx, "d"); merr != nil {
		t.Fatalf("MakeDir: %v", merr)
	}
	werr := rapidFS.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if werr == nil {
		t.Fatal("秒传未命中 + 复核不匹配 ⇒ 必须报错（fail-closed）✗")
	}
	if st, serr := rapidFS.Stat(ctx, "d/f.bin"); serr != nil || st != nil {
		t.Fatalf("失败写不得留下半态（Stat=%v err=%v）✗", st, serr)
	}
	// **底层无残留**（P2-1/P2-7② 修复，2026-10-19）：秒传未命中 ⇒ 内层已写入分块后返回错误
	// （模拟「写成功但复核失败」），失败块/meta 必须能被回滚删掉——否则索引层无半态但**远端
	// 留孤儿 blob**。只断言索引层会漏掉这个方向。允许残留**目录 meta**（路径解析锚点，非
	// 本次写产物）。
	rapid.mu.Lock()
	var orphans []string
	for k := range rapid.files {
		if shardseal.ClassifyName(path.Base(k)) != shardseal.KindDirMeta {
			orphans = append(orphans, k)
		}
	}
	rapid.mu.Unlock()
	if len(orphans) != 0 {
		corte := orphans
		if len(corte) > 3 {
			corte = corte[:3]
		}
		t.Fatalf("fail-closed 回滚后底层仍残留 %d 个非目录 meta blob（远端孤儿）✗ 例如 %v", len(orphans), corte)
	}
	rapid.rmu.Lock()
	misses, uploads := rapid.refreshMiss, rapid.uploads
	rapid.rmu.Unlock()
	if misses == 0 || uploads == 0 {
		t.Fatalf("探针失效：未观测到秒传未命中/重试（misses=%d uploads=%d）", misses, uploads)
	}
	t.Logf("秒传未命中实测：重试传输 %d 次 · 刷新未命中 %d 次 ⇒ fail-closed（无半态）", uploads, misses)
}
