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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
	if mk := os.MkdirAll(filepath.Dir(cp), 0o700); mk == nil {
		tmp := fmt.Sprintf("%s.part.%d", cp, os.Getpid())
		if werr := os.WriteFile(tmp, b, 0o600); werr == nil {
			_ = os.Rename(tmp, cp)
		} else {
			_ = os.Remove(tmp)
		}
	}
	return b, nil
}

// dropBlobCache 删除某 blob 的本地缓存（解密失败时调用，实现自愈）。
func (s *SecretdataFS) dropBlobCache(rel string) {
	if s.opts.MetaCacheDir == "" {
		return
	}
	_ = os.Remove(s.metaCachePath(rel))
}
