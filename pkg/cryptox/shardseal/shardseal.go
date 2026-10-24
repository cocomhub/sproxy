// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package shardseal 实现随机分块 + AES-256-GCM + scrypt 派生的自描述加密分块算法
// （设计 docs/designs/2026-10-01-secret-volume.md）。对外 API：EncryptShards /
// DecryptFile；命名含三段 16hex 校验和截断 + 随机段，meta 含全 stat + 每分块 stat。
package shardseal

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBlockPolicy 返回**本地卷**默认分块策略（设计 §4.1：1MB-200MB 随机；blocklet 细分
// 64KB-4MB fixed——块内定长 blocklet，支持视频关键帧随机访问只下载/解密目标段）。
//
// 本地卷取 4MiB blocklet：page cache + 零网络开销，大段减少请求数、meta 更小。
func DefaultBlockPolicy() BlockPolicy {
	return BlockPolicy{
		Mode: "random", Min: 1 << 20, Max: 200 << 20,
		BlockletMode: "fixed", BlockletMin: 64 << 10, BlockletMax: 4 << 20,
	}
}

// externalBlockletMax 是**外部/网盘卷**默认 blocklet 上限（2026-10-21 用户裁定）：256KiB。
// 权衡（实测网盘上行 ~1MB/s、下行 ~0.5MB/s、GCM 3.5GB/s）：
//   - blocklet **不影响上传**（上传单位是「块」= 1MB-200MB random ⇒ 3GB 恒 30 块），
//     只影响「随机读一次下载的密文量」与 meta 大小；
//   - 64KiB 随机读最快（range 1.37s vs 4MiB 的 10.8s），但 3GB meta 7.1MiB（挂载偏慢）；
//   - 256KiB：单段 256KiB → 网盘 ~0.5s/段（首帧/seek 足够流畅）、3GB meta 仅 1.8MiB、
//     解密 ~0.07ms（可忽略）——最平衡。低带宽 + 强 seek 场景可人工降 64KiB。
//
// 本地卷仍用 DefaultBlockPolicy（4MiB）。
const externalBlockletMax = 256 << 10

// ExternalDefaultBlockPolicy 返回**外部/网盘卷**默认分块策略：块级仍 1MB-200MB random，
// blocklet 上限降为 256KiB（详见 externalBlockletMax 注释）。
// 用户显式 `extra.block_policy` 优先，不受本默认影响。
func ExternalDefaultBlockPolicy() BlockPolicy {
	bp := DefaultBlockPolicy()
	bp.BlockletMax = externalBlockletMax
	return bp
}

// Planner 由 BlockPolicy 构造双层规划器（块 BlockPlanner + 块内 blocklet 规划器）。
// 块模式 "random"/"" → RandomPlanner；blocklet 模式 "fixed"/"" → FixedBlockletPlanner，
// "video-keyframe" 预留（未实现）。任一模式未知/预留 → 返回 (nil, nil)，调用方 fail-closed。
func (p BlockPolicy) Planner() (BlockPlanner, BlockletPlanner) {
	bp := p.blockPlanner()
	lp := p.blockletPlanner()
	if bp == nil || lp == nil {
		return nil, nil
	}
	return bp, lp
}

// blockPlanner 构造块规划器（仅 random/空；未知 fail-closed nil）。
func (p BlockPolicy) blockPlanner() BlockPlanner {
	if p.Mode == "" || p.Mode == "random" {
		minSize, maxSize := p.Min, p.Max
		if minSize <= 0 {
			minSize = 1
		}
		if maxSize < minSize {
			maxSize = minSize
		}
		return &RandomPlanner{Min: minSize, Max: maxSize}
	}
	return nil
}

// blockletPlanner 构造 blocklet 规划器（BlockletModeFixed/空 默认；BlockletModeVideoKeyframe
// 经 BlockPolicy 内已注入的 Indexer 构造；未知 fail-closed）。默认区间 64KB-4MB，
// 若未配置则以默认补齐。
func (p BlockPolicy) blockletPlanner() BlockletPlanner {
	mode := p.BlockletMode
	if mode == "" {
		mode = BlockletModeFixed
	}
	switch mode {
	case BlockletModeFixed:
		mn, mx := p.BlockletMin, p.BlockletMax
		if mn <= 0 {
			mn = 64 << 10
		}
		if mx < mn {
			mx = mn
		}
		return &FixedBlockletPlanner{Min: mn, Max: mx}
	case BlockletModeVideoKeyframe:
		// Indexer 由装配层（secretdata 写路径）按文件类型经 ResolveBlockletMode 注入
		// policy；未注入（无 keyframe 提供者注册）→ fail-closed nil（调用方报错）。
		if p.Indexer == nil {
			return nil
		}
		return NewVideoKeyframeBlockletPlanner(p.BlockletMin, p.BlockletMax, p.Indexer, p.Fallback...)
	default:
		return nil
	}
}

