// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// verify_test.go 钉住流式下载校验（VerifyReadSeeker）：全量读逐分块校验通过、
// 篡改分块 fail-closed、多余数据拒绝、Range 部分读跳过不误报、整文件哈希全量比对。

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"
)

// refFileMeta 构造 n 字节、分块 chunkSize 的合法 FileMeta（哈希由实内容算）。
func refFileMeta(t *testing.T, content []byte, chunkSize int64) *FileMeta {
	t.Helper()
	calc, cerr := NewCalculator(int64(len(content)), chunkSize)
	if cerr != nil {
		t.Fatalf("NewCalculator: %v", cerr)
	}
	if _, rerr := calc.ReadFrom(bytes.NewReader(content)); rerr != nil {
		t.Fatalf("ReadFrom: %v", rerr)
	}
	fm := calc.Finish()
	fm.Name = "f.bin"
	return fm
}

// readAll 把校验流读尽（失败返回错误——fail-closed 语义即 ServeContent 中断）。
func readAll(r io.Reader) (string, error) {
	var b strings.Builder
	_, err := io.Copy(&b, r)
	return b.String(), err
}

// TestVerifyReadSeeker_Ok 全量读逐分块校验通过 + 内容一致。
func TestVerifyReadSeeker_Ok(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("verify-ok-"), 100) // 800B，1MiB 分块单块
	fm := refFileMeta(t, content, 1024)
	v := VerifyReadSeeker(bytes.NewReader(content), fm)
	got, err := readAll(v)
	if err != nil {
		t.Fatalf("校验通过路径不应报错: %v", err)
	}
	if got != string(content) {
		t.Fatalf("读回内容不符")
	}
}

// TestVerifyReadSeeker_TamperedChunk 篡改数据（分块哈希不一致）→ fail-closed 报错。
func TestVerifyReadSeeker_TamperedChunk(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("verify-tamper-"), 200) // 1400B，两块 1024+376
	fm := refFileMeta(t, content, 1024)
	tampered := append([]byte(nil), content...)
	tampered[100] ^= 0xFF // 改第一块内一个字节
	v := VerifyReadSeeker(bytes.NewReader(tampered), fm)
	if _, err := readAll(v); err == nil {
		t.Fatal("篡改分块应 fail-closed 报错（不发损坏内容）")
	}
}

// TestVerifyReadSeeker_ExtraData 内容超过 fm.Size → 拒绝多余数据。
func TestVerifyReadSeeker_ExtraData(t *testing.T) {
	t.Parallel()
	content := []byte("verify-extra")
	fm := refFileMeta(t, content, 1024)
	extra := append([]byte(nil), content...)
	extra = append(extra, []byte("garbage-tail")...)
	v := VerifyReadSeeker(bytes.NewReader(extra), fm)
	if _, err := readAll(v); err == nil {
		t.Fatal("多余数据应拒绝（防附加垃圾伪装合法内容）")
	}
}

// TestVerifyReadSeeker_RangePartial 部分读（Range）块不完整 → 不误报（跳过无法验证的块）。
// 真实 Range：ServeContent Seek(offset) 后读——用 bytes.Reader（Seek 返回绝对位置，
// 与 *os.File 同语义）模拟。
func TestVerifyReadSeeker_RangePartial(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("verify-range-"), 300) // 2100B，三块 1024
	fm := refFileMeta(t, content, 1024)
	// 模拟 Range：Seek 到 offset 500 读到 EOF（第一块只读部分——块首缺数据，无法验证跳过）。
	br := bytes.NewReader(content)
	v := VerifyReadSeeker(br, fm)
	if pos, serr := v.Seek(500, io.SeekStart); serr != nil || pos != 500 {
		t.Fatalf("Seek(500) = %d, %v", pos, serr)
	}
	// 读 Range 部分（offset 500 → EOF）。
	got, err := readAll(io.LimitReader(v, int64(len(content))-500))
	if err != nil {
		t.Fatalf("Range 部分读不完整块应跳过（不误报）: %v", err)
	}
	if got != string(content[500:]) {
		t.Fatalf("Range 读回内容不符")
	}
}

