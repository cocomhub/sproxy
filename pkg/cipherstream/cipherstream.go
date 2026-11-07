// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cipherstream 是**分块 AES-256-GCM 流加密格式**的唯一实现。
//
// 格式（三个使用方此前各自实现过一份，逐字相同）：
//
//	头：   [magic][4B BE 块大小]
//	每块： [4B BE len(nonce+ciphertext+tag)][nonce(12B)][ciphertext][tag(16B)]
//
// **魔数由调用方提供**：客户端 E2EE、服务端 at-rest 加密卷、归档 cipher 是三个**不同密钥域**，
// 魔数不同是有意的域分隔（同一份密文不会被另一域误读）。帧布局则必须完全一致——
// 否则一端改布局，另一端写出的历史数据就解不出（失败点离改动很远）。
//
// 为什么放在 pkg/：`cmd/sclient` 是独立 module，其 `internal/` 不能被根模块 import，
// 但**它可以 import 根模块的 pkg/**。此前客户端因此复制了一份实现，靠一个「两份代码必须
// 逐字一致」的源码门禁防分叉（R57）——那是重复的症状，不是解法。

package cipherstream

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// MaxHeaderChunk 是头部声明的块大小上界（防畸形头导致超大分配）。
const MaxHeaderChunk = 64 << 20

// gcmOverhead 是非前缀开销上界（nonce 12B + tag 16B = 28B；留 4B 余量）。
const gcmOverhead = 32

// Config 描述一个加密流格式实例。
type Config struct {
	// Magic 是域分隔魔数（写入头部、读取时严格校验）。
	Magic []byte
	// ChunkSize 是写侧块大小（字节）；<=0 时用 64 KiB。
	ChunkSize int
	// VerifyChunkSize 为真时，读侧要求头部块大小与 ChunkSize 相等（归档 cipher 的历史语义）；
	// 为假时以头部声明为准（加密卷/E2EE 的历史语义）。
	VerifyChunkSize bool
	// Label 是错误信息前缀（"cipher" / "encrypted" / "e2ee"），保持各调用方的告警口径。
	Label string
}

func (c Config) chunkSize() int {
	if c.ChunkSize <= 0 {
		return 64 << 10
	}
	return c.ChunkSize
}

func (c Config) label() string {
	if c.Label == "" {
		return "cipherstream"
	}
	return c.Label
}

func (c Config) newGCM(key []byte) (cipher.AEAD, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("%s: 密钥必须 32B", c.label())
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.label(), err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", c.label(), err)
	}
	return gcm, nil
}

// NewWriter 返回流式加密 writer（**写完必须 Close**：Close 冲刷最后一块）。
func NewWriter(key []byte, w io.Writer, cfg Config) (io.WriteCloser, error) {
	gcm, err := cfg.newGCM(key)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(cfg.Magic); err != nil {
		return nil, err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(cfg.chunkSize()))
	if _, err := w.Write(sz[:]); err != nil {
		return nil, err
	}
	return &writer{gcm: gcm, w: w, chunk: cfg.chunkSize(), label: cfg.label()}, nil
}

type writer struct {
	gcm   cipher.AEAD
	w     io.Writer
	buf   bytes.Buffer
	chunk int
	label string
}

func (w *writer) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for w.buf.Len() >= w.chunk {
		chunk := make([]byte, w.chunk)
		if _, err := io.ReadFull(&w.buf, chunk); err != nil {
			return 0, err
		}
		if err := w.encryptChunk(chunk); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (w *writer) encryptChunk(plain []byte) error {
	nonce := make([]byte, w.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := w.gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 4+len(nonce)+len(ct))
	binary.BigEndian.PutUint32(out[:4], uint32(len(nonce)+len(ct)))
	copy(out[4:], nonce)
	copy(out[4+len(nonce):], ct)
	_, err := w.w.Write(out)
	return err
}

func (w *writer) Close() error {
	if w.buf.Len() > 0 {
		if err := w.encryptChunk(w.buf.Bytes()); err != nil {
			return err
		}
		w.buf.Reset()
	}
	return nil
}

// NewReader 返回流式解密 reader（读 NewWriter 的产物）。
func NewReader(key []byte, r io.Reader, cfg Config) (io.Reader, error) {
	head := make([]byte, len(cfg.Magic)+4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("%s: 加密头读取失败: %w", cfg.label(), err)
	}
	if !bytes.Equal(head[:len(cfg.Magic)], cfg.Magic) {
		return nil, fmt.Errorf("%s: 非加密格式（魔数不匹配）", cfg.label())
	}
	chunk := int(binary.BigEndian.Uint32(head[len(cfg.Magic):]))
	if chunk <= 0 || chunk > MaxHeaderChunk {
		return nil, fmt.Errorf("%s: 非法块大小 %d", cfg.label(), chunk)
	}
	if cfg.VerifyChunkSize && chunk != cfg.chunkSize() {
		return nil, fmt.Errorf("%s: 块大小不匹配 %d != %d", cfg.label(), chunk, cfg.chunkSize())
	}
	gcm, err := cfg.newGCM(key)
	if err != nil {
		return nil, err
	}
	return &reader{gcm: gcm, r: r, chunk: chunk, label: cfg.label()}, nil
}

type reader struct {
	gcm     cipher.AEAD
	r       io.Reader
	chunk   int
	label   string
	pending []byte
}

func (r *reader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		plain, err := r.readFrame()
		if err != nil {
			return 0, err
		}
		r.pending = plain
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

func (r *reader) readFrame() ([]byte, error) {
	var ln [4]byte
	if _, err := io.ReadFull(r.r, ln[:]); err != nil {
		if err == io.EOF {
			return nil, io.EOF
		}
		return nil, err
	}
	blen := int(binary.BigEndian.Uint32(ln[:]))
	if blen <= 0 || blen > r.chunk+gcmOverhead {
		return nil, fmt.Errorf("%s: 非法密文块长 %d", r.label, blen)
	}
	enc := make([]byte, blen)
	if _, err := io.ReadFull(r.r, enc); err != nil {
		return nil, err
	}
	ns := r.gcm.NonceSize()
	if len(enc) < ns {
		return nil, fmt.Errorf("%s: 密文块过短", r.label)
	}
	plain, err := r.gcm.Open(nil, enc[:ns], enc[ns:], nil)
	if err != nil {
		return nil, fmt.Errorf("%s: 解密失败（密钥错或密文篡改）: %w", r.label, err)
	}
	return plain, nil
}
