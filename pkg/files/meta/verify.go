// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package meta

// verify.go 实现**流式下载校验**（信任保证演进：普通下载路径读回比对 meta，异常
// fail-closed——跨信任边界的静默损坏在下发到客户端前被逐分块拦截）。
//
// 设计：包装 io.ReadSeeker（http.ServeContent 消费）——读流时按 FileMeta 分块边界
// 逐块算 sha256 与 fm.Chunks[i].SHA256 比对。**只有从块首开始完整读到的块才校验**
// （Range 从中间开始的块首缺数据，无法验证部分块，跳过不误报）。EOF 时校验总覆盖
// （实际读字节 == fm.Size 才比对整文件 TotalSHA256；多余字节拒绝）。任一不匹配 →
// 返回错误（ServeContent 中断，不把损坏内容发给客户端）。
//
// 与 Calculator（写时算）互逆：Calculator 是写入流式累计，本原语是读取流式校验。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
)

// VerifyReadSeeker 包装底层 ReadSeeker 为"读时逐分块校验"流（异常 fail-closed）。
// fm 为下载文件的 FileMeta；底层须支持 Seek（Range）。返回保持 Seek 语义，
// 校验状态随 Seek 重置（从新位置重新累计，跨块完整读仍校验）。
func VerifyReadSeeker(r io.ReadSeeker, fm *FileMeta) io.ReadSeeker {
	v := &verifyReadSeeker{inner: r, fm: fm, curChunk: -1}
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
}

// Read 读数据并逐分块校验。返回错误 = 分块不一致 / 多余数据 / EOF 总校验失败。
func (v *verifyReadSeeker) Read(p []byte) (int, error) {
	if v.extra {
		return 0, fmt.Errorf("meta: 校验失败：内容超过 FileMeta.Size %d（多余数据）", v.fm.Size)
	}
	n, err := v.inner.Read(p)
	if n > 0 {
		if verr := v.consume(p[:n]); verr != nil {
			return 0, verr
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
	v.verified = false
	v.extra = false
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

// consume 把读入数据按分块边界喂入哈希；块从块首开始完整读满 → 校验。
func (v *verifyReadSeeker) consume(data []byte) error {
	for len(data) > 0 {
		if v.curChunk >= len(v.fm.Chunks) {
			v.extra = true // 所有分块读完仍有数据
			return nil
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
			// 块读满：从块首开始才校验（Range 中间块跳过不误报）。
			if !v.skipCur {
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

// verifyEOF EOF 总校验：多余数据拒绝；全量读（== fm.Size）比对整文件 TotalSHA256。
// **未满块不校验**——Range 中间块或尾块不完整，无法验证部分块（不误报）；
// 全量完整性由 TotalSHA256（totalRead==fm.Size 才可比对）覆盖。
func (v *verifyReadSeeker) verifyEOF() error {
	if v.verified {
		return nil
	}
	v.verified = true
	if v.extra {
		return fmt.Errorf("meta: 校验失败：内容超过 FileMeta.Size %d", v.fm.Size)
	}
	// 整文件哈希只在全量读时可比对（totalRead == fm.Size）。
	if v.totalRead == v.fm.Size && v.fm.TotalSHA256 != "" {
		if got := hex.EncodeToString(v.totalSHA.Sum(nil)); got != v.fm.TotalSHA256 {
			return fmt.Errorf("meta: 校验失败：整文件哈希不一致（卷静默损坏？）")
		}
	}
	return nil
}

// Close 委托底层（标准库风格自动补 Close：底层支持 io.Closer 则关闭，不支持则 no-op
// ——SeekRead 无 Close 的流经包装后恒可 Close，ServeContent/调用方统一关闭语义）。
func (v *verifyReadSeeker) Close() error {
	if c, ok := v.inner.(io.Closer); ok {
		return c.Close()
	}
	return nil
}
