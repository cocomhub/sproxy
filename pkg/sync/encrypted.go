// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

// encrypted.go 是 at-rest 加密卷包装（roadmap P2 at-rest 加密）：
//
//   - EncryptedFS 实现 sync.FS 全接口，转发底层 FS 的目录/元数据操作，
//     对 WriteFile 流式 AES-256-GCM 分块加密、OpenRead 流式解密（透明）。
//   - 密文格式：[magic][chunkSize] + 逐块 [4B len][nonce|ct|tag]（同 pkg/files
//     cipher 格式但自含——sync 不能 import files 防环）。
//
// 纯标准库（crypto/aes + cipher + rand + hmac），不引第三方。

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// encMagic 是加密流魔数头。
var encMagic = []byte("SPROXY-FS-ENC-v1")

// EncryptedFS 是透明加密 sync.FS 包装。
type EncryptedFS struct {
	inner FS
	key   []byte
}

// NewEncryptedFS 构造加密卷包装（inner 为底层 FS；key 必须 32 字节）。
func NewEncryptedFS(inner FS, key []byte) *EncryptedFS {
	return &EncryptedFS{inner: inner, key: key}
}

// ListDir 转发。
func (e *EncryptedFS) ListDir(ctx context.Context, path string) ([]Entry, error) {
	return e.inner.ListDir(ctx, path)
}

// Stat 转发（size 为密文大小——调用方按 opaque 处理）。
func (e *EncryptedFS) Stat(ctx context.Context, path string) (*Entry, error) {
	return e.inner.Stat(ctx, path)
}

// OpenRead 解密读（透明）。
func (e *EncryptedFS) OpenRead(ctx context.Context, path string) (io.ReadCloser, error) {
	rc, err := e.inner.OpenRead(ctx, path)
	if err != nil {
		return nil, err
	}
	dr, derr := newEncReader(e.key, rc)
	if derr != nil {
		rc.Close()
		return nil, derr
	}
	return &encReadCloser{r: dr, c: rc}, nil
}

// WriteFile 加密写（透明）。
func (e *EncryptedFS) WriteFile(ctx context.Context, path string, r io.Reader, size, mtime int64) error {
	var buf bytes.Buffer
	ew, err := newEncWriter(e.key, &buf)
	if err != nil {
		return err
	}
	if _, err := io.Copy(ew, r); err != nil {
		return err
	}
	if err := ew.Close(); err != nil {
		return err
	}
	return e.inner.WriteFile(ctx, path, bytes.NewReader(buf.Bytes()), int64(buf.Len()), mtime)
}

// Rename / Delete / MakeDir 转发。
func (e *EncryptedFS) Rename(ctx context.Context, from, to string) error {
	return e.inner.Rename(ctx, from, to)
}
func (e *EncryptedFS) Delete(ctx context.Context, path string) error {
	return e.inner.Delete(ctx, path)
}
func (e *EncryptedFS) MakeDir(ctx context.Context, path string) error {
	return e.inner.MakeDir(ctx, path)
}

// 编译期断言。
var _ FS = (*EncryptedFS)(nil)

// encWriter 分块加密 writer。
type encWriter struct {
	gcm   cipher.AEAD
	w     io.Writer
	buf   bytes.Buffer
	chunk int
}

func newEncWriter(key []byte, w io.Writer) (*encWriter, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encrypted: 密钥必须 32B")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(encMagic); err != nil {
		return nil, err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], 64<<10)
	if _, err := w.Write(sz[:]); err != nil {
		return nil, err
	}
	return &encWriter{gcm: gcm, w: w, chunk: 64 << 10}, nil
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

// encReader 分块解密 reader。
type encReader struct {
	gcm     cipher.AEAD
	r       io.Reader
	pending []byte
}

func newEncReader(key []byte, r io.Reader) (*encReader, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("encrypted: 密钥必须 32B")
	}
	head := make([]byte, len(encMagic)+4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("encrypted: 非加密格式: %w", err)
	}
	if !bytes.Equal(head[:len(encMagic)], encMagic) {
		return nil, fmt.Errorf("encrypted: 非加密卷格式")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &encReader{gcm: gcm, r: r}, nil
}

func (r *encReader) Read(p []byte) (int, error) {
	if len(r.pending) == 0 {
		var ln [4]byte
		if _, err := io.ReadFull(r.r, ln[:]); err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, err
		}
		blen := binary.BigEndian.Uint32(ln[:])
		if blen > 1<<20 {
			return 0, fmt.Errorf("encrypted: 块长度异常 %d", blen)
		}
		enc := make([]byte, blen)
		if _, err := io.ReadFull(r.r, enc); err != nil {
			return 0, err
		}
		ns := r.gcm.NonceSize()
		if len(enc) < ns {
			return 0, fmt.Errorf("encrypted: 块过短")
		}
		plain, err := r.gcm.Open(nil, enc[:ns], enc[ns:], nil)
		if err != nil {
			return 0, fmt.Errorf("encrypted: 解密失败（密钥错或密文篡改）: %w", err)
		}
		r.pending = plain
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// encReadCloser 组合解密 reader + 底层 closer。
type encReadCloser struct {
	r io.Reader
	c io.Closer
}

func (e *encReadCloser) Read(p []byte) (int, error) { return e.r.Read(p) }
func (e *encReadCloser) Close() error               { return e.c.Close() }

var _ = sync.Mutex{}
