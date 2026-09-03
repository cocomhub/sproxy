// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"sync"
)

// placeholderReserve 是外部下载在真实大小不可知时的占位预留（1 GiB）。
const placeholderReserve int64 = 1 << 30

// QuotaWriter 仅供外部下载（cloud download 等大小不可知路径）使用：
// 写前预留（已知 Content-Length 用其值，否则占位 1 GiB），写中 commitUp，
// 预留不够自动补留，写失败保留 reserve，Finish 结算。
//
// 语义（对齐规格「记账模型」）：
//   - 每次 Write 把本次实际写入量从 reserved 划入 committed；
//   - 本次写入量超过剩余预留时先自动补齐预留再写，补齐失败返回 ErrStorageFull
//     （且不回调已写量——reserved/written 保持本次调用前的值）；
//   - 底层 writer 写失败不退还 reserve，也不销账，已立账 account 供上层重试复用；
//   - Finish(true, oldSize)：成功——释放未用 reserve + 覆盖写 ReleaseUsage(oldSize)；
//     Finish(false, _)：放弃——ReleaseUsage(written) 回拨已 commit 部分 + 释放剩余 reserve。
type QuotaWriter struct {
	mu       sync.Mutex // 叶锁：下载 goroutine 裸调 Write 与取消/删除持 m.mu 调 Committed/Finish 并发访问，整体加锁防数据竞争
	scope    *Scope
	writer   io.Writer
	written  int64 // 已确认 committed 的实际写入量
	reserved int64 // 已立账预占（写后递减）
}

// NewQuotaWriter 创建针对 scope 的 QuotaWriter，写前预留 estimate（<=0 时用 1 GiB 占位）。
// 预留失败返回 ErrStorageFull；调用方不持有预留句柄（由 QuotaWriter 自行管理）。
func NewQuotaWriter(s *Scope, w io.Writer, estimate int64) (*QuotaWriter, error) {
	if estimate <= 0 {
		estimate = placeholderReserve
	}
	return newQuotaWriterExact(s, w, estimate)
}

// newQuotaWriterExact 创建 QuotaWriter 并精确预留 amount（size==0 时预留 0）。
func newQuotaWriterExact(s *Scope, w io.Writer, amount int64) (*QuotaWriter, error) {
	if err := s.pool.reserveUp(amount); err != nil {
		return nil, err
	}
	return &QuotaWriter{scope: s, writer: w, reserved: amount}, nil
}

// Write 把本次写入量从 reserved 划入 committed；预留不够自动补留。
// 补留失败返回 ErrStorageFull 且不回调已写量（preserve）。底层 writer 失败同样保留 reserve。
func (w *QuotaWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	amount := int64(len(p))
	if amount > w.reserved {
		if err := w.scope.pool.reserveUp(amount - w.reserved); err != nil {
			return 0, err
		}
		w.reserved = amount // 补齐到本次需要
	}
	n, err := w.writer.Write(p)
	if err != nil {
		// 写失败保留 reserve（不回调不销账），供上层重试复用同一 account。
		return n, err
	}
	n64 := int64(n)
	w.scope.pool.commitUp(n64, n64)
	w.reserved -= n64
	w.written += n64
	return n, nil
}

// SetWriter 替换底层写入目标（续传/重试复用同一 account：保留 reserved/committed 状态，
// 仅换 sink 文件句柄）。调用方保证新 writer 是同一部分文件的追加句柄（或继续写入的空句柄）。
func (w *QuotaWriter) SetWriter(dst io.Writer) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writer = dst
}

// Committed 返回本 writer 已确认 committed 的字节数（Finish/结算前调用）。
// 供调用方在结算时把已 commit 部分记入任务账本（QuotaWriter 本身 Finish 后清零）。
func (w *QuotaWriter) Committed() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.written
}

// ReleaseReserve 释放剩余 reserve 但保留已 commit 部分（写失败保留 .partial 供续传：
// 已 commit 字节继续占账，未用 reserve 归还）。本 writer 结算后不再使用。
// 幂等：reserved 已归零（重复调用/与 Finish 混用）时为空操作，防多扣。
func (w *QuotaWriter) ReleaseReserve() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.reserved <= 0 {
		return
	}
	w.scope.pool.releaseUp(w.reserved)
	w.reserved = 0
}

// Finish 结算预留。
// success=true 释放未用 reserve + 覆盖写 ReleaseUsage(oldSize)；
// success=false 放弃：ReleaseUsage(written) 回拨已 commit 部分 + 释放剩余 reserve。
func (w *QuotaWriter) Finish(success bool, oldSize int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if success {
		w.scope.pool.releaseUp(w.reserved) // 释放未用 reserve
		w.scope.ReleaseUsage(oldSize)      // 覆盖写释放旧文件占用
	} else {
		w.scope.ReleaseUsage(w.written)    // 回拨已 commit 部分
		w.scope.pool.releaseUp(w.reserved) // 释放剩余 reserve
	}
	w.reserved = 0
	w.written = 0
}

// BoundWriter 是通用限定长度写入（内部路径 seek 直写/新分片与重传分片都用），
// limit 为分片最大长度，防客户端字节越界写坏相邻分片（配合乱序安全性）。
// offset 由调用方在构造时给定（写入落于 offset+written），Writer 只保护长度。
type BoundWriter struct {
	f       *os.File
	offset  int64
	limit   int64
	written int64
}

// NewBoundWriter 创建针对文件 f、落区 [offset, offset+limit) 的限长写入器。
// written 为累计已写量（续传/重传时从 session 恢复）。offset 由调用方决策。
func NewBoundWriter(f *os.File, offset, limit, written int64) *BoundWriter {
	return &BoundWriter{f: f, offset: offset, limit: limit, written: written}
}

// Write 在 bound 内写入。room<=0 返回 io.EOF；len(p)>room 截断到 room。
// 返回值为实际写入字符数与错误（含写满 limit 后下一次调用的 io.EOF）。
func (w *BoundWriter) Write(p []byte) (int, error) {
	room := w.limit - w.written
	if room <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > room {
		p = p[:room]
	}
	n, err := w.f.WriteAt(p, w.offset+w.written)
	w.written += int64(n)
	return n, err
}

// WriteFileQuota 是内部整文件写工具（upload/archive/version/sync 本地写侧）：
// 先 TryReserve(size) 一次预留，再 io.Copy(f, r) 落地；成功 Finish(true, oldSize) 释放未用
// reserve + 覆盖写 ReleaseUsage(oldSize)；Copy 出错时 Finish(false) 回拨已 commit 部分并释放
// 剩余 reserve，返回 error（临时文件由上层清理/保留重试用，本函数不改写文件内容）。
func (s *Scope) WriteFileQuota(ctx context.Context, f *os.File, size int64, r io.Reader, oldSize int64) (int64, error) {
	_ = ctx
	w, err := newQuotaWriterExact(s, f, size)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(w, r)
	if err != nil {
		w.Finish(false, 0)
		return n, err
	}
	w.Finish(true, oldSize)
	return n, nil
}

// VerifyChunkChecksum seek 读回 f 中 offset 起的分片重算 SHA-256 hex 与 want 比较，
// 供重启逐块校验（chunk 完整复用校验）。want 为空跳过校验返回 true。
func (s *Scope) VerifyChunkChecksum(f *os.File, offset int64, want string) (bool, error) {
	if want == "" {
		return true, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	h := sha256.New()
	if _, err := io.CopyBuffer(h, f, make([]byte, 256*1024)); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}
