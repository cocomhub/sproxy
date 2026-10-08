// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	//nolint:gosec // G505: GCID 是 PikPak 官方 hash 算法（sha1(concat(sha1(分块)))，算法定义
	// 必须用 SHA-1，非安全用途——复算权威 hash 需与官方一致，不可替换。
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// GCIDCandidates 是 PikPak GCID 复算的候选分块集合（字节），供外部（如 pikpak 下载器）
// 直接复用：256KB / 512KB / 1MB / 2MB / 4MB。
var GCIDCandidates = []int64{262144, 524288, 1048576, 2097152, 4194304}

// gcidDefaultCandidates 是 ComputeGCID 单分块模式下的默认分块大小（blockSize<=0 时用
// 最大候选 4MB——单分块复算语义，供外部已知分块的调用方）。
var gcidDefaultCandidates = GCIDCandidates

// ComputeGCID 计算 PikPak GCID：sha1(concat(sha1(每个 blockSize 分块)))。
// blockSize <= 0 时退回 gcidDefaultCandidates（取最大候选）；空数据返回空串（调用方
// 判 ok 前先验 size）。
func ComputeGCID(data []byte, blockSize int64) string {
	if blockSize <= 0 {
		blockSize = gcidDefaultCandidates[len(gcidDefaultCandidates)-1]
	}
	if len(data) == 0 {
		return ""
	}
	h := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	for off := int64(0); off < int64(len(data)); off += blockSize {
		end := min(off+blockSize, int64(len(data)))
		chunkHash := sha1.Sum(data[off:end]) //nolint:gosec // G401: GCID 算法必须 sha1
		h.Write(chunkHash[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// RecomputeGCID 对内存数据尝试候选分块集合复算 GCID：返回**首个整除候选**的命中值
// + ok（兼容测试/外部调用；文件流式用 RecomputeGCIDAll 取全候选）。
// RecomputeGCID 对内存数据尝试候选分块集合复算 GCID：返回**首个整除候选**的命中值
// + ok（兼容测试/外部调用；文件流式用 RecomputeGCIDAll 取全候选）。
// 注意：此处保留整除限制是「候选预筛」（与 RecomputeGCIDAll 尾块语义的差异）——调用方
// 应优先用 RecomputeGCIDAll（文件流式，尾块正确计入）。本函数为测试基准/小数据入口。
func RecomputeGCID(data []byte, candidates []int64) (string, bool) {
	for _, bs := range candidates {
		if bs <= 0 || len(data) == 0 || int64(len(data))%bs != 0 {
			continue
		}
		if g, ok := indexedBlockGCID(data, bs); ok {
			return g, true
		}
	}
	return "", false
}

// indexedBlockGCID 按固定分块复算 GCID（测试内部基准用，块大小校验）。
func indexedBlockGCID(data []byte, blockSize int64) (string, bool) {
	if blockSize <= 0 || len(data) == 0 {
		return "", false
	}
	h := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	for off := int64(0); off < int64(len(data)); off += blockSize {
		end := min(off+blockSize, int64(len(data)))
		chunkHash := sha1.Sum(data[off:end]) //nolint:gosec // G401: GCID 算法必须 sha1
		h.Write(chunkHash[:])
	}
	return hex.EncodeToString(h.Sum(nil)), true
}

// RecomputeGCIDFile 对文件流式复算 GCID（避免整文件读入内存，适配多 GB 视频）：
// 返回第一个整除候选的 GCID（兼容旧签名）+ ok；无候选整除 → ok=false。
func RecomputeGCIDFile(path string, candidates []int64) (string, bool, error) {
	all, err := RecomputeGCIDAll(path, candidates)
	if err != nil {
		return "", false, err
	}
	if len(all) == 0 {
		return "", false, nil
	}
	return all[0], true, nil
}

// RecomputeGCIDAll 返回**全部整除候选**的 GCID 值（R3-I1：官方分块粒度未知，调用方
// 逐一与官方 hash 比对，任一命中即权威——提升命中率，非首整除即返）。
// I/O 成本说明（对抗评审 R3-Minor + R2 尾块修正）：**每个候选都需全文件扫描一次**——
// 尾块修复后不再要求整除（真实文件几乎不可能整除，整除跳过使权威匹配恒失效），故
// 对**每个文件**恒 5× 全读（不再依赖「size 同时整除全部候选」）。PikPak 官方分块粒度
// 未实证（大文件非固定 256KB），全候选是正确性优先（小候选不命中不代表大候选不命中）；
// 视频下载后核对属低频路径，5×GB I/O 相对下载带宽可接受。需要严格省 I/O 时可改为
// 「逐候选算一比对一、命中即返」。
func RecomputeGCIDAll(path string, candidates []int64) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	var out []string
	for _, bs := range candidates {
		if bs <= 0 {
			continue
		}
		// 尾块语义与内存版 ComputeGCID 对齐（end=min，余量尾块参与计算）——不再要求
		// size%bs==0（真实文件尺寸几乎不可能恰为候选整数倍，整除限制使权威复算对多数
		// 文件失效，恒回落 ModeLocalOnly，权威匹配形同虚设）。官方 GCID 对小文件按
		// 256KB 分块，余量尾块必须计入。
		outer := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		block := make([]byte, bs)
		inner := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
		for off := int64(0); off < size; off += bs {
			end := min(off+bs, size)
			n, rerr := f.ReadAt(block[:end-off], off)
			if rerr != nil && rerr != io.EOF {
				return nil, rerr
			}
			// R7-F1：短读（Stat 与 Read 间被并发截断/IO 异常）时只写实际读到的 n 字节
			// （block 尾部残留旧数据/零填充会产出脏 GCID——命中率极低但属正确性隐患）。
			// 尾块（EOF）也是合法输入：n 即尾块实际长度，参与计算（不再有"EOF 后继续
			// 空块累积 sha1('')"的问题——循环按 off<size 推进，尾块即最后一块）。
			inner.Reset()
			inner.Write(block[:n])
			outer.Write(inner.Sum(nil))
		}
		out = append(out, hex.EncodeToString(outer.Sum(nil)))
	}
	return out, nil
}

// GCIDBlockedResult 是「分块大小 → GCID」对（pikget hash 标注块大小用）。
type GCIDBlockedResult struct {
	Block int64  // 分块大小（字节）
	GCID  string // 该分块复算的 GCID
}

// RecomputeGCIDBlocked 返回全候选（按 GCIDCandidates 顺序）的 (块大小, GCID) 列表，
// 供人工校验工具标注每个 GCID 对应的分块大小（如 256KiB / 512KiB / 1MiB）。
func RecomputeGCIDBlocked(path string) ([]GCIDBlockedResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	out := make([]GCIDBlockedResult, 0, len(GCIDCandidates))
	for _, bs := range GCIDCandidates {
		gcid, gerr := computeGCIDFile(path, f, bs, size)
		if gerr != nil {
			return nil, gerr
		}
		out = append(out, GCIDBlockedResult{Block: bs, GCID: gcid})
	}
	return out, nil
}

// RecomputeGCIDBlock 对文件按指定分块大小复算 GCID（pikget hash --block 用）。
func RecomputeGCIDBlock(path string, block int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := fi.Size()
	if size == 0 || block <= 0 {
		return "", nil
	}
	return computeGCIDFile(path, f, block, size)
}

// RecomputeGCIDBlockedRecommended 按【文件大小推荐候选】复算 GCID（大小引导，
// 同下载校验 RecomputeGCIDOrdered 的推荐规则）——pikget hash 默认输出。
func RecomputeGCIDBlockedRecommended(path string) ([]GCIDBlockedResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := fi.Size()
	if size == 0 {
		return nil, nil
	}
	rec := gcidRangeFor(size)
	out := make([]GCIDBlockedResult, 0, len(rec))
	for _, bs := range rec {
		gcid, gerr := computeGCIDFile(path, f, bs, size)
		if gerr != nil {
			return nil, gerr
		}
		out = append(out, GCIDBlockedResult{Block: bs, GCID: gcid})
	}
	return out, nil
}

// VerifyGCID 对文件复算 GCID 并与目标官方 hash 比对（通用校验入口）。
// 任一候选分块命中（官方分块粒度未知，全候选比对）→ 返回 (true, nil)；
// 全候选均未命中 → (false, nil)；复算失败（文件读错）→ (false, err)。
// **所有下载器（PikpakDownloader / HybridDownloader / 未来扩展）共用此入口**，
// 避免 GCID 算法/候选集合多处维护。
func VerifyGCID(path string, candidates []int64, targetHash string) (bool, error) {
	return VerifyGCIDStats(path, candidates, targetHash, nil)
}

// VerifyGCIDStats 按【文件大小引导候选 + 大→小计算】复算 GCID 比对（用户规则 2026-10-07）：
//   - 推荐候选（<256M→256K；256M~1G→512K/1M；1G~4G→1M/2M；>4G→4M）优先，从大到小；
//   - 范围内未命中 → 其余候选从大到小补齐；
//   - 命中即返；统计首次/二次/后续/未命中率 + 记录「实际命中≠首次」文件详情。
//
// stats 为 nil 时退化为全候选顺序（无统计）。
func VerifyGCIDStats(path string, candidates []int64, targetHash string, stats *GCIDVerifyStats) (bool, error) {
	if targetHash == "" || path == "" {
		return false, nil // 无权威 hash/无文件：无法校验
	}
	hitRound, _, _, err := RecomputeGCIDOrdered(path, targetHash, stats)
	if err != nil {
		return false, err
	}
	return hitRound > 0, nil
}

// gcidRange 是按文件大小推荐的候选分块区间（用户规则 2026-10-07）：
//
//	<256MiB   → [256KiB]
//	256MiB~1G → [512KiB, 1MiB]
//	1G~4G     → [1MiB, 2MiB]
//	>4G       → [4MiB]
//
// 范围内推荐不一致 → 从其余所有候选从大到小补齐（命中即返）。
var gcidRanges = []struct {
	minSize    int64
	candidates []int64
}{
	{minSize: 0, candidates: []int64{262144}},                  // <256MiB
	{minSize: 256 << 20, candidates: []int64{524288, 1048576}}, // 256M~1G
	{minSize: 1 << 30, candidates: []int64{1048576, 2097152}},  // 1G~4G
	{minSize: 4 << 30, candidates: []int64{4194304}},           // >4G
}

// gcidRangeFor 返回文件大小对应的推荐候选（按大小升序，计算时反转为大→小）。
func gcidRangeFor(size int64) []int64 {
	for _, gcidRange := range slices.Backward(gcidRanges) {
		if size >= gcidRange.minSize {
			return gcidRange.candidates
		}
	}
	return gcidRanges[0].candidates
}

// GCIDVerifyStats 是 GCID 权威校验命中统计（供决策：官方分块粒度规律）。
// 线程安全（原子计数）——多下载器并发复用。
type GCIDVerifyStats struct {
	// 命中轮次计数（round 1-5：候选最多 5 个）——用户明示 1-5 次命中都要统计。
	HitByRound    [6]atomic.Int64 // HitByRound[1..5] 第 N 轮命中；[0] 未用
	MissTotal     atomic.Int64    // 全候选最终未命中
	MismatchFirst atomic.Int64    // 实际命中 ≠ 首次推荐候选的文件数（需记录详情）
	// 详情缓冲（实际命中与首次不一致的文件）：大小/期望/最终（上限防膨胀）
	detailsMu sync.Mutex
	details   []GCIDMismatchDetail
}

// GCIDMismatchDetail 记录「实际命中与首次推荐候选不一致」的文件详情（供决策）。
type GCIDMismatchDetail struct {
	Size         int64  `json:"size"`
	FirstGCID    string `json:"first_gcid"`    // 首次推荐候选复算值（期望）
	MatchedGCID  string `json:"matched_gcid"`  // 最终命中候选复算值
	MatchedBlock int64  `json:"matched_block"` // 最终命中分块大小
}

// RecomputeGCIDOrdered 按【文件大小引导 + 大→小】顺序复算候选，返回 (gcid 列表, 命中轮次, 实际分块)。
// 推荐候选（gcidRangeFor）优先；范围内未命中 → 其余候选从大到小补齐。
// 返回命中轮次（0=未命中）与实际命中分块，供统计与详情记录。
func RecomputeGCIDOrdered(path string, targetHash string, stats *GCIDVerifyStats) (hitRound int, hitBlock int64, hitGCID string, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, "", err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return 0, 0, "", err
	}
	size := fi.Size()
	if size == 0 {
		return 0, 0, "", nil
	}
	// 候选顺序：推荐候选降序在前 + 其余候选降序在后（用户规则：推荐优先，范围内
	// 未命中才补其余候选；各自内部从大到小）——命中即返，统计轮次。
	order := gcidOrderFor(size)
	round := 0
	firstGCID := ""
	for _, bs := range order {
		round++
		gcid, gerr := computeGCIDFile(path, f, bs, size)
		if gerr != nil {
			return 0, 0, "", gerr
		}
		if round == 1 {
			firstGCID = gcid
		}
		if targetHash != "" && strings.EqualFold(gcid, targetHash) {
			recordGCIDHit(stats, round, size, firstGCID, gcid, bs)
			return round, bs, gcid, nil
		}
	}
	// 全候选未命中
	if stats != nil {
		stats.MissTotal.Add(1)
	}
	return 0, 0, "", nil
}

// gcidOrderFor 返回文件大小对应的候选计算顺序（推荐降序 + rest 降序，命中即返优先）。
func gcidOrderFor(size int64) []int64 {
	recommended := gcidRangeFor(size)
	all := slices.Clone(GCIDCandidates)
	// 其余 = 全候选 - 推荐
	rest := make([]int64, 0, len(all))
	for _, c := range all {
		if !slices.Contains(recommended, c) {
			rest = append(rest, c)
		}
	}
	// 各自降序（不全局 sort：推荐优先，rest 大块不插到推荐前）
	recommendedDesc := slices.Clone(recommended)
	sort.SliceStable(recommendedDesc, func(i, j int) bool { return recommendedDesc[i] > recommendedDesc[j] })
	sort.SliceStable(rest, func(i, j int) bool { return rest[i] > rest[j] })
	return append(recommendedDesc, rest...)
}

// recordGCIDHit 记录命中轮次统计 + 不一致详情（实际命中 ≠ 首次推荐候选）。
func recordGCIDHit(stats *GCIDVerifyStats, round int, size int64, firstGCID, gcid string, bs int64) {
	if stats == nil {
		return
	}
	if round >= 1 && round <= 5 {
		stats.HitByRound[round].Add(1)
	}
	if round > 1 && firstGCID != "" && !strings.EqualFold(firstGCID, gcid) {
		stats.MismatchFirst.Add(1)
		stats.detailsMu.Lock()
		if len(stats.details) < 1000 {
			stats.details = append(stats.details, GCIDMismatchDetail{
				Size: size, FirstGCID: firstGCID, MatchedGCID: gcid, MatchedBlock: bs,
			})
		}
		stats.detailsMu.Unlock()
	}
}

// RecomputeGCIDBlock 对文件按指定分块大小复算 GCID（pikget hash --block 用）。
func computeGCIDFile(path string, f *os.File, bs, size int64) (string, error) {
	outer := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	block := make([]byte, bs)
	inner := sha1.New() //nolint:gosec // G401: GCID 算法必须 sha1（官方定义）
	for off := int64(0); off < size; off += bs {
		end := min(off+bs, size)
		n, rerr := f.ReadAt(block[:end-off], off)
		if rerr != nil && rerr != io.EOF {
			return "", rerr
		}
		inner.Reset()
		inner.Write(block[:n])
		outer.Write(inner.Sum(nil))
	}
	return hex.EncodeToString(outer.Sum(nil)), nil
}

// GCIDStatsSnapshot 返回统计快照（供导出/日志）。
func (s *GCIDVerifyStats) GCIDStatsSnapshot() map[string]int64 {
	if s == nil {
		return nil
	}
	out := map[string]int64{
		"miss_total":     s.MissTotal.Load(),
		"mismatch_first": s.MismatchFirst.Load(),
	}
	for r := 1; r <= 5; r++ {
		out[fmt.Sprintf("round%d_hit", r)] = s.HitByRound[r].Load()
	}
	return out
}

// GCIDMismatchDetails 返回不一致详情（供审计）。
func (s *GCIDVerifyStats) GCIDMismatchDetails() []GCIDMismatchDetail {
	if s == nil {
		return nil
	}
	s.detailsMu.Lock()
	defer s.detailsMu.Unlock()
	return append([]GCIDMismatchDetail{}, s.details...)
}
