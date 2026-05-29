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
)

const DefaultChunkSize = 64 * 1024

// maxChunkLen 是 DecryptStream 接受的单帧密文最大字节数，
// 等于 DefaultChunkSize 明文 + GCM nonce(12) + tag(16) + 一点冗余。
// 用于在解析帧长度后拒绝异常巨大的 chunk，避免 make([]byte, chunkLen) 触发 OOM。
const maxChunkLen = DefaultChunkSize + 64

// EncryptStream 从 r 中分块读取数据，使用 AES-256-GCM 独立加密每个块，
// 并将帧格式的密文写入 w。
//
// 帧格式：每个块先写入 4 字节大端序的密文长度，再写入 [nonce(12字节) | ciphertext | tag(16字节)]。
// 每个块使用随机 nonce 独立加密，块大小为 DefaultChunkSize（64KB），
// 最后一个块可能小于该值。
//
// 返回写入 w 的总字节数。当 r 返回 io.EOF 时正常结束。
func EncryptStream(key []byte, r io.Reader, w io.Writer) (int64, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return 0, fmt.Errorf("encrypt stream: create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return 0, fmt.Errorf("encrypt stream: create gcm: %w", err)
	}

	var written int64
	buf := make([]byte, DefaultChunkSize)

	for {
		n, readErr := io.ReadFull(r, buf)
		if n == 0 && readErr == io.EOF {
			break
		}
		if readErr != nil && readErr != io.EOF && readErr != io.ErrUnexpectedEOF {
			return written, fmt.Errorf("encrypt stream: read: %w", readErr)
		}

		nonce := make([]byte, gcm.NonceSize())
		if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
			return written, fmt.Errorf("encrypt stream: generate nonce: %w", err)
		}

		sealed := gcm.Seal(nonce, nonce, buf[:n], nil)

		lenBuf := make([]byte, 4)
		binary.BigEndian.PutUint32(lenBuf, uint32(len(sealed)))

		if _, err := w.Write(lenBuf); err != nil {
			return written, fmt.Errorf("encrypt stream: write length: %w", err)
		}
		written += 4

		nw, err := w.Write(sealed)
		written += int64(nw)
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
func DecryptStream(key []byte, r io.Reader, w io.Writer) (int64, error) {
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
		if chunkLen > maxChunkLen {
			return written, fmt.Errorf("decrypt stream: chunk too large: %d > %d", chunkLen, maxChunkLen)
		}
		chunk := make([]byte, chunkLen)
		if _, err := io.ReadFull(r, chunk); err != nil {
			return written, fmt.Errorf("decrypt stream: read chunk: %w", err)
		}

		nonceSize := gcm.NonceSize()
		if len(chunk) < nonceSize {
			return written, fmt.Errorf("decrypt stream: chunk too short")
		}

		nonce, ciphertext := chunk[:nonceSize], chunk[nonceSize:]
		plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
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
