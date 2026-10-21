// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// range_stream.go —— `OpenRangeRead` 的**惰性流式**实现（2026-10-21 缺陷修复）。
//
// 背景：旧实现 `rangeReadBytes` 把目标区间 [offset, offset+size) 覆盖的**全部分块**先下载
// + 解密成 `[]byte`，再 `io.NopCloser(bytes.NewReader(out))` 包成流 ⇒ 首帧必须等整个
// RangeSeeker 窗口（4MiB）物化完（网盘场景数秒）；拖进度条也要重下整个新窗口。
//
// 本实现按「块 → 段」惰性推进：`Read` 每取一块（`readChunkRangeBytes` 已按目标段只拉
// 含区间的最小段密文）解出该块明文再交付，首字节延迟 = **第一块**（而非整个窗口）。
// 段级能力由 crypto 层既有原语提供（`DecryptBlockletSegmentStandalone`/`decryptBlockletAt`），
// 本文件只做**流式接线**（不重复实现定位/解密）。
//
// **ctx 处理（containedctx 收敛，与 `pkg/sync.RangeSeeker` 同一范式）**：ctx 是本次读流
// 的取消信号，由构造函数捕获进 `readChunk`/`beginSpan` 闭包（struct **不持** ctx 字段）。
//
// 契约（与 eager 版一致，见 lazy_range_test.go）：
//   - 恰好 `size` 字节；到达终点返回 io.EOF（提前 EOF 即错误——流内推进到终点才 EOF）；
//   - 中途段读/解密失败 → 流返回错误（fail-closed，不静默截断成短流）；
//   - Close 幂等（不要求读完）。

import (
	"context"
	"io"

	"github.com/cocomhub/sproxy/pkg/audit"
	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// rangeStream 是惰性流式随机读句柄：逐块推进（块 = 加密分块），每块经 readChunk 拉最小
// 目标段密文并解密后交付。位置状态在流内（不修改 meta 索引）。
//
// 审计 span 在**首次推进时**才开始、在 EOF/失败/Close 时结束（惰性语义要求构造期不开 span：
// OpenRangeRead 返回后可能长时间不读或直接关闭）。
type rangeStream struct {
	// readChunk 拉取单个分块在目标区间内的明文（闭包捕获 ctx / FS / 元信息）。
	readChunk func(ci shardseal.ChunkInfo) ([]byte, error)
	// beginSpan 开审计 span（闭包捕获 ctx；无审计服务时返回 nil）。
	beginSpan func() *audit.Step
	// offset/end 是目标区间（明文坐标，半开）。
	offset int64
	end    int64
	// remaining 是尚未交付的明文长度（区间内）；到 0 才 EOF。
	remaining int64
	// chunks 是目标区间相交的分块（升序；构造时预筛）。
	chunks []shardseal.ChunkInfo
	ciIdx  int
	// cur 是当前块已物化的明文（readChunk 对单块区间的结果，已裁切）。
	cur    []byte
	curOff int
	// delivered 是已交付明文累计（审计 span 的 bytes 计量）。
	delivered int64
	err       error
	done      bool
	span      *audit.Step // 首次 Read 时 Begin（惰性）
}

var (
	_ io.Reader = (*rangeStream)(nil)
	_ io.Closer = (*rangeStream)(nil)
)

// Read 惰性推进：当前块未耗尽则交付；否则拉下一相交块（readChunk：按目标段只下载/解密含
// 区间的最小段密文）。块间无依赖，逐块「读→解→交」，首字节只需第一块。
func (r *rangeStream) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.span == nil {
		r.span = r.beginSpan() // 惰性：首次推进才开 span
	}
	for {
		if r.curOff < len(r.cur) {
			n := copy(p, r.cur[r.curOff:])
			r.curOff += n
			r.remaining -= int64(n)
			r.delivered += int64(n)
			if r.remaining <= 0 {
				r.done = true
				r.finishSpan(nil)
				return n, io.EOF // 恰好交付 size 字节后 EOF
			}
			return n, nil
		}
		if r.ciIdx >= len(r.chunks) {
			// 已无更多块但 remaining>0（块覆盖不足——正常文件不会发生；防御）。
			r.err = io.ErrUnexpectedEOF
			r.finishSpan(r.err)
			return 0, r.err
		}
		ci := r.chunks[r.ciIdx]
		r.ciIdx++
		seg, serr := r.readChunk(ci)
		if serr != nil {
			r.err = serr
			r.finishSpan(serr)
			return 0, serr
		}
		if len(seg) == 0 {
			continue
		}
		r.cur, r.curOff = seg, 0
	}
}

// finishSpan 结束审计 span（幂等；err 非 nil 记失败）。
func (r *rangeStream) finishSpan(err error) {
	if r.span == nil {
		return
	}
	if err != nil {
		r.span.Fail(err)
	} else {
		r.span.End("bytes", r.delivered)
	}
	r.span = nil
}

// Close 幂等关闭（不做任何清理——流状态只读索引与 ctx 闭包）；未读完即关闭也结束 span。
func (r *rangeStream) Close() error {
	r.done = true
	r.finishSpan(nil)
	return nil
}

// newRangeStream 构造惰性流：预筛目标区间相交的分块（升序），其余状态留到 Read 推进。
//
// ctx 捕获进 readChunk/beginSpan 闭包（struct 不持 ctx 字段，与 sync.RangeSeeker 同范式）。
func newRangeStream(ctx context.Context, s *SecretdataFS, e *metaEntry, keyBytes, salt []byte, offset, size int64) *rangeStream {
	end := offset + size
	chunks := make([]shardseal.ChunkInfo, 0, len(e.meta.Chunks))
	for _, ci := range e.meta.Chunks {
		if ci.Offset >= end || ci.Offset+ci.OrigSize <= offset {
			continue
		}
		chunks = append(chunks, ci)
	}
	return &rangeStream{
		readChunk: func(ci shardseal.ChunkInfo) ([]byte, error) {
			return s.readChunkRangeBytes(ctx, e, keyBytes, salt, ci, offset, end)
		},
		beginSpan: func() *audit.Step { return audit.BeginSpan(ctx, "decrypt") },
		offset:    offset,
		end:       end,
		remaining: size,
		chunks:    chunks,
	}
}
