// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
)

// DefaultChunkSize 是 EncryptStream 默认的块大小（64 KB）。
// 每个块使用随机 nonce 独立加密。可在调用 EncryptStream 前修改此变量以调整块大小。
var DefaultChunkSize = 64 * 1024

// maxChunkLen 是 DecryptStream 接受的单帧密文最大字节数，
// 等于 DefaultChunkSize 明文 + GCM nonce(12) + tag(16) + 一点冗余。
// 用于在解析帧长度后拒绝异常巨大的 chunk，避免 make([]byte, chunkLen) 触发 OOM。
// 若运行时修改了 DefaultChunkSize，应同步更新 maxChunkLen。
var maxChunkLen = DefaultChunkSize + 64

// chunkPool 减少 chunk 级别 []byte 缓冲区的分配次数。
// 缓冲区容量为 DefaultChunkSize+64，可同时满足加密明文缓冲和解密密文缓冲需求。
var chunkPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, DefaultChunkSize+64)
		return &b
	},
}

// getBuf 从 chunkPool 获取一个至少 size 字节的 []byte。
// 返回的切片 len 为 size，cap 至少为 size。
func getBuf(size int) []byte {
	bp := chunkPool.Get().(*[]byte) //nolint:errcheck
	if cap(*bp) >= size {
		return (*bp)[:size]
	}
	// 缓冲区太小，放回池中并分配新的
	chunkPool.Put(bp)
	return make([]byte, size)
}

// putBuf 将 buf 的前 used 个字节清零后归还到 chunkPool。
// used 应为 buf 中实际被使用的字节数。
func putBuf(buf []byte, used int) {
	for i := range used {
		buf[i] = 0
	}
	chunkPool.Put(&buf)
}

// EncryptStream 从 r 中分块读取数据，使用 AES-256-GCM 独立加密每个块，
// 并将帧格式的密文写入 w。
//
// 帧格式：每个块先写入 4 字节大端序的密文长度，再写入 [nonce(12字节) | ciphertext | tag(16字节)]。
// 每个块使用随机 nonce 独立加密，块大小为 DefaultChunkSize（64KB），
// 最后一个块可能小于该值。
//
// 返回写入 w 的总字节数。当 r 返回 io.EOF 时正常结束。
//
// EncryptStream 等价于 EncryptStreamWithChunkSize(key, r, w, DefaultChunkSize)。
func EncryptStream(key []byte, r io.Reader, w io.Writer) (int64, error) {
	return EncryptStreamWithChunkSize(key, r, w, DefaultChunkSize)
}

// EncryptStreamWithChunkSize 与 EncryptStream 行为一致，但允许指定块大小 chunkSize。
//
// chunkSize 每块的明文大小。较大的块可降低帧头（4 字节长度 + nonce）开销，
// 但会增大单次分配和加密延迟。
func EncryptStreamWithChunkSize(key []byte, r io.Reader, w io.Writer, chunkSize int) (int64, error) {
	enc, err := NewStreamEncryptor(key, chunkSize)
	if err != nil {
		return 0, err
	}
	return enc.EncryptStream(r, w)
}

// DecryptStream 从 r 中读取 EncryptStream 生成的帧格式密文，
// 使用 AES-256-GCM 解密每个块，并将明文写入 w。
//
// 读取格式：先读 4 字节大端序长度获取块密文长度，再读取对应字节数的密文块。
// 每个密文块格式为 [nonce(12字节) | ciphertext | tag(16字节)]，
// nonce 为块的前 12 字节，用于 GCM 解密。
//
// 返回写入 w 的总字节数。当 r 返回 io.EOF（无更多块）时正常结束。
// 如果任一块解密失败，返回错误。
//
// DecryptStream 等价于 DecryptStreamWithChunkSize(key, r, w, maxChunkLen)。
func DecryptStream(key []byte, r io.Reader, w io.Writer) (int64, error) {
	return DecryptStreamWithChunkSize(key, r, w, maxChunkLen)
}

// DecryptStreamWithChunkSize 与 DecryptStream 行为一致，但允许指定最大块大小 maxChunkSize。
//
// maxChunkSize 为单帧密文最大允许字节数，超出时返回错误，防止恶意超大帧触发 OOM。
func DecryptStreamWithChunkSize(key []byte, r io.Reader, w io.Writer, maxChunkSize int) (int64, error) {
	dec, err := NewStreamDecryptor(key, maxChunkSize)
	if err != nil {
		return 0, err
	}
	return dec.DecryptStream(r, w)
}

// StreamEncryptor 封装 AES-256-GCM 流式加密。
type StreamEncryptor struct {
	gcm       cipher.AEAD
	chunkSize int
	lenBuf    []byte
}

