// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// interleaveProbeFS 包装底层 FS，在**每次内层写**前采样本地 temp 目录的条目构成 +
// 是否经 ctx 交了「已暂存本地路径」，并模拟内层对同一 stagedPath 的**多遍读**
// （md5 预扫 / 上传 / 复核重试 / 秒传刷新都要读它 ⇒ 生命周期契约：调用方在内层返回前不得删）。
//
// 为什么按**条目数**判定本地峰值：本仓实测（Windows）打开中的文件对 os.ReadDir 可能
// Size()=0 ⇒ 字节判据会假绿；条目始终可见、确定性（与 read_temp_peak_test.go 同判据）。
type interleaveProbeFS struct {
	syncpkg.FS
	tempDir string
	// stageReads 模拟内层的多遍读次数（每写一次读 N 遍）。
	stageReads int

	mu           sync.Mutex
	writes       int
	records      []writeRecord
	maxEntries   int
	entriesAt    []int
	stagedWrites int
	memoryWrites int
	stageReadErr error
	stageShort   int
}

// sample 递归统计 temp 目录下的**文件**数（`chunks-*` 目录本身不计——判据是「同时存在多少份
// 本地分块副本」）。
func (p *interleaveProbeFS) sample() int {
	n := 0
	_ = filepath.WalkDir(p.tempDir, func(_ string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		n++
		return nil
	})
	p.mu.Lock()
	p.writes++
	if n > p.maxEntries {
		p.maxEntries = n
	}
	p.entriesAt = append(p.entriesAt, n)
	p.mu.Unlock()
	return n
}

func (p *interleaveProbeFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	n := p.sample()
	sp, staged := syncpkg.StagedLocalPathFrom(ctx)
	p.mu.Lock()
	p.records = append(p.records, writeRecord{path: path, localFiles: n, staged: staged, size: size})
	p.mu.Unlock()
	// P1（2026-10-21）：断言每块**数据分块**（经暂存复用交付）都带**通用上传提示**
	// （RandomContent + 已算好的 md5）——baidupcs 整块免重扫的接线前提。
	// 注：meta 走内存交付（无 stagedPath），不要求提示（内层自算）。
	if staged {
		uh, ok := syncpkg.UploadHintsFrom(ctx)
		var hintErr error
		switch {
		case !ok:
			hintErr = fmt.Errorf("分块应带 UploadHints（含 RandomContent+MD5）")
		case !uh.RandomContent:
			hintErr = fmt.Errorf("分块应带 RandomContent 提示")
		case uh.MD5 == "":
			hintErr = fmt.Errorf("分块应带已算好的 MD5 提示（免重扫）")
		}
		if hintErr != nil {
			p.mu.Lock()
			if p.stageReadErr == nil {
				p.stageReadErr = hintErr
			}
			p.mu.Unlock()
		}
	}
	if !staged {
		p.mu.Lock()
		p.memoryWrites++
		p.mu.Unlock()
		return p.FS.WriteFile(ctx, key, path, r, size, mtime)
	}
	p.mu.Lock()
	p.stagedWrites++
	p.mu.Unlock()
	// 模拟内层多遍读同一 stagedPath（md5 预扫 / 上传 / 复核 / 秒传刷新）——任一遍读不到
	// 内容（调用方提前删除）即记录失败，供断言定位。
	for i := 0; i < max(p.stageReads, 1); i++ {
		b, err := os.ReadFile(sp)
		if err != nil {
			p.mu.Lock()
			if p.stageReadErr == nil {
				p.stageReadErr = err
			}
			p.mu.Unlock()
			return fmt.Errorf("interleaveProbeFS: 内层第 %d 遍读 stagedPath 失败: %w", i+1, err)
		}
		if size > 0 && int64(len(b)) != size {
			p.mu.Lock()
			p.stageShort++
			p.mu.Unlock()
		}
	}
	// 最后以真实内容委托底层（模拟内层消费该 stagedPath）。
	f, oerr := os.Open(sp)
	if oerr != nil {
		return oerr
	}
	defer f.Close()
	sz, serr := f.Seek(0, io.SeekEnd)
	if serr == nil {
		_, serr = f.Seek(0, io.SeekStart)
	}
	if serr != nil {
		return serr
	}
	return p.FS.WriteFile(ctx, key, path, f, sz, mtime)
}

