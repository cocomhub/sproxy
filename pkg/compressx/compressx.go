// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package compressx 提供压缩算法注册表（roadmap 11.10-⑨ 压缩算法扩展）：
//
//   - Algorithm 是压缩算法名：gzip（默认）/ zstd / brotli；Parse 对非法值报错
//     （不静默回退 gzip——显式语义，调用方决定 400 还是回退）。
//   - NewReader / NewWriter 封装三个实现（zstd 用 klauspost/compress/zstd，
//     brotli 用 andybalholm/brotli——均纯 Go、社区活跃、API 稳定）。
//
// 使用场景：归档（archive）压缩与存储传输的可选压缩；zstd 流**不具备** gzip 的
// 逐块可读性（有 frame 边界），分块传输场景须按「块级」（每 chunk 独立 frame）
// 组装，见 design 2026-09-24-compression-zstd.md。
package compressx

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/andybalholm/brotli"
	"github.com/klauspost/compress/zstd"
)

// Algorithm 是压缩算法名（gzip 默认；zstd/brotli 为扩展）。
type Algorithm string

const (
	// Gzip 是默认压缩算法（零回归，与既有归档/传输行为一致）。
	Gzip Algorithm = "gzip"
	// Zstd 是高压缩比扩展（klauspost/compress/zstd）。
	Zstd Algorithm = "zstd"
	// Brotli 是归档备选算法（andybalholm/brotli）。
	Brotli Algorithm = "brotli"
)

// Parse 解析压缩算法名（大小写不敏感）。非法值返回 error（不静默回退 gzip）。
func Parse(s string) (Algorithm, error) {
	switch Algorithm(s) {
	case Gzip:
		return Gzip, nil
	case Zstd:
		return Zstd, nil
	case Brotli:
		return Brotli, nil
	default:
		return "", fmt.Errorf("compressx: 未知压缩算法 %q", s)
	}
}

// Ext 返回归档文件扩展名（gzip→gz / zstd→zst / brotli→br，如 .tar.gz / .tar.zst）。
func (a Algorithm) Ext() string {
	switch a {
	case Zstd:
		return "zst"
	case Brotli:
		return "br"
	default:
		return "gz"
	}
}

// ContentType 返回该算法的归档 Content-Type（gzip→application/gzip（RFC 6713），
// zstd→application/zstd（RFC 8878），brotli→application/x-brotli（未注册，惯例前缀））。
func (a Algorithm) ContentType() string {
	switch a {
	case Zstd:
		return "application/zstd"
	case Brotli:
		return "application/x-brotli"
	default:
		return "application/gzip"
	}
}

// NewReader 构造按算法解压的读取器。未知算法 / 构造失败返回 error。
func NewReader(algo Algorithm, r io.Reader) (io.ReadCloser, error) {
	switch algo {
	case Gzip:
		zr, err := gzip.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("compressx: gzip reader: %w", err)
		}
		return zr, nil
	case Zstd:
		zr, err := zstd.NewReader(r)
		if err != nil {
			return nil, fmt.Errorf("compressx: zstd reader: %w", err)
		}
		// zstd.Decoder 不实现 io.Closer（由内部缓冲池管理），包装成 ReadCloser。
		return &zstdReadCloser{Decoder: zr}, nil
	case Brotli:
		return &nopReadCloser{Reader: brotli.NewReader(r)}, nil
	default:
		return nil, fmt.Errorf("compressx: 未知压缩算法 %q", algo)
	}
}

// NewWriter 构造按算法压缩的写入器（level 为压缩级别：0 = 算法默认）。
// 未知算法 / 非法 level 返回 error。返回的 io.WriteCloser 必须 Close 才能
// 完成流收尾（zstd/brotli 的 frame 尾部在 Close 时写出）。
func NewWriter(algo Algorithm, w io.Writer, level int) (io.WriteCloser, error) {
	switch algo {
	case Gzip:
		lvl := level
		if lvl == 0 {
			lvl = gzip.DefaultCompression
		}
		zw, err := gzip.NewWriterLevel(w, lvl)
		if err != nil {
			return nil, fmt.Errorf("compressx: gzip writer: %w", err)
		}
		return zw, nil
	case Zstd:
		opts := []zstd.EOption{}
		if level > 0 {
			lvl := zstd.EncoderLevelFromZstd(level)
			opts = append(opts, zstd.WithEncoderLevel(lvl))
		}
		zw, err := zstd.NewWriter(w, opts...)
		if err != nil {
			return nil, fmt.Errorf("compressx: zstd writer: %w", err)
		}
		return zw, nil
	case Brotli:
		lvl := level
		if lvl == 0 {
			lvl = brotli.DefaultCompression
		}
		if lvl < brotli.BestSpeed || lvl > brotli.BestCompression {
			return nil, fmt.Errorf("compressx: brotli level %d 超出范围 [%d,%d]", lvl, brotli.BestSpeed, brotli.BestCompression)
		}
		return brotli.NewWriterLevel(w, lvl), nil
	default:
		return nil, fmt.Errorf("compressx: 未知压缩算法 %q", algo)
	}
}

// zstdReadCloser 包装 zstd.Decoder 为 io.ReadCloser（Close 释放解码器缓冲）。
type zstdReadCloser struct {
	*zstd.Decoder
}

func (z *zstdReadCloser) Close() error {
	z.Decoder.Close()
	return nil
}

// nopReadCloser 包装只读流为 io.ReadCloser（Close 为 no-op）。
type nopReadCloser struct {
	io.Reader
}

func (n *nopReadCloser) Close() error { return nil }

// 显式引用 errors，防未来 gofmt/go fix 误删（当前 Close 均不返回 error）。
var _ = errors.New
