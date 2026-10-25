// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// verify.go 实现**流式下载校验**（信任保证演进：普通下载路径读回比对 meta，异常
// fail-closed——跨信任边界的静默损坏在下发到客户端前被逐分块拦截）。
//
// 设计：包装 io.ReadSeeker（http.ServeContent 消费）——读流时按 FileMeta 分块边界
// 逐块算 sha256 与 fm.Chunks[i].SHA256 比对。**只有从块首开始完整读到的块才校验**
// （Range 从中间开始的块首缺数据，无法验证部分块，跳过不误报）。EOF 时校验总覆盖：
//   - **底层内容长度**（构造时 SeekEnd 探测）必须等于 fm.Size——截断/追加立即 fail-closed；
//   - **全量读**（从 offset 0 读满 fm.Size）比对整文件 TotalSHA256；
//   - **中段读**（Range 从 N>0 起）不比对整文件哈希（只覆盖后缀），也不判短读。
//
// 任一不匹配 → 返回错误（ServeContent 中断，不把损坏内容发给客户端）。
//
// 与 Calculator（写时算）互逆：Calculator 是写入流式累计，本原语是读取流式校验。

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
)

// ErrStaleMeta 标记「sidecar 陈旧」：解析成功但 FileMeta.Size 与实测内容长度不一致。
//
// 典型成因是**两次独立原子写之间的崩溃窗口**（主文件已落新内容、sidecar 尚未更新），
// 也可能是内容被截断/追加。两类在**信任语义上无法区分**（都无法提供可靠的完整性证据），
// 故调用方口径不变（拒绝下发未校验内容，C2），但可用 `errors.Is(err, ErrStaleMeta)` 把它
// 与「sidecar 损坏（解析失败/超限）」区分开，给出**可操作**告警与修复指引（重建 sidecar）。
var ErrStaleMeta = errors.New("meta: sidecar 陈旧（内容长度与 FileMeta.Size 不一致）")

// VerifyReadSeeker 包装底层 ReadSeeker 为"读时逐分块校验"流（异常 fail-closed）。
// fm 为下载文件的 FileMeta；底层须支持 Seek（Range）。返回保持 Seek 语义，
// 校验状态随 Seek 重置（从新位置重新累计，跨块完整读仍校验）。
//
// 构造时探测底层内容长度（SeekEnd）并与 fm.Size 比对：不一致（截断/追加）→ 记录
// fatal，首次 Read 即返回错误（不等读到 EOF——ServeContent 的 Stat/CopyN 精确读满
// 时本就不会产生底层 EOF 的那次 Read，仅靠 EOF 判据会漏掉截断）。SeekEnd 失败
// （极端定制 reader）→ 退化为流式判据（EOF 时判短读/总哈希）。
func VerifyReadSeeker(r io.ReadSeeker, fm *FileMeta) io.ReadSeeker {
	return VerifyReadSeekerWithOpts(r, fm, VerifyOpts{})
}

// VerifyOpts 是 VerifyReadSeeker 的可选行为开关。
type VerifyOpts struct {
	// SkipChunkAhead 跳过 verifyChunkAhead 的「整块回读式先验」（2026-10-25 性能修复）：
	// 调用方确认底层数据源**读路径自带逐段校验**（如 secretdata 的窗口密文 sha256 +
	// 逐 blocklet GCM）时置 true——那些校验已等价覆盖，回读纯属冗余，且对外部卷大分块
	// 会按「每 Range 请求回读一整分块（32MiB）」放大 N 倍（实测 2.1GB 下载 8-20 倍）。
	//
	// 语义边界：只跳过回读式先验；流式 consume 逐块校验与 EOF 总哈希校验仍保留。
	SkipChunkAhead bool
}

