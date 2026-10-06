// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// calculator.go 是 FileMeta 的**流式计算器**：读写封装时顺便计算整文件 sha256/md5
// + 按 ChunkSize 分块每块 sha256/md5（一次流式两遍哈希齐算，零额外 I/O 遍数）。

import (
	"crypto/md5" //nolint:gosec // 双算法中的 md5（baidupcs 等需要 md5 校验和），非安全用途
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// Calculator 流式累计 FileMeta：整文件 sha256+md5，按 ChunkSize 边界分块各算
// sha256+md5。Write/ReadFrom 驱动数据，Finish 产出完整 FileMeta。
type Calculator struct {
	size      int64 // 文件总大小（ChunkSizeForSize 依据 + Validate 覆盖）
	chunkSize int64
	totalSHA  hash.Hash
	totalMD5  hash.Hash
	// 当前分块累计（按 chunkSize 边界切分；每满一块产出 ChunkMeta 后重置）。
	curOffset int64
	curSize   int64
	curSHA    hash.Hash
	curMD5    hash.Hash
	chunks    []ChunkMeta
	done      bool // Finish 后禁止继续 Write（fail-closed）
}

// NewCalculator 构造计算器。size 为文件总大小（0/负 = 未知，分块大小取默认 1MiB，
// Validate 的覆盖校验由调用方按已知 size 检查）。chunkSize <= 0 时按 ChunkSizeForSize 自适应。
func NewCalculator(size, chunkSize int64) (*Calculator, error) {
	if chunkSize <= 0 {
		chunkSize = ChunkSizeForSize(size)
	}
	if chunkSize <= 0 {
		return nil, fmt.Errorf("meta: 分块大小非法 %d", chunkSize)
	}
	return &Calculator{
		size:      size,
		chunkSize: chunkSize,
		totalSHA:  sha256.New(),
		totalMD5:  md5.New(), //nolint:gosec // 双算法中的 md5（baidupcs 等需要 md5 校验和），非安全用途
		curSHA:    sha256.New(),
		curMD5:    md5.New(), //nolint:gosec // 同 totalMD5
	}, nil
}

// Write 累计一个数据块（流式；内部按 chunkSize 边界切分逐块哈希）。
func (c *Calculator) Write(p []byte) (int, error) {
	if c.done {
		return 0, fmt.Errorf("meta: Finish 后禁止继续 Write")
	}
	n := len(p)
	for len(p) > 0 {
		remaining := c.chunkSize - c.curSize // 当前分块剩余容量
		part := p
		if int64(len(p)) > remaining {
			part = p[:remaining]
		}
		_, _ = c.totalSHA.Write(part)
		_, _ = c.totalMD5.Write(part)
		_, _ = c.curSHA.Write(part)
		_, _ = c.curMD5.Write(part)
		c.curSize += int64(len(part))
		p = p[len(part):]
		if c.curSize == c.chunkSize {
			c.flushChunk()
		}
	}
	return n, nil
}

// flushChunk 收尾当前分块：产出 ChunkMeta 并重置（偏移前进到下一块起点）。
func (c *Calculator) flushChunk() {
	c.chunks = append(c.chunks, ChunkMeta{
		Index:  len(c.chunks),
		Offset: c.curOffset,
		Size:   c.curSize,
		SHA256: hex.EncodeToString(c.curSHA.Sum(nil)),
		MD5:    hex.EncodeToString(c.curMD5.Sum(nil)),
	})
	c.curOffset += c.curSize
	c.curSize = 0
	c.curSHA.Reset()
	c.curMD5.Reset()
}

// ReadFrom 从 reader 读出全部数据并累计（io.Reader 适配，返回读到的总字节）。
func (c *Calculator) ReadFrom(r io.Reader) (int64, error) {
	buf := make([]byte, 64<<10) // 64KiB 读缓冲（分块切分在 Write 内完成）
	var total int64
	for {
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := c.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return total, err
		}
	}
	// 末块（不足 chunkSize 的尾块）收尾。
	if c.curSize > 0 {
		c.flushChunk()
	}
	return total, nil
}

// Finish 产出完整 FileMeta（整文件 + 全部分块，含尾块收尾）。之后禁止继续 Write。
// 零大小文件：无数据块，产出空分块列表（调用方特判零文件不落 meta 或空 meta）。
func (c *Calculator) Finish() *FileMeta {
	c.done = true
	if c.curSize > 0 {
		c.flushChunk() // 尾块收尾（TeeReader 等直接 Write 路径下尾块不足 chunkSize）
	}
	return &FileMeta{
		Version:     metaVersion,
		Size:        c.size,
		TotalSHA256: hex.EncodeToString(c.totalSHA.Sum(nil)),
		TotalMD5:    hex.EncodeToString(c.totalMD5.Sum(nil)),
		ChunkSize:   c.chunkSize,
		Chunks:      c.chunks,
	}
}

// WrapReader 封装 reader 为"读写时顺便计算"的流：读取数据经流的同时累计 meta 哈希。
// 返回的 reader 关闭时可通过 Calc() 取计算器（Finish 由调用方决定时机）。
type metaReader struct {
	inner io.Reader
	calc  *Calculator
	used  bool
}

// WrapReader 包装 io.Reader：读数据经过 wrap 时顺便累计 FileMeta 哈希。
func WrapReader(r io.Reader, size, chunkSize int64) (*metaReader, error) {
	c, err := NewCalculator(size, chunkSize)
	if err != nil {
		return nil, err
	}
	return &metaReader{inner: r, calc: c}, nil
}

func (mr *metaReader) Read(p []byte) (int, error) {
	n, err := mr.inner.Read(p)
	if n > 0 {
		if _, werr := mr.calc.Write(p[:n]); werr != nil {
			return n, werr
		}
		mr.used = true
	}
	return n, err
}

func (mr *metaReader) Close() error {
	if closer, ok := mr.inner.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

// Calc 返回计算器（调用方在读完数据后 Finish）。
func (mr *metaReader) Calc() *Calculator { return mr.calc }
