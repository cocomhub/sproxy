// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secretdata 是 secret_data 加密封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.2）：wrapper 卷，写入手动分块加密上传底层卷 `data/<校验和前16>/`、读取经 meta 解密还原。
// 实现 sync.FS，对上层透明（OpenRead/WriteFile/Stat/Delete/MakeDir 转发）。
//
// 卷内布局：
//
//	<secretdata根>/data/<校验和前16>/<加密分块文件>
//	<secretdata根>/meta/<校验和前16>/<meta文件>
//
// 寻址 `secretdata://<卷名>/<path>`；旧卷加载 = 扫描 meta/ 建内存索引 + 按需还原。
package secretdata

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/registry"
)

// Options 是 SecretdataFS 的构造参数（装配层从 v.Extra 解析填充）。
type Options struct {
	// Secret 是密钥字节（来自 secrets:// 卷，装配层解析后注入；必填）。
	Secret []byte
	// Algorithm 加密算法（默认 shardseal/aes-256-gcm）。
	Algorithm string
	// Block 分块策略（默认 random 1MB-200MB）。
	Block shardseal.BlockPolicy
	// TempDir 本地临时空间（默认 os.TempDir()/sproxy-secretdata）。
	TempDir string
}

// metaEntry 是索引条目：逻辑路径 ↔ 底层 meta/分块位置。
type metaEntry struct {
	size     int64
	mtime    int64
	hash16   string // data/<hash16> 的目录段
	metaPath string // meta/<hash16>/<metaname>
	dataDir  string // data/<hash16>
	meta     *shardseal.Meta
}

// SecretdataFS 是透明加解密的 sync.FS 包装，底层为任意 sync.FS。
// index 是内存态：逻辑路径 → meta 信息（旧卷加载时由 meta/ 扫入）。
type SecretdataFS struct {
	inner  syncpkg.FS
	secret []byte
	opts   Options
	temp   string

	mu    sync.RWMutex
	index map[string]*metaEntry
}

// NewFS 构造 secretdata FS。inner：底层卷 FS（secretdata 根视图）；opts：参数。
func NewFS(inner syncpkg.FS, opts Options) (*SecretdataFS, error) {
	if opts.Secret == nil {
		return nil, fmt.Errorf("secretdata: 密钥未提供（Options.Secret 必填）")
	}
	if opts.Algorithm == "" {
		opts.Algorithm = shardseal.AlgorithmName
	}
	if opts.Block.Min <= 0 || opts.Block.Max <= 0 {
		opts.Block = shardseal.DefaultBlockPolicy()
	}
	if opts.TempDir == "" {
		opts.TempDir = filepath.Join(os.TempDir(), "sproxy-secretdata")
	}
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("secretdata: 创建临时目录 %s 失败: %w", opts.TempDir, err)
	}
	fs := &SecretdataFS{inner: inner, secret: opts.Secret, opts: opts, temp: opts.TempDir, index: map[string]*metaEntry{}}
	if err := fs.loadIndex(context.Background()); err != nil {
		return nil, err
	}
	return fs, nil
}

// backend 是 ExternalBackend 实现。
type backend struct{ fs *SecretdataFS }

func (b *backend) FS() syncpkg.FS { return b.fs }
func (b *backend) Close() error   { return nil }

var _ registry.ExternalBackend = (*backend)(nil)

// NewBackend 构造 secretdata 的 registry.ExternalBackend。底层 target FS 由装配层注入
// （NewFS 负责构造）；v.Extra 只消费 target 元数据用于错误文案与类型判定。
func NewBackend(ctx context.Context, v volume.Volume, inner syncpkg.FS, opts Options) (registry.ExternalBackend, error) {
	if v.Type == "" || v.Type == volume.TypeLocal {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 类型 %q 不是外部 secretdata 卷", v.Name, v.Type)
	}
	if inner == nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q 底层 FS 未注入", v.Name)
	}
	fs, err := NewFS(inner, opts)
	if err != nil {
		return nil, fmt.Errorf("secretdata backend: 卷 %q FS 构造失败: %w", v.Name, err)
	}
	return &backend{fs: fs}, nil
}

