// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// verify_test.go 钉住流式下载校验（VerifyReadSeeker）：全量读逐分块校验通过、
// 篡改分块 fail-closed、多余数据拒绝、Range 部分读跳过不误报、整文件哈希全量比对。

import (
	"bytes"
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