// EncryptShards 把本地文件加密为分块 + meta，返回分块与 meta 信息。
// srcFile：原始文件路径；outDir：加密分块输出目录；secret：密钥；policy：分块策略；
// padTarget：meta 加密 padding 目标（整块落盘总长，0=不 padding；secretdata 卷传
// min_block_size 附近值）；v：算法版本（写路径由装配层按 Options.Algorithm 解析，
// 不明文进 blob、仅经 KDF 派生域影响 key）。流程：读全文件 → 分块 → 每块按 blocklet
// 细分加密（块 blob：boot+段序列+index，见 crypto.go）→ 写分块文件 → 生成 meta（全
// stat + 每块 stat + blocklet 索引）→ meta 明文整体加密到 padTarget 并落盘 → 返回包含
// 最终 MetaBlob 的产物。磁盘上不出现明文 meta JSON（含文件名/size/sha256）），meta 名
// 三段真实补齐并锚定最终 blob（首段=明文哈希，中段=总校验和，末段=MetaBlob 哈希）。
func EncryptShards(srcFile, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	src, err := os.Open(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 打开源文件 %s 失败: %w", srcFile, err)
	}
	defer src.Close()

	data, err := io.ReadAll(src)
	if err != nil {
		return nil, fmt.Errorf("shardseal: 读源文件失败: %w", err)
	}
	st, err := os.Stat(srcFile)
	if err != nil {
		return nil, fmt.Errorf("shardseal: stat 源文件失败: %w", err)
	}
	return encryptShards(data, srcMeta{name: filepath.Base(srcFile), size: st.Size(), mtime: st.ModTime(), mode: uint32(st.Mode().Perm()), path: srcFile}, blobDest{outDir: outDir}, secret, policy, padTarget, v)
}

// EncryptShardsBytes 把已读入内存的明文加密为分块 + meta（EncryptShards 的内存变体）。
// 供 secretdata 写路径直接传入已持有的明文（避免「写临时源 → 再 io.ReadAll」的二次整读，
// 峰值从 2× 文件降到 1×；Imp-2 部分修复）。origName 为逻辑文件名（meta.original.name，
// 与临时源变体行为一致——临时源以 sanitizeName 命名、EncryptShards 取 filepath.Base）。
// 语义与 EncryptShards 相同（统一落盘格式、meta 名三段锚定最终 blob）。
func EncryptShardsBytes(data []byte, origName, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	return encryptShards(data, srcMeta{name: origName, size: int64(len(data)), mtime: time.Now(), mode: 0o600}, blobDest{outDir: outDir}, secret, policy, padTarget, v)
}

// ChunkSink 是**单个数据分块 blob 的落点**（EncryptShardsReaderToChunks 参数）：每产出一块即调用一次，
// name 为该块最终 blob 名（三段哈希命名，与落盘变体逐字节一致）、blob 为该块密文字节。
// 生命周期：blob 仅在调用期间有效——sink 若要保留（如异步上传）须自行拷贝。
//
// 用途（块级交错，docs/roadmap.md ①-附）：调用方可在收到每块时立即上传并丢弃本地副本
// ⇒ 本地临时峰值从 O(整个文件) 降到 **O(1 块)**；分块命名/布局/meta 与落盘变体完全一致。
type ChunkSink func(name string, blob []byte) error

// blobDest 是加密产物的落点（两条形态，同一实现）：
//   - outDir 非空（EncryptShards/Bytes/Reader）：分块与 meta 都写入该目录（既有行为，逐字节不变）；
//   - sinkMode（EncryptShardsReaderToChunks）：**数据分块**逐块交给 chunkSink（不落盘；chunkSink 为 nil
//     时只取 meta 信息的调用方 ⇒ 分块也不落盘），meta **不落盘**——它的密文以
//     `EncryptionResult.MetaBlob`（+ MetaName）返回给调用方（加密卷写路径本就在内存持有 meta
//     再自行上传，meta 的临时落盘是冗余 I/O）。
type blobDest struct {
	outDir    string
	chunkSink ChunkSink
	sinkMode  bool
	// skipMeta（仅 sink 形态使用）报告调用方**自行**按最终 meta 明文加密落盘（其 meta 归属
	// 字段如 mtime/BaseVersion 在加密期间才定型）⇒ 本实现不加密 meta（MetaBlob/MetaName 留空），
	// 消除「先加密一份、再按变更后的字段重加密」的冗余计算（见 EncryptShardsReaderToChunks）。
	skipMeta bool
}

// writeChunk 落一个数据分块（sink 形态 = 交调用方/丢弃，落盘形态 = 写 outDir）。
func (d blobDest) writeChunk(name string, blob []byte) error {
	if d.sinkMode {
		if d.chunkSink == nil {
			return nil // 只取 meta 信息的调用方：分块不落盘也不交付
		}
		return d.chunkSink(name, blob)
	}
	return writeBlobFile(d.outDir, name, blob)
}

