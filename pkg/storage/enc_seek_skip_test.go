// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// enc_seek_skip_test.go 锁定评审 R14 C10：加密卷 Range/Seek 必须**只解密目标块**。
//
// 旧实现 `seekStart` 用 `io.CopyN(io.Discard, r, offset)` ⇒ 前面每一块都被解密：
// 1 GiB 文件末尾 Range 请求要解密 1 GiB；8 MiB 分块下载 1 GiB 累计约 64 GiB 解密量。

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func writeEncFile(t *testing.T, root *Root, rel string, plain []byte) {
	t.Helper()
	wc, err := root.OpenFileEncrypted(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("OpenFileEncrypted: %v", err)
	}
	if _, werr := wc.Write(plain); werr != nil {
		_ = wc.Close()
		t.Fatalf("Write: %v", werr)
	}
	if cerr := wc.Close(); cerr != nil {
		t.Fatalf("Close: %v", cerr)
	}
}

// countingReadSeeker 统计读取字节数（模拟「解密前需要读多少密文」）。
type countingReadSeeker struct {
	r     *bytes.Reader
	read  int64
	total *int64 // 可选：跨多次重开的累计读取量（接线用例用）
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += int64(n)
	if c.total != nil {
		*c.total += int64(n)
	}
	return n, err
}

func (c *countingReadSeeker) Seek(off int64, whence int) (int64, error) {
	return c.r.Seek(off, whence)
}

// TestEncSeekPlain_SkipsCiphertextBlocks 定位到文件末尾时只读「头 + 一个块」。
func TestEncSeekPlain_SkipsCiphertextBlocks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	if serr := root.SetEncryption(key); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}

	// 1 MiB 明文 = 16 个 64KiB 块（远大于单块）。
	plain := bytes.Repeat([]byte("0123456789abcdef"), 64<<10)
	writeEncFile(t, root, "big.bin", plain)
	cipherBytes, rerr := os.ReadFile(filepath.Join(dir, "big.bin"))
	if rerr != nil {
		t.Fatalf("读密文: %v", rerr)
	}

	// 定位到「最后 1KiB」的起点。
	target := int64(len(plain)) - 1024
	cr := &countingReadSeeker{r: bytes.NewReader(cipherBytes)}
	er, nerr := newEncRootReader(key, cr)
	if nerr != nil {
		t.Fatalf("newEncRootReader: %v", nerr)
	}
	if serr := er.SeekPlain(target); serr != nil {
		t.Fatalf("SeekPlain: %v", serr)
	}
	got, _ := io.ReadAll(er)
	if int64(len(got)) != 1024 {
		t.Fatalf("定位后剩余明文长度=%d, want 1024", len(got))
	}
	if !bytes.Equal(got, plain[target:]) {
		t.Fatal("定位后内容与明文不符")
	}
	// 只允许读：头(15+4) + 最后一块的头(4)+密文体(chunk+28)。允许 2 块的余量。
	limit := int64(len(encRootMagic)) + 4 + 2*(4+64<<10+encRootOverhead)
	if cr.read > limit {
		t.Fatalf("SeekPlain 读入 %d 字节（应 ≈ 一个块 ≤ %d）—— 仍在逐块解密跳读", cr.read, limit)
	}
	if root.Close() != nil {
		t.Fatal("close")
	}
}

// TestEncSeekPlain_MatchesSequentialRead 定位读与「从头读后切片」逐字节一致（正确性）。
func TestEncSeekPlain_MatchesSequentialRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(255 - i)
	}
	if serr := root.SetEncryption(key); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	plain := make([]byte, 3*(64<<10)+1234)
	for i := range plain {
		plain[i] = byte(i % 251)
	}
	writeEncFile(t, root, "mixed.bin", plain)

	rcRaw, oerr := root.OpenDecrypted("mixed.bin")
	if oerr != nil {
		t.Fatalf("OpenDecrypted: %v", oerr)
	}
	defer rcRaw.Close()
	rc, ok := rcRaw.(io.ReadSeeker)
	if !ok {
		t.Fatal("解密流必须支持 Seek（Range/续传依赖）")
	}
	for _, off := range []int64{0, 1, 64<<10 - 1, 64 << 10, 2*(64<<10) + 7, int64(len(plain)) - 1} {
		if _, serr := rc.Seek(off, io.SeekStart); serr != nil {
			t.Fatalf("seek %d: %v", off, serr)
		}
		got, _ := io.ReadAll(rc)
		if !bytes.Equal(got, plain[off:]) {
			t.Fatalf("offset %d 处内容不一致（len got=%d want=%d）", off, len(got), len(plain)-int(off))
		}
	}
	if root.Close() != nil {
		t.Fatal("close")
	}
}

// nopCloser 是 io.Closer 空实现（seekStart 会 Close 旧文件）。
type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// TestEncReadCloserSeek_UsesBlockSkip 锁定**接线**：`encReadCloser.Seek` 必须走块头跳过，
// 而不是退回复制丢弃（只测 SeekPlain 会漏掉 wiring —— 变异验证曾因此假绿）。
func TestEncReadCloserSeek_UsesBlockSkip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root, err := OpenRoot(dir)
	if err != nil {
		t.Fatalf("OpenRoot: %v", err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 7)
	}
	if serr := root.SetEncryption(key); serr != nil {
		t.Fatalf("SetEncryption: %v", serr)
	}
	plain := bytes.Repeat([]byte("abcdefgh"), 128<<10) // 1 MiB ⇒ 16 块
	writeEncFile(t, root, "w.bin", plain)
	cipherBytes, rerr := os.ReadFile(filepath.Join(dir, "w.bin"))
	if rerr != nil {
		t.Fatalf("读密文: %v", rerr)
	}

	var total int64
	newReader := func() (io.Reader, error) {
		// 底层仍是 io.ReadSeeker（*countingReadSeeker）⇒ 类型断言在 seekStart 内成立。
		return newEncRootReader(key, &countingReadSeeker{r: bytes.NewReader(cipherBytes), total: &total})
	}
	ec := &encReadCloser{c: nopCloser{}, open: newReader, size: int64(len(plain))}
	target := int64(len(plain)) - 1024
	total = 0 // 只统计 Seek 期间的读取
	if _, serr := ec.Seek(target, io.SeekStart); serr != nil {
		t.Fatalf("Seek: %v", serr)
	}
	rest, _ := io.ReadAll(ec)
	if !bytes.Equal(rest, plain[target:]) {
		t.Fatalf("Seek 后内容不符（len=%d）", len(rest))
	}
	// 允许：头(15+4) + 最后两块（头+密文体），远小于整段密文。
	limit := int64(len(encRootMagic)) + 4 + 2*(4+64<<10+encRootOverhead)
	if total > limit {
		t.Fatalf("Seek 读入 %d 字节（应只读块头 + 目标块 ≤ %d）——接线仍走复制丢弃", total, limit)
	}
}
