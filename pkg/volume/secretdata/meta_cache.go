// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_cache.go —— 装配期 meta blob 的**本机磁盘密文缓存**（2026-10-30）。
//
// 问题：装配期每个容器/文件都要读一次 meta blob。外部卷（baidupcs）每读一次 =
// 启动一个 `BaiduPCS-Go download` 子进程并落到 `os.MkdirTemp` 的**随机临时目录**
// （adapter.go 的 baidupcs-dl-*），Close 即删 ⇒ **每次进程启动都重新下载全部 meta**
// （272 部视频 ≈ 452 个 meta，实测 20-30 分钟）。
//
// 方案（与 span_disk_cache.go 同口径）：把 meta blob 的**密文**落到固定目录
// （`Options.MetaCacheDir`，空 = 禁用零回归），下次启动直接读本地文件。
//
// 安全与正确性：
//   - 只缓存**密文**（磁盘不出现明文文件名/哈希）；
//   - 不使用 MD5 比对：读到缓存后要过 AES-GCM 认证，内容被篡改/密钥不符会解密失败 ⇒
//     调用方删除该缓存并重新下载（自愈），比 MD5 更强；
//   - 写入用 `.part` + rename：进程被杀不会留下半截文件被误用。

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// metaCachePath 返回某 blob 相对路径在缓存目录下的落地路径（两级散列避免单目录过大）。
func (s *SecretdataFS) metaCachePath(rel string) string {
	sum := sha256.Sum256([]byte(rel))
	name := hex.EncodeToString(sum[:])
	return filepath.Join(s.opts.MetaCacheDir, name[:2], name)
}

// readBlobCached 读 blob（密文），优先读本机缓存；未命中则从底层读并落缓存。
// 缓存失败绝不影响读：写缓存任何错误都只忽略。
func (s *SecretdataFS) readBlobCached(ctx context.Context, rel string) ([]byte, error) {
	if s.opts.MetaCacheDir == "" {
		return readBlob(ctx, s.inner, rel)
	}
	cp := s.metaCachePath(rel)
	if b, err := os.ReadFile(cp); err == nil {
		return b, nil
	}
	b, rerr := readBlob(ctx, s.inner, rel)
	if rerr != nil {
		return nil, rerr
	}
	s.putBlobCache(rel, b)
	return b, nil
}

// putBlobCache 把**密文 blob** 写入本机缓存（.part + rename 原子落盘，失败静默忽略）。
// 写路径（上传 meta blob 成功）也调用它 ⇒ 上传后重启装配直接读本地，不再重复下载。
func (s *SecretdataFS) putBlobCache(rel string, b []byte) {
	if s.opts.MetaCacheDir == "" || len(b) == 0 {
		return
	}
	cp := s.metaCachePath(rel)
	if mk := os.MkdirAll(filepath.Dir(cp), 0o700); mk != nil {
		return
	}
	tmp := fmt.Sprintf("%s.part.%d", cp, os.Getpid())
	if werr := os.WriteFile(tmp, b, 0o600); werr != nil {
		_ = os.Remove(tmp)
		return
	}
	if rerr := os.Rename(tmp, cp); rerr != nil {
		_ = os.Remove(tmp)
	}
}

// dropBlobCache 删除某 blob 的本地缓存（解密失败时调用，实现自愈）。
func (s *SecretdataFS) dropBlobCache(rel string) {
	if s.opts.MetaCacheDir == "" {
		return
	}
	_ = os.Remove(s.metaCachePath(rel))
}

// metaCacheCompactMagic 标记「精简缓存」文件：内容 = AES-256-GCM(JSON(Meta))，**无 padding**。
// 原始 meta blob 因 MetaPadBytes（默认 1MB）每个 1-2MB，而 JSON 只有几 KB ⇒ 精简后省约 300×。
const metaCacheCompactMagic = "SDMC1"

// compactKey 派生精简缓存加密 key（与索引快照/内容加密域分离）。
func compactKey(secret []byte) [32]byte {
	sum := sha256.Sum256(append([]byte("secretdata-metacache-compact-v1"), secret...))
	return sum
}

