// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cipherstream

// random_access.go 是分块加密格式的**随机访问与尺寸探测**能力（同一份格式知识的另一半）：
//
//   - `SeekPlain`：把解密流定位到**明文偏移**。跳过前面的块时只读 4 字节块头并 Seek，
//     **不解密**（朴素实现逐块解密丢弃 ⇒ 1 GiB 文件读末尾 1 KiB 要解 1 GiB；加密卷 Range /
//     断点续传 / 播放器拖动都走这里）。
//   - `PlaintextSize`：不解密、仅顺序走每块长度头，算出明文总长（供 Content-Length / 容量账本；
//     误用密文长度会让客户端 unexpected EOF）。
//   - `SealOverhead` / `ChunkOverhead`：由**真实 cipher 实例**派生的开销常量（长度前缀/nonce/tag），
//     供物理尺寸公式使用——避免「常量与实现漂移」。
//
// 这些能力此前在 pkg/storage 里另有一份自实现（encrypted_root.go / root.go），已收敛到此处。

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// ErrSeekUnsupported 表示底层流不支持 Seek（调用方应回落「顺序读并丢弃」路径）。
var ErrSeekUnsupported = errors.New("cipherstream: 底层流不支持 Seek")

// PlainSeeker 是「可按明文偏移定位」的解密流能力接口（*Reader 在底层可 Seek 时满足）。
type PlainSeeker interface {
	io.Reader
	// SeekPlain 把流定位到明文偏移 off；越界（含 == 明文末尾）时后续读取返回 io.EOF。
	SeekPlain(off int64) error
}

var (
	// sealOverheadOnce 缓存「nonce + GCM tag」开销（AES-256-GCM 下为 12 + 16 = 28）。
	sealOverheadOnce = sync.OnceValue(func() int {
		block, err := aes.NewCipher(make([]byte, 32))
		if err != nil {
			return 12 + 16
		}
		gcm, err := cipher.NewGCM(block)
		if err != nil {
			return 12 + 16
		}
		return gcm.NonceSize() + gcm.Overhead()
	})
	// chunkOverheadOnce 缓存每块落盘开销（4B 长度前缀 + nonce + tag；AES-256-GCM 下为 32）。
	chunkOverheadOnce = sync.OnceValue(func() int { return 4 + sealOverheadOnce() })
)

// SealOverhead 返回每块**密文体**内除明文外的开销：nonce + GCM tag。
// 用于由「密文体长度 l」反推明文长（plainLen = l - SealOverhead）。
func SealOverhead() int { return sealOverheadOnce() }

// ChunkOverhead 返回每块**落盘**的固定开销：4B 长度前缀 + nonce + GCM tag。
func ChunkOverhead() int { return chunkOverheadOnce() }

// SeekPlain 把解密流定位到**明文偏移** off（底层须实现 io.Seeker）。
//
// 定位语义：流被重置到文件头，跳过前的块只读 4 字节长度头 + Seek（不解密），命中的块
// 解密后丢弃块内前 skip 字节；后续 Read 从 off 继续。off == 明文总长时为合法位置（读到 EOF）。
func (r *Reader) SeekPlain(off int64) error {
	if off < 0 {
		return fmt.Errorf("%s: 非法明文偏移 %d", r.label, off)
	}
	sk, ok := r.r.(io.Seeker)
	if !ok {
		return ErrSeekUnsupported
	}
	r.pending = r.pending[:0]
	if _, err := sk.Seek(int64(len(r.magic))+4, io.SeekStart); err != nil {
		return err
	}
	seal := int64(SealOverhead())
	var plainSoFar int64
	for off > 0 {
		l, err := r.readBlockLen()
		if err != nil {
			return err
		}
		sealLen := l - seal
		if off < plainSoFar+sealLen {
			return r.readBlockSkip(l, off-plainSoFar)
		}
		plainSoFar += sealLen
		if _, err := sk.Seek(l, io.SeekCurrent); err != nil {
			return err
		}
	}
	return nil
}

// readBlockLen 读当前块的**密文体**长度（4B 大端）并校验上界。
func (r *Reader) readBlockLen() (int64, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r.r, lenBuf[:]); err != nil {
		if err == io.EOF {
			return 0, io.ErrUnexpectedEOF
		}
		return 0, err
	}
	l := int64(binary.BigEndian.Uint32(lenBuf[:]))
	if l <= 0 || l > int64(r.chunk)+gcmOverhead {
		return 0, fmt.Errorf("%s: 非法密文块长 %d", r.label, l)
	}
	return l, nil
}

// readBlockSkip 读取并解密**当前块**（密文体长度 l），把明文自 skip 起挂到 pending。
func (r *Reader) readBlockSkip(l, skip int64) error {
	blob := make([]byte, l)
	if _, err := io.ReadFull(r.r, blob); err != nil {
		return err
	}
	ns := r.gcm.NonceSize()
	if len(blob) < ns {
		return fmt.Errorf("%s: 密文块过短", r.label)
	}
	plain, err := r.gcm.Open(nil, blob[:ns], blob[ns:], nil)
	if err != nil {
		return fmt.Errorf("%s: 解密失败（密钥错或密文篡改）: %w", r.label, err)
	}
	if skip > int64(len(plain)) {
		return io.ErrUnexpectedEOF
	}
	r.pending = append(r.pending, plain[skip:]...)
	return nil
}

// PlaintextSize 探测密文的明文总长（**不解密**，仅顺序走每块长度头后 Seek 跳过密文体）。
//
// 格式：`[magic][4B chunk]` 重复 `[4B l][nonce|ct|tag]`，其中 `l = SealOverhead + 明文长`。
// magic 用于定位第一块（长度即 head 偏移）；上层文件是否真为密文由调用方负责（本函数只做尺寸推导）。
func PlaintextSize(rs io.ReadSeeker, magic []byte) (int64, error) {
	if _, err := rs.Seek(int64(HeaderLen(magic)), io.SeekStart); err != nil {
		return 0, err
	}
	seal := int64(SealOverhead())
	var total int64
	var lenBuf [4]byte
	for {
		if _, err := io.ReadFull(rs, lenBuf[:]); err != nil {
			if err == io.EOF {
				return total, nil
			}
			return 0, err
		}
		l := int64(binary.BigEndian.Uint32(lenBuf[:]))
		if l < seal {
			return 0, fmt.Errorf("cipherstream: 非法密文块长 %d", l)
		}
		total += l - seal
		if _, err := rs.Seek(l, io.SeekCurrent); err != nil {
			return 0, err
		}
	}
}
