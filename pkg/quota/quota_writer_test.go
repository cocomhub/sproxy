// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

// errTestWrite / errTestRead 用于在 QuotaWriter / WriteFileQuota 路径注入确定性失败。
var (
	errTestWrite = errors.New("test write failure")
	errTestRead  = errors.New("test read failure")
)

// failWriter 恒定写失败（返回 0 字节 + 错误）。
// 用于验证 QuotaWriter 写失败保留 reserve 的语义。
// 通过字段替换实现「重试复用同一 account」（测试与实现同包，直接改 w.writer）。
type failWriter struct{}

func (w *failWriter) Write(p []byte) (int, error) { return 0, errTestWrite }

// failAfterReader 先吐出 n 字节真实数据，随后 Read 返回错误。
// 用于验证 WriteFileQuota Copy 中失败的回拨语义。
type failAfterReader struct {
	emitted int
}

func (r *failAfterReader) Read(p []byte) (int, error) {
	if r.emitted < 3 && len(p) > 0 {
		r.emitted++
		p[0] = 'x'
		return 1, nil
	}
	return 0, errTestRead
}

// expectedHashFromOffset 计算 f[offset:] 的 SHA-256 hex，供 VerifyChunkChecksum 断言。
func expectedHashFromOffset(t *testing.T, f *os.File, offset int64) string {
	t.Helper()
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		t.Fatalf("hash: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func TestQuotaWriterReserveThenCommit(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 100)
	w, err := NewQuotaWriter(s, io.Discard, 30) // 写前预留 30
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}
	if got := s.Reserved(); got != 30 {
		t.Fatalf("Reserved=%d want 30（写前预留）", got)
	}

	if n, err := w.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write()=(%d,%v) want (5,nil)", n, err)
	}
	// Write 后 committed 增 reserved 减
	if got := s.Usage(); got != 5 {
		t.Fatalf("Usage=%d want 5（committed 增）", got)
	}
	if got := s.Reserved(); got != 25 {
		t.Fatalf("Reserved=%d want 25（reserved 减）", got)
	}
}

func TestQuotaWriterAutoTopUp(t *testing.T) {
	root := NewPool(30) // 全局兜底 30
	s := root.Scope("/t", 100)
	w, err := NewQuotaWriter(s, io.Discard, 10) // 先预留 10
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}

	// 写 15 字节：预留 10 不够，自动 TryReserve 补 5 再 commit。
	if n, err := w.Write(make([]byte, 15)); n != 15 || err != nil {
		t.Fatalf("Write()=(%d,%v) want (15,nil)", n, err)
	}
	if got := s.Usage(); got != 15 {
		t.Fatalf("Usage=%d want 15", got)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Reserved=%d want 0", got)
	}

	// 再写 20 字节：可用 15（30-15），补留 20 失败 → ErrStorageFull，且不回调已写量。
	if n, err := w.Write(make([]byte, 20)); !errors.Is(err, ErrStorageFull) || n != 0 {
		t.Fatalf("Write()=(%d,%v) want (0, ErrStorageFull)", n, err)
	}
	if got := s.Usage(); got != 15 {
		t.Fatalf("补留失败后 Usage=%d want 15（不回调已写量）", got)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("补留失败后 Reserved=%d want 0", got)
	}
}

