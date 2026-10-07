// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package integrity

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

// init 把 TarChecker 装配进包级默认注册表（shardseal 模式：Kind 全局唯一）。
func init() {
	Register(TarKind, func() Checker { return TarChecker{} })
}

// TarKind 是 tar 校验器的注册键/类型标识（全局唯一；Register/Kind 共用——S1192 去重字面量）。
const TarKind = "archive/tar"

// TarChecker 校验 tar 归档语义可用性：可被标准库 archive/tar 遍历且至少含一个条目。
// gzip 包装可选：`.tar.gz`/`.tgz` 等文件若 gzip 头可解析则先解压再遍历（纯标准库）。
// `.tar.zst`/`.tar.br` 配独立压缩解包器（compressx）拆卸后再由本校验器校验，本任务
// 不引入压缩外部依赖；扩展名匹配与注册表规则（设计 §77）保持一致。
type TarChecker struct{}

// EstimateMem 预估一次 Check 的峰值内存。
//
// **关键修正（对抗评审 R3-Impt-1）**：TarChecker.Check **只遍历 header**（tar.Reader.Next()
// 逐条读取 512B header 块，不读文件内容）——峰值内存是常量级（每 header 小分配），
// 与文件大小/解压膨胀比**无关**。此前按 size×inflateRatioMax 估算会把 >1MiB 的合法
// tar.gz 高估到 ≥500MiB → 误触内存配额跳过校验标记 unverified，覆盖率系统性下降。
// 膨胀比上限（inflateRatioMax）只用于 Check 遍历时的 gzip 炸弹拦截，不参与内存估算。
// 返回固定小值（tar header 缓冲 ~512B × 少量 + 运行时余量）。
func (TarChecker) EstimateMem(path string, size int64) int64 {
	return 64 << 10 // 64KiB 固定常量：仅 header 遍历，不读内容，无膨胀分配
}

// Kind 返回类型独立 key（注册键，全局唯一）。
func (TarChecker) Kind() string { return TarKind }

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
// inflateRatioMax 是 gzip 解压膨胀比安全上界（Check 用 inflated>x*ratio 判超额异常；
// EstimateMem 用同值估算内存配额）。F2（对抗评审）：100 → 500——txt/log 等高压缩文件
// 解压可超百倍，100 会误伤合法高压缩。
const inflateRatioMax = int64(500)

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
	return checkTarEntries(ctx, reader, size)
}

// checkTarEntries 遍历 tar 条目并做解压炸弹防护（Check 的子步骤，抽方法控 gocognit）。
// ctx 取消：多 GB 归档可耗时数分钟，任务取消后须能提前中止（与 video 校验器一致）。
// R4-P2：解压炸弹防护——gzip 极端压缩比全量解压无上限会撑爆内存，统计条目累计
// 解压字节，超 size×inflateRatioMax（500，纯文本合法高压缩可达百倍）即判异常。
func checkTarEntries(ctx context.Context, reader io.Reader, size int64) (*Report, error) {
	tr := tar.NewReader(reader)
	count := 0
	var inflated int64
	for {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("tar check cancelled: %w", ctx.Err())
		}
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return &Report{OK: false, Reason: err.Error()}, nil
		}
		count++
		inflated += hdr.Size
		if size > 0 && inflated > size*inflateRatioMax {
			return &Report{OK: false, Reason: "tar 解压膨胀超限（疑似炸弹）"}, nil
		}
	}
	if count == 0 {
		return &Report{OK: false, Reason: "empty tar"}, nil
	}
	return &Report{OK: true}, nil
}