// writeMeta 落 meta blob（sink 形态 = 不落盘，经 EncryptionResult.MetaBlob 返回；
// skipMeta 形态 = 完全跳过加密，由调用方自行产出）。
func (d blobDest) writeMeta(name string, blob []byte) error {
	if d.sinkMode {
		return nil
	}
	return writeBlobFile(d.outDir, name, blob)
}

// writeBlobFile 把 blob 写为 outDir/name（0600）。
func writeBlobFile(outDir, name string, blob []byte) error {
	if err := os.WriteFile(filepath.Join(outDir, name), blob, 0o600); err != nil {
		return fmt.Errorf("shardseal: 写 %s 失败: %w", name, err)
	}
	return nil
}

// EncryptShardsReader 流式分块加密：src 提供整文件随机读（可 seek），逐块 ReadAt + 流式哈希，
// 内存峰值 O(单块上限)。磁盘格式、meta 与块布局与 EncryptShards/EncryptShardsBytes 完全一致
// （向后兼容，旧读路径可读新写）。
// src 须实现 io.Seeker / io.ReaderAt（multipart.File、*os.File、SectionReader 等）；
// 纯流不可 seek 时回退 io.ReadAll（功能不变，仅内存不省）。
func EncryptShardsReader(src io.Reader, size int64, origName, outDir string, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	return encryptShardsReaderTo(src, size, origName, blobDest{outDir: outDir}, secret, policy, padTarget, v)
}

// EncryptShardsReaderToChunks 与 EncryptShardsReader 同构，但**数据分块 blob 不落盘**（逐块交给
// sink，见 ChunkSink）且**不加密 meta**（MetaBlob/MetaName 留空，故不接收 padTarget）：供
// 「meta 归属字段（如 mtime / 乐观锁版本）在加密期间才定型」的调用方
// **自行按最终 meta 明文加密落盘**——否则内部先加密一份（当时字段未定型）会被丢弃、调用方再加密
// 一次，白做一遍 AES+JSON 序列化（P2 性能修复，2026-10-19）。数据分块的命名/密文字节与其它变体
// **逐字节一致**（同一实现）。padTarget 由调用方自行加密 meta 时决定，故本入口不接收它。
func EncryptShardsReaderToChunks(src io.Reader, size int64, origName string, secret []byte, policy BlockPolicy, v AlgoVersion, sink ChunkSink) (*EncryptionResult, error) {
	return encryptShardsReaderTo(src, size, origName, blobDest{chunkSink: sink, sinkMode: true, skipMeta: true}, secret, policy, 0, v)
}

// encryptShardsReaderTo 是流式两个公开入口的共享实现（落盘形态 / sink 形态同一代码路径）。
func encryptShardsReaderTo(src io.Reader, size int64, origName string, dest blobDest, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	ra, ok := src.(io.ReaderAt)
	if !ok {
		if s, ok2 := src.(io.Seeker); ok2 {
			ra = io.NewSectionReader(readSeekerFrom{s, src}, 0, size)
		} else {
			data, rerr := io.ReadAll(src)
			if rerr != nil {
				return nil, fmt.Errorf("shardseal: 读源失败: %w", rerr)
			}
			return encryptShards(data, srcMeta{name: origName, size: size, mtime: time.Now(), mode: 0o600}, dest, secret, policy, padTarget, v)
		}
	}
	return encryptShardsAt(ra, size, srcMeta{name: origName, size: size, mtime: time.Now(), mode: 0o600}, dest, secret, policy, padTarget, v)
}

// readSeekerFrom 把 io.Seeker 同时兼 io.Reader 的实现（如 multipart.File / *os.File
// 双实现；纯 Seeker 需补 Read）适配为 io.ReaderAt：ReadAt 经 Seek + ReadFull 顺序读。
type readSeekerFrom struct {
	s io.Seeker
	r io.Reader
}

func (r readSeekerFrom) Read(p []byte) (int, error) { return r.r.Read(p) }
func (r readSeekerFrom) ReadAt(p []byte, off int64) (int, error) {
	if _, err := r.s.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	n, err := io.ReadFull(r.r, p)
	return n, err
}

// srcMeta 是一次加密的源文件逻辑元数据（EncryptShards 文件变体取 os.Stat、内存变体取
// len/调用方；S107 收敛：origName/size/mtime/mode 四个来源参数归组）。
type srcMeta struct {
	name  string
	size  int64
	mtime time.Time
	mode  uint32
	// path 是源文件真实路径（仅 EncryptShards 文件变体提供；内存变体为空）。关键帧
	// 解析（ffprobe）有真实路径走文件路径模式（可 seek 最优），无则 stdin 流（接受
	// 不可 seek 降级——绝不落临时文件复制数据）。
	path string
}