func TestQuotaWriterFailureKeepsReserve(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 100)
	w, err := NewQuotaWriter(s, &failWriter{}, 30)
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}

	// 底层 writer 失败 → 本 Write 不产生记账，reserved 原样保留。
	if n, err := w.Write([]byte("12345")); !errors.Is(err, errTestWrite) || n != 0 {
		t.Fatalf("Write()=(%d,%v) want (0,errTestWrite)", n, err)
	}
	if got := s.Reserved(); got != 30 {
		t.Fatalf("Write 失败后 Reserved=%d want 30（保留 reserve）", got)
	}
	if got := s.Usage(); got != 0 {
		t.Fatalf("Write 失败后 Usage=%d want 0", got)
	}

	// 重试复用同一 account（换用正常 writer）。
	w.writer = io.Discard
	if n, err := w.Write([]byte("12345")); n != 5 || err != nil {
		t.Fatalf("重试 Write()=(%d,%v) want (5,nil)", n, err)
	}
	if got := s.Usage(); got != 5 {
		t.Fatalf("重试后 Usage=%d want 5", got)
	}
	if got := s.Reserved(); got != 25 {
		t.Fatalf("重试后 Reserved=%d want 25", got)
	}

	// Finish(true) 才释放多余 reserve（未用 25）。
	w.Finish(true, 0)
	if got := s.Reserved(); got != 0 {
		t.Fatalf("Finish(true) 后 Reserved=%d want 0（释放多余 reserve）", got)
	}
	if got := s.Usage(); got != 5 {
		t.Fatalf("Finish(true) 后 Usage=%d want 5", got)
	}
}

func TestQuotaWriterFinish(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30) // 预留 30
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if _, err := w.Write([]byte("1234567890")); err != nil { // 写 10
			t.Fatalf("Write: %v", err)
		}
		if got := s.Usage(); got != 10 {
			t.Fatalf("Write 后 Usage=%d want 10", got)
		}
		if got := s.Reserved(); got != 20 {
			t.Fatalf("Write 后 Reserved=%d want 20", got)
		}

		// 成功：释放未用 reserve（20）+ 覆盖写 ReleaseUsage(oldSize=7) → committed 10-7=3。
		w.Finish(true, 7)
		if got := s.Reserved(); got != 0 {
			t.Fatalf("Finish(true) Reserved=%d want 0", got)
		}
		if got := s.Usage(); got != 3 {
			t.Fatalf("Finish(true) Usage=%d want %d（10-7=%d，含 ReleaseUsage(oldSize)）", got, 10-7, 10-7)
		}
	})

	t.Run("abandon", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30)
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if _, err := w.Write([]byte("1234567890")); err != nil { // 写 10
			t.Fatalf("Write: %v", err)
		}
		if got := s.Usage(); got != 10 {
			t.Fatalf("Write 后 Usage=%d want 10", got)
		}

		// 放弃：ReleaseUsage(written=10) 回拨已 commit + 释放剩余 reserve 20。
		w.Finish(false, 0)
		if got := s.Reserved(); got != 0 {
			t.Fatalf("Finish(false) Reserved=%d want 0", got)
		}
		if got := s.Usage(); got != 0 {
			t.Fatalf("Finish(false) Usage=%d want 0（回拨已 commit 部分）", got)
		}
	})
	t.Run("constructor excess refused", func(t *testing.T) {
		root := NewPool(10)
		s := root.Scope("/t", 100)
		if _, err := NewQuotaWriter(s, io.Discard, 11); !errors.Is(err, ErrStorageFull) {
			t.Fatalf("NewQuotaWriter 超限应拒绝, got %v", err)
		}
	})
}