// putCompactMeta 把**已解密的 meta 明文 JSON** 加密（无 padding）写入缓存。
// 失败静默忽略（缓存是加速器，绝不影响写路径）。
func (s *SecretdataFS) putCompactMeta(rel string, plain []byte, blobLen int64) {
	if s.opts.MetaCacheDir == "" || len(plain) == 0 {
		return
	}
	key := compactKey(s.secret)
	block, berr := aes.NewCipher(key[:])
	if berr != nil {
		return
	}
	gcm, gerr := cipher.NewGCM(block)
	if gerr != nil {
		return
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, rerr := rand.Read(nonce); rerr != nil {
		return
	}
	var lb [8]byte
	binary.BigEndian.PutUint64(lb[:], uint64(blobLen))
	out := append([]byte(metaCacheCompactMagic), lb[:]...)
	out = append(out, nonce...)
	out = gcm.Seal(out, nonce, plain, nil)
	cp := s.metaCachePath(rel)
	if mk := os.MkdirAll(filepath.Dir(cp), 0o700); mk != nil {
		return
	}
	tmp := fmt.Sprintf("%s.part.%d", cp, os.Getpid())
	if werr := os.WriteFile(tmp, out, 0o600); werr != nil {
		_ = os.Remove(tmp)
		return
	}
	if rerr := os.Rename(tmp, cp); rerr != nil {
		_ = os.Remove(tmp)
	}
}

// getCompactMeta 读取并解密精简缓存；任何异常（无 magic/解密失败/格式不符）返回 ok=false
// —— 调用方必须回落到权威源（meta blob），缓存永不作为唯一真相。
func (s *SecretdataFS) getCompactMeta(rel string) ([]byte, int64, bool) {
	if s.opts.MetaCacheDir == "" {
		return nil, 0, false
	}
	raw, err := os.ReadFile(s.metaCachePath(rel))
	if err != nil || len(raw) <= len(metaCacheCompactMagic)+8 || string(raw[:len(metaCacheCompactMagic)]) != metaCacheCompactMagic {
		return nil, 0, false
	}
	blobLen := int64(binary.BigEndian.Uint64(raw[len(metaCacheCompactMagic):]))
	key := compactKey(s.secret)
	block, berr := aes.NewCipher(key[:])
	if berr != nil {
		return nil, 0, false
	}
	gcm, gerr := cipher.NewGCM(block)
	if gerr != nil {
		return nil, 0, false
	}
	body := raw[len(metaCacheCompactMagic)+8:]
	if len(body) <= gcm.NonceSize() {
		return nil, 0, false
	}
	plain, derr := gcm.Open(nil, body[:gcm.NonceSize()], body[gcm.NonceSize():], nil)
	if derr != nil {
		return nil, 0, false
	}
	return plain, blobLen, true
}

// readFileMetaFast 读文件 meta：优先精简缓存（几 KB），未命中则读权威 blob 并回填。
// 任何缓存异常都退回 blob（缓存永不是唯一真相）。
func (s *SecretdataFS) readFileMetaFast(ctx context.Context, rel string) (*shardseal.Meta, int64, error) {
	if plain, blobLen, ok := s.getCompactMeta(rel); ok {
		var m shardseal.Meta
		if json.Unmarshal(plain, &m) == nil {
			return &m, blobLen, nil
		}
	}
	blob, rerr := s.readBlobCached(ctx, rel)
	if rerr != nil {
		return nil, 0, rerr
	}
	m, derr := s.decryptFileMeta(blob)
	if derr != nil {
		s.dropBlobCache(rel)
		return nil, 0, derr
	}
	if b, jerr := json.Marshal(m); jerr == nil {
		s.putCompactMeta(rel, b, int64(len(blob)))
	}
	return m, int64(len(blob)), nil
}

// readDirMetaFast 读目录 meta：同 readFileMetaFast（精简缓存优先 + blob 回退）。
func (s *SecretdataFS) readDirMetaFast(ctx context.Context, rel string) (*dirMeta, int64, error) {
	if plain, blobLen, ok := s.getCompactMeta(rel); ok {
		var dm dirMeta
		if json.Unmarshal(plain, &dm) == nil {
			return &dm, blobLen, nil
		}
	}
	blob, rerr := s.readBlobCached(ctx, rel)
	if rerr != nil {
		return nil, 0, rerr
	}
	dm, derr := s.decryptDirMeta(blob)
	if derr != nil {
		s.dropBlobCache(rel)
		return nil, 0, derr
	}
	if b, jerr := json.Marshal(dm); jerr == nil {
		s.putCompactMeta(rel, b, int64(len(blob)))
	}
	return dm, int64(len(blob)), nil
}

// ensureEntryMeta 确保索引条目带 meta：缺失（旧条目/快照降级）时从权威 blob 回源自愈，
// 并回填精简缓存 —— 保证「启动后访问文件」在 meta 缺失时仍能正确读出内容，且下次启动
// 直接命中缓存。任一失败返回错误（调用方按既有语义处理）。
func (s *SecretdataFS) ensureEntryMeta(ctx context.Context, e *metaEntry) error {
	s.mu.RLock()
	loaded := e.meta != nil
	s.mu.RUnlock()
	if loaded {
		return nil
	}
	m, _, merr := s.readFileMetaFast(ctx, path.Join(e.dirSeg, e.metaName))
	if merr != nil {
		return merr
	}
	s.mu.Lock()
	e.meta = m
	s.mu.Unlock()
	return nil
}
