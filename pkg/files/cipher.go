// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// cipher.go 是加密归档插件化（roadmap P2 加密归档插件化）：
//
//   - RegisterCipher 注册加密算法（分块大小）；LookupCipher 按名查。
//   - NewCipherWriter/NewCipherReader：流式 AES-256-GCM 分块加密/解密
//     （64KiB 块 + 每块随机 nonce + 头格式 [magic][version][chunkSize][keyID]）。
//   - 密文格式：每块 = [4B 长度][nonce|ct|tag]，顺序拼接；空流 = 空输出。
//
// 纯标准库（crypto/aes + crypto/cipher + crypto/rand），不引第三方。

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"sync"
)

// CipherConfig 是加密算法配置（LookupCipher 返回）。
type CipherConfig struct {
	// ChunkSize 是分块加密的明文块大小（64KiB 默认；GCM 认证粒度）。
	ChunkSize int
}

// cipherRegistry 是加密算法注册表（算法名 → 配置），与 transformRegistry 同构。
var cipherRegistry = struct {
	mu sync.RWMutex
	m  map[string]CipherConfig
}{m: make(map[string]CipherConfig)}

func init() {
	// 内建 AES-256-GCM（64KiB 分块）。
	RegisterCipher("aes-256-gcm", 64<<10)
}

// RegisterCipher 注册加密算法（算法名 → 分块大小）。返回 true = 新注册。
func RegisterCipher(name string, chunkSize int) bool {
	if name == "" || chunkSize <= 0 {
		return false
	}
	cipherRegistry.mu.Lock()
	defer cipherRegistry.mu.Unlock()
	if _, ok := cipherRegistry.m[name]; ok {
		return false
	}
	cipherRegistry.m[name] = CipherConfig{ChunkSize: chunkSize}
	return true
}

// LookupCipher 按算法名查询配置（ok=false = 未注册）。
func LookupCipher(name string) (CipherConfig, bool) {
	cipherRegistry.mu.RLock()
	defer cipherRegistry.mu.RUnlock()
	cfg, ok := cipherRegistry.m[name]
	return cfg, ok
}

// ---- 测试隔离辅助（与 transformRegistry 同构）----

// cipherRegistrySnapshot 返回当前注册表快照（测试隔离恢复用）。
func cipherRegistrySnapshot() map[string]CipherConfig {
	cipherRegistry.mu.RLock()
	defer cipherRegistry.mu.RUnlock()
	out := make(map[string]CipherConfig, len(cipherRegistry.m))
	maps.Copy(out, cipherRegistry.m)
	return out
}

// cipherRegistryClear 清空注册表（测试隔离）。
func cipherRegistryClear() {
	cipherRegistry.mu.Lock()
	defer cipherRegistry.mu.Unlock()
	cipherRegistry.m = make(map[string]CipherConfig)
}

// cipherRegistryRestore 恢复注册表快照（测试隔离）。
func cipherRegistryRestore(snapshot map[string]CipherConfig) {
	cipherRegistry.mu.Lock()
	defer cipherRegistry.mu.Unlock()
	cipherRegistry.m = make(map[string]CipherConfig, len(snapshot))
	maps.Copy(cipherRegistry.m, snapshot)
}

// cipherMagic 是加密流的魔数头（防误读明文/其它格式）。
var cipherMagic = []byte("SPROXY-AES-GCM-v1")

