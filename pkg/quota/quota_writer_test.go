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
	"sync"
	"sync/atomic"
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

// --- 配额磁盘封顶审查补充测试（quota_writer 侧） ---

// TestQuotaWriter_ReleaseReserve 锁定 ReleaseReserve 语义：释放剩余 reserve 但保留已 commit；
// 幂等：重复调用/与 SetWriter+Finish 混用不重复释放。
func TestQuotaWriter_ReleaseReserve(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 100)
	w, err := NewQuotaWriter(s, io.Discard, 30)
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}
	if _, err := w.Write([]byte("1234567890")); err != nil { // 写 10
		t.Fatalf("Write: %v", err)
	}
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("Write 后 Usage=%d want %d", got, want)
	}
	if got, want := s.Reserved(), int64(20); got != want {
		t.Fatalf("Write 后 Reserved=%d want %d", got, want)
	}

	w.ReleaseReserve()
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("ReleaseReserve 后 Reserved=%d want %d（未用 reserve 归还）", got, want)
	}
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("ReleaseReserve 后 Usage=%d want %d（已 commit 保留占账）", got, want)
	}
	// Committed() 保留已 commit（ReleaseReserve 只释放 reserve，不销 committed 账本）。
	if got, want := w.Committed(), int64(10); got != want {
		t.Fatalf("ReleaseReserve 后 Committed=%d want %d", got, want)
	}

	// 幂等：重复调用不重复释放（reserved 已归零）。
	w.ReleaseReserve()
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("重复 ReleaseReserve Reserved=%d want %d", got, want)
	}
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("重复 ReleaseReserve Usage=%d want %d", got, want)
	}
	// SetWriter + Finish(true) 混用后仍不重复释放（reserved 0、written 0 → 两个空操作）。
	w.SetWriter(io.Discard)
	w.Finish(true, 0)
	if got, want := s.Reserved(), int64(0); got != want {
		t.Fatalf("Finish 后 Reserved=%d want %d", got, want)
	}
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("Finish 后 Usage=%d want %d（覆盖写 oldSize=0，已 commit 10 保留）", got, want)
	}
}

// TestQuotaWriter_Committed 锁定 Committed() 累计值随 Write 增长；
// Finish（true/false）后清零；ReleaseReserve 不清零（保留的是已 commit 字节占账）。
func TestQuotaWriter_Committed(t *testing.T) {
	t.Run("accumulates_with_write", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30)
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if got, want := w.Committed(), int64(0); got != want {
			t.Fatalf("初始 Committed=%d want %d", got, want)
		}
		if _, err := w.Write([]byte("12345")); err != nil { // 5
			t.Fatalf("Write#1: %v", err)
		}
		if _, err := w.Write([]byte("67890")); err != nil { // 5
			t.Fatalf("Write#2: %v", err)
		}
		if got, want := w.Committed(), int64(10); got != want {
			t.Fatalf("两次 Write 后 Committed=%d want %d（随 Write 累计）", got, want)
		}
		if got, want := s.Usage(), int64(10); got != want {
			t.Fatalf("Scope Usage=%d want %d", got, want)
		}
	})

	t.Run("finish_success_clears", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30)
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if _, err := w.Write([]byte("1234567890")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		w.Finish(true, 0)
		if got, want := w.Committed(), int64(0); got != want {
			t.Fatalf("Finish(true) 后 Committed=%d want %d（结算后清零）", got, want)
		}
	})

	t.Run("finish_abandon_clears", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30)
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if _, err := w.Write([]byte("1234567890")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		w.Finish(false, 0)
		if got, want := w.Committed(), int64(0); got != want {
			t.Fatalf("Finish(false) 后 Committed=%d want %d（结算后清零）", got, want)
		}
	})

	t.Run("release_reserve_keeps_committed", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		w, err := NewQuotaWriter(s, io.Discard, 30)
		if err != nil {
			t.Fatalf("NewQuotaWriter: %v", err)
		}
		if _, err := w.Write([]byte("1234567890")); err != nil {
			t.Fatalf("Write: %v", err)
		}
		w.ReleaseReserve()
		if got, want := w.Committed(), int64(10); got != want {
			t.Fatalf("ReleaseReserve 后 Committed=%d want %d（已 commit 字节继续占账，结算前供上层回拨）", got, want)
		}
	})
}

