// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package e2ee 是 sclient 客户端 E2EE（roadmap P2 at-rest 加密残余）：
// 零知识——上传前 AES-256-GCM 流式加密（64KiB 块 + 每块随机 nonce + 魔数头，
// 格式与 pkg/files cipher 互通），下载后解密。纯标准库不引第三方。
package e2ee

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
)

// Magic 是加密流魔数头（与 pkg/files cipher 同值互通）。
var Magic = []byte("SPROXY-CIPHER-v1")

// NewEncryptWriter 返回流式加密 writer（写完需 Close）。
func NewEncryptWriter(key []byte, w io.Writer) (io.WriteCloser, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("e2ee: 密钥必须 32B")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(Magic); err != nil {
		return nil, err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], 64<<10)
	if _, err := w.Write(sz[:]); err != nil {
		return nil, err
	}
	return &encWriter{gcm: gcm, w: w, chunk: 64 << 10}, nil
}

type encWriter struct {
	gcm   cipher.AEAD
	w     io.Writer
	buf   bytes.Buffer
	chunk int
}

func (w *encWriter) Write(p []byte) (int, error) {
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

func (w *encWriter) encryptChunk(plain []byte) error {
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

func (w *encWriter) Close() error {
	if w.buf.Len() > 0 {
		if err := w.encryptChunk(w.buf.Bytes()); err != nil {
			return err
		}
		w.buf.Reset()
	}
	return nil
}

// NewDecryptReader 返回流式解密 reader（读完需 Close）。
func NewDecryptReader(key []byte, r io.Reader) (io.Reader, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("e2ee: 密钥必须 32B")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	magic := make([]byte, len(Magic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("e2ee: 读加密头: %w", err)
	}
	if !bytes.Equal(magic, Magic) {
		return nil, fmt.Errorf("e2ee: 非加密格式（魔数不匹配）")
	}
	var sz [4]byte
	if _, err := io.ReadFull(r, sz[:]); err != nil {
		return nil, fmt.Errorf("e2ee: 读块大小: %w", err)
	}
	chunk := int(binary.BigEndian.Uint32(sz[:]))
	if chunk <= 0 {
		return nil, fmt.Errorf("e2ee: 非法块大小 %d", chunk)
	}
	return &decReader{gcm: gcm, r: r, chunk: chunk}, nil
}

type decReader struct {
	gcm    cipher.AEAD
	r      io.Reader
	chunk  int
	remain []byte
	done   bool
}

func (r *decReader) Read(p []byte) (int, error) {
	if len(r.remain) > 0 {
		n := copy(p, r.remain)
		r.remain = r.remain[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	var lenBuf [4]byte
	if _, err := io.ReadFull(r.r, lenBuf[:]); err != nil {
		if err == io.EOF {
			r.done = true
			return 0, io.EOF
		}
		return 0, err
	}
	l := int(binary.BigEndian.Uint32(lenBuf[:]))
	if l <= 0 || l > r.chunk+32 {
		return 0, fmt.Errorf("e2ee: 非法密文块长 %d", l)
	}
	blob := make([]byte, l)
	if _, err := io.ReadFull(r.r, blob); err != nil {
		return 0, err
	}
	nonceSize := r.gcm.NonceSize()
	if l < nonceSize {
		return 0, fmt.Errorf("e2ee: 密文块过短")
	}
	plain, err := r.gcm.Open(nil, blob[:nonceSize], blob[nonceSize:], nil)
	if err != nil {
		return 0, fmt.Errorf("e2ee: 解密失败: %w", err)
	}
	n := copy(p, plain)
	if n < len(plain) {
		r.remain = plain[n:]
	}
	return n, nil
}