// NewCipherWriter 构造流式加密 writer（分块 AES-256-GCM）。
// 头：[magic][4B 块大小]；每块：[4B 密文长度][nonce(12B)|ct|tag]。
func NewCipherWriter(algo string, key []byte, w io.Writer) (io.WriteCloser, error) {
	cfg, ok := LookupCipher(algo)
	if !ok {
		return nil, fmt.Errorf("cipher: 未知算法 %q", algo)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("cipher: 密钥必须 32B")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	if _, err := w.Write(cipherMagic); err != nil {
		return nil, err
	}
	var sz [4]byte
	binary.BigEndian.PutUint32(sz[:], uint32(cfg.ChunkSize))
	if _, err := w.Write(sz[:]); err != nil {
		return nil, err
	}
	return &cipherWriter{gcm: gcm, w: w, buf: &bytes.Buffer{}, chunk: cfg.ChunkSize}, nil
}

// cipherWriter 缓冲分块加密。
type cipherWriter struct {
	gcm   cipher.AEAD
	w     io.Writer
	buf   *bytes.Buffer
	chunk int
}

func (cw *cipherWriter) Write(p []byte) (int, error) {
	cw.buf.Write(p)
	for cw.buf.Len() >= cw.chunk {
		chunk := make([]byte, cw.chunk)
		if _, err := io.ReadFull(cw.buf, chunk); err != nil {
			return 0, err
		}
		if err := cw.encryptChunk(chunk); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

func (cw *cipherWriter) encryptChunk(plain []byte) error {
	nonce := make([]byte, cw.gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	ct := cw.gcm.Seal(nil, nonce, plain, nil)
	out := make([]byte, 4+len(nonce)+len(ct))
	binary.BigEndian.PutUint32(out[:4], uint32(len(nonce)+len(ct)))
	copy(out[4:], nonce)
	copy(out[4+len(nonce):], ct)
	_, err := cw.w.Write(out)
	return err
}

// Close 冲刷剩余块并关闭。
func (cw *cipherWriter) Close() error {
	if cw.buf.Len() > 0 {
		if err := cw.encryptChunk(cw.buf.Bytes()); err != nil {
			return err
		}
		cw.buf.Reset()
	}
	return nil
}

// NewCipherReader 构造流式解密 reader（读 NewCipherWriter 产物）。
func NewCipherReader(algo string, key []byte, r io.Reader) (io.Reader, error) {
	cfg, ok := LookupCipher(algo)
	if !ok {
		return nil, fmt.Errorf("cipher: 未知算法 %q", algo)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("cipher: 密钥必须 32B")
	}
	// 读头。
	head := make([]byte, len(cipherMagic)+4)
	if _, err := io.ReadFull(r, head); err != nil {
		return nil, fmt.Errorf("cipher: 头读取失败: %w", err)
	}
	if !bytes.Equal(head[:len(cipherMagic)], cipherMagic) {
		return nil, fmt.Errorf("cipher: 非加密归档格式")
	}
	if got := binary.BigEndian.Uint32(head[len(cipherMagic):]); got != uint32(cfg.ChunkSize) {
		return nil, fmt.Errorf("cipher: 块大小不匹配 %d != %d", got, cfg.ChunkSize)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("cipher: %w", err)
	}
	return &cipherReader{gcm: gcm, r: r}, nil
}

// cipherReader 逐块解密。
type cipherReader struct {
	gcm     cipher.AEAD
	r       io.Reader
	pending []byte
}

func (cr *cipherReader) Read(p []byte) (int, error) {
	if len(cr.pending) == 0 {
		// 读下一块。
		var ln [4]byte
		if _, err := io.ReadFull(cr.r, ln[:]); err != nil {
			if err == io.EOF {
				return 0, io.EOF
			}
			return 0, err
		}
		blen := binary.BigEndian.Uint32(ln[:])
		if blen > 1<<20 {
			return 0, fmt.Errorf("cipher: 块长度异常 %d", blen)
		}
		enc := make([]byte, blen)
		if _, err := io.ReadFull(cr.r, enc); err != nil {
			return 0, err
		}
		nonceSize := cr.gcm.NonceSize()
		if len(enc) < nonceSize {
			return 0, fmt.Errorf("cipher: 块过短")
		}
		plain, err := cr.gcm.Open(nil, enc[:nonceSize], enc[nonceSize:], nil)
		if err != nil {
			return 0, fmt.Errorf("cipher: 解密失败（密钥错或密文篡改）: %w", err)
		}
		cr.pending = plain
	}
	n := copy(p, cr.pending)
	cr.pending = cr.pending[n:]
	return n, nil
}
