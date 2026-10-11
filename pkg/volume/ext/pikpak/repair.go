// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// repair.go 提供「零尾扫描」与分片重建能力：
//
// 背景（2026-10-11 用户实测 3 次失败）：分片下载的落盘方式是
// `preallocate(Truncate 全尺寸) + 逐块 WriteAt`——预分配让文件**大小恒等于目标**，
// 因此「分片没写」表现为**块内尾部连续 0x00 的空洞**（稀疏文件/未写区），
// 文件大小与内容正确性完全解耦：
//   - 大小正确但块被跳过（manifest 记录与磁盘不一致）→ 播放到该处花屏/中断；
//   - 若把 .hybrid.downloading 当成品改名，就是静默损坏。
//
// 因此判定规则（用户明示）：**块内尾部存在连续 0x00（≥ 阈值）即视为未写完的异常块**，
// 需要重建。扫描只读每块尾部一个窗口（默认 1MiB），代价与块数成正比而非文件大小。
//
// 注意：零尾是**启发式**（视频/音频里合法的大段零也可能命中，如 PCM 静音段），
// 所以它只用于「定位候选」，最终正确性恒由源权威哈希（GCID）兜底：
// 假阳性最多多下一次分片，绝不会据此产出「校验通过但内容错」的文件。
package pikpak

import (
	"fmt"
	"os"
	"sync"
)

// 零尾扫描默认参数（ZeroScanOptions 零值即用）。
const (
	defaultZeroProbe = 1 << 20  // 每块探测窗口（从块尾向前读 1MiB）
	defaultZeroRun   = 64 << 10 // 尾部连续零 ≥ 64KiB 判定未写完
	defaultZeroWork  = 4        // 扫描并发
)

// ChunkSpan 是一个分片区间（offset + length）。
type ChunkSpan struct {
	Offset int64
	Length int64
}

// ZeroScanOptions 是零尾扫描参数（零值 = 默认）。
type ZeroScanOptions struct {
	TailProbe   int64 // 每块探测窗口（0 → 1MiB；恒 ≥ ZeroRun）
	ZeroRun     int64 // 尾部连续零判定阈值（0 → 64KiB）
	Concurrency int   // 并发度（0 → 4，上限 16）
}

// normalize 填充默认值并保证 TailProbe ≥ ZeroRun。
func (o ZeroScanOptions) normalize() ZeroScanOptions {
	out := o
	if out.ZeroRun <= 0 {
		out.ZeroRun = defaultZeroRun
	}
	if out.TailProbe < out.ZeroRun || out.TailProbe <= 0 {
		out.TailProbe = max(defaultZeroProbe, out.ZeroRun)
	}
	if out.Concurrency <= 0 {
		out.Concurrency = defaultZeroWork
	}
	if out.Concurrency > 16 {
		out.Concurrency = 16
	}
	return out
}

// ZeroTailSuspect 是「尾部为连续零」的可疑块（判定为未写完，需重建）。
type ZeroTailSuspect struct {
	Offset int64 // 块起始偏移
	Length int64 // 块长度
	Zeros  int64 // 尾部连续零长度（≥ 阈值）
}

// PlanChunkSpans 按 chunkSize 把 [0,total) 切成分片区间（供全文件扫描）。
func PlanChunkSpans(total, chunkSize int64) []ChunkSpan {
	if total <= 0 || chunkSize <= 0 {
		return nil
	}
	out := make([]ChunkSpan, 0, total/chunkSize+1)
	for off := int64(0); off < total; off += chunkSize {
		out = append(out, ChunkSpan{Offset: off, Length: min(chunkSize, total-off)})
	}
	return out
}

// ScanZeroTailSpans 扫描整个文件（按 chunkSize 分块），返回尾部连续零 ≥ ZeroRun 的可疑块。
func ScanZeroTailSpans(path string, total, chunkSize int64, opt ZeroScanOptions) ([]ZeroTailSuspect, error) {
	return ScanZeroTail(path, PlanChunkSpans(total, chunkSize), opt)
}

// ScanZeroTail 扫描指定分片区间，返回尾部连续零 ≥ ZeroRun 的可疑块。
// 文件不存在/小于分片区间上界 → 报错（调用方另行判定「缺失」，不在此误判为正常）。
func ScanZeroTail(path string, spans []ChunkSpan, opt ZeroScanOptions) ([]ZeroTailSuspect, error) {
	o := opt.normalize()
	if len(spans) == 0 {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	end := spans[0].Offset + spans[0].Length
	for _, s := range spans {
		end = max(end, s.Offset+s.Length)
	}
	if fi.Size() < end {
		return nil, fmt.Errorf("pikpak repair: file %s size %d < required %d", path, fi.Size(), end)
	}
	// 并发扫描（*os.File.ReadAt 并发安全；每块只读尾部一个窗口）。
	sem := make(chan struct{}, o.Concurrency)
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		suspect []ZeroTailSuspect
	)
	for _, span := range spans {
		if span.Length <= 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(span ChunkSpan) {
			defer wg.Done()
			defer func() { <-sem }()
			s, serr := scanOneChunkTail(f, span, o)
			if serr != nil || s == nil {
				return // 读取失败交由后续权威校验兜底（不误报可疑块）
			}
			mu.Lock()
			suspect = append(suspect, *s)
			mu.Unlock()
		}(span)
	}
	wg.Wait()
	return suspect, nil
}

// scanOneChunkTail 检测单块尾部连续零（≥ ZeroRun 时返回可疑块）。
func scanOneChunkTail(f *os.File, span ChunkSpan, o ZeroScanOptions) (*ZeroTailSuspect, error) {
	probe := min(o.TailProbe, span.Length)
	if probe <= 0 {
		return nil, nil
	}
	buf := make([]byte, probe)
	n, err := f.ReadAt(buf, span.Offset+span.Length-probe)
	if err != nil && n <= 0 {
		return nil, err
	}
	zeros := trailingZeros(buf[:n])
	if zeros < o.ZeroRun {
		return nil, nil
	}
	return &ZeroTailSuspect{Offset: span.Offset, Length: span.Length, Zeros: zeros}, nil
}

// trailingZeros 返回尾部连续 0x00 的长度（逐字节回退，遇到非零即停）。
func trailingZeros(b []byte) int64 {
	var n int64
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0 {
			break
		}
		n++
	}
	return n
}
