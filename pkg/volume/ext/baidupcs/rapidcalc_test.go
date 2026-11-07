// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

// rapidcalc_test.go —— 离线锁定 rapidupload 参数计算（S5 最易出错处）：
//  1. 分块大小规则与库一致（用库公开常量与阈值）；
//  2. 随机片段偏移与库 `creaetDataOffset` 逐位一致（**独立计算的金值向量**）；
//  3. checksum 复用库实现后确实与独立计算一致（md5/crc32/slice-md5/分块数）；
//  4. 随机片段内容确实是文件 [offset, offset+len) 的真内容（抓"偏移算错"）。

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"hash/crc32"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBlockSizeForRapid_MatchesUploadBlock —— **设计约束**：rapid 的 blockList 分块
// 必须与本包上传分块一致（否则上传后刷新时块索引对不上）。
func TestBlockSizeForRapid_MatchesUploadBlock(t *testing.T) {
	t.Parallel()
	if blockSizeForRapid() != uploadBlockSize {
		t.Fatalf("rapid 分块 %d 必须等于上传分块 %d（否则块索引不一致）",
			blockSizeForRapid(), uploadBlockSize)
	}
}

// TestRapidDataOffset_GoldenVector 偏移必须与库 creaetDataOffset 逐位一致。
// 金值由**独立实现**（Python hashlib）计算：uk=123456789、content="0123…"（固定 32 位 hex）、
// dataTime=1700000000、subSize=4096 ⇒ size=10485760 时 offset=464813。
func TestRapidDataOffset_GoldenVector(t *testing.T) {
	t.Parallel()
	const (
		uk      = int64(123456789)
		content = "0123456789abcdef0123456789abcdef"
		ts      = int64(1700000000)
		sub     = int64(4096)
	)
	got, err := rapidDataOffset(content, uk, ts, 10485760, sub)
	if err != nil {
		t.Fatalf("rapidDataOffset: %v", err)
	}
	if got != 464813 {
		t.Fatalf("偏移与库金值不一致：got %d want 464813（秒传参数错误 ⇒ 秒传必失败）", got)
	}
	// 确定性：同输入同输出。
	again, _ := rapidDataOffset(content, uk, ts, 10485760, sub)
	if again != got {
		t.Fatalf("同输入应同输出，got %d vs %d", again, got)
	}
	// 窗口不足 ⇒ 0（size == sub 与 size < sub 均为 0，且不得除零/负模）。
	if off, err := rapidDataOffset(content, uk, ts, sub, sub); err != nil || off != 0 {
		t.Fatalf("size==sub 应得 0，got %d err=%v", off, err)
	}
	if off, err := rapidDataOffset(content, uk, ts, 100, sub); err != nil || off != 0 {
		t.Fatalf("size<sub 应得 0，got %d err=%v", off, err)
	}
	// 不同 uk ⇒ 偏移不同（概率极高；固定输入下确定）。
	other, _ := rapidDataOffset(content, uk+1, ts, 10485760, sub)
	if other == got {
		t.Logf("uk 变化后偏移相同（%d）—— 概率事件，仅记录", other)
	}
}

// writePatternFile 写一个 1MiB 确定性内容文件（与金值计算的构造一致）。
func writePatternFile(t *testing.T) string {
	t.Helper()
	const n = 1024 * 1024
	buf := make([]byte, n)
	for i := range buf {
		buf[i] = byte((i*7 + 13) % 251)
	}
	p := filepath.Join(t.TempDir(), "pattern.bin")
	if err := os.WriteFile(p, buf, 0o600); err != nil {
		t.Fatalf("write pattern file: %v", err)
	}
	return p
}

