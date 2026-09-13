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
	"os"
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
// aad 是额外认证数据，将每个块密文绑定到特定上下文（如 AADStream）。
// 返回写入 w 的总字节数。当 r 返回 io.EOF 时正常结束。
//
// EncryptStream 等价于 EncryptStreamWithChunkSize(key, r, w, DefaultChunkSize, aad)。
func EncryptStream(key []byte, r io.Reader, w io.Writer, aad []byte) (int64, error) {
	return EncryptStreamWithChunkSize(key, r, w, DefaultChunkSize, aad)
}

// EncryptStreamWithChunkSize 与 EncryptStream 行为一致，但允许指定块大小 chunkSize。
//
// chunkSize 每块的明文大小。较大的块可降低帧头（4 字节长度 + nonce）开销，
// 但会增大单次分配和加密延迟。
func EncryptStreamWithChunkSize(key []byte, r io.Reader, w io.Writer, chunkSize int, aad []byte) (int64, error) {
	enc, err := NewStreamEncryptor(key, chunkSize)
	if err != nil {
		return 0, err
	}
	return enc.EncryptStream(r, w, aad)
}

// DecryptStream 从 r 中读取 EncryptStream 生成的帧格式密文，
// 使用 AES-256-GCM 解密每个块，并将明文写入 w。
//
// 读取格式：先读 4 字节大端序长度获取块密文长度，再读取对应字节数的密文块。
// 每个密文块格式为 [nonce(12字节) | ciphertext | tag(16字节)]，
// nonce 为块的前 12 字节，用于 GCM 解密。
//
// aad 必须与加密时使用的 aad 一致，否则解密失败。
// 返回写入 w 的总字节数。当 r 返回 io.EOF（无更多块）时正常结束。
// 如果任一块解密失败，返回错误。
//
// 前提：w **不得**是短写 Writer（同 DecryptChunk 的 M2 说明）——树内调用点恒为
// io.PipeWriter；短写 Writer 需调用方自行循环写足后再传入。
//
// DecryptStream 等价于 DecryptStreamWithChunkSize(key, r, w, maxChunkLen, aad)。
func DecryptStream(key []byte, r io.Reader, w io.Writer, aad []byte) (int64, error) {
	return DecryptStreamWithChunkSize(key, r, w, maxChunkLen, aad)
}

