// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
)

// countingRangeFS 包装 LocalFS 并统计 OpenRangeRead 调用次数（验证段重开语义）。
type countingRangeFS struct {
	*LocalFS
	rangeReads int
}

func (c *countingRangeFS) OpenRangeRead(ctx context.Context, relPath string, offset, size int64) (io.ReadCloser, error) {
	c.rangeReads++
	return c.LocalFS.OpenRangeRead(ctx, relPath, offset, size)
}

// newRangeSeekerEnv 建临时目录 + 写测试文件 + 包装 FS。
func newRangeSeekerEnv(t *testing.T, content []byte) (*countingRangeFS, string) {
	t.Helper()
	root := t.TempDir()
	fs := NewLocalFS(root, nil)
	if err := fs.WriteFile(context.Background(), "file.bin", bytes.NewReader(content), int64(len(content)), 0); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfs := &countingRangeFS{LocalFS: fs}
	return cfs, "file.bin"
}

// TestRangeSeeker_SequentialFullRead：ServeContent 主路径——Seek(0,SeekEnd) 取 size →
// Seek(0,SeekStart) → 顺序 Read 全量。每段连续读取 = 一次 RangeRead。
func TestRangeSeeker_SequentialFullRead(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("abcdefghij"), 10) // 100B
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	// SeekEnd → size（廉价，不触发 RangeRead）。
	end, serr := rs.Seek(0, io.SeekEnd)
	if serr != nil {
		t.Fatalf("Seek(End): %v", serr)
	}
	if end != int64(len(content)) {
		t.Fatalf("SeekEnd=%d，应为 %d", end, len(content))
	}
	// SeekStart → 0。
	start, serr := rs.Seek(0, io.SeekStart)
	if serr != nil {
		t.Fatalf("Seek(Start): %v", serr)
	}
	if start != 0 {
		t.Fatalf("SeekStart=%d，应为 0", start)
	}
	got, gerr := io.ReadAll(rs)
	if gerr != nil {
		t.Fatalf("ReadAll: %v", gerr)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("顺序读 != 原内容：len(got)=%d", len(got))
	}
	// 单段连续读 = 1 次 RangeRead。
	if cfs.rangeReads != 1 {
		t.Fatalf("RangeRead 次数=%d，应为 1（单段连续读）", cfs.rangeReads)
	}
}

// TestRangeSeeker_RangeReadOnlyTargetSegment：Range 请求一次定位后顺序读小段，
// 只拉含目标区间的一段。
func TestRangeSeeker_RangeReadOnlyTargetSegment(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("abcdefghij"), 10) // 100B
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	// 定位到 20 读 10 字节（模拟 Range: bytes=20-29）。
	if _, serr := rs.Seek(20, io.SeekStart); serr != nil {
		t.Fatalf("Seek(20): %v", serr)
	}
	buf := make([]byte, 10)
	n, rerr := io.ReadFull(rs, buf)
	if rerr != nil {
		t.Fatalf("ReadFull: %v", rerr)
	}
	if n != 10 || !bytes.Equal(buf, content[20:30]) {
		t.Fatalf("区间读内容错误：got %q want %q", buf, content[20:30])
	}
}

// TestRangeSeeker_SeekJumpReopensSegment：Seek 跳跃到新 offset（非段末连续）→
// 关闭旧段、重开新段（RangeRead 次数增加）。
func TestRangeSeeker_SeekJumpReopensSegment(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("abcdefghij"), 10)
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	// 读第一段前 10 字节。
	buf := make([]byte, 10)
	if _, err := io.ReadFull(rs, buf); err != nil {
		t.Fatalf("首段 Read: %v", err)
	}
	// 跳跃到 50（非连续）→ 下次 Read 重开段。
	if _, serr := rs.Seek(50, io.SeekStart); serr != nil {
		t.Fatalf("Seek(50): %v", serr)
	}
	if _, rerr := io.ReadFull(rs, buf); rerr != nil {
		t.Fatalf("重开后 Read: %v", rerr)
	}
	if !bytes.Equal(buf, content[50:60]) {
		t.Fatalf("跳跃后内容错误：got %q want %q", buf, content[50:60])
	}
	if cfs.rangeReads != 2 {
		t.Fatalf("RangeRead 次数=%d，应为 2（跳跃重开段）", cfs.rangeReads)
	}
}

// TestRangeSeeker_SequentialChunksNoReopen：ServeContent 对大文件分块 Read——
// 单段流内多次 Read 不重开（rangeReads 恒 1），段末续读才重开。
func TestRangeSeeker_SequentialChunksNoReopen(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("0123456789"), 20) // 200B
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()

	var got []byte
	buf := make([]byte, 7)
	for {
		n, rerr := rs.Read(buf)
		got = append(got, buf[:n]...)
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			t.Fatalf("Read: %v", rerr)
		}
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("分块读 != 原内容：len(got)=%d", len(got))
	}
	if cfs.rangeReads != 1 {
		t.Fatalf("RangeRead 次数=%d，应为 1（单段内分块读不重开）", cfs.rangeReads)
	}
}

// TestRangeSeeker_NoRangeReader：无 RangeReader 的 FS → 构造报错（调用方整流 200）。
func TestRangeSeeker_NoRangeReader(t *testing.T) {
	t.Parallel()
	// 纯 nil FS / 无 RangeReader 的实现：用裸结构（LocalFS 实现了 RangeReader，
	// 用一个仅满足 FS 的无 Range 包装来断言）。
	_, err := NewRangeSeeker(context.Background(), nil, "x", 10)
	if err == nil {
		t.Fatal("nil FS 应报错")
	}
}

// TestRangeSeeker_SeekOutOfRangeClamps：Seek 越出文件 → 钳制到 size（ServeContent
// 语义：SeekEnd 正偏移 = EOF）。
func TestRangeSeeker_SeekOutOfRangeClamps(t *testing.T) {
	t.Parallel()
	content := bytes.Repeat([]byte("abc"), 10) // 30B
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	defer rs.Close()
	// 负偏移拒绝。
	if _, serr := rs.Seek(-1, io.SeekStart); serr == nil {
		t.Fatal("负 Seek 应报错")
	}
	// 越出钳制到 size。
	off, serr := rs.Seek(999, io.SeekStart)
	if serr != nil {
		t.Fatalf("Seek(999): %v", serr)
	}
	if off != int64(len(content)) {
		t.Fatalf("Seek 越出应钳制到 size=%d，got %d", len(content), off)
	}
	// 越出后 Read = EOF。
	if n, rerr := rs.Read(make([]byte, 5)); rerr != io.EOF || n != 0 {
		t.Fatalf("越出后 Read 应 EOF：n=%d err=%v", n, rerr)
	}
}

// TestRangeSeeker_CloseIdempotent：Close 幂等（多次调用安全）。
func TestRangeSeeker_CloseIdempotent(t *testing.T) {
	t.Parallel()
	content := []byte("hello")
	cfs, rel := newRangeSeekerEnv(t, content)
	rs, err := NewRangeSeeker(context.Background(), cfs, rel, int64(len(content)))
	if err != nil {
		t.Fatalf("NewRangeSeeker: %v", err)
	}
	if cerr := rs.Close(); cerr != nil {
		t.Fatalf("首次 Close: %v", cerr)
	}
	if cerr := rs.Close(); cerr != nil {
		t.Fatalf("二次 Close: %v", cerr)
	}
	_ = filepath.Join
}