// ---- sync.FS 接口 ----

// ListDir 列出逻辑根下（或 rel 目录下）的直接子项。
func (s *SecretdataFS) ListDir(ctx context.Context, rel string) ([]syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []syncpkg.Entry
	prefix := strings.TrimSuffix(rel, "/")
	if prefix != "" {
		prefix += "/"
	}
	for p, e := range s.index {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := strings.TrimPrefix(p, prefix)
		if rest == "" {
			continue
		}
		seg := rest
		if before, _, ok := strings.Cut(rest, "/"); ok {
			seg = before
		}
		if strings.Contains(rest, "/") {
			continue // 只列直接子项
		}
		out = append(out, syncpkg.Entry{Name: seg, Path: path.Join(rel, seg), Size: e.size, IsDir: e.meta == nil})
	}
	return out, nil
}

func (s *SecretdataFS) Stat(ctx context.Context, rel string) (*syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.index[strings.TrimPrefix(rel, "/")]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: e.size, IsDir: e.meta == nil}, nil
}

func (s *SecretdataFS) OpenRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	return s.openRead(ctx, strings.TrimPrefix(rel, "/"))
}

func (s *SecretdataFS) WriteFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	return s.writeFile(ctx, strings.TrimPrefix(rel, "/"), r, size, mtime)
}

func (s *SecretdataFS) Rename(ctx context.Context, from, to string) error {
	return fmt.Errorf("secretdata: Rename 暂不支持（逻辑路径改名走 delete+write）")
}

func (s *SecretdataFS) Delete(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[key]
	if !ok {
		return nil // 幂等：不存在视为已删
	}
	// 删除底层 meta/分块（best-effort）。
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(ctx, path.Join(e.dataDir, ci.FileName))
	}
	if e.meta != nil {
		_ = s.inner.Delete(ctx, e.metaPath)
	}
	delete(s.index, key)
	return nil
}

func (s *SecretdataFS) MakeDir(ctx context.Context, rel string) error {
	return s.inner.MakeDir(ctx, rel) // 目录不做加密，转发
}

var _ syncpkg.FS = (*SecretdataFS)(nil)

// ---- 内部实现 ----

// writeFile：读全量明文 → 分块加密到临时 → 上传 data/ 与 meta/ → 更新索引。
func (s *SecretdataFS) writeFile(ctx context.Context, rel string, r io.Reader, size, mtime int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("secretdata: 读明文失败: %w", err)
	}
	tmp, err := os.MkdirTemp(s.temp, "chunks-*")
	if err != nil {
		return fmt.Errorf("secretdata: 创建临时分块目录失败: %w", err)
	}
	defer os.RemoveAll(tmp)

	out, perr := encryptContent(string(data), tmp, s.secret, s.opts.Block, rel)
	if perr != nil {
		return fmt.Errorf("secretdata: 分块加密失败: %w", perr)
	}

	hash16 := entryHash16(out.Meta)
	dataDir := path.Join("data", hash16)
	metaDir := path.Join("meta", hash16)
	for _, cn := range out.ChunkNames {
		blob, rerr := os.ReadFile(filepath.Join(tmp, cn))
		if rerr != nil {
			return fmt.Errorf("secretdata: 读本地分块 %s 失败: %w", cn, rerr)
		}
		if werr := s.inner.WriteFile(ctx, path.Join(dataDir, cn), bytesReader(blob), int64(len(blob)), mtime); werr != nil {
			return fmt.Errorf("secretdata: 上传分块 %s 失败: %w", cn, werr)
		}
	}
	metaJSON, _ := json.Marshal(out.Meta)
	metaPath := path.Join(metaDir, out.MetaName)
	if werr := s.inner.WriteFile(ctx, metaPath, bytesReader(metaJSON), int64(len(metaJSON)), mtime); werr != nil {
		return fmt.Errorf("secretdata: 上传 meta 失败: %w", werr)
	}

	s.mu.Lock()
	s.index[rel] = &metaEntry{
		size:     int64(len(data)),
		mtime:    mtime,
		hash16:   hash16,
		metaPath: metaPath,
		dataDir:  dataDir,
		meta:     out.Meta,
	}
	s.mu.Unlock()
	return nil
}