// TestPlanRapid_OfflineChecksums checksum 必须与独立计算一致（复用库实现的效果验证）。
func TestPlanRapid_OfflineChecksums(t *testing.T) {
	t.Parallel()
	p := writePatternFile(t)
	plan, err := planRapid(p, 123456789, 1700000000)
	if err != nil {
		t.Fatalf("planRapid: %v", err)
	}
	// 金值（独立计算，见测试文件头注释）。
	if plan.ContentMD5 != "e67297d9c0a78553e44b948379ae477a" {
		t.Fatalf("content md5 不符：got %s", plan.ContentMD5)
	}
	if plan.CRC32 != "1755447164" {
		t.Fatalf("crc32 不符：got %s", plan.CRC32)
	}
	if plan.SliceMD5 != "69efa252ba13e6676a6f54043c4ec983" {
		t.Fatalf("slice md5 不符：got %s", plan.SliceMD5)
	}
	if plan.BlockSize != uploadBlockSize {
		t.Fatalf("分块应为上传块 %d，got %d", uploadBlockSize, plan.BlockSize)
	}
	if len(plan.BlocksList) != 1 {
		t.Fatalf("1MiB 文件（< 4MiB 块）应只有 1 块，got %d", len(plan.BlocksList))
	}
	if plan.BlocksList[0] != hex.EncodeToString(md5Sum(t, p)) {
		t.Fatalf("单块 md5 应与整文件 md5 相同，got %s", plan.BlocksList[0])
	}
	if plan.DataLength != rapidSubSize || plan.DataTime != 1700000000 {
		t.Fatalf("片段长度/时间错误：len=%d time=%d", plan.DataLength, plan.DataTime)
	}
}

// TestPlanRapid_DataContentMatchesFileSlice —— 强断言：base64 片段必须**就是**文件
// [offset, offset+len) 的真实内容（偏移算错的历史风险在这里被抓住）。
func TestPlanRapid_DataContentMatchesFileSlice(t *testing.T) {
	t.Parallel()
	p := writePatternFile(t)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	plan, err := planRapid(p, 123456789, 1700000000)
	if err != nil {
		t.Fatalf("planRapid: %v", err)
	}
	if plan.Offset < 0 || plan.Offset+plan.DataLength > int64(len(data)) {
		t.Fatalf("片段区间越界：offset=%d len=%d size=%d", plan.Offset, plan.DataLength, len(data))
	}
	// 库用 StdEncoding 且去掉尾部 '='（Go 的 StdEncoding 解码允许缺 padding）。
	// 与库一致：**不得带 base64 padding**（库显式 TrimRight '='）。
	if strings.HasSuffix(plan.DataContent, "=") {
		t.Fatalf("dataContent 不得带 base64 padding（库会 TrimRight '='，否则参数无效）: %q", plan.DataContent)
	}
	decoded, err := base64.RawStdEncoding.DecodeString(plan.DataContent) // 无 padding（库去掉了 '='）
	if err != nil {
		t.Fatalf("base64 解码失败: %v", err)
	}
	want := data[plan.Offset : plan.Offset+plan.DataLength]
	if !bytes.Equal(decoded, want) {
		t.Fatalf("随机片段内容与文件区间不一致（偏移或读取错误）")
	}
	// crc32 用 IEEE 多项式（与库 hash/crc32 默认一致）。
	if got := crc32.ChecksumIEEE(data); got != 1755447164 {
		t.Fatalf("独立 crc32 计算不符：%d", got)
	}
}

// TestPlanRapid_ErrorPaths 缺文件/目录路径必须显式报错（不得 panic 或静默）。
func TestPlanRapid_ErrorPaths(t *testing.T) {
	t.Parallel()
	if _, err := planRapid(filepath.Join(t.TempDir(), "missing.bin"), 1, 1); err == nil {
		t.Fatal("文件不存在应报错")
	}
	if _, err := planRapid(t.TempDir(), 1, 1); err == nil {
		t.Fatal("路径为目录应报错")
	}
}

// md5Sum 独立计算整文件 md5（测试用）。
func md5Sum(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	sum := md5.Sum(b)
	return sum[:]
}
