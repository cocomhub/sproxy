// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// RangeSeeker 把「可选 RangeReader + 已知 size」适配为 io.ReadSeeker，供
// http.ServeContent 服务端转发随机访问（A/C 态：secretdata 加密卷 / 私密外部卷）。
//
// ServeContent 的用法恰好是：Seek(0, SeekEnd) 取 size → Seek(0, SeekStart) →
// 顺序 Read（Range 请求也是一次定位后顺序读）。RangeSeeker 在**窗口边界**（每
// rangeWindow 字节）或 Seek 跳跃时重开一次 OpenRangeRead，逐窗口续读——每窗口
// 连续读取 = 一次 RangeRead。
//
// offset 语义：明文逻辑位置（与 http.ServeContent 收到的 Range offset 一致；
// 加密卷 OpenRangeRead 的 offset 同样为明文坐标，无需额外寻址转换）。
//
// **窗口上界（评审 C1 修复，2026-10-05）**：openRange 传 min(size-off, rangeWindow)
// 而非 [off, EOF)——secretdata OpenRangeRead 是急切物化覆盖区间（rangeReadBytes
// 把整个 [off, off+size) 下载+解密成单 []byte）；若传尾段，随机访问会每次拉整个
// 文件尾并驻留内存，完全抵消关键帧随机访问目标。窗口把单次物化限制为 ≤窗口明文
// （覆盖窗口的块仍整块下载——PR #733 已知边界）。顺带激活 Read 的段末续读分支
// （rcEnd < size 恒假死代码消除）。
//
// ctx 处理（containedctx 收敛）：ctx 是单次 ServeContent 操作的取消信号，由
// NewRangeSeeker 捕获进 openRange 闭包（struct 不持 context.Context 字段）——
// 关闭调用方 ctx 即终止在途 RangeRead 与段流读取。
//
// 无 RangeReader 的 FS：NewRangeSeeker 返回错误（调用方退整流 200 路径，不 500）。
type RangeSeeker struct {
	openRange func(off int64) (io.ReadCloser, error) // 重开 [off, off+window) 段流（闭包捕获 ctx）
	size      int64                                  // 逻辑文件大小（Stat 已知）
	window    int64                                  // 单段窗口上界（评审 C1：防尾段急切物化）
	off       int64                                  // 当前逻辑 offset

	rc    io.ReadCloser // 当前区间流；nil = 未打开
	rcEnd int64         // 当前区间流的逻辑终点（rc 读到此处即段末）
}

// rangeWindow 是 RangeSeeker 的单段窗口上界（2026-10-05 评审 C1）：限制单次
// OpenRangeRead 物化的明文量。取 4MiB——播放器 Range 请求通常 ≤1MiB（单段命中），
// 顺序播放按窗口逐段续读（每段一次 RangeRead 开销可接受）。与 secretdata 块大小
// （64KiB-4MiB 级）同量级，覆盖窗口的块整块下载边界不变。
const rangeWindow = 4 << 20

// NewRangeSeeker 构造 RangeSeeker。fs 必须实现 RangeReader（否则返回错误——调用方
// 决定整流 200 或 fail）。ctx 为本次 ServeContent 操作的取消信号（见类型注释）。
func NewRangeSeeker(ctx context.Context, fs FS, rel string, size int64) (*RangeSeeker, error) {
	if fs == nil {
		return nil, fmt.Errorf("sync: RangeSeeker 底层 FS 为 nil")
	}
	rr := AssertRangeReader(fs)
	if rr == nil {
		return nil, fmt.Errorf("sync: FS 未实现 RangeReader，无法随机访问（服务端整流路径）")
	}
	if size < 0 {
		return nil, fmt.Errorf("sync: 非法文件大小 %d", size)
	}
	return &RangeSeeker{
		openRange: func(off int64) (io.ReadCloser, error) {
			if off < 0 || off > size {
				return nil, fmt.Errorf("sync: RangeSeeker 区间偏移 %d 越出 [0,%d]", off, size)
			}
			want := min(size-off, rangeWindow)
			return rr.OpenRangeRead(ctx, rel, off, want)
		},
		size:   size,
		window: rangeWindow,
	}, nil
}

// Read 从当前 offset 读入 p。窗口流耗尽时重开 [off, off+window) 的 RangeRead 续读。
func (s *RangeSeeker) Read(p []byte) (int, error) {
	if s.off >= s.size {
		if s.rc != nil {
			s.rc.Close()
			s.rc = nil
		}
		return 0, io.EOF
	}
	if s.rc == nil {
		if err := s.open(); err != nil {
			return 0, err
		}
	}
	n, err := s.rc.Read(p)
	s.off += int64(n)
	if errors.Is(err, io.EOF) && s.rcEnd < s.size {
		// 窗口流读完但文件未耗尽：关旧段、重开下一窗口续读（顺序读大文件逐段）。
		s.rc.Close()
		s.rc = nil
		return n, nil
	}
	return n, err
}

// Seek 实现 io.Seeker：SeekEnd 返回 size（廉价，不真 seek）；SeekStart 设置偏移，
// 若与当前段流连续（恰在段末）则续用，否则关闭段流待下次 Read 重开。
func (s *RangeSeeker) Seek(offset int64, whence int) (int64, error) {
	var next int64
	switch whence {
	case io.SeekStart:
		next = offset
	case io.SeekCurrent:
		next = s.off + offset
	case io.SeekEnd:
		next = s.size + offset
	default:
		return s.off, fmt.Errorf("sync: 非法 whence %d", whence)
	}
	if next < 0 {
		return s.off, fmt.Errorf("sync: 负 seek 偏移 %d", next)
	}
	if next > s.size {
		next = s.size
	}
	// 连续（当前段流刚读到段末且 next 恰好接上）→ 保留段流；否则关闭重开。
	if s.rc != nil && next != s.rcEnd {
		s.rc.Close()
		s.rc = nil
	}
	s.off = next
	return s.off, nil
}

// Close 关闭当前区间流（幂等）。
func (s *RangeSeeker) Close() error {
	if s.rc != nil {
		err := s.rc.Close()
		s.rc = nil
		return err
	}
	return nil
}

// open 重开 [off, min(size, off+window)) 的 RangeRead 窗口流（评审 C1：窗口上界
// 限制单次物化，激活 Read 段末续读分支）。
func (s *RangeSeeker) open() error {
	if s.off >= s.size {
		return io.EOF
	}
	rc, err := s.openRange(s.off)
	if err != nil {
		return fmt.Errorf("sync: RangeSeeker 打开区间 [%d,%d) 失败: %w", s.off, s.off+s.window, err)
	}
	s.rc = rc
	end := min(s.off+s.window, s.size)
	s.rcEnd = end
	return nil
}