// NewStreamEncryptor 创建流加密器。
func NewStreamEncryptor(key []byte, chunkSize int) (*StreamEncryptor, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encrypt stream: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt stream: create gcm: %w", err)
	}
	return &StreamEncryptor{gcm: gcm, chunkSize: chunkSize, lenBuf: make([]byte, 4)}, nil
}

// EncryptChunk 加密 single plaintext 块并写入 w。
// 返回写入的字节数（4B 长度前缀 + nonce + ciphertext + tag）。
func (e *StreamEncryptor) EncryptChunk(plaintext []byte, w io.Writer) (int, error) {
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, fmt.Errorf("encrypt stream: generate nonce: %w", err)
	}
	// Seal 将密文+tag 追加到 nonce 之后，返回 [nonce | ciphertext | tag]
	sealed := e.gcm.Seal(nonce, nonce, plaintext, nil)
	binary.BigEndian.PutUint32(e.lenBuf, uint32(len(sealed)))
	if _, err := w.Write(e.lenBuf); err != nil {
		return 0, fmt.Errorf("encrypt stream: write length: %w", err)
	}
	total := 4
	nw, err := w.Write(sealed)
	total += nw
	if err != nil {
		return total, fmt.Errorf("encrypt stream: write chunk: %w", err)
	}
	return total, nil
}

// EncryptStream 使用流加密器从 r 中分块读取数据并加密写入 w。
// 返回写入 w 的总字节数。
func (e *StreamEncryptor) EncryptStream(r io.Reader, w io.Writer) (int64, error) {
	var written int64
	for {
		buf := getBuf(e.chunkSize)
		n, readErr := io.ReadFull(r, buf)
		if n == 0 && readErr == io.EOF {
			putBuf(buf, 0)
			break
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			putBuf(buf, n)
			return written, fmt.Errorf("encrypt stream: read: %w", readErr)
		}

		nw, err := e.EncryptChunk(buf[:n], w)
		written += int64(nw)
		putBuf(buf, n)
		if err != nil {
			return written, err
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}
	return written, nil
}

// StreamDecryptor 封装 AES-256-GCM 流式解密。
type StreamDecryptor struct {
	gcm         cipher.AEAD
	maxChunkLen int
	lenBuf      []byte
}

// NewStreamDecryptor 创建流解密器。
func NewStreamDecryptor(key []byte, maxChunkLen int) (*StreamDecryptor, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("decrypt stream: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt stream: create gcm: %w", err)
	}
	return &StreamDecryptor{gcm: gcm, maxChunkLen: maxChunkLen, lenBuf: make([]byte, 4)}, nil
}

// DecryptChunk 从 r 中读取一个加密块并解密，将明文写入 w。
func (d *StreamDecryptor) DecryptChunk(r io.Reader, w io.Writer) (int, error) {
	if _, err := io.ReadFull(r, d.lenBuf); err != nil {
		return 0, fmt.Errorf("decrypt stream: read length: %w", err)
	}
	chunkLen := binary.BigEndian.Uint32(d.lenBuf)
	if chunkLen > uint32(d.maxChunkLen) {
		return 0, fmt.Errorf("decrypt stream: chunk too large: %d > %d", chunkLen, d.maxChunkLen)
	}

	chunk := getBuf(int(chunkLen))
	if _, err := io.ReadFull(r, chunk); err != nil {
		putBuf(chunk, len(chunk))
		return 0, fmt.Errorf("decrypt stream: read chunk: %w", err)
	}

	nonceSize := d.gcm.NonceSize()
	if len(chunk) < nonceSize {
		putBuf(chunk, len(chunk))
		return 0, fmt.Errorf("decrypt stream: chunk too short")
	}

	nonce, ciphertext := chunk[:nonceSize], chunk[nonceSize:]
	plaintext, err := d.gcm.Open(nil, nonce, ciphertext, nil)
	putBuf(chunk, len(chunk))
	if err != nil {
		return 0, fmt.Errorf("decrypt stream: decrypt chunk: %w", err)
	}

	nw, err := w.Write(plaintext)
	if err != nil {
		return nw, fmt.Errorf("decrypt stream: write: %w", err)
	}
	return nw, nil
}

// DecryptStream 从 r 中读取并解密流，将明文写入 w。
func (d *StreamDecryptor) DecryptStream(r io.Reader, w io.Writer) (int64, error) {
	var written int64
	for {
		nw, err := d.DecryptChunk(r, w)
		written += int64(nw)
		if err != nil {
			if errors.Is(err, io.EOF) && err.Error() == "decrypt stream: read length: EOF" {
				return written, nil
			}
			return written, err
		}
	}
}