// encryptShards 是 EncryptShards 系列的核心（共享实现，控制认知复杂度 #727）。
// data 为整文件明文；src 为逻辑元数据（来源变体提供：文件变体取 os.Stat，内存变体取
// len/调用方）。
func encryptShards(data []byte, src srcMeta, dest blobDest, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	blocksPlanner, blockletsPlanner := policy.Planner()
	if blocksPlanner == nil || blockletsPlanner == nil {
		return nil, fmt.Errorf("shardseal: 未知分块策略 %q 或 blocklet 模式 %q", policy.Mode, policy.BlockletMode)
	}
	blocks, err := blocksPlanner.Plan(int64(len(data)))
	if err != nil {
		return nil, err
	}

	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	key, err := DeriveKey(secret, salt, v)
	if err != nil {
		return nil, err
	}

	// 分组盲签：同一原始文件的全部分块/meta 共享（中段），供无 meta 时盲分组恢复；
	// 基于 HMAC(secret, 整文件SHA256)，无 secret 无法反推明文或探测内容存在性。
	fullSum := sha256.Sum256(data)
	group := GroupSig(secret, fullSum[:])

	res := &EncryptionResult{Meta: &Meta{
		Version:     metaVersion,
		Algorithm:   algorithmName(v),
		AlgoVersion: v,
		KDF:         "scrypt",
		Salt:        toBase64(salt),
		Original: OriginalInfo{
			Name:      src.name,
			Size:      src.size,
			SHA256:    Hash256(data),
			MTime:     src.mtime.UTC().Format(time.RFC3339Nano),
			Mode:      src.mode,
			MediaType: mediaType(src.name),
		},
		Block: policy,
	}}

	names, chunkInfos, cerr := encryptWriteChunks(&chunkPlan{blocks: blocks, blp: blockletsPlanner, srcPath: src.path}, data, key, salt, group, dest, v)
	if cerr != nil {
		return nil, cerr
	}
	res.ChunkNames = names
	res.Meta.Chunks = chunkInfos
	// **fMP4/关键帧解析失败的文件名日志（2026-10-04）**：解析器（indexer）层无文件名，
	// 此处（有 src.name）在 planner 有失败记录时补一条带文件名的 warn——metric 已精确统计
	// fMP4 计数（sproxy_keyframe_fmp4_total），日志定位具体文件供后续人工确认（两者互补：
	// 计数看趋势、日志看明细）。
	if rep, ok := blockletsPlanner.(failureReporter); ok {
		if fails := rep.Failures(); len(fails) > 0 {
			slog.Warn("shardseal: 视频关键帧解析失败，blocklet 降级 fixed（fMP4/截断等）",
				"file", src.name, "size", src.size, "failures", len(fails))
		}
	}

	metaName, merr := encryptWriteMeta(res, key, salt, group, dest, padTarget)
	if merr != nil {
		return nil, merr
	}
	res.MetaName = metaName
	return res, nil
}

// encryptShardsAt 流式分块加密（EncryptShardsReader 的核心）：src 提供整文件随机读。
// 先一次流式哈希（io.Copy，O(1) 内存）得整文件 SHA256+组签，再逐块 ReadAt 加密写盘，
// 内存峰值 O(单块上限)。磁盘格式、meta 与块布局/命名与 encryptShards 完全一致（向后兼容）。
func encryptShardsAt(src io.ReaderAt, size int64, srcMetaV srcMeta, dest blobDest, secret []byte, policy BlockPolicy, padTarget int, v AlgoVersion) (*EncryptionResult, error) {
	blocksPlanner, blockletsPlanner := policy.Planner()
	if blocksPlanner == nil || blockletsPlanner == nil {
		return nil, fmt.Errorf("shardseal: 未知分块策略 %q 或 blocklet 模式 %q", policy.Mode, policy.BlockletMode)
	}
	blocks, err := blocksPlanner.Plan(size)
	if err != nil {
		return nil, err
	}

	salt, err := newSalt()
	if err != nil {
		return nil, err
	}
	key, err := DeriveKey(secret, salt, v)
	if err != nil {
		return nil, err
	}

	// 流式哈希 pass：整文件一遍，O(1) 内存，得 full SHA256 hash（group+Original.SHA256 用它）。
	fullHasher := sha256.New()
	if _, cerr := io.CopyBuffer(fullHasher, io.NewSectionReader(src, 0, size), make([]byte, 128<<10)); cerr != nil {
		return nil, fmt.Errorf("shardseal: 流式哈希读源失败: %w", cerr)
	}
	fullSum := fullHasher.Sum(nil)
	group := GroupSig(secret, fullSum)

	res := &EncryptionResult{Meta: &Meta{
		Version:     metaVersion,
		Algorithm:   algorithmName(v),
		AlgoVersion: v,
		KDF:         "scrypt",
		Salt:        toBase64(salt),
		Original: OriginalInfo{
			Name: srcMetaV.name,
			Size: srcMetaV.size,
			// fullSum 已是整文件 SHA-256（流式哈希），直接 hex 编码——不可再经 Hash256
			// 二次哈希（sha256(sha256(content)) 会与内容哈希不一致，DecryptFile 完整性校验失败）。
			SHA256:    hex.EncodeToString(fullSum),
			MTime:     srcMetaV.mtime.UTC().Format(time.RFC3339Nano),
			Mode:      srcMetaV.mode,
			MediaType: mediaType(srcMetaV.name),
		},
		Block: policy,
	}}

	names, chunkInfos, cerr := encryptWriteChunksAt(&chunkPlan{blocks: blocks, blp: blockletsPlanner}, &sectionReader{ra: src, sz: size}, key, salt, group, dest, v)
	if cerr != nil {
		return nil, cerr
	}
	res.ChunkNames = names
	res.Meta.Chunks = chunkInfos
	if rep, ok := blockletsPlanner.(failureReporter); ok {
		if fails := rep.Failures(); len(fails) > 0 {
			slog.Warn("shardseal: 视频关键帧解析失败，blocklet 降级 fixed（fMP4/截断等）",
				"file", srcMetaV.name, "size", srcMetaV.size, "failures", len(fails))
		}
	}
	metaName, merr := encryptWriteMeta(res, key, salt, group, dest, padTarget)
	if merr != nil {
		return nil, merr
	}
	res.MetaName = metaName
	return res, nil
}

