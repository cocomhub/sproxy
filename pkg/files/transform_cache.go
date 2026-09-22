// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// transformCacheKey 派生派生内容缓存键：sha256(rel|checksum|mtime|size|transform|width)。
// checksum 为空（台账未装配）时用 rel+mtime+size 回退——原文件任何变化都会改键，杜绝脏缓存。
func transformCacheKey(rel, checksum string, size, mtime int64, transform string, width int) string {
	h := sha256.New()
	h.Write([]byte(rel))
	h.Write([]byte{0})
	h.Write([]byte(checksum))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", mtime)
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", size)
	h.Write([]byte{0})
	h.Write([]byte(transform))
	h.Write([]byte{0})
	fmt.Fprintf(h, "%d", width)
	return hex.EncodeToString(h.Sum(nil))
}

// transformCachePath 返回租户 meta/transform 缓存目录的键文件路径（目录不存在时返回 ""）。
func transformCachePath(t *storage.Tenant, key string) string {
	abs, ok := t.Root().Abs(filepath.ToSlash(filepath.Join("meta", "transform", key)))
	if !ok {
		return ""
	}
	return abs
}

// loadTransformCache 尝试读缓存文件；不存在/不可读 → (nil, false)。
func loadTransformCache(t *storage.Tenant, key string) (io.ReadCloser, bool) {
	p := transformCachePath(t, key)
	if p == "" {
		return nil, false
	}
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	return f, true
}

// storeTransformCache 原子写缓存（tmp + rename；目录不存在时 MkdirAll）。
// 失败静默（缓存是加速层，失败不影响派生内容生成）。
func storeTransformCache(t *storage.Tenant, key string, data []byte) {
	p := transformCachePath(t, key)
	if p == "" {
		return
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