// VerifyReadSeekerWithOpts 同 VerifyReadSeeker，按 opts 调整行为。
func VerifyReadSeekerWithOpts(r io.ReadSeeker, fm *FileMeta, opts VerifyOpts) io.ReadSeeker {
	v := &verifyReadSeeker{inner: r, fm: fm, curChunk: -1, skipChunkAhead: opts.SkipChunkAhead}
	// 入口 fail-closed：畸形 meta（分块重叠/空洞/负 size/哈希格式错…）不得进入流式
	// 校验（可能致 slice 越界 panic 或校验被静默弱化）；与 cloud/transfer.go 的 M7a
	// 同口径（P3 对抗评审）。
	if fm == nil {
		v.fatal = fmt.Errorf("meta: 校验失败：FileMeta 为空")
		return v
	}
	if err := Validate(fm); err != nil {
		v.fatal = fmt.Errorf("meta: 校验失败：FileMeta 非法: %w", err)
		return v
	}
	if end, err := r.Seek(0, io.SeekEnd); err == nil {
		if end != fm.Size {
			v.fatal = fmt.Errorf("meta: 校验失败：底层内容长度 %d 与 FileMeta.Size %d 不一致（截断/追加）: %w", end, fm.Size, ErrStaleMeta)
		}
		_, _ = r.Seek(0, io.SeekStart)
	}
	v.reset(0)
	return v
}

// verifyReadSeeker 是逐分块校验 ReadSeeker。
type verifyReadSeeker struct {
	inner     io.ReadSeeker
	fm        *FileMeta
	curChunk  int       // 当前累计分块索引（-1 = 未定位）
	curSHA    hash.Hash // 当前分块已累计哈希
	curRead   int64     // 当前分块已读字节
	skipCur   bool      // 当前块从中间开始（块首缺数据，读满也不校验）
	totalSHA  hash.Hash // 整文件已读哈希累计（全量读时比对）
	totalRead int64     // 已读总字节（绝对文件偏移）
	verified  bool      // EOF 已校验（防二次读）
	extra     bool      // 已读超过 fm.Size（多余数据）
	fromStart bool      // 本次累计是否从 offset 0 起（仅此才比对整文件哈希/判短读）
	fatal     error     // 构造时探测到的致命不一致（长度 != fm.Size）
	// aheadTried/aheadVerified 服务「先验后发」（C3）：从块中间起始时把**整块**读回比对，
	// 每位置只试一次；aheadVerified=true 表示该块已整体验过（consume 读满时不再重复比对）。
	aheadTried    bool
	aheadVerified bool
	// aheadFailed 标记先验不可行（底层不可 Seek/短读）→ 该块维持跳过（降级，不误报）。
	aheadFailed bool
	// skipChunkAhead 标记跳过 verifyChunkAhead 的「整块回读式先验」（2026-10-25 性能修复，
	// 见 VerifyReadSeekerWithOpts 头注释）。
	skipChunkAhead bool
	// expectEnd 是调用方声明的「本次下发的最后字节位置」（独占；0 = 未告知）。用于在
	// **Range 结束于块中间**时于 Close 补验尾部半块——C3 的 sealPartialChunk 只由 EOF 触发，
	// 块边界对齐且不读到 EOF 的 Range 会整段零校验（第 10 轮评审 P1）。
	expectEnd int64
}

// RangeVerifier 是「Range 端点提示」可选能力：调用方在知道本次下发的最后字节位置时告知，
// 使 Close() 能补验「Range 在块中间结束」的尾部半块。未告知 = Close 不做额外校验（行为不变，
// 避免客户端中途断开被误判为校验失败）。
type RangeVerifier interface{ SetExpectedEnd(end int64) }

// SetExpectedEnd 声明本次下发的**独占**结束偏移（end<=0 表示清除提示）。
func (v *verifyReadSeeker) SetExpectedEnd(end int64) { v.expectEnd = end }