func (p *interleaveProbeFS) snapshot() (writes, maxEntries, stagedWrites, memoryWrites, stageShort int, entriesAt []int, stageReadErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.writes, p.maxEntries, p.stagedWrites, p.memoryWrites, p.stageShort, append([]int(nil), p.entriesAt...), p.stageReadErr
}

// writeRecord 是一次内层写的观测：目标路径 / 此刻 temp 内的**本地分块副本数** / 是否交本地路径。
type writeRecord struct {
	path       string
	localFiles int
	staged     bool
	size       int64
}

func (p *interleaveProbeFS) recordsSnapshot() []writeRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]writeRecord(nil), p.records...)
}

// newInterleaveFS 构造「探针内层 + 可 seek 源」的加密卷（32KiB 定长块 ⇒ 512KiB = 16 块）。
func newInterleaveFS(t *testing.T, probeReads int) (*SecretdataFS, *interleaveProbeFS, string) {
	t.Helper()
	tempDir := t.TempDir()
	raw := syncpkg.NewLocalFS(t.TempDir(), nil)
	probe := &interleaveProbeFS{FS: raw, tempDir: tempDir, stageReads: probeReads}
	sfs, err := NewFS(probe, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
		Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return sfs, probe, tempDir
}

// TestWriteFile_InterleavedChunkUploadLocalPeakO1Block 钉住**块级交错上传**（docs/roadmap.md
// ①-附 推荐形态 1）：分块密文「加密一块 → 上传一块 → 丢弃」，本地 temp 峰值 **O(1 块)**——任一
// 时刻 temp 目录至多存在**当前那一块**的本地副本；meta 需全部分块信息 ⇒ 留到末尾且**以内存态
// 上传**（不落本地文件）。
//
// 实测（512KiB / 32KiB 块 = 16 块）：修复前 temp 同时存在 **16 个**分块文件（先全部落盘再逐个
// 上传）；交错后恒为 **1**（meta 上传时刻为 **0**）。断言 maxEntries<=1 与 meta 时刻 ==0。
// 变异验证：撤销交错（回到「先全部加密落 temp 再上传」）⇒ maxEntries=16 必红。
func TestWriteFile_InterleavedChunkUploadLocalPeakO1Block(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("interleaved-upload-"), 30*1024) // 510KiB
	sfs, probe, _ := newInterleaveFS(t, 1)

	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	rc, rerr := sfs.OpenRead(ctx, "d/f.bin")
	if rerr != nil {
		t.Fatalf("OpenRead: %v", rerr)
	}
	got, gerr := io.ReadAll(rc)
	_ = rc.Close()
	if gerr != nil {
		t.Fatalf("读回失败: %v", gerr)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("读回内容不一致（%d vs %d 字节）", len(got), len(payload))
	}

	writes, maxEntries, stagedWrites, memoryWrites, _, entries, stageReadErr := probe.snapshot()
	t.Logf("交错后实测：内层写 %d 次（分块经暂存复用 %d 次 · 内存交付 %d 次）· 本地分块副本数峰值 %d（逐次 %v）",
		writes, stagedWrites, memoryWrites, maxEntries, entries)
	if stageReadErr != nil {
		t.Fatalf("内层读 stagedPath 失败（生命周期契约被破坏）: %v", stageReadErr)
	}
	if maxEntries > 1 {
		t.Fatalf("本地 temp 同时存在 %d 份分块副本 ⇒ 未交错（峰值应为 O(1 块)）✗ 逐次采样 %v", maxEntries, entries)
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e == nil {
		t.Fatal("夹具异常：无索引条目")
	}
	// 每个数据分块都经暂存复用交付（本地路径），数量与 meta 记录的分块数一致。
	if stagedWrites != len(e.meta.Chunks) {
		t.Fatalf("分块未全部经 WithStagedLocalPath 交付（staged %d / 分块 %d）✗", stagedWrites, len(e.meta.Chunks))
	}
	// meta：**内存态**（staged=false）且此刻本地分块副本数 == 0（meta 未落本地）。
	found := false
	for _, rec := range probe.recordsSnapshot() {
		if !strings.HasSuffix(rec.path, "/"+e.metaName) {
			continue
		}
		found = true
		if rec.staged {
			t.Fatalf("meta 走了本地路径交付（应恒为内存态）✗: %+v", rec)
		}
		if rec.localFiles != 0 {
			t.Fatalf("meta 上传时刻本地仍有 %d 份分块副本 ⇒ 未交错 ✗", rec.localFiles)
		}
	}
	if !found {
		t.Fatalf("未观测到 meta 的写入（探针失效；metaName=%s）", e.metaName)
	}
}

// TestWriteFile_StagedPathAliveAcrossInnerReads 钉住**秒传按块刷新**的前提（docs/roadmap.md
// ①-附 §1）：分块本地产物必须在**内层整个 WriteFile 调用期间**保持在场——内层的 md5 预扫 /
// 上传 / ETag 复核重试 / 秒传（RapidUpload）刷新都要读同一 stagedPath（若是 reader 或提前删除
// 就退化为「二次落 staging」或直接失败）。
//
// 内层模拟 3 遍读（预扫 + 上传 + 刷新）；断言 3 遍都读到且长度一致。
// 变异验证：把 `defer os.Remove(local)` 提前到内层调用**之前**（或改回不交本地路径）⇒ 红。
func TestWriteFile_StagedPathAliveAcrossInnerReads(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("staged-alive-"), 20*1024) // 240KiB
	sfs, probe, _ := newInterleaveFS(t, 3)

	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	writes, _, stagedWrites, _, stageShort, _, stageReadErr := probe.snapshot()
	if stageReadErr != nil {
		t.Fatalf("内层多遍读 stagedPath 失败 ⇒ 秒传刷新拿不到本地产物: %v", stageReadErr)
	}
	if stageShort != 0 {
		t.Fatalf("stagedPath 读回长度与声明不一致 %d 次 ⇒ 内层拿到的内容不完整 ✗", stageShort)
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e == nil {
		t.Fatal("夹具异常：无索引条目")
	}
	// 每个数据分块都必须经本地路径交付（内层共读 %d 遍）；meta / 目录 meta 为内存态（设计如此）。
	if stagedWrites != len(e.meta.Chunks) {
		t.Fatalf("分块交付形态不一致（staged %d / 分块 %d；总写 %d）⇒ 有分块退化为二次 staging ✗",
			stagedWrites, len(e.meta.Chunks), writes)
	}
	t.Logf("实测：内层写 %d 次（其中分块 %d 次全部经本地路径，每块被内层读 3 遍）", writes, stagedWrites)
}

// newStagingUnavailableFS 构造「本地临时目录不可用」的卷：NewFS 后删除 temp 目录
// ⇒ 交错上传所需的本地分块目录无法创建（跨平台确定性触发；权限/ENOSPC 走同一错误分支）。
func newStagingUnavailableFS(t *testing.T) (*SecretdataFS, *syncpkg.LocalFS, string) {
	t.Helper()
	tempDir := t.TempDir()
	loc := syncpkg.NewLocalFS(t.TempDir(), nil)
	sfs, err := NewFS(loc, Options{
		Secret:    []byte("test-secret-key-000"),
		Algorithm: testAlgo,
		TempDir:   tempDir,
		Block:     shardseal.BlockPolicy{Min: 32 << 10, Max: 32 << 10},
	})
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	if err := os.RemoveAll(tempDir); err != nil {
		t.Fatalf("移除 temp 目录: %v", err)
	}
	return sfs, loc, tempDir
}

// TestWriteFile_LocalStagingUnavailable_ReplayableSourceSucceeds 钉住**回退链 ②**
// （docs/roadmap.md ①-附 §2）：本地 staging 不可用时，若**源可重读**（本地文件 / 已暂存路径
// ⇒ 产物随时可由源重算）⇒ 降级为**内存交付**（不落本地产物），写**仍成功**——即「空间不足
// 回退而不是直接失败」。
//
// 变异验证：去掉回退（MkdirTemp 失败即返回错误）⇒ 本用例必红。
func TestWriteFile_LocalStagingUnavailable_ReplayableSourceSucceeds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("fallback-recompute-"), 16*1024) // 288KiB
	sfs, _, _ := newStagingUnavailableFS(t)

	srcPath := filepath.Join(t.TempDir(), "src.bin")
	if err := os.WriteFile(srcPath, payload, 0o644); err != nil {
		t.Fatalf("写源文件: %v", err)
	}
	src, oerr := os.Open(srcPath) // *os.File（本地文件 ⇒ 可重读 ⇒ 可重算）
	if oerr != nil {
		t.Fatalf("打开源: %v", oerr)
	}
	defer src.Close()
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", src, int64(len(payload)), 0); err != nil {
		t.Fatalf("源可重读时本地 staging 不可用应回退（内存交付）而不是失败: %v", err)
	}
	if !mustReadEqual(t, sfs, ctx, "d/f.bin", payload) {
		t.Fatal("回退路径写出的内容不可读回/不一致 ✗")
	}
}