func TestBoundWriter(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bw")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	t.Run("fills_limit_then_eof", func(t *testing.T) {
		bw := NewBoundWriter(f, 0, 5, 0)
		if n, err := bw.Write([]byte("hello")); n != 5 || err != nil {
			t.Fatalf("Write()=(%d,%v) want (5,nil)", n, err)
		}
		if n, err := bw.Write([]byte("world")); !errors.Is(err, io.EOF) || n != 0 {
			t.Fatalf("超限 Write()=(%d,%v) want (0,io.EOF)", n, err)
		}
	})

	t.Run("truncates_oversized_but_keeps_bounds", func(t *testing.T) {
		// 在 offset=0 写 20 字节哨兵，用于检测越界写坏相邻区域。
		if _, err := f.WriteAt(bytes.Repeat([]byte{'S'}, 20), 0); err != nil {
			t.Fatalf("sentinel write: %v", err)
		}
		reg := make([]byte, 20)
		if _, err := f.ReadAt(reg, 0); err != nil {
			t.Fatalf("sentinel read: %v", err)
		}
		_ = reg

		bw := NewBoundWriter(f, 4, 5, 0)      // 限长 5 位于 [4,9)
		chunk := bytes.Repeat([]byte{'A'}, 8) // 超长 8
		n, err := bw.Write(chunk)
		if n != 5 || err != nil {
			t.Fatalf("Write()=(%d,%v) want (5,nil)（截断到 room=5）", n, err)
		}

		got := make([]byte, 6)
		if _, err := f.ReadAt(got, 4); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if string(got[:5]) != "AAAAA" {
			t.Fatalf("区域 [4,9) 内容=%q want %q", got[:5], "AAAAA")
		}
		if got[5] != 'S' {
			t.Fatalf("越界字节 [9]=%q want 'S'（不得写坏相邻分片）", got[5])
		}
	})

	t.Run("writes_at_offset", func(t *testing.T) {
		bw := NewBoundWriter(f, 100, 5, 0)
		if _, err := bw.Write([]byte("abcde")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		got := make([]byte, 5)
		if _, err := f.ReadAt(got, 100); err != nil {
			t.Fatalf("ReadAt: %v", err)
		}
		if string(got) != "abcde" {
			t.Fatalf("content=%q want %q（Write 应落位于 offset）", got, "abcde")
		}
	})
}

func TestWriteFileQuota(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		f, err := os.CreateTemp(t.TempDir(), "wfq")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()

		n, err := s.WriteFileQuota(context.Background(), f, int64(len("data")), strings.NewReader("data"), 0)
		if err != nil || n != 4 {
			t.Fatalf("WriteFileQuota()=(%d,%v) want (4,nil)", n, err)
		}
		if got := s.Usage(); got != 4 {
			t.Fatalf("WriteFileQuota 后 Usage=%d want 4", got)
		}
		if got := s.Reserved(); got != 0 {
			t.Fatalf("WriteFileQuota 后 Reserved=%d want 0", got)
		}
	})

	t.Run("copy_error_rolls_back", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		f, err := os.CreateTemp(t.TempDir(), "wfq")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()

		n, err := s.WriteFileQuota(context.Background(), f, 10, &failAfterReader{}, 0)
		if !errors.Is(err, errTestRead) {
			t.Fatalf("WriteFileQuota() error=%v want errTestRead", err)
		}
		if n != 3 {
			t.Fatalf("WriteFileQuota() n=%d want 3（失败前已写字节）", n)
		}
		// Copy error 时 Finish(false) 回拨已 commit + 释放剩余 reserve。
		if got := s.Usage(); got != 0 {
			t.Fatalf("copy 失败后 Usage=%d want 0（回拨）", got)
		}
		if got := s.Reserved(); got != 0 {
			t.Fatalf("copy 失败后 Reserved=%d want 0（释放 reserve）", got)
		}
	})
}

func TestVerifyChunkChecksum(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 100)
	f, err := os.CreateTemp(t.TempDir(), "chk")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	data := []byte("hello world, chunk data!")
	if _, err := f.WriteAt(data, 10); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	want := expectedHashFromOffset(t, f, 10)

	if ok, err := s.VerifyChunkChecksum(f, 10, want); !ok || err != nil {
		t.Fatalf("VerifyChunkChecksum()=(%v,%v) want (true,nil)", ok, err)
	}
	if ok, err := s.VerifyChunkChecksum(f, 10, "deadbeef"); ok || err != nil {
		t.Fatalf("VerifyChunkChecksum()=(%v,%v) want (false,nil)", ok, err)
	}
	// 空 want 跳过校验返回 true（与 checksum.go 的 verifyChecksum 约定一致）。
	if ok, err := s.VerifyChunkChecksum(f, 99999, ""); !ok || err != nil {
		t.Fatalf("空 want 应跳过校验, got (%v,%v)", ok, err)
	}
}