// openRead：读取逻辑路径 → 还原解密到临时文件 → 返回 ReadCloser。
func (s *SecretdataFS) openRead(ctx context.Context, rel string) (io.ReadCloser, error) {
	s.mu.RLock()
	e, ok := s.index[rel]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("secretdata: 文件 %q 不存在", rel)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: %q 是目录", rel)
	}
	tmp, err := os.CreateTemp(s.temp, "decrypt-*")
	if err != nil {
		return nil, fmt.Errorf("secretdata: 创建解密临时文件失败: %w", err)
	}
	chunkLocalDir := filepath.Join(s.temp, "view-"+e.hash16)
	if err := os.MkdirAll(chunkLocalDir, 0o700); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	defer os.RemoveAll(chunkLocalDir)
	for _, ci := range e.meta.Chunks {
		rc, rerr := s.inner.OpenRead(ctx, path.Join(e.dataDir, ci.FileName))
		if rerr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("secretdata: 读底层分块 %s 失败: %w", ci.FileName, rerr)
		}
		blob, berr := io.ReadAll(rc)
		rc.Close()
		if berr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, fmt.Errorf("secretdata: 读底层分块 %s 失败: %w", ci.FileName, berr)
		}
		if werr := os.WriteFile(filepath.Join(chunkLocalDir, ci.FileName), blob, 0o600); werr != nil {
			tmp.Close()
			os.Remove(tmp.Name())
			return nil, werr
		}
	}
	if derr := shardseal.DecryptFile(e.meta, chunkLocalDir, tmp.Name(), s.secret); derr != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, derr
	}
	if _, serr := tmp.Seek(0, 0); serr != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, serr
	}
	return &cleanupReadCloser{rc: tmp, path: tmp.Name()}, nil
}

// loadIndex：扫描底层 meta/ 目录建索引（旧卷加载，§10）。
func (s *SecretdataFS) loadIndex(ctx context.Context) error {
	metaRoot := "meta"
	dirs, err := s.inner.ListDir(ctx, metaRoot)
	if err != nil {
		return nil // meta 目录不存在 → 空卷（首次使用）
	}
	for _, d := range dirs {
		if !d.IsDir {
			continue
		}
		sub, serr := s.inner.ListDir(ctx, path.Join(metaRoot, d.Name))
		if serr != nil {
			continue
		}
		for _, f := range sub {
			if f.IsDir || !shardseal.IsMetaName(f.Name) {
				continue
			}
			rc, oerr := s.inner.OpenRead(ctx, path.Join(metaRoot, d.Name, f.Name))
			if oerr != nil {
				continue
			}
			blob, berr := io.ReadAll(rc)
			rc.Close()
			if berr != nil {
				continue
			}
			var m shardseal.Meta
			if jerr := json.Unmarshal(blob, &m); jerr != nil {
				continue
			}
			// 逻辑路径从 meta.original.Name 推断（卷内以原始文件名为逻辑键）。
			rel := m.Original.Name
			if rel == "" {
				continue
			}
			s.mu.Lock()
			s.index[rel] = &metaEntry{
				size:     m.Original.Size,
				hash16:   d.Name,
				metaPath: path.Join(metaRoot, d.Name, f.Name),
				dataDir:  path.Join("data", d.Name),
				meta:     &m,
			}
			s.mu.Unlock()
		}
	}
	return nil
}

// bytesReader 包装 []byte 为 io.Reader。
func bytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }

// entryHash16 返回分块目录 hash16（writeFile 期计算）：
// 用 meta.Original.SHA256 前 16hex 作为目录段——data/ 与 meta/ 目录一致，
// 且解密按 meta 定位分块目录不受影响。loadIndex 已按扫描的 d.Name 记入条目，
// 本函数仅新建条目时需要确定性目录名。
func entryHash16(m *shardseal.Meta) string {
	if len(m.Original.SHA256) < 16 {
		return ""
	}
	return m.Original.SHA256[:16]
}