// sectionReader 是 io.ReaderAt + io.Reader 的适配（encryptWriteChunksAt 需要既能按偏移
// 随机读（分块 ReadAt）又能作为 io.Reader 交给 PlanBlocklets/加密；*os.File/multipart.File
// 原生双实现，纯 ReaderAt 经此补 Read 顺序读）。ReadAt 透传底层；Read 从当前偏移顺序读。
type sectionReader struct {
	ra  io.ReaderAt
	off int64
	sz  int64
}

func (s *sectionReader) ReadAt(p []byte, off int64) (int, error) { return s.ra.ReadAt(p, off) }

func (s *sectionReader) Read(p []byte) (int, error) {
	n, err := s.ra.ReadAt(p, s.off)
	s.off += int64(n)
	return n, err
}

// encryptWriteChunksAt 流式逐块加密（encryptShardsAt 的分块循环）：每块从 src ReadAt 读入
// （内存 O(块上限)），加密写盘；块布局/命名/元数据与 encryptWriteChunks 一致（group 已由
// 预扫描哈希给定，blob 名与 encryptShards 相同）。src 为 io.ReaderAt（已由 EncryptShardsReader
// 归一为 ReaderAt）；PlanBlocklets 收到 *sectionReader 同现双能力（Reader+ReaderAt）。
// video-keyframe：本路径**支持** keyframe 规划——parseOnce 接收 io.ReaderAt（下方把
// *sectionReader 交给 PlanBlocklets），经 ReaderAt 扫描关键帧表即可，与逐块加密兼容；
// 上层**无** ReadAll 回退分支（见 shardseal_stream_test.go:TestEncryptShardsReader_VideoKeyframeParity
// 与 docs/secret-volume.md §9.1）。

// windowPlanFor 为块密文 enc 规划写侧窗口（首位相连 + salt 入首窗 + 每窗 sha256 checksum）。
// 规划失败（异常区间）⇒ 返回 nil（读侧动态规划兜底，零回归；不 fail-closed——窗口是
// 加速/完整性增强，不阻塞正常加密）。
func windowPlanFor(enc []byte, blocklets []BlockletInfo) ([]WindowInfo, error) {
	w := PlanChunkWindows(int64(len(enc)), blocklets, 4<<20)
	if w == nil {
		return nil, nil
	}
	if err := FillWindowChecksums(w, enc); err != nil {
		return nil, err
	}
	return w, nil
}

func encryptWriteChunksAt(plan *chunkPlan, src io.ReaderAt, key, salt []byte, group string, dest blobDest, v AlgoVersion) ([]string, []ChunkInfo, error) {
	var names []string
	var chunks []ChunkInfo
	for _, b := range plan.blocks {
		chunk := make([]byte, b.Size)
		if _, rerr := src.ReadAt(chunk, b.Offset); rerr != nil && rerr != io.EOF {
			return nil, nil, fmt.Errorf("shardseal: 读块 [%d,%d) 失败: %w", b.Offset, b.Size, rerr)
		}
		blocklets, perr := plan.blp.PlanBlocklets(&sectionReader{ra: src, sz: planSrcSize(plan)}, planSrcSize(plan), b.Offset, b.Size)
		if perr != nil {
			return nil, nil, perr
		}
		enc, entries, eerr := encryptBlocklets(key, salt, blocklets, chunk, v)
		if eerr != nil {
			return nil, nil, eerr
		}
		name, nerr := chunkBlobNameAndWrite(enc, group, dest)
		if nerr != nil {
			return nil, nil, nerr
		}
		names = append(names, name)
		ci := ChunkInfo{
			Index: len(chunks), FileName: name, Offset: b.Offset, OrigSize: int64(len(chunk)),
			OrigSHA256: Hash256(chunk), EncSize: int64(len(enc)), EncSHA256: Hash256(enc),
			Blocklets: blockletInfosOf(blocklets, entries, chunk, b.Offset),
		}
		ci.Windows, _ = windowPlanFor(enc, ci.Blocklets)
		if rep, ok := plan.blp.(failureReporter); ok {
			ci.Failures = rep.Failures()
		}
		chunks = append(chunks, ci)
	}
	return names, chunks, nil
}