// Read 读数据并逐分块校验。返回错误 = 分块不一致 / 长度不一致 / 多余数据 / EOF 总校验失败。
func (v *verifyReadSeeker) Read(p []byte) (int, error) {
	if v.fatal != nil {
		return 0, v.fatal
	}
	if v.extra {
		return 0, fmt.Errorf("meta: 校验失败：内容超过 FileMeta.Size %d（多余数据）", v.fm.Size)
	}
	v.verifyChunkAhead()
	if v.fatal != nil {
		return 0, v.fatal // 先验后发发现损坏：本批字节一个都不下发
	}
	n, err := v.inner.Read(p)
	if n > 0 {
		if verr := v.consume(p[:n]); verr != nil {
			return 0, verr // 发现损坏即不交付本批字节（fail-closed）
		}
	}
	if err == io.EOF {
		if verr := v.verifyEOF(); verr != nil {
			return n, verr
		}
		return n, io.EOF
	}
	return n, err
}

// Seek 重置校验状态到底层新位置（Range 支持）：按绝对位置定位分块、块内已读偏移。
func (v *verifyReadSeeker) Seek(offset int64, whence int) (int64, error) {
	pos, err := v.inner.Seek(offset, whence)
	if err != nil {
		return 0, err
	}
	v.reset(pos)
	return pos, nil
}

// reset 按绝对位置 pos 初始化校验状态。
func (v *verifyReadSeeker) reset(pos int64) {
	v.curChunk, v.curRead, v.skipCur = v.chunkPos(pos)
	v.curSHA = sha256.New()
	v.totalSHA = sha256.New()
	v.totalRead = pos
	v.fromStart = pos == 0
	v.verified = false
	v.extra = false
	v.aheadTried = false
	v.aheadVerified = false
	v.aheadFailed = false
}

// chunkPos 由绝对位置推算 (分块索引, 块内已读, 是否从中间开始)。
func (v *verifyReadSeeker) chunkPos(pos int64) (int, int64, bool) {
	for i, c := range v.fm.Chunks {
		if pos >= c.Offset && pos < c.Offset+c.Size {
			return i, pos - c.Offset, pos > c.Offset // 从块首（Offset）开始才可校验
		}
	}
	// pos >= Size（越界/EOF 后）：多余数据判定由 consume/extra 处理。
	return len(v.fm.Chunks), 0, true
}

