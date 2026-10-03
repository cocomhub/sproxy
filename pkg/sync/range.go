// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
	"io"
)

// RangeReader 是可选能力接口：FS 支持定点多段读取时实现。未实现的 FS
// （HTTPTransport / s3 / baidupcs / pikpak 等）由调用方（secretdata）回退整块读取，
// 零回归——严格复用 BlockAccessor 的 optional-interface 范式。
//
// 用途：视频关键帧随机访问——secretdata.OpenRangeRead 对目标块只拉
// [EncOffset, EncOffset+EncSize) 段，避免 seek 某时间点下载 1-200MB 整块。
type RangeReader interface {
	// OpenRangeRead 打开路径的定点读 [offset, offset+size)，返回区间字节流。
	// offset/size 为 0-based 文件字节偏移；区间越出文件 → 错误（fail-closed）。
	OpenRangeRead(ctx context.Context, path string, offset, size int64) (io.ReadCloser, error)
}

// AssertRangeReader 断言 FS 实现 RangeReader；未实现返回 nil（调用方整块回退）。
func AssertRangeReader(fs FS) RangeReader {
	if rr, ok := fs.(RangeReader); ok {
		return rr
	}
	return nil
}

// RangeReadAll 按 RangeReader 读取 [offset, offset+size) 并读入内存。
// rr 为 nil（底层无 Range 能力）时返回 (nil, false, nil)，调用方回退整块读取。
func RangeReadAll(rr RangeReader, ctx context.Context, path string, offset, size int64) ([]byte, bool, error) {
	if rr == nil {
		return nil, false, nil
	}
	rc, err := rr.OpenRangeRead(ctx, path, offset, size)
	if err != nil {
		return nil, true, fmt.Errorf("范围读取 %s[%d,+%d) 失败: %w", path, offset, size, err)
	}
	defer rc.Close()
	buf, err := io.ReadAll(rc)
	if err != nil {
		return nil, true, fmt.Errorf("范围读取 %s[%d,+%d) 失败: %w", path, offset, size, err)
	}
	if int64(len(buf)) != size {
		return nil, true, fmt.Errorf("范围读取长度 %d 与期望 %d 不符", len(buf), size)
	}
	return buf, true, nil
}
