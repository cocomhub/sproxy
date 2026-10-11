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
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// transformCacheKey 派生派生内容缓存键：sha256(rel|checksum|mtime|size|transform|width)。
// checksum 为空（台账未装配）时用 rel+mtime+size 回退——原文件任何变化都会改键，杜绝脏缓存。
func transformCacheKey(rel, checksum string, size, mtime int64, transform string, width int, watermark string) string {
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
	// 分享水印（审查 P1 修复，批次9）：watermark 参与缓存键——带水印与无水印（或不同
	// seed）请求必须隔离缓存，否则共享条目导致水印绕过（先缓存无水印图 → 带水印请求
	// 命中返回无水印）或反向污染（带水印写同键 → 无水印请求拿水印图）。
	h.Write([]byte{0})
	h.Write([]byte(watermark))
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

// transformCacheRel 返回租户根相对的缓存键路径（`meta/transform/<key>`）。
func transformCacheRel(key string) string {
	return filepath.ToSlash(filepath.Join("meta", "transform", key))
}

// loadTransformCache 尝试读缓存文件；不存在/不可读 → (nil, false)。
//
// **加密感知**（评审 P2）：派生缓存（缩略图/gzip 等**明文派生**）落在卷上时必须与卷的 at-rest
// 状态一致——裸 `os.Open` 会在加密卷上把明文读回（自洽所以不报错，但泄漏点隐蔽）。经
// `storage.Root` 的 `OpenDecrypted` 读取（非加密卷等价 raw）。
func loadTransformCache(t *storage.Tenant, key string) (io.ReadCloser, bool) {
	rc, err := t.Root().OpenDecrypted(transformCacheRel(key))
	if err != nil {
		return nil, false
	}
	return rc, true
}

// storeTransformCache 原子写缓存（tmp + rename；目录不存在时 MkdirAll）。
// 失败静默（缓存是加速层，失败不影响派生内容生成）。
// storeTransformCache 原子写缓存（tmp + rename；目录不存在时 MkdirAll）。
// 失败静默（缓存是加速层，失败不影响派生内容生成）。**加密感知**：经 Root 的
// `OpenFileEncrypted` 落盘（加密卷上密文，避免明文派生数据以明文形态留在加密卷上，评审 P2）。
func storeTransformCache(t *storage.Tenant, key string, data []byte) {
	root := t.Root()
	rel := transformCacheRel(key)
	dirRel := filepath.ToSlash(filepath.Dir(rel))
	if err := root.MkdirAll(dirRel, 0o755); err != nil {
		return
	}
	tmpRel := rel + ".tmp"
	w, err := root.OpenFileEncrypted(tmpRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return
	}
	if _, werr := w.Write(data); werr != nil {
		_ = w.Close()
		_ = root.Remove(tmpRel)
		return
	}
	if cerr := w.Close(); cerr != nil {
		_ = root.Remove(tmpRel)
		return
	}
	_ = root.Rename(tmpRel, rel)
}

// TransformCacheGCOptions 是派生缓存 GC 参数（零值 = 默认语义）。
type TransformCacheGCOptions struct {
	// MaxAge 是缓存文件最大保留期（mtime 起算）；0 = 默认 7 天。
	MaxAge time.Duration
}

// defaultTransformCacheMaxAge 是默认缓存保留期（7 天）。
const defaultTransformCacheMaxAge = 7 * 24 * time.Hour

// CleanupTransformCache 清理租户的派生缓存目录：
//   - 孤儿 tmp（*.tmp，异常退出残留）删除；
//   - 超过 MaxAge 的缓存文件删除（按 mtime）。
//
// 幂等；目录不存在安全（no-op）。返回值统计（清理数，供日志/测试断言）。
func CleanupTransformCache(t *storage.Tenant, opts TransformCacheGCOptions) (removed int) {
	if t == nil {
		return 0
	}
	dir, ok := t.Root().Abs(filepath.ToSlash(filepath.Join("meta", "transform")))
	if !ok {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0 // 目录不存在/不可读 = no-op
	}
	maxAge := opts.MaxAge
	if maxAge <= 0 {
		maxAge = defaultTransformCacheMaxAge
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		// 孤儿 tmp 恒删（原子写的中途残留，键文件可能已不存在）。
		if strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name))
			removed++
			continue
		}
		// 过期键：mtime + MaxAge < now → 删。
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > maxAge {
			_ = os.Remove(filepath.Join(dir, name))
			removed++
		}
	}
	return removed
}