// TestWriteFile_LocalStagingUnavailable_NonReplayableSourceFailsClosed 钉住**回退链 ③**
// （对照组，docs/roadmap.md ①-附 §2）：源**不可重读**（内存 reader = 一次性源）且本地产物
// 无法落盘 ⇒ 必须 **fail-closed**（明确错误 + 无半态残留），绝不静默接受不可重试/不可校验的写。
//
// 变异验证：去掉 `sourceReplayable` 门槛（任何源都降级内存交付）⇒ 本用例必红。
func TestWriteFile_LocalStagingUnavailable_NonReplayableSourceFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("fail-closed-"), 16*1024)
	sfs, raw, _ := newStagingUnavailableFS(t)

	err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0)
	if err == nil {
		t.Fatal("源不可重读 + 本地产物不可落盘 ⇒ 必须报错（fail-closed）✗")
	}
	if !strings.Contains(err.Error(), "源不可重读") {
		t.Fatalf("错误应明确指出原因（源不可重读），实测: %v", err)
	}
	if st, serr := sfs.Stat(ctx, "d/f.bin"); serr != nil || st != nil {
		t.Fatalf("失败写不得留下半态（Stat=%v err=%v）✗", st, serr)
	}
	ents, lerr := raw.ListDir(ctx, "")
	if lerr != nil {
		t.Fatalf("ListDir: %v", lerr)
	}
	// 允许遗留**空根容器目录**（路径解析锚点，恒不删），但不得有文件残留。
	for _, e := range ents {
		if !e.IsDir {
			t.Fatalf("失败写应在底层留 0 个文件（实测残留 %q，%d 字节）⇒ 未回滚 ✗", e.Name, e.Size)
		}
	}
}