// DecryptStreamWithChunkSize 与 DecryptStream 行为一致，但允许指定最大块大小 maxChunkSize。
//
// maxChunkSize 为单帧密文最大允许字节数，超出时返回错误，防止恶意超大帧触发 OOM。
func DecryptStreamWithChunkSize(key []byte, r io.Reader, w io.Writer, maxChunkSize int, aad []byte) (int64, error) {
	dec, err := NewStreamDecryptor(key, maxChunkSize)
	if err != nil {
		return 0, err
	}
	return dec.DecryptStream(r, w, aad)
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

// writeFull 把 b 全部写入 w，循环处理短写。
//
// 必要性：mux.Stream.Write 是**窗口受限的短写**语义（剩余流控窗口小于 b 时只投递
// 窗口允许的一段，返回 n < len(b) 且 err == nil）。实现见 pkg/tunnel/mux/stream.go
// 的 (*stream).Write（等待窗口 >0 后只投递 min(len(p), window)）；行为示例见
// pkg/tunnel/mux/edge_test.go 的 TestWrite_BiggerThanWindow（该测试只 t.Log 记录了
// 70 KB 写入，**对返回的 n 不做任何断言**，因此它只是示例而非门禁）。
//
// 真正的门禁在本包与 iostream 包：TestWriteFull_LoopsOnShortWrite（构造 limit 短写
// writer）、TestTunnel_LargeBodyRoundTrip（经真 mux + 真加密断言 1 KB/60 KB/70 KB/
// 300 KB 往返逐字节一致）、TestTunnel_PlaintextLargeBodyRoundTrip（明文分支同理）。
// 任何忽略返回 n 的写入点都会**静默截断**——明文分支表现为响应体少一段且无任何错误
// 上抛，密文分支表现为对端解密 "unexpected EOF" 或 GCM 认证失败（数据损坏）。
// 本包的密文写入（EncryptChunk / sendRequestMeta / writeEncryptedResponse）都必须
// 走本函数，明文分支走 iostream.CopyFull（内部同为循环写足）。
//
// 若 w 返回 (n<=0, nil) 或 n > len(b)（违反 io.Writer 契约），返回 io.ErrShortWrite
// 而不是死循环或越界。
func writeFull(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

// EncryptChunk 加密 single plaintext 块并写入 w。
// 使用 aad 作为 GCM 的额外认证数据，将密文绑定到特定上下文。
//
// 返回值语义（M3 订正，调用方请按此理解）：
//   - 成功：本次**已写足**的字节数 = 4（长度前缀）+ len(nonce|密文|tag)，恒等于
//     4 + len(sealed)；
//   - 失败：出错前**已完整写完的整段**字节数——长度前缀写失败返回 0，密文体写失败
//     返回 4（前缀已写足，密文体一个字节都没算）。注意 writeFull 内部的**部分**写入
//     不回报进度，故真实落盘字节数可能略小于该值；此计数仅供调用方累计/诊断，不是
//     精确的"实际写入字节数"。
//
// 长度前缀与密文体都经 writeFull 写足（w 短写时不静默截断）。
func (e *StreamEncryptor) EncryptChunk(plaintext []byte, w io.Writer, aad []byte) (int, error) {
	diagEncN++ // diag(#213)
	fmt.Fprintf(os.Stderr, "[DIAG213] enc n=%d plain=%d\n", diagEncN, len(plaintext))
	nonce := make([]byte, e.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return 0, fmt.Errorf("encrypt stream: generate nonce: %w", err)
	}
	// Seal 将密文+tag 追加到 nonce 之后，返回 [nonce | ciphertext | tag]
	sealed := e.gcm.Seal(nonce, nonce, plaintext, aad)
	binary.BigEndian.PutUint32(e.lenBuf, uint32(len(sealed)))
	if err := writeFull(w, e.lenBuf); err != nil {
		return 0, fmt.Errorf("encrypt stream: write length: %w", err)
	}
	if err := writeFull(w, sealed); err != nil {
		return 4, fmt.Errorf("encrypt stream: write chunk: %w", err)
	}
	return 4 + len(sealed), nil
}

// EncryptStream 使用流加密器从 r 中分块读取数据并加密写入 w。
// 每个块使用 aad 作为 GCM 的额外认证数据。
// 返回写入 w 的总字节数。
func (e *StreamEncryptor) EncryptStream(r io.Reader, w io.Writer, aad []byte) (int64, error) {
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

		nw, err := e.EncryptChunk(buf[:n], w, aad)
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

// DecryptChunk 从 r 中读取一个加密块并使用 aad 解密，将明文写入 w。
// aad 必须与加密时使用的 aad 一致，否则解密失败。
//
// 前提（M2）：`w.Write(plaintext)` 只调用一次且**不处理短写**（返回的 n 原样返回并
// 被 DecryptStream 累加）——若 w 短写，本块余下的明文会被静默丢弃。树内全部调用点
// 的 w 都是**不会短写**的 Writer（io.PipeWriter / bytes.Buffer：前者写满或返回错误，
// 后者恒全写），故当前不可达；对手写的短写 Writer（如直接传 mux.Stream），调用方
// 必须自行套一层循环写足（参考 iostream.WriteFull）后再交给本函数。
func (d *StreamDecryptor) DecryptChunk(r io.Reader, w io.Writer, aad []byte) (int, error) {
	if _, err := io.ReadFull(r, d.lenBuf); err != nil {
		return 0, fmt.Errorf("decrypt stream: read length: %w", err)
	}
	chunkLen := binary.BigEndian.Uint32(d.lenBuf)
	diagDecN++ // diag(#213)
	fmt.Fprintf(os.Stderr, "[DIAG213] dec n=%d chunkLen=%d\n", diagDecN, chunkLen)
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
	plaintext, err := d.gcm.Open(nil, nonce, ciphertext, aad)
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
// 每个块使用 aad 作为 GCM 的额外认证数据。
func (d *StreamDecryptor) DecryptStream(r io.Reader, w io.Writer, aad []byte) (int64, error) {
	var written int64
	for {
		nw, err := d.DecryptChunk(r, w, aad)
		written += int64(nw)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return written, nil
			}
			return written, err
		}
	}
}

// diag(#213) 临时诊断计数器（非并发安全场景：每流单 goroutine）。
var diagEncN, diagDecN int