// blockletInfosOf 从加密结果 entries 与块明文切片构建 blocklet 索引元数据（随机访问定位用）。
// 与 encryptWriteChunks 的 blocklet 索引逻辑一致（错误段不入索引——覆盖越界）。
func blockletInfosOf(blocklets []Blocklet, entries []BlobIndexEntry, chunk []byte, blockOffset int64) []BlockletInfo {
	var blInfos []BlockletInfo
	for i, bl := range blocklets {
		e := entries[i]
		if blockletType(bl) == BlockletTypeError {
			continue
		}
		segment := chunk[bl.Offset-blockOffset : bl.Offset-blockOffset+bl.Size]
		blInfos = append(blInfos, BlockletInfo{
			Offset: bl.Offset, Size: bl.Size, EncOffset: int64(e.EncOffset), EncSize: e.EncSize,
			OrigSHA256: Hash16(segment), Used: !bl.Padding, Type: byte(blockletType(bl)),
		})
	}
	return blInfos
}

// planSrcSize 返回 chunkPlan 覆盖的源文件长度（blocks 末块 Offset+Size）。
func planSrcSize(plan *chunkPlan) int64 {
	if n := len(plan.blocks); n > 0 {
		b := plan.blocks[n-1]
		return b.Offset + b.Size
	}
	return 0
}

// encryptWriteMeta 把 meta 明文 JSON 整体加密到 padTarget 落盘并返回 meta 文件名
// （EncryptShards 的 meta 处理，抽方法控制认知复杂度 #727 gocognit=15）。meta 明文 =
// [4B jsonLen][metaJSON][rand padding 到 padTarget]，整体加密为统一格式
// [R][8B 密文长][salt][nonce][ct+tag]。meta 名三段锚定最终 blob：首/末段 = 同一加密
// meta blob 的两窗口（offset 0/16）base62，中段 = HMAC 分组盲签——磁盘上不出现明文
// meta JSON，也不出现 meta 明文哈希（明文哈希可被内容存在性探测，匿名性收敛）。
func encryptWriteMeta(res *EncryptionResult, key, salt []byte, group string, dest blobDest, padTarget int) (string, error) {
	if dest.skipMeta {
		return "", nil // 调用方自行按最终 meta 明文加密（见 EncryptShardsReaderToChunks）
	}
	metaJSON, err := json.Marshal(res.Meta)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 序列化失败: %w", err)
	}
	metaBlob, err := EncryptMetaJSON(key, salt, metaJSON, padTarget)
	if err != nil {
		return "", fmt.Errorf("shardseal: meta 加密失败: %w", err)
	}
	res.MetaBlob = metaBlob
	// 新命名：首/末段 = 同一加密 meta blob 的不同窗口（offset 0/16）base62 编码，
	// 自包含可不解密验证密文完整；中段 = HMAC 分组盲签（与分块一致）。
	// 磁盘上不出现明文 meta JSON，也不出现 meta 明文哈希（明文哈希可被内容存在性探测）。
	metaA, metaB := Hash48Pair(metaBlob, 0, 16)
	metaName, nerr := MetaName(metaA, group, metaB)
	if nerr != nil {
		return "", nerr
	}
	if werr := dest.writeMeta(metaName, metaBlob); werr != nil {
		return "", werr
	}
	return metaName, nil
}

// chunkBlobNameAndWrite 计算加密分块 blob 名并写盘（encryptWriteChunks 循环体内步骤，
// 抽方法控制认知复杂度 #727 gocognit=15）。首尾段 = 同一加密 blob 的不同窗口
// （offset 0/16）base62 编码，中段 = HMAC 分组盲签；ChunkName 段长非法返回 error
// （不 panic，fail-closed）。
func chunkBlobNameAndWrite(enc []byte, group string, dest blobDest) (string, error) {
	encA, encB := Hash48Pair(enc, 0, 16) // 单次 SHA-256 取两窗口（大块省一次哈希）
	name, nerr := ChunkName(encA, group, encB)
	if nerr != nil {
		return "", nerr
	}
	if werr := dest.writeChunk(name, enc); werr != nil {
		return "", werr
	}
	return name, nil
}

// chunkPlan 是 encryptShards 的分块规划结果（块 + blocklet 规划器；S107 收敛：blocks/blp
// 两个规划参数归组）。由 policy.Planner() 一次性产出。
type chunkPlan struct {
	blocks []Block
	blp    BlockletPlanner
	// srcPath 是源文件真实路径（EncryptShards 文件变体提供；内存变体为空）。
	// video-keyframe 解析传 KeyframeRequest.Path——ffprobe 有路径走文件模式（可 seek）。
	srcPath string
}