// TestQuotaWriter_SetWriter 锁定 SetWriter 替换底层 sink 的连续性：
// 换 writer 后写继续，两 sink 各自保留前段；Scope 账本（Usage/Reserved）连续。
func TestQuotaWriter_SetWriter(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 100)
	var buf1 bytes.Buffer
	var buf2 bytes.Buffer
	w, err := NewQuotaWriter(s, &buf1, 30)
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}
	if _, err := w.Write([]byte("12345")); err != nil { // 第一段 5 → buf1
		t.Fatalf("Write#1: %v", err)
	}
	w.SetWriter(&buf2)                                  // 续传/重试切 sink，保留 reserved/committed 状态
	if _, err := w.Write([]byte("67890")); err != nil { // 第二段 5 → buf2
		t.Fatalf("Write#2: %v", err)
	}
	if got, want := buf1.String(), "12345"; got != want {
		t.Fatalf("buf1=%q want %q（第一段落入旧 sink）", got, want)
	}
	if got, want := buf2.String(), "67890"; got != want {
		t.Fatalf("buf2=%q want %q（第二段落入新 sink）", got, want)
	}
	if got, want := w.Committed(), int64(10); got != want {
		t.Fatalf("Committed=%d want %d（换 sink 后状态连续）", got, want)
	}
	if got, want := s.Usage(), int64(10); got != want {
		t.Fatalf("Usage=%d want %d", got, want)
	}
	if got, want := s.Reserved(), int64(20); got != want {
		t.Fatalf("Reserved=%d want %d（30 − 已 commit 10）", got, want)
	}
}

// TestWriteFileQuota_OverwriteOldSize 锁定覆盖写尺寸对账（diff 语义）：
// Finish(true, oldSize) = commitUp(size,size) + ReleaseUsage(oldSize)：
//   - oldSize=3 写 4 → committed 4 − 3 = 1（净增量）；
//   - oldSize>新大小（如 10 写 4）→ committed 4 − 10 下溢归 0，不反负。
func TestWriteFileQuota_OverwriteOldSize(t *testing.T) {
	t.Run("old_size_3_write_4_diff_1", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		f, err := os.CreateTemp(t.TempDir(), "wfq")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()

		n, err := s.WriteFileQuota(context.Background(), f, 4, strings.NewReader("data"), 3)
		if err != nil || n != 4 {
			t.Fatalf("WriteFileQuota()=(%d,%v) want (4,nil)", n, err)
		}
		if got, want := s.Usage(), int64(1); got != want {
			t.Fatalf("覆盖写后 Usage=%d want %d（4−3，diff 语义）", got, want)
		}
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Reserved=%d want %d", got, want)
		}
	})

	t.Run("old_size_gt_new_clamps_zero", func(t *testing.T) {
		root := NewPool(1000)
		s := root.Scope("/t", 100)
		f, err := os.CreateTemp(t.TempDir(), "wfq")
		if err != nil {
			t.Fatalf("CreateTemp: %v", err)
		}
		defer os.Remove(f.Name())
		defer f.Close()

		// releaseCommittedUp(10) 对 4 下溢 → 0，不反负。
		n, err := s.WriteFileQuota(context.Background(), f, 4, strings.NewReader("data"), 10)
		if err != nil || n != 4 {
			t.Fatalf("WriteFileQuota()=(%d,%v) want (4,nil)", n, err)
		}
		if got, want := s.Usage(), int64(0); got != want {
			t.Fatalf("覆盖写缩小后 Usage=%d want %d（下溢归 0）", got, want)
		}
		if got, want := s.Reserved(), int64(0); got != want {
			t.Fatalf("Reserved=%d want %d", got, want)
		}
	})
}

// TestWriteFileQuota_ReserveExceeded 锁定 size 超 scope 上限时返回 ErrStorageFull
// 且文件不被写（构造预留即失败，未进入 Copy）。
func TestWriteFileQuota_ReserveExceeded(t *testing.T) {
	root := NewPool(1000)
	s := root.Scope("/t", 8) // 子池上限 8
	f, err := os.CreateTemp(t.TempDir(), "wfq")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer os.Remove(f.Name())
	defer f.Close()

	n, err := s.WriteFileQuota(context.Background(), f, 9, strings.NewReader("123456789"), 0)
	if !errors.Is(err, ErrStorageFull) {
		t.Fatalf("WriteFileQuota(size=9) error=%v want ErrStorageFull", err)
	}
	if n != 0 {
		t.Fatalf("WriteFileQuota() n=%d want 0（不写任何字节）", n)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("文件内容长度=%d want 0（不得写入临时文件）", len(got))
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("失败后 Reserved=%d want 0（预留失败不立账）", got)
	}
	if got := s.Usage(); got != 0 {
		t.Fatalf("失败后 Usage=%d want 0", got)
	}
}

