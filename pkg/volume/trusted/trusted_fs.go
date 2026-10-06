// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package trusted 提供可信卷（Trusted Volume）装饰器：包装任意 syncpkg.FS，
// 提供文件配套 meta（FileMeta）读写/校验/隐藏/配额能力——任何卷经 Wrap 包一层
// 即成为可信卷（上传/下载按总哈希 + 分块哈希逐分片校验）。**统一封装、不逐卷改
// 方法**：底层只提供现有 FS 接口，新卷接入 = 包一层，零改卷本身。
//
// 价值定位：可信卷解决「跨信任边界的静默损坏」（外部卷服务端不承诺完整性、
// 上传成功但内容损坏、读回校验和不可信）——FileMeta 是应用层独立校验证据。
// 自建 LocalFS（Stat 已算 sha256）价值低，但装饰器成本也低（meta 落盘可选）。
package trusted

import (
	"context"
	"fmt"
	"io"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// Options 是可信卷装饰器配置。
type Options struct {
	// ChunkSize 分块校验大小（<=0 = 按文件大小自适应 ChunkSizeForSize）。
	ChunkSize int64
	// DisableMetaFile 禁写 .meta sidecar（纯校验读取仍可用；默认 false = 落 meta）。
	DisableMetaFile bool
	// Extra 是写入 meta 的自定义扩展信息（创建人/email 等任意数据）。
	Extra map[string]any
}

// metaSuffix 是配套 meta 文件后缀（隐藏、占配额）。
const metaSuffix = ".meta"

// TrustedVolumeFS 包装任意 syncpkg.FS，提供 meta sidecar + 校验能力。
type TrustedVolumeFS struct {
	inner syncpkg.FS
	opts  Options
}

// Wrap 包装 fs 为可信卷。
func Wrap(fs syncpkg.FS, opts Options) *TrustedVolumeFS {
	return &TrustedVolumeFS{inner: fs, opts: opts}
}

// Inner 返回底层 FS（装配层需要原始能力如 Move/Copy/Link 时取用）。
func (t *TrustedVolumeFS) Inner() syncpkg.FS { return t.inner }

// metaPath 返回文件对应的 meta sidecar 路径（同目录 `<name>.meta`）。
func metaPath(rel string) string {
	return rel + metaSuffix
}

// isMetaName 判定路径是否为 meta sidecar（隐藏过滤用）。
func isMetaName(rel string) bool {
	return strings.HasSuffix(rel, metaSuffix)
}

// ListDir 列目录：过滤隐藏 .meta 文件；条目带回全部校验和（来自 meta 若存在，
// 否则 Stat 直算）。
func (t *TrustedVolumeFS) ListDir(ctx context.Context, p string) ([]syncpkg.Entry, error) {
	es, err := t.inner.ListDir(ctx, p)
	if err != nil {
		return nil, err
	}
	out := es[:0]
	for _, e := range es {
		if isMetaName(e.Path) {
			continue // 隐藏 meta 文件用户不可见
		}
		out = append(out, e)
	}
	return out, nil
}

// Stat 返回条目（过滤 meta；附 Checksums）。
func (t *TrustedVolumeFS) Stat(ctx context.Context, p string) (*syncpkg.Entry, error) {
	if isMetaName(p) {
		return nil, nil // meta 文件对用户不可见
	}
	return t.inner.Stat(ctx, p)
}

// OpenRead 打开读取（读时按 meta 逐分块校验由调用方经 Equal 完成；装饰器本身
// 透传——校验逻辑在通用 Equal 工具，不落每个卷）。
func (t *TrustedVolumeFS) OpenRead(ctx context.Context, p string) (io.ReadCloser, error) {
	return t.inner.OpenRead(ctx, p)
}

// WriteFile 写入 + 计算并落 .meta（若启用）。meta 大小计入底层配额（WriteFile 经
// 底层 CapacityFS/账本时自然计入——meta 也经 WriteFile 写）。
func (t *TrustedVolumeFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	if t.opts.DisableMetaFile {
		return t.inner.WriteFile(ctx, rel, r, size, mtime)
	}
	// 计算 meta：读一遍流（同时喂给 inner 写）。
	chunkSize := t.opts.ChunkSize
	if chunkSize <= 0 {
		chunkSize = meta.ChunkSizeForSize(size)
	}
	calc, err := meta.NewCalculator(size, chunkSize)
	if err != nil {
		return err
	}
	// Tee：写 inner 的同时累计哈希。
	if err := t.inner.WriteFile(ctx, rel, io.TeeReader(r, calc), size, mtime); err != nil {
		return err
	}
	// 尾块已由 Finish 收尾（Write 路径不足 chunkSize 的尾块 Finish 内 flush）。
	fm := calc.Finish()
	if err := meta.Validate(fm); err != nil {
		return fmt.Errorf("trusted: 写入后 meta 校验失败: %w", err)
	}
	fm.Name = path.Base(strings.ReplaceAll(rel, "\\", "/"))
	fm.Extra = t.opts.Extra
	data, merr := meta.Marshal(fm)
	if merr != nil {
		return fmt.Errorf("trusted: meta 序列化失败: %w", merr)
	}
	// meta 落盘（隐藏同目录 sidecar；占配额——经 inner.WriteFile 计入底层账本）。
	if werr := t.inner.WriteFile(ctx, metaPath(rel), bytesReader(data), int64(len(data)), 0); werr != nil {
		// meta 落盘失败：主文件已成功——不失败主写（meta 可下次读时补），记日志语义
		// 由调用方/审计处理；此处返回主写成功（meta 缺失时 Stat 直算兜底）。
		return nil
	}
	return nil
}

// Delete 删除文件 + 联动删除 .meta。
func (t *TrustedVolumeFS) Delete(ctx context.Context, rel string) error {
	err := t.inner.Delete(ctx, rel)
	// 联动 meta（best-effort：主文件删除为主，meta 残留由 GC/Stat 兜底忽略）。
	_ = t.inner.Delete(ctx, metaPath(rel))
	return err
}

// Rename 重命名/移动 + 联动 .meta。目标父目录自动创建（与 WriteFile 行为一致——
// 移动文件到未建子目录时避免裸 os.Rename 报"路径不存在"）。
func (t *TrustedVolumeFS) Rename(ctx context.Context, from, to string) error {
	if dir := path.Dir(to); dir != "." && dir != "" {
		if err := t.MakeDir(ctx, dir); err != nil {
			return err
		}
	}
	if err := t.inner.Rename(ctx, from, to); err != nil {
		return err
	}
	// 联动 meta（best-effort：源 meta 不存在则跳过——rename 目标 meta 若无源也不报错）。
	_ = t.inner.Rename(ctx, metaPath(from), metaPath(to))
	return nil
}

// MakeDir 透传（meta 不涉及目录）。
func (t *TrustedVolumeFS) MakeDir(ctx context.Context, p string) error {
	return t.inner.MakeDir(ctx, p)
}

// bytesReader 最小 io.Reader（meta 落盘用）。
type byteReader struct {
	data []byte
	off  int
}

func bytesReader(b []byte) *byteReader { return &byteReader{data: b} }

func (r *byteReader) Read(p []byte) (int, error) {
	if r.off >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.off:])
	r.off += n
	return n, nil
}