// stagedNameProbeFS 记录**每次内层写**的目标名与交付形态（本地路径 / 内存），供「所有分块
// 上传点都经 WithStagedLocalPath」的断言（含 parity / dedup 两个非主路径的分块上传点）。
type stagedNameProbeFS struct {
	syncpkg.FS
	mu     sync.Mutex
	staged map[string]bool
	memory map[string]bool
}

func (p *stagedNameProbeFS) WriteFile(ctx context.Context, key syncpkg.LogicalKey, path string, r io.Reader, size, mtime int64) error {
	sp, ok := syncpkg.StagedLocalPathFrom(ctx)
	p.mu.Lock()
	if ok {
		p.staged[path] = true
	} else {
		p.memory[path] = true
	}
	p.mu.Unlock()
	if ok {
		b, rerr := os.ReadFile(sp)
		if rerr != nil {
			return fmt.Errorf("内层读 stagedPath %s 失败: %w", sp, rerr)
		}
		return p.FS.WriteFile(ctx, key, path, bytes.NewReader(b), int64(len(b)), mtime)
	}
	return p.FS.WriteFile(ctx, key, path, r, size, mtime)
}

func newStagedProbeFS(t *testing.T, opts Options) (*SecretdataFS, *stagedNameProbeFS) {
	t.Helper()
	probe := &stagedNameProbeFS{FS: syncpkg.NewLocalFS(t.TempDir(), nil), staged: map[string]bool{}, memory: map[string]bool{}}
	opts.Secret = []byte("test-secret-key-000")
	opts.Algorithm = testAlgo
	opts.TempDir = t.TempDir()
	if opts.Block.Min == 0 {
		opts.Block = shardseal.BlockPolicy{Min: 16 << 10, Max: 16 << 10}
	}
	sfs, err := NewFS(probe, opts)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return sfs, probe
}