// TestBoundWriter_ConstructorFull 锁定 BoundWriter 构造即写满（written==limit）时
// 首次 Write 返回 (0, io.EOF)。
func TestBoundWriter_ConstructorFull(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "bw")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer f.Close()

	bw := NewBoundWriter(f, 0, 5, 5) // written==limit，无剩余 room
	if n, err := bw.Write([]byte("x")); !errors.Is(err, io.EOF) || n != 0 {
		t.Fatalf("构造即满 Write()=(%d,%v) want (0,io.EOF)", n, err)
	}
	if raw, _ := os.ReadFile(f.Name()); len(raw) != 0 {
		t.Fatalf("构造即满不得写文件，内容长度=%d want 0", len(raw))
	}
}

// countingWriter 统计写入字节（并发写盘断言用，不并发于同一 writer——QuotaWriter 叶锁
// 已串行化底层 writer 调用）。
type countingWriter struct {
	n atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}

// TestQuotaWriter_WriteAndFinishConcurrent 直测审查 C-1 内部锁（quota_writer.go:30 w.mu）：
// 并发 goroutine 混调 Write/Committed/ReleaseReserve/Finish，-race 下无数据竞争，且
// 账本终态不反负（Scope committed/reserved 均在 [0, 上限] 区间，随累计写盘量单调一致）。
//
// 场景设计对齐真实并发形态：下载 goroutine 裸调 Write（不持调用方锁），取消/删除路径
// 持 m.mu 调 Committed/ReleaseReserve/Finish——两者并发访问同一 QuotaWriter。
func TestQuotaWriter_WriteAndFinishConcurrent(t *testing.T) {
	root := NewPool(100000)
	s := root.Scope("/t", 100000)
	sink := &countingWriter{}
	w, err := NewQuotaWriter(s, sink, 10)
	if err != nil {
		t.Fatalf("NewQuotaWriter: %v", err)
	}

	const (
		writers   = 8
		perWriter = 50
		chunk     = 5
		total     = writers * perWriter * chunk // 2000
	)

	// 每个 writer 独占一段时间连续 Write（模拟下载流），随后随机混调 Finish/ReleaseReserve；
	// 另起一个专用 goroutine 高频 Committed()（模拟快照读取）。
	errCh := make(chan error, writers)
	var wg sync.WaitGroup
	start := make(chan struct{})

	wg.Add(writers + 1)
	for i := range writers {
		go func(idx int) {
			defer wg.Done()
			<-start
			for range perWriter {
				if _, err := w.Write(make([]byte, chunk)); err != nil {
					errCh <- err
					return
				}
			}
			// 写完后按 writer 序号确定性地混调 Finish/ReleaseReserve
			// （偶数 Finish(true,0) 释放 reserve+ReleaseUsage(0)，奇数 ReleaseReserve 只释放 reserve）。
			if idx%2 == 0 {
				w.Finish(true, 0)
			} else {
				w.ReleaseReserve()
			}
		}(i)
	}
	go func() {
		defer wg.Done()
		<-start
		for range 2000 {
			_ = w.Committed()
		}
	}()

	close(start)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("并发 Write 出错: %v", err)
	}

	// 终态账本不反负：committed 应等于累计写盘量（ReleaseReserve/Finish 只释放 reserve 与
	// 覆盖写旧 size，不扣减本次累计 commit；此处全部 success=true 或 ReleaseReserve）。
	if got := s.Usage(); got != total {
		t.Fatalf("并发后 Scope Usage=%d want %d（累计写盘量，不反负）", got, total)
	}
	if got := s.Reserved(); got != 0 {
		t.Fatalf("并发后 Reserved=%d want 0（无残余 reserve，不反负）", got)
	}
	if got := sink.n.Load(); got != total {
		t.Fatalf("sink 收到字节=%d want %d", got, total)
	}
}
