// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// defaultBlockSize 是块级增量同步的默认块大小（1 MiB）。
// 与分块上传 chunkSize 默认语义一致（分块基建的块校验和粒度），
// 复用其 SHA-256 校验和计算模式（blockChecksum 与 ChunkChecksums 同算法）。
const defaultBlockSize int64 = 1024 * 1024

// BlockDiff 对比 src 与 dst 的固定块内容，返回差异块索引列表。
//
// 语义（roadmap 4.3 P2 块级增量同步 v1 核心原语）：
//   - 按 blockSize 固定分块（最后一块可能不足大小），逐块算 SHA-256 比对；
//   - 返回 src 中与 dst 对应块内容不同的块索引（升序）；
//   - 目标端旧文件存在时才有收益（相同块跳过 → 只传差异块）；
//   - 目标为空（无旧文件）= 全量差异（零回归，与原全量复制行为一致）；
//   - src 与 dst 块数不同（尾部追加/截断）：多余块全部差异。
//
// 复用说明：块校验和用 SHA-256 hex（与 pkg/files chunked_store 的
// ChunkChecksums、pkg/client chunked 断点续传 MissingChunks 同算法）——
// 后续跨 FS 块级增量可无缝衔接分块上传基建（服务端按块表标 received）。
//
// blockSize <= 0 时用 defaultBlockSize。src/dst 为 nil 视为空（0 块）。
func BlockDiff(src, dst io.ReaderAt, blockSize int64) ([]int, error) {
	if blockSize <= 0 {
		blockSize = defaultBlockSize
	}
	srcSize, err := readerAtLen(src)
	if err != nil {
		return nil, fmt.Errorf("读取源长度: %w", err)
	}
	dstSize, err := readerAtLen(dst)
	if err != nil {
		return nil, fmt.Errorf("读取目标长度: %w", err)
	}
	srcBlocks := (srcSize + blockSize - 1) / blockSize
	dstBlocks := (dstSize + blockSize - 1) / blockSize
	if dstSize == 0 {
		dstBlocks = 0
	}

	var diffs []int
	for i := range srcBlocks {
		// 目标没有对应块（尾部追加）或块内容不同 → 差异。
		if i >= dstBlocks {
			diffs = append(diffs, int(i))
			continue
		}
		srcChk, err := readerAtBlockChecksum(src, i, blockSize, srcSize)
		if err != nil {
			return nil, err
		}
		dstChk, err := readerAtBlockChecksum(dst, i, blockSize, dstSize)
		if err != nil {
			return nil, err
		}
		if srcChk != dstChk {
			diffs = append(diffs, int(i))
		}
	}
	return diffs, nil
}

// readerAtLen 返回 ReaderAt 的总长度（最后一块的结尾偏移）。
// 通过逐步探测（1、2、4... 指数倍 + 二分）——io.ReaderAt 无标准 Len 接口，
// 探测到 ErrEOF 即边界。文件实现（*os.File）走 Stat 快路径。
func readerAtLen(r io.ReaderAt) (int64, error) {
	if r == nil {
		return 0, nil
	}
	// 快路径：实现 io.Seeker（*os.File 等）→ 尾部偏移。
	if s, ok := r.(io.Seeker); ok {
		cur, err := s.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		end, err := s.Seek(0, io.SeekEnd)
		if err != nil {
			return 0, err
		}
		if _, err := s.Seek(cur, io.SeekStart); err != nil {
			return 0, err
		}
		return end, nil
	}
	// 慢路径：指数探测 + 二分（针对纯 ReaderAt 实现）。
	var lo int64
	hi := int64(1)
	for {
		buf := make([]byte, 1)
		if _, err := r.ReadAt(buf, hi); err == io.EOF {
			break
		} else if err != nil {
			return 0, err
		}
		lo = hi
		hi *= 2
		if hi <= 0 { // 溢出保护（>2^63）
			hi = lo + 1
			break
		}
	}
	// 二分定位：lo 可读、hi 不可读。
	for lo+1 < hi {
		mid := lo + (hi-lo)/2
		buf := make([]byte, 1)
		if _, err := r.ReadAt(buf, mid); err == io.EOF {
			hi = mid
		} else if err != nil {
			return 0, err
		} else {
			lo = mid
		}
	}
	return lo + 1, nil
}

// readerAtBlockChecksum 计算 ReaderAt 第 blockIdx 块的 SHA-256 hex。
// 最后一块可能不足 blockSize（按实际长度读）。
func readerAtBlockChecksum(r io.ReaderAt, blockIdx, blockSize, total int64) (string, error) {
	offset := blockIdx * blockSize
	length := blockSize
	if offset+length > total {
		length = total - offset
	}
	if length <= 0 {
		return "", nil
	}
	buf := make([]byte, length)
	if _, err := r.ReadAt(buf, offset); err != nil {
		return "", fmt.Errorf("读取块 %d（offset=%d len=%d）: %w", blockIdx, offset, length, err)
	}
	return blockChecksum(buf), nil
}

// blockChecksum 计算一块数据的 SHA-256 hex。
// 与分块上传基建（pkg/files ChunkChecksums）同算法——块级增量的校验和
// 可与服务端已存块表直接比对，实现「只传差异块」的跨 FS 复用。
func blockChecksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