// verifyChunkAhead 对「从块中间起始」的当前块做**先验后发**（C3 修复，P0）：seek 回块首
// 读满整块、重算 sha256 与 meta 比对——不一致立即 fail-closed（本批字节都不下发），
// 一致则复位底层到原位置继续正常读。
//
// 背景：此前 `chunkPos` 对中段起始把 `skipCur` 置真，读满也不校验，且 `verifyEOF` 对
// `!fromStart` 直接 return nil ⇒ 起点非 0、又不覆盖整块的 Range（单块/粗分块文件尤甚）
// **零校验**下发——「异常 fail-closed」承诺在 Range 路径瓦解。
//
// 成本：仅在首块（中段起始）多一次整块读（≤32MiB）；后续块从块首起，走常规流式校验。
// 失败（不可 Seek/短读）→ 维持跳过（降级，不误报也不阻断）；每位置只试一次。
//
// **segmentVerified 跳过（2026-10-25 性能修复）**：底层 FS 自带逐段校验（窗口 sha256 +
// GCM）时，本次回读纯属冗余（且对外部卷大分块 = 每 Range 请求回读 32MiB）→ 直接跳过。
func (v *verifyReadSeeker) verifyChunkAhead() {
	if v.skipChunkAhead {
		return
	}
	if !v.skipCur || v.aheadTried || v.aheadFailed || v.verified || v.fatal != nil {
		return
	}
	if v.curChunk < 0 || v.curChunk >= len(v.fm.Chunks) || v.curRead <= 0 {
		return
	}
	v.aheadTried = true
	chunk := v.fm.Chunks[v.curChunk]
	pos := chunk.Offset + v.curRead
	if _, err := v.inner.Seek(chunk.Offset, io.SeekStart); err != nil {
		v.aheadFailed = true
		return
	}
	h := sha256.New()
	remaining := chunk.Size
	buf := make([]byte, 64<<10)
	for remaining > 0 {
		want := min(int64(len(buf)), remaining)
		n, rerr := v.inner.Read(buf[:want])
		if n > 0 {
			_, _ = h.Write(buf[:n])
			remaining -= int64(n)
		}
		if rerr != nil {
			break
		}
	}
	if remaining > 0 {
		v.aheadFailed = true // 短读/出错：不可验 → 维持跳过
		_, _ = v.inner.Seek(pos, io.SeekStart)
		return
	}
	if _, err := v.inner.Seek(pos, io.SeekStart); err != nil {
		v.aheadFailed = true
		return
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != chunk.SHA256 {
		v.fatal = fmt.Errorf("meta: 校验失败：分块 %d 内容不一致（卷静默损坏？）", v.curChunk)
		return
	}
	// 该块已整体验过：区间内字节不再重复累计比对（见 consume 的 aheadVerified 分支）。
	v.aheadVerified = true
	v.skipCur = false
}

// sealPartialChunk 在 EOF 为**尾部半块**补验（C3）：消费者读到的最后一块可能不满整块
// （Range 在块中间结束）——此时块首字节已累计、缺尾部；seek 到块内已读位置读回到块尾，
// 补入 curSHA 后比对整块哈希（只影响校验，不动消费者可见字节/totalSHA）。
// 不可 Seek/短读 → 维持跳过（降级）。返回错误 = 该块内容不一致（fail-closed）。
func (v *verifyReadSeeker) sealPartialChunk() error {
	if v.aheadVerified || v.skipCur || v.fatal != nil {
		return nil
	}
	if v.curChunk < 0 || v.curChunk >= len(v.fm.Chunks) || v.curRead <= 0 {
		return nil
	}
	chunk := v.fm.Chunks[v.curChunk]
	if v.curRead >= chunk.Size {
		return nil // 已满块（consume 已比对）
	}
	pos := chunk.Offset + v.curRead
	if _, err := v.inner.Seek(pos, io.SeekStart); err != nil {
		return nil // 不可 Seek：降级（不误报）
	}
	remaining := chunk.Size - v.curRead
	buf := make([]byte, 64<<10)
	for remaining > 0 {
		want := min(int64(len(buf)), remaining)
		n, rerr := v.inner.Read(buf[:want])
		if n > 0 {
			_, _ = v.curSHA.Write(buf[:n])
			remaining -= int64(n)
		}
		if rerr != nil {
			break
		}
	}
	if remaining > 0 {
		return nil // 短读：降级（不误报）
	}
	if got := hex.EncodeToString(v.curSHA.Sum(nil)); got != chunk.SHA256 {
		return fmt.Errorf("meta: 校验失败：分块 %d 内容不一致（卷静默损坏？）", v.curChunk)
	}
	return nil
}

// consume 把读入数据按分块边界喂入哈希；块从块首开始完整读满 → 校验。
// 越界（所有分块读完仍有数据）**立即返回错误**（不延迟到下一次 Read——io.CopyN
// 消费者精确读满即止，下一次 Read 不会发生，延迟报错会静默放行越界字节）。
func (v *verifyReadSeeker) consume(data []byte) error {
	for len(data) > 0 {
		if v.curChunk >= len(v.fm.Chunks) {
			v.extra = true // 所有分块读完仍有数据
			return fmt.Errorf("meta: 校验失败：内容超过 FileMeta.Size %d（多余数据）", v.fm.Size)
		}
		want := v.fm.Chunks[v.curChunk].Size - v.curRead
		part := data
		if int64(len(part)) > want {
			part = data[:want]
		}
		_, _ = v.curSHA.Write(part)
		_, _ = v.totalSHA.Write(part)
		v.curRead += int64(len(part))
		v.totalRead += int64(len(part))
		data = data[len(part):]
		if v.curRead == v.fm.Chunks[v.curChunk].Size {
			// 块读满：从块首开始才校验（Range 中间块跳过不误报）；**先验后发**过的块
			// （C3：中段起始块已整体比对过）不再重复比对——否则只累计了区间内字节的
			// curSHA 与整块哈希必然不等，会误报。
			if v.aheadVerified {
				v.aheadVerified = false
			} else if !v.skipCur {
				wantSHA := v.fm.Chunks[v.curChunk].SHA256
				if got := hex.EncodeToString(v.curSHA.Sum(nil)); got != wantSHA {
					return fmt.Errorf("meta: 校验失败：分块 %d 内容不一致（卷静默损坏？）", v.curChunk)
				}
			}
			v.curChunk++
			v.curSHA = sha256.New()
			v.curRead = 0
			v.skipCur = false // 后续块从块首开始，正常校验
		}
	}
	return nil
}

// verifyEOF EOF 总校验：长度不一致/多余数据拒绝；全量读（从 offset 0 读满）比对整
// 文件 TotalSHA256；短读（从 offset 0 读但不足 fm.Size）也拒绝；中段起始读（Range）
// 只依赖构造时的 extent 探测与逐块校验，不比对整文件哈希（只覆盖后缀会误报）。
// **未满块不校验**——Range 中间块或尾块不完整，无法验证部分块（不误报）。
func (v *verifyReadSeeker) verifyEOF() error {
	if v.fatal != nil {
		return v.fatal
	}
	if v.verified {
		return nil
	}
	v.verified = true
	if v.extra {
		return fmt.Errorf("meta: 校验失败：内容超过 FileMeta.Size %d", v.fm.Size)
	}
	if serr := v.sealPartialChunk(); serr != nil {
		return serr
	}
	if v.expectEnd > 0 && v.expectEnd < v.fm.Size {
		// **部分 Range（端点 < 整文件）**：调用方明确声明本次只下发 [0, expectEnd)，尾部半块
		// 已由 sealPartialChunk 补验 ⇒ 短读/整文件哈希判据不适用（Range 语义）。
		// 缺此判据时 `Range: bytes=0-1023` 这类**从 0 开始但不覆盖整文件**的请求会在 Close
		// 收尾时被判「内容长度 1024 短于 FileMeta.Size …（截断/静默丢失）」⇒ 中断连接
		// （客户端 EOF）——播放器从 0 seek 的 Range 全被误杀。
		return nil
	}
	if !v.fromStart {
		return nil // 中段起始读：整文件哈希/短读判据不适用（Range 语义）
	}
	if v.totalRead < v.fm.Size {
		return fmt.Errorf("meta: 校验失败：内容长度 %d 短于 FileMeta.Size %d（截断/静默丢失）", v.totalRead, v.fm.Size)
	}
	if v.totalRead == v.fm.Size && v.fm.TotalSHA256 != "" {
		if got := hex.EncodeToString(v.totalSHA.Sum(nil)); got != v.fm.TotalSHA256 {
			return fmt.Errorf("meta: 校验失败：整文件哈希不一致（卷静默损坏？）")
		}
	}
	return nil
}

// Close 委托底层（标准库风格自动补 Close：底层支持 io.Closer 则关闭，不支持则 no-op
// ——SeekRead 无 Close 的流经包装后恒可 Close，ServeContent/调用方统一关闭语义）。
// Close 关闭底层流；若调用方声明过 Range 端点且**恰好读满该端点**（≤ fm.Size），则在此补验
// 尾部半块（sealPartialChunk 语义）——覆盖「块边界对齐、Range 在块中间结束」这一零校验分支。
// 未读满端点（客户端中途断开/上层提前停止）→ 不校验（避免把「提前停止」误判为损坏）。
// 校验错误优先于底层关闭错误返回（调用方据此中断连接）。
func (v *verifyReadSeeker) Close() error {
	var verr error
	if v.expectEnd > 0 && v.expectEnd <= v.fm.Size && v.totalRead == v.expectEnd && !v.verified && v.fatal == nil {
		verr = v.verifyEOF()
	}
	if c, ok := v.inner.(io.Closer); ok {
		if cerr := c.Close(); cerr != nil && verr == nil {
			return cerr
		}
	}
	return verr
}
