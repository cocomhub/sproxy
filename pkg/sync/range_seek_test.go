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

// openOnlyFS 只有 OpenRead（无 RangeReader）——验证「退整流 seeker」路径。
type openOnlyFS struct{ data []byte }

func (o *openOnlyFS) ListDir(context.Context, string) ([]Entry, error) { return nil, nil }
func (o *openOnlyFS) Stat(context.Context, string) (*Entry, error)     { return nil, nil }
func (o *openOnlyFS) WriteFile(context.Context, string, io.Reader, int64, int64) error {
	return nil
}
func (o *openOnlyFS) Rename(context.Context, string, string) error { return nil }
func (o *openOnlyFS) Delete(context.Context, string) error         { return nil }
func (o *openOnlyFS) MakeDir(context.Context, string) error        { return nil }
func (o *openOnlyFS) OpenRead(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(o.data)), nil
}

// TestStreamSeeker_FullAndRange 无 RangeReader 的卷也能整读/按 Range 读（修正下载恒 500）。
func TestStreamSeeker_FullAndRange(t *testing.T) {
	t.Parallel()
	fs := &openOnlyFS{data: []byte("0123456789abcdef")}
	s := NewStreamSeeker(context.Background(), fs, "x", int64(len(fs.data)))
	if n, err := s.Seek(0, io.SeekEnd); err != nil || n != 16 {
		t.Fatalf("SeekEnd = %d,%v want 16", n, err)
	}
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	all, err := io.ReadAll(s)
	if err != nil || string(all) != "0123456789abcdef" {
		t.Fatalf("整流读 = %q,%v", all, err)
	}
	// Range 读 [4,8)：先 Seek(0,End)（ServeContent 用法）→ Seek(4,Start) → 读 4 字节。
	if _, err := s.Seek(0, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Seek(4, io.SeekStart); err != nil || n != 4 {
		t.Fatalf("Seek(4) = %d,%v", n, err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(s, buf); err != nil || string(buf) != "4567" {
		t.Fatalf("Range 读 = %q,%v", buf, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

// hijackRangeFS 模拟透明装饰器链中的**能力回显**：无条件实现 OpenRangeRead（inner
// 无能力时运行期返 ErrUnsupported），并实现 Inner() 供 Innermost 下探。
type hijackRangeFS struct {
	baseFS
	inner FS
}

func (h *hijackRangeFS) Inner() FS { return h.inner }
func (h *hijackRangeFS) OpenRangeRead(ctx context.Context, p string, off, size int64) (io.ReadCloser, error) {
	if rr := AssertRangeReader(h.inner); rr != nil {
		return rr.OpenRangeRead(ctx, p, off, size)
	}
	return nil, ErrUnsupported
}

// TestNewRangeSeeker_TransparentDecoratorDoesNotFakeCapability P1 回归（第 5 轮对抗评审）：
// 透明装饰器无条件实现 OpenRangeRead 不得使 NewRangeSeeker 误判底层具备 Range 能力——
// 否则调用方（external_source.go）的「整流 seeker 回落」永远不会执行，无 Range 能力的
// 外部卷（S3/WebDAV/SFTP/FTP）下载在首个 Read 才报错、响应已被截断。
func TestNewRangeSeeker_TransparentDecoratorDoesNotFakeCapability(t *testing.T) {
	t.Parallel()
	// 装饰层回显 OpenRangeRead，最内层 baseFS 无该能力 → 必须报错（回落到 StreamSeeker）。
	fs := &hijackRangeFS{inner: &baseFS{}}
	if _, err := NewRangeSeeker(context.Background(), fs, "x.bin", 10); err == nil {
		t.Fatal("透明装饰器不得让 NewRangeSeeker 误判具备 Range 能力（回落应为死代码的反例）")
	}
	// 多级装饰叠加（Guard(Wrap(CapacityFS(raw))) 形态）同样必须报错。
	outer := &hijackRangeFS{inner: fs}
	if _, err := NewRangeSeeker(context.Background(), outer, "x.bin", 10); err == nil {
		t.Fatal("多级透明装饰不得让 NewRangeSeeker 误判具备 Range 能力")
	}
}

// TestNewRangeSeeker_RealRangeCapableLayerStillWorks：最内层具备真实 RangeReader 时
// 判据仍成立（无回归）。
func TestNewRangeSeeker_RealRangeCapableLayerStillWorks(t *testing.T) {
	t.Parallel()
	cfs, rel := newRangeSeekerEnv(t, []byte("0123456789"))
	// 装饰层 + 真实 Range 内层 → 构造成功。
	fs := &hijackRangeFS{inner: cfs}
	rs, err := NewRangeSeeker(context.Background(), fs, rel, 10)
	if err != nil {
		t.Fatalf("真实 Range 内层应构造成功: %v", err)
	}
	defer rs.Close()
	buf, err := io.ReadAll(rs)
	if err != nil {
		t.Fatalf("读: %v", err)
	}
	if string(buf) != "0123456789" {
		t.Fatalf("got %q", buf)
	}
}
