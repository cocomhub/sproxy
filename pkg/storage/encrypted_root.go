// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package storage

// encrypted_root.go 是 at-rest 加密卷根（roadmap P2 at-rest 加密残余：
// volumes[].extra.encrypt 装配开关）：
//
//   - EncryptedRoot 透明包装 storage.Root：Open 返回解密 reader、OpenFile 写入
//     加密 writer；目录/元数据操作转发。
//   - 密文格式与 pkg/sync.EncryptedFS 同构（[magic][chunkSize] + 逐块
//     [4B len][nonce|ct|tag]）但自含——storage 不 import sync 防环（同 #518
//     sync/files 双实现先例）。
//
// 纯标准库（crypto/aes + cipher + rand），不引第三方。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"os"
)

// encRootMagic 是加密卷根魔数头（与 sync.EncryptedFS 同值，格式互通）。
var encRootMagic = []byte("SPROXY-FS-ENC-v1")

// EncryptedRoot 是透明加密 storage.Root 包装。
type EncryptedRoot struct {
	inner *Root
	key   []byte
}

// NewEncryptedRoot 构造加密卷根包装（inner 为底层 Root；key 必须 32 字节）。
func NewEncryptedRoot(inner *Root, key []byte) (*EncryptedRoot, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: 加密卷 key 必须 32 字节")
	}
	return &EncryptedRoot{inner: inner, key: key}, nil
}

// Close 转发。
func (er *EncryptedRoot) Close() error { return er.inner.Close() }

// AbsPath 转发（物理路径，供元数据引用）。
func (er *EncryptedRoot) AbsPath() string { return er.inner.AbsPath() }

// Abs 转发（路径解析）。
func (er *EncryptedRoot) Abs(rel string) (string, bool) { return er.inner.Abs(rel) }

// ReadFile 读取并解密（透明）。
func (er *EncryptedRoot) ReadFile(rel string) ([]byte, error) {
	f, err := er.inner.Open(rel)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dr, err := newEncRootReader(er.key, f)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(dr)
}

// WriteFile 加密写（透明，流式：io.Pipe 加密 goroutine，底层 FS 流式读）。
func (er *EncryptedRoot) WriteFile(rel string, data []byte, perm os.FileMode) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	go func() {
		ew, werr := newEncRootWriter(er.key, pw, 64<<10)
		if werr != nil {
			_ = pw.CloseWithError(werr)
			errCh <- werr
			return
		}
		if _, cerr := ew.Write(data); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		if cerr := ew.Close(); cerr != nil {
			_ = pw.CloseWithError(cerr)
			errCh <- cerr
			return
		}
		errCh <- pw.Close()
	}()
	// 底层用 OpenFile(O_CREATE|O_TRUNC|O_WRONLY) 流式写 pipe。
	// 中间目录可能不存在（与 os.Root.WriteFile 同语义：MkdirAll(dir)）。
	if d := dirOf(rel); d != "" {
		if merr := er.inner.MkdirAll(d, 0o755); merr != nil {
			_ = pr.CloseWithError(merr)
			return merr
		}
	}
	f, oerr := er.inner.OpenFile(rel, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if oerr != nil {
		_ = pr.CloseWithError(oerr)
		return oerr
	}
	_, cerr := io.Copy(f, pr)
	clerr := f.Close()
	encErr := <-errCh
	if cerr != nil {
		return cerr
	}
	if clerr != nil {
		return clerr
	}
	return encErr
}

// dirOf 返回 rel 的父目录（无分隔符返回空）。
func dirOf(rel string) string {
	for i := len(rel) - 1; i >= 0; i-- {
		if rel[i] == '/' || rel[i] == '\\' {
			return rel[:i]
		}
	}
	return ""
}

// encRootWriter 流式分块加密 writer（格式与 sync.EncryptedFS 同构）。
type encRootWriter struct {
	gcm   cipher.AEAD
	w     io.Writer
	buf   bytes.Buffer
	chunk int
}

func newEncRootWriter(key []byte, w io.Writer, chunk int) (*encRootWriter, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: 加密卷 key 必须 32B")
	}
	if chunk <= 0 {
		chunk = 64 << 10
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if _, err := w.Write(encRootMagic); err != nil {
		return nil, err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(chunk))
	if _, err := w.Write(sz[:]); err != nil {
		return nil, err
	}
	return &encRootWriter{gcm: gcm, w: w, chunk: chunk}, nil
}

func (w *encRootWriter) Write(p []byte) (int, error) {
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

func (w *encRootWriter) encryptChunk(plain []byte) error {
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

func (w *encRootWriter) Close() error {
	// 尾部残块（< chunk）加密写出。
	if w.buf.Len() > 0 {
		if err := w.encryptChunk(w.buf.Bytes()); err != nil {
			return err
		}
		w.buf.Reset()
	}
	return nil
}

// encRootReader 流式解密 reader。
type encRootReader struct {
	gcm    cipher.AEAD
	r      io.Reader
	chunk  int
	remain []byte
	done   bool
}

func newEncRootReader(key []byte, r io.Reader) (*encRootReader, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("storage: 加密卷 key 必须 32B")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// 校验魔数头。
	magic := make([]byte, len(encRootMagic))
	if _, err := io.ReadFull(r, magic); err != nil {
		return nil, fmt.Errorf("storage: 读加密头: %w", err)
	}
	if !bytes.Equal(magic, encRootMagic) {
		return nil, fmt.Errorf("storage: 非加密卷格式（魔数不匹配）")
	}
	var sz [4]byte
	if _, err := io.ReadFull(r, sz[:]); err != nil {
		return nil, fmt.Errorf("storage: 读块大小: %w", err)
	}
	chunk := int(binary.BigEndian.Uint32(sz[:]))
	if chunk <= 0 {
		return nil, fmt.Errorf("storage: 非法块大小 %d", chunk)
	}
	return &encRootReader{gcm: gcm, r: r, chunk: chunk}, nil
}

func (r *encRootReader) Read(p []byte) (int, error) {
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
		return 0, fmt.Errorf("storage: 非法密文块长 %d", l)
	}
	blob := make([]byte, l)
	if _, err := io.ReadFull(r.r, blob); err != nil {
		return 0, err
	}
	nonceSize := r.gcm.NonceSize()
	if l < nonceSize {
		return 0, fmt.Errorf("storage: 密文块过短")
	}
	nonce := blob[:nonceSize]
	ct := blob[nonceSize:]
	plain, err := r.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return 0, fmt.Errorf("storage: 解密失败: %w", err)
	}
	n := copy(p, plain)
	if n < len(plain) {
		r.remain = plain[n:]
	}
	return n, nil
}
