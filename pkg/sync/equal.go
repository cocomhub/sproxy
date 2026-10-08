// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// equal.go 是通用文件比对工具（可信卷校验核心）：调用方提供**可信源**与**结果源**
// 的 FS + 路径，工具经 Stat 校验和（可用时）或流式比对（分段兜底）判定两文件是否
// 一致。**校验逻辑不落每个卷**——卷只提供读能力与 Stat 校验和，比对经此工具统一
// 完成（用户裁定：导出通用方法，每个卷无需各自实现校验）。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
)

// compareChunkSize 是流式分段比对的分段大小（无校验和可用时的比对粒度）。
const compareChunkSize = 1 << 20 // 1MiB

// Equal 判定 src.FS 中 srcPath 与 dst.FS 中 dstPath 内容一致。
//
// 判定顺序（用户裁定"优先用可用校验和"）：
//  1. 两侧 Stat 均提供同一算法的校验和（Checksums 交集，优先 sha256→md5→etag）→
//     比对校验和值（零流量，最快）；
//  2. 无共同校验和 → 流式分段比对（1MiB 段 sha256 逐步比，发现差异即返回 false；
//     全部一致才 true——不整文件缓存，大文件内存有界）。
//
// 任一侧 stat/read 失败 → 返回错误（fail-closed，不臆测一致）。文件不存在按错误返回。
func Equal(ctx context.Context, src FS, srcPath string, dst FS, dstPath string) (bool, error) {
	se, err := src.Stat(ctx, srcPath)
	if err != nil {
		return false, fmt.Errorf("equal: stat 源 %s: %w", srcPath, err)
	}
	if se == nil {
		return false, fmt.Errorf("equal: 源文件不存在: %s", srcPath)
	}
	de, err := dst.Stat(ctx, dstPath)
	if err != nil {
		return false, fmt.Errorf("equal: stat 目标 %s: %w", dstPath, err)
	}
	if de == nil {
		return false, fmt.Errorf("equal: 目标文件不存在: %s", dstPath)
	}
	if se.Size != de.Size {
		return false, nil // 尺寸不同必不一致
	}
	// 1. 共同校验和比对（Checksums 交集，优先 sha256）。
	if algo, sok, dok := commonChecksumAlgo(se, de); sok && dok {
		sw, swk := se.Checksums[algo]
		dw, dwk := de.Checksums[algo]
		if swk && dwk {
			return sw == dw, nil
		}
		// 传统单字段：Checksum + ChecksumType 匹配该算法时兜底。
		if se.Checksum != "" && se.ChecksumType == algo && de.Checksum != "" && de.ChecksumType == algo {
			return se.Checksum == de.Checksum, nil
		}
	}
	// 2. 流式分段比对（1MiB 段 sha256 逐步比，有界内存）。
	return streamEqual(ctx, src, srcPath, dst, dstPath, se.Size)
}

// commonChecksumAlgo 取两侧都已知的校验和算法（优先 sha256 > md5 > 其它）。
func commonChecksumAlgo(se, de *Entry) (algo string, srcHas, dstHas bool) {
	for _, a := range []string{"sha256", "md5"} {
		_, ok1 := se.Checksums[a]
		_, ok2 := de.Checksums[a]
		if ok1 && ok2 {
			return a, true, true
		}
	}
	return "", len(se.Checksums) > 0, len(de.Checksums) > 0
}

// streamEqual 流式分段比对：两文件逐 1MiB 段算 sha256 比对（读一段比一段，不等即 false）。
// 不整文件缓存；两 reader 独立打开，读进度对齐。
func streamEqual(ctx context.Context, src FS, srcPath string, dst FS, dstPath string, size int64) (bool, error) {
	rc, err := src.OpenRead(ctx, srcPath)
	if err != nil {
		return false, fmt.Errorf("equal: 打开源 %s: %w", srcPath, err)
	}
	defer rc.Close()
	dc, err := dst.OpenRead(ctx, dstPath)
	if err != nil {
		return false, fmt.Errorf("equal: 打开目标 %s: %w", dstPath, err)
	}
	defer dc.Close()
	sr := newSectionHasher(rc, compareChunkSize)
	dr := newSectionHasher(dc, compareChunkSize)
	for {
		equal, sErr, dErr := compareSections(sr, dr)
		switch {
		case sErr == nil && dErr == nil:
			if !equal {
				return false, nil
			}
		case sErr == io.EOF && dErr == io.EOF:
			return true, nil // 两流同时结束
		case sErr == io.EOF || dErr == io.EOF:
			return false, nil // 长度不一致（尺寸守卫兜底）
		default:
			return false, fmt.Errorf("equal: 流式比对失败: src=%v dst=%v", sErr, dErr)
		}
	}
}

// compareSections 比对两侧各一段；返回 (是否一致, srcErr, dstErr)。
func compareSections(sr, dr *sectionHasher) (bool, error, error) {
	sha, sErr := sr.next()
	dha, dErr := dr.next()
	if sErr != nil || dErr != nil {
		return false, sErr, dErr
	}
	return sha == dha, nil, nil
}

// sectionHasher 按固定分段读源并逐段产出 sha256（供两侧对齐比对）。
type sectionHasher struct {
	r     io.Reader
	chunk int64
}

func newSectionHasher(r io.Reader, chunk int64) *sectionHasher {
	return &sectionHasher{r: r, chunk: chunk}
}

// next 读下一段并返回其 sha256；EOF 时返回 (_, io.EOF)。段内不足 chunk 长度即文件尾。
func (s *sectionHasher) next() (string, error) {
	h := sha256.New()
	buf := make([]byte, 32<<10) // 段内读缓冲
	var got int64
	for got < s.chunk {
		n, err := s.r.Read(buf)
		if n > 0 {
			_, _ = h.Write(buf[:n])
			got += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				if got == 0 {
					return "", io.EOF
				}
				break // 末段（不足 chunk）已累计
			}
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