// TestWriteFile_ErasureParityUploadsViaStagedPath 钉住**parity 分块上传点**也经既有暂存复用机制
// （docs/roadmap.md ①-附 §2 第 2 项）：erasure 卷的 parity 段此前以 `bytes.NewReader` 内存交付
// （内层需再落一份 staging ⇒ 二次拷贝、秒传刷新拿不到本地产物）；现与数据分块同口径——以本地
// 分块文件为源，经 `WithStagedLocalPath` 交内层。
//
// 变异验证：parity 回到内存交付（uploadBlob）⇒ 「parity 在 staged 集合内」必红。
func TestWriteFile_ErasureParityUploadsViaStagedPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("parity-staged-"), 8*1024) // 128KiB / 16KiB 块 = 8 块
	sfs, probe := newStagedProbeFS(t, Options{Erasure: true})
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile(erasure): %v", err)
	}
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e == nil || e.meta.Parity == nil {
		t.Fatalf("夹具异常：无 parity 记录（%+v）", e)
	}
	probe.mu.Lock()
	parityStaged := probe.staged[filepath.ToSlash(filepath.Join(e.dirSeg, e.meta.Parity.FileName))]
	probe.mu.Unlock()
	if !parityStaged {
		t.Fatalf("parity 分块未经暂存复用上传（staged 集合缺 %s）⇒ 内层会再落一份 staging ✗",
			e.meta.Parity.FileName)
	}
	if !mustReadEqual(t, sfs, ctx, "d/f.bin", payload) {
		t.Fatal("erasure 写回内容不一致")
	}
}

// TestWriteFile_DedupChunkUploadsViaStagedPath 钉住**dedup 分块上传点**同口径（docs/roadmap.md
// ①-附 §2 第 2 项）：去重池单分块此前 `os.ReadFile` 读进内存后以 reader 交内层（整块驻留 +
// 内层二次 staging）；现以本地分块文件为源并经 `WithStagedLocalPath` 交付。
//
// 变异验证：改回内存交付 ⇒ 必红。
func TestWriteFile_DedupChunkUploadsViaStagedPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	payload := bytes.Repeat([]byte("dedup-staged-"), 4*1024)
	sfs, probe := newStagedProbeFS(t, Options{Dedup: true})
	if err := sfs.WriteFile(ctx, syncpkg.LogicalKey{}, "d/f.bin", bytes.NewReader(payload), int64(len(payload)), 0); err != nil {
		t.Fatalf("WriteFile(dedup): %v", err)
	}
	probe.mu.Lock()
	nStaged := len(probe.staged)
	nMemory := len(probe.memory)
	probe.mu.Unlock()
	sfs.mu.RLock()
	e := sfs.index["d/f.bin"]
	sfs.mu.RUnlock()
	if e == nil || len(e.meta.Chunks) == 0 {
		t.Fatalf("夹具异常：无索引/无分块（%+v）", e)
	}
	chunkPath := filepath.ToSlash(filepath.Join(sfs.dataSeg(e), e.meta.Chunks[0].FileName))
	probe.mu.Lock()
	chunkStaged := probe.staged[chunkPath]
	probe.mu.Unlock()
	if !chunkStaged {
		t.Fatalf("去重内容分块未经暂存复用上传（staged 集合缺 %s；staged=%d memory=%d）⇒ 内层会再落一份 staging ✗",
			chunkPath, nStaged, nMemory)
	}
	if !mustReadEqual(t, sfs, ctx, "d/f.bin", payload) {
		t.Fatal("dedup 写回内容不一致")
	}
}