// failureReporter 是可选接口：blocklet 规划器携带解析失败记录（video-keyframe 模式）。
// 非 nil 时失败记入每个 ChunkInfo.Failures（全密文 meta 内）。
type failureReporter interface {
	Failures() []BlockErrorMsg
}

// injectPlannerSrcPath 把 chunkPlan.srcPath 注入 video-keyframe planner（首个块前一次）；
// 关键帧解析（ffprobe）需真实路径走文件模式（可 seek 最优）。非 video-keyframe 规划器
// 为 no-op。
func injectPlannerSrcPath(plan *chunkPlan, blockOffset int64) {
	if blockOffset != 0 {
		return // 首个块前注入一次即可（parseOnce 在首块触发）
	}
	if vp, ok := plan.blp.(*VideoKeyframeBlockletPlanner); ok && vp.SrcPath == "" {
		vp.SrcPath = plan.srcPath
	}
}

// encryptWriteChunks 逐块加密并写盘，返回分块文件名与 ChunkInfo（EncryptShards 的
// 分块处理，抽方法控制认知复杂度 #727 gocognit=15）。Index 为 0 基顺序号；每块按
// blocklet 规划细分并记录 BlockletInfo（随机访问索引）。
func encryptWriteChunks(plan *chunkPlan, data []byte, key, salt []byte, group string, dest blobDest, v AlgoVersion) ([]string, []ChunkInfo, error) {
	var names []string
	var chunks []ChunkInfo
	for _, b := range plan.blocks {
		chunk := data[b.Offset : b.Offset+b.Size]
		injectPlannerSrcPath(plan, b.Offset)
		blocklets, perr := plan.blp.PlanBlocklets(bytes.NewReader(data), int64(len(data)), b.Offset, b.Size)
		if perr != nil {
			return nil, nil, perr
		}
		enc, entries, cerr := encryptBlocklets(key, salt, blocklets, chunk, v)
		if cerr != nil {
			return nil, nil, cerr
		}
		// 新命名：首尾段 = 同一加密 blob 的不同窗口（offset 0/16）base62 编码，无明文哈希外泄；
		// 中段 = HMAC 分组盲签（同文件共享）。注意 ChunkName 签名 (encA, group, encB)。
		name, nerr := chunkBlobNameAndWrite(enc, group, dest)
		if nerr != nil {
			return nil, nil, nerr
		}
		var blInfos []BlockletInfo
		for i, bl := range blocklets {
			e := entries[i] // 与 blocklets 同序
			if blockletType(bl) == BlockletTypeError {
				// 错误段：诊断信息留在 blob 密文内（全密文、GCM 认证），**不写入
				// meta.Blocklets**——validateChunkBlocklets 要求 blocklet 连续覆盖
				// [Offset, Offset+OrigSize)，错误段 Offset=块末会把覆盖推超界。
				// 失败列表经 planner.Failures() 记入 ChunkInfo.Failures（见下）。
				continue
			}
			segment := chunk[bl.Offset-b.Offset : bl.Offset-b.Offset+bl.Size]
			segHex := Hash16(segment)
			blInfos = append(blInfos, BlockletInfo{
				Offset:     bl.Offset,
				Size:       bl.Size,
				EncOffset:  int64(e.EncOffset),
				EncSize:    e.EncSize,
				OrigSHA256: segHex,
				Used:       !bl.Padding,
				Type:       byte(blockletType(bl)),
			})
		}
		names = append(names, name)
		ci := ChunkInfo{
			Index:      len(chunks),
			FileName:   name,
			Offset:     b.Offset,
			OrigSize:   int64(len(chunk)),
			OrigSHA256: Hash256(chunk), // 完整 64 hex（256-bit，审计/校验强度），不复用 48bit 截断
			EncSize:    int64(len(enc)),
			EncSHA256:  Hash256(enc),
			Blocklets:  blInfos,
		}
		ci.Windows, _ = windowPlanFor(enc, ci.Blocklets)
		// 解析失败信息是**文件级**共享的（planner 生命周期内失败记录，非单块专属）；因
		// 任一块的错误段 JSON 都含全部失败，meta 选择在**每个块**冗余记录同一列表——保证
		// 解密/审计任一块都能看到失败全貌（读取不依赖特定块）。语义注释见 ChunkInfo.Failures。
		if rep, ok := plan.blp.(failureReporter); ok {
			ci.Failures = rep.Failures()
		}
		chunks = append(chunks, ci)
	}
	return names, chunks, nil
}

