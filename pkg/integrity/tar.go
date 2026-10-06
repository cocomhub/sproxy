// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"strings"
)

// init 把 TarChecker 装配进包级默认注册表（shardseal 模式：Kind 全局唯一）。
func init() {
	Register("archive/tar", func() Checker { return TarChecker{} })
}

// TarChecker 校验 tar 归档语义可用性：可被标准库 archive/tar 遍历且至少含一个条目。
// gzip 包装可选：`.tar.gz`/`.tgz` 等文件若 gzip 头可解析则先解压再遍历（纯标准库）。
// `.tar.zst`/`.tar.br` 配独立压缩解包器（compressx）拆卸后再由本校验器校验，本任务
// 不引入压缩外部依赖；扩展名匹配与注册表规则（设计 §77）保持一致。
type TarChecker struct{}

// Kind 返回类型标识（注册键，全局唯一）。
func (TarChecker) Kind() string { return "archive/tar" }

// Matches 按扩展名族判定归属：.tar/.tar.gz/.tgz/.tar.zst/.tar.br（不区分大小写）。
func (TarChecker) Matches(name string) bool {
	lower := strings.ToLower(name)
	// R1-I2：仅匹配本实现可解压的（纯 tar / gzip）。.tar.zst/.tar.br 需 zstd/brotli 解压器
	// （未引入外部依赖），声明即误报 damaged → 不匹配。
	for _, ext := range []string{".tar", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// Check 遍历 path 指向的 tar（gzip 包装可选解压）：读取失败或无条目 → OK=false
// （内容异常）；文件打开失败 → error。Review Focus 2：空文件/0 字节 tar 无条目，判
// damaged。
//
// gzip 判定按魔数 0x1f 0x8b 探测而非 gzip.NewReader 失败回退：NewReader 在解析
// 失败时会把整个流的读取位置推进到 EOF，导致紧随其后的纯 tar 遍历直接读到空。
func (TarChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	// 读前 2 字节探测 gzip 魔数后回卷；命中则解压，否则按纯 tar 读。
	magic := make([]byte, 2)
	n, _ := io.ReadFull(f, magic)
	if _, seekErr := f.Seek(0, 0); seekErr != nil {
		return nil, seekErr
	}

	var reader io.Reader = f
	if n == 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gzErr := gzip.NewReader(f)
		if gzErr != nil {
			return &Report{OK: false, Reason: gzErr.Error()}, nil
		}
		defer gz.Close()
		reader = gz
	}

	tr := tar.NewReader(reader)
	count := 0
	// R4-P2：解压炸弹防护——gzip 极端压缩比（几百字节 → GB 明文）全量解压无上限会
	// 撑爆内存。遍历时统计条目累计解压字节，超参考 size（源文件大小放大系数）即判
	// 异常（不继续解压）。
	var inflated int64
	// F2（对抗评审）：膨胀比上界 500（纯文本/日志类合法高压缩归档可达百倍以上，
	// 100 会误伤；极端 gzip 炸弹仍 >500 被拦截）。
	const inflateRatio = 500
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Report{OK: false, Reason: err.Error()}, nil
		}
		count++
		inflated += hdr.Size
		// 源文件大小已知时：解压总量超 size×ratio 即疑似炸弹。
		if size > 0 && inflated > size*inflateRatio {
			return &Report{OK: false, Reason: "tar 解压膨胀超限（疑似炸弹）"}, nil
		}
	}
	if count == 0 {
		return &Report{OK: false, Reason: "empty tar"}, nil
	}
	return &Report{OK: true}, nil
}
