// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
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
	bp := chunkPool.Get().(*[]byte)
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
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, fmt.Errorf("encrypt stream: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, fmt.Errorf("encrypt stream: create gcm: %w", err)
	}

	var written int64
	nonce := make([]byte, gcm.NonceSize())
	lenBuf := make([]byte, 4)

	for {
		buf := getBuf(chunkSize)
		n, readErr := io.ReadFull(r, buf)
		if n == 0 && readErr == io.EOF {
			putBuf(buf, 0)
			break
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			putBuf(buf, n)
			return written, fmt.Errorf("encrypt stream: read: %w", readErr)
		}

		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			putBuf(buf, n)
			return written, fmt.Errorf("encrypt stream: generate nonce: %w", err)
		}

		// Seal 将密文+tag 追加到 nonce 之后，返回 [nonce | ciphertext | tag]
		// nonce 变量本身不受 Seal 影响（cap=12，不够容纳密文，会分配新内存），
		// 因此可在下一轮循环中安全地覆写 nonce。
		sealed := gcm.Seal(nonce, nonce, buf[:n], nil)

		binary.BigEndian.PutUint32(lenBuf, uint32(len(sealed)))
		if _, err := w.Write(lenBuf); err != nil {
			putBuf(buf, n)
			return written, fmt.Errorf("encrypt stream: write length: %w", err)
		}
		written += 4

		nw, err := w.Write(sealed)
		written += int64(nw)
		putBuf(buf, n)
		if err != nil {
			return written, fmt.Errorf("encrypt stream: write chunk: %w", err)
		}

		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
	}

	return written, nil
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
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, fmt.Errorf("decrypt stream: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, fmt.Errorf("decrypt stream: create gcm: %w", err)
	}

	var written int64
	lenBuf := make([]byte, 4)

	for {
		_, err := io.ReadFull(r, lenBuf)
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, fmt.Errorf("decrypt stream: read length: %w", err)
		}

		chunkLen := binary.BigEndian.Uint32(lenBuf)
		if chunkLen > uint32(maxChunkSize) {
			return written, fmt.Errorf("decrypt stream: chunk too large: %d > %d", chunkLen, maxChunkSize)
		}

		chunk := getBuf(int(chunkLen))
		if _, err := io.ReadFull(r, chunk); err != nil {
			putBuf(chunk, len(chunk))
			return written, fmt.Errorf("decrypt stream: read chunk: %w", err)
		}

		nonceSize := gcm.NonceSize()
		if len(chunk) < nonceSize {
			putBuf(chunk, len(chunk))
			return written, fmt.Errorf("decrypt stream: chunk too short")
		}

		nonce, ciphertext := chunk[:nonceSize], chunk[nonceSize:]
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
		// chunk 中的内容已通过 gcm.Open 消费完毕，立即归还到池中
		putBuf(chunk, len(chunk))
		if err != nil {
			return written, fmt.Errorf("decrypt stream: decrypt chunk: %w", err)
		}

		nw, err := w.Write(plaintext)
		written += int64(nw)
		if err != nil {
			return written, fmt.Errorf("decrypt stream: write: %w", err)
		}
	}

	return written, nil
}