// DecryptFile 用 meta + 分块还原原始文件到 dstFile。
// meta：meta 内容；chunkDir：分块所在目录；dstFile：还原目标；secret：密钥。
func DecryptFile(meta *Meta, chunkDir, dstFile string, secret []byte) error {
	// **唯一实现**（2026-10-18 架构级修复 Step 1）：本函数 = ChunkDecryptor + 「本地目录」来源 +
	// 「本地文件」目标。对外行为与错误语义（含「整文件校验失败 ⇒ 删残件」）保持不变；
	// 需要不落密文临时文件的调用方直接用 ChunkDecryptor 注入自己的来源。
	dec, err := NewChunkDecryptor(secret, meta)
	if err != nil {
		return err
	}
	mode := os.FileMode(meta.Original.Mode)
	if mode == 0 {
		mode = 0o644
	}
	f, err := os.OpenFile(dstFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("shardseal: 创建还原文件 %s 失败: %w", dstFile, err)
	}
	derr := dec.DecryptAllTo(f, func(ci ChunkInfo) ([]byte, error) {
		blob, rerr := os.ReadFile(filepath.Join(chunkDir, ci.FileName))
		if rerr != nil {
			return nil, fmt.Errorf("shardseal: 读分块 %s 失败: %w", ci.FileName, rerr)
		}
		return blob, nil
	})
	if derr != nil {
		f.Close()
		if errors.Is(derr, ErrIntegrityMismatch) {
			_ = os.Remove(dstFile) // 与旧行为一致：整文件校验失败删残件
		}
		return derr
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("shardseal: 关闭还原文件失败: %w", err)
	}
	return nil
}

// decodeSalt 解码 meta 的文件级盐（meta.Salt 为 base64）。空/非法即失败（fail-closed）。
func decodeSalt(meta *Meta) ([]byte, error) {
	if meta == nil || meta.Salt == "" {
		return nil, fmt.Errorf("shardseal: meta 缺少 salt")
	}
	salt, err := base64.StdEncoding.DecodeString(meta.Salt)
	if err != nil {
		return nil, fmt.Errorf("shardseal: meta salt 解码失败: %w", err)
	}
	if len(salt) != SaltLen {
		return nil, fmt.Errorf("shardseal: meta salt 长度 %d，应为 %d", len(salt), SaltLen)
	}
	return salt, nil
}

// MediaKindOf 依据文件名扩展名返回媒体**容器族**标识（blocklet 自动选型判据）。secretdata
// 写路径按此查注册表选型；**未注册的容器族 Kind 由注册表回落默认 fixed**（不「宣称支持实为
// 必败降级」）。
//
// **容器族口径（对抗评审 + 库选型评估定稿，2026-10-04）**：Kind 按容器族拆分（video/mp4、
// video/mkv、video/ts、video/avi…），注册表按容器族注册解析器（go-mp4→video/mp4、
// ffprobe 通配→video）。2026-10-21 扩展：补全 wmv/asf、rmvb/rm、flv/f4v、mpg/mpeg/vob/
// mod/dat、ogv、3gp/3g2、m4v、m2ts/mts、divx 等常见视频扩展名；同一容器族共享 Kind，
// 不逐扩展名分叉（同一文件容器由容器族唯一表示）。
func MediaKindOf(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	// ISO-BMFF：go-mp4 可解析（含 fMP4 降级）
	case ".mp4", ".mov", ".m4v":
		return "video/mp4"
	// EBML：无解析器注册 → 回落 fixed（未来 ebml-go）
	case ".mkv", ".webm":
		return "video/mkv"
	// MPEG-TS：无解析器注册 → 回落 fixed（未来 go-astits）
	case ".ts", ".m2ts", ".mts":
		return "video/ts"
	// RIFF：无解析器注册 → 回落 fixed（未来手写 RIFF）
	case ".avi", ".divx":
		return "video/avi"
	// ASF/WMV：ffprobe 通配命中时 video-keyframe
	case ".wmv", ".asf":
		return "video/asf"
	// RealMedia：ffprobe 通配命中时 video-keyframe
	case ".rmvb", ".rm":
		return "video/rm"
	// FLV：ffprobe 通配命中时 video-keyframe
	case ".flv", ".f4v":
		return "video/flv"
	// MPEG-PS（mpg/mpeg/vob/mod/dat）：ffprobe 通配命中时 video-keyframe
	case ".mpg", ".mpeg", ".vob", ".mod", ".dat":
		return "video/mpeg"
	// Ogg：ffprobe 通配命中时 video-keyframe
	case ".ogv":
		return "video/ogg"
	// 3GPP：ffprobe 通配命中时 video-keyframe
	case ".3gp", ".3g2":
		return "video/3gpp"
	default:
		return ""
	}
}

// mediaType 依据扩展名返回 media_type（meta 审计字段；未知返回空）。
func mediaType(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	switch ext {
	case ".mp4", ".mkv", ".webm", ".mov", ".avi":
		return "video" + ext
	case ".jpg", ".jpeg", ".png", ".gif", ".webp":
		return "image" + ext
	case ".mp3", ".flac", ".wav":
		return "audio" + ext
	}
	return ""
}
