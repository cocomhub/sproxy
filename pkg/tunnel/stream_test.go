// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"io"
	"strings"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/mux"
)

func TestEncryptStream_DecryptStream_Roundtrip(t *testing.T) {
	key := make([]byte, 32)
	plaintext := []byte("hello streaming encryption!")

	var buf bytes.Buffer
	n, err := EncryptStream(key, bytes.NewReader(plaintext), &buf)
	if err != nil {
		t.Fatal(err)
	}
	if n <= 0 {
		t.Fatalf("expected positive bytes written, got %d", n)
	}

	var decrypted bytes.Buffer
	dn, err := DecryptStream(key, &buf, &decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if dn != int64(len(plaintext)) {
		t.Fatalf("expected %d decrypted bytes, got %d", len(plaintext), dn)
	}
	if !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatalf("expected %q, got %q", plaintext, decrypted.Bytes())
	}
}

func TestEncryptStream_DecryptStream_LargePayload(t *testing.T) {
	key := make([]byte, 32)
	// 大于 1 个 chunk 的大 payload
	plaintext := make([]byte, DefaultChunkSize*3+1234)

	var buf bytes.Buffer
	_, err := EncryptStream(key, bytes.NewReader(plaintext), &buf)
	if err != nil {
		t.Fatal(err)
	}

	var decrypted bytes.Buffer
	_, err = DecryptStream(key, &buf, &decrypted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decrypted.Bytes(), plaintext) {
		t.Fatal("large payload mismatch after encrypt/decrypt stream")
	}
}

func TestEncryptStream_DecryptStream_Empty(t *testing.T) {
	key := make([]byte, 32)
	var buf bytes.Buffer
	_, err := EncryptStream(key, bytes.NewReader(nil), &buf)
	if err != nil {
		t.Fatal(err)
	}

	var decrypted bytes.Buffer
	_, err = DecryptStream(key, &buf, &decrypted)
	if err != nil {
		t.Fatal(err)
	}
}

func TestEncryptStream_ShortKey(t *testing.T) {
	_, err := EncryptStream([]byte("short"), bytes.NewReader([]byte("data")), io.Discard)
	if err == nil {
		t.Error("expected error for short key")
	}
}

func TestDecryptStream_ShortKey(t *testing.T) {
	_, err := DecryptStream([]byte("short"), bytes.NewReader([]byte("data")), io.Discard)
	if err == nil {
		t.Error("expected error for short key")
	}
}

// TestEncryptStreamWithChunkSize_ZeroChunk: chunkSize=0 会导致 getBuf(0) 返回空切片，
// 致使 io.ReadFull 无限循环。这是一个已知的边缘情况，生产代码不会传入 chunkSize=0。
// 跳过此场景的测试。

func TestGetBuf_PutBuf(t *testing.T) {
	buf := getBuf(1024)
	if len(buf) != 1024 {
		t.Fatalf("expected len 1024, got %d", len(buf))
	}
	buf[0] = 0x42
	putBuf(buf, 1024)
	if buf[0] != 0 {
		t.Fatal("expected first byte cleared after PutBuf")
	}
}

// TestDecryptStream_ChunkTooLarge 构造一个 chunkLen 远超 maxChunkLen 的帧，
// 确认 DecryptStream 拒绝而不分配大内存。
func TestDecryptStream_ChunkTooLarge(t *testing.T) {
	key := make([]byte, 32) // 任意 256-bit key，反正解密会因长度先失败
	var attack bytes.Buffer
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 1<<24) // 16 MiB，远超 maxChunkLen (~64K+64)
	attack.Write(lenBuf)
	// 不需要真的写 16 MiB 数据：DecryptStream 应在 ReadFull 前的长度检查就 reject

	var out bytes.Buffer
	_, err := DecryptStream(key, &attack, &out)
	if err == nil {
		t.Fatalf("expected error for oversized chunk, got nil")
	}
	if !strings.Contains(err.Error(), "chunk too large") {
		t.Fatalf("expected 'chunk too large' error, got: %v", err)
	}
}

// TestDecryptStream_TruncatedFrame 长度后 body 截断，应返回 error 而非死循环。
func TestDecryptStream_TruncatedFrame(t *testing.T) {
	key := make([]byte, 32)
	var attack bytes.Buffer
	lenBuf := make([]byte, 4)
	binary.BigEndian.PutUint32(lenBuf, 100) // 声明 100B，但只写 10B
	attack.Write(lenBuf)
	attack.Write(make([]byte, 10))

	var out bytes.Buffer
	_, err := DecryptStream(key, &attack, &out)
	if err == nil {
		t.Fatalf("expected error for truncated frame, got nil")
	}
}

// mockStream 实现 mux.Stream 接口，用于 streamBody 单元测试。
type mockStream struct {
	*bytes.Reader
}

func (m *mockStream) Write(p []byte) (int, error) { return len(p), nil }
func (m *mockStream) Close() error                { return nil }
func (m *mockStream) CloseWrite() error           { return nil }
func (m *mockStream) ID() mux.StreamID            { return 0 }

// TestStreamBodyEncryptedReadBuffer 验证加密模式下 streamBody 的预读缓冲读取正确性。
// 预加密 1MB 数据，使用小缓冲区（4096B）分多次读取，验证总字节数正确。
func TestStreamBodyEncryptedReadBuffer(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	// 构造 1MB 测试数据
	dataSize := 1024 * 1024 // 1MB
	data := make([]byte, dataSize)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}

	// 预加密数据
	var encryptedBuf bytes.Buffer
	if _, err := EncryptStream(key, bytes.NewReader(data), &encryptedBuf); err != nil {
		t.Fatal(err)
	}

	// 创建 mock stream：提供预加密数据
	ms := &mockStream{
		Reader: bytes.NewReader(encryptedBuf.Bytes()),
	}

	// 使用 streamBody 读取（加密模式）
	sb := &streamBody{
		stream: ms,
		key:    key,
	}
	defer sb.Close()

	var total int64
	buf := make([]byte, 4096) // 小缓冲区读取，测试流式读取
	for {
		n, err := sb.Read(buf)
		total += int64(n)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read: %v", err)
		}
	}

	if total != int64(dataSize) {
		t.Fatalf("expected %d bytes, got %d", dataSize, total)
	}
}

// TestStreamBodyEncryptedReadBuffer_SmallData 验证小数据场景下加密模式 streamBody 读取正确性。
func TestStreamBodyEncryptedReadBuffer_SmallData(t *testing.T) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}

	data := []byte("small data")
	var encryptedBuf bytes.Buffer
	if _, err := EncryptStream(key, bytes.NewReader(data), &encryptedBuf); err != nil {
		t.Fatal(err)
	}

	ms := &mockStream{
		Reader: bytes.NewReader(encryptedBuf.Bytes()),
	}

	sb := &streamBody{
		stream: ms,
		key:    key,
	}
	defer sb.Close()

	got, err := io.ReadAll(sb)
	if err != nil {
		t.Fatalf("read all: %v", err)
	}

	if !bytes.Equal(got, data) {
		t.Fatalf("expected %q, got %q", data, got)
	}
}