// TestVerifyReadSeeker_TotalHashFullRead EOF 全量读 → 整文件 TotalSHA256 比对。
func TestVerifyReadSeeker_TotalHashFullRead(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("verify-total-"), 200)
	fm := refFileMeta(t, content, 1024)
	v := VerifyReadSeeker(bytes.NewReader(content), fm)
	if _, err := readAll(v); err != nil {
		t.Fatalf("全量读校验应通过: %v", err)
	}
}

// TestVerifyReadSeeker_ShortContent 底层内容短于 fm.Size（截断/尾块静默丢失）→ fail-closed。
// 钉住 P1-1：ServeContent 的 CopyN 按实际（截断）长度精确读满，不会产生底层 EOF 的那次
// Read；仅靠 EOF 判据会静默放行，必须靠构造时的长度探测拦截。
func TestVerifyReadSeeker_ShortContent(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("A"), 3000)
	fm := refFileMeta(t, content, 1024)
	v := VerifyReadSeeker(bytes.NewReader(content[:1500]), fm)
	if _, err := readAll(v); err == nil {
		t.Fatal("截断内容必须 fail-closed 报错（不得静默交付）")
	}
	// ServeContent 语义：按实际（截断）长度精确读满，不产生底层 EOF 的那次 Read——
	// 仅靠 EOF 判据会漏；必须靠构造时的长度探测使首次 Read 即报错。
	v2 := VerifyReadSeeker(bytes.NewReader(content[:1500]), fm)
	if _, err := readAll(io.LimitReader(v2, 1500)); err == nil {
		t.Fatal("按实际长度精确读满时也必须 fail-closed（extent 探测）")
	}
}

// TestVerifyReadSeeker_ExtraTailExtent 内容长于 fm.Size（附加垃圾）→ 构造时长度探测即拒。
func TestVerifyReadSeeker_ExtraTailExtent(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("B"), 3000)
	fm := refFileMeta(t, content, 1024)
	extra := append(append([]byte(nil), content...), []byte("garbage-tail")...)
	v := VerifyReadSeeker(bytes.NewReader(extra), fm)
	// 即使消费者只按 fm.Size 精确读取（不触发底层 EOF），也必须拒绝。
	if _, err := readAll(io.LimitReader(v, fm.Size)); err == nil {
		t.Fatal("越界内容必须 fail-closed 报错（不得靠下一次 Read 才报）")
	}
}

// TestVerifyReadSeeker_MidSeekToEOF_NoFalsePositive 中段 Seek 后读到 EOF 不得误报
// 「整文件哈希不一致」——钉住 P2-1（fromStart 语义）。
func TestVerifyReadSeeker_MidSeekToEOF_NoFalsePositive(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("C"), 3000)
	fm := refFileMeta(t, content, 1024)
	v := VerifyReadSeeker(bytes.NewReader(content), fm)
	if _, err := v.Seek(1500, io.SeekStart); err != nil {
		t.Fatalf("Seek: %v", err)
	}
	// io.Copy 会读到 bytes.Reader 的真实 EOF（与 RangePartial 的 LimitReader 不同）。
	got, err := readAll(v)
	if err != nil {
		t.Fatalf("中段读到 EOF 不应误报损坏: %v", err)
	}
	if got != string(content[1500:]) {
		t.Fatalf("中段读回内容不符")
	}
}

// TestVerifyReadSeeker_InvalidMetaFailsClosed 入口 Validate fail-closed：畸形 meta
// （哈希格式错等）应首次 Read 即报错，不进入流式校验（可能 slice 越界/校验弱化）。
func TestVerifyReadSeeker_InvalidMetaFailsClosed(t *testing.T) {
	t.Parallel()
	h := sha256.Sum256([]byte("abcd"))
	sum := hex.EncodeToString(h[:])
	// ChunkSize=0 属 Validate 拒绝项，但分块/总哈希与实际内容一致（不 Validate 时流式
	// 校验会通过）——测试专门钉住入口 Validate 门。
	bad := &FileMeta{
		Version: 1, Size: 4, TotalSHA256: sum, ChunkSize: 0,
		Chunks: []ChunkMeta{{Index: 0, Offset: 0, Size: 4, SHA256: sum}},
	}
	v := VerifyReadSeeker(strings.NewReader("abcd"), bad)
	if _, err := v.Read(make([]byte, 4)); err == nil {
		t.Fatal("畸形 meta 应首次 Read 即 fail-closed")
	}
}
