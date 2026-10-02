// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package secretdata 是 secret_data 加密封装卷（设计 docs/designs/2026-10-01-secret-volume.md
// §6.2）：wrapper 卷，写入手动分块加密上传底层卷随机容器目录、读取经 meta 解密还原。
// 实现 sync.FS，对上层透明（OpenRead/WriteFile/Stat/Delete/MakeDir 转发）。
//
// 卷内布局（无顶层 data/meta 结构词，全随机容器）：
//
//	<secretdata根>/<randDir 5-30>/           # 一个逻辑目录 = 一个随机命名容器目录
//	      目录meta（@ 标记，含逻辑 path，加密）
//	      文件meta（-/_ 标记，含文件根信息，加密）
//	      加密分块（无标记）
//
// 寻址 `secretdata://<卷名>/<path>`；旧卷加载 = 扫描容器目录 meta → path、文件 meta →
// name 重建完整路径索引 + 按需还原（修复 F-1：逻辑路径与索引键在写路径与重启恢复一致）。
package secretdata

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	// TempDir 本地临时空间（默认 os.MkdirTemp 随机目录，0700 不可预测）。
	TempDir string
	// MetaPadBytes 文件/目录 meta 加密落盘的 pad 目标基准（默认 = Block.Min）。
	// pad 目标 = MetaPadBytes + rand(MetaPadBytes)，受统一格式 R 地板（192B）约束：
	// 取 max(192, 目标) 使 meta blob 与底层分块大小分布重叠，难以凭文件大小区分。
	// 装配层从 extra.meta_pad_bytes 传入（任务 6）。
	MetaPadBytes int64
}

// metaEntry 是索引条目：逻辑文件路径 ↔ 底层容器/分块位置。
type metaEntry struct {
	size     int64
	mtime    int64
	dirSeg   string // 随机容器目录名（5-30）
	metaName string // 文件 meta 加密 blob 名（含 -/_ 标记）
	meta     *shardseal.Meta
}

// dirMeta 是目录 meta 的加密 JSON（@ 标记，记录逻辑 path；目录移动仅更新 path）。
type dirMeta struct {
	Version   int    `json:"version"`
	Algorithm string `json:"algorithm"`
	KDF       string `json:"kdf"`
	Type      string `json:"type"`
	Path      string `json:"path"`
	MTime     string `json:"mtime"`
	DirID     string `json:"dir_id"`
}

// SecretdataFS 是透明加解密的 sync.FS 包装，底层为任意 sync.FS。
// index/dirs/dirSegs 是内存态：index 逻辑文件 → meta；dirs 逻辑目录集合；
// dirSegs 逻辑目录 → 随机容器目录名（写路径定位/创建容器，旧卷加载由目录 meta 重建）。
type SecretdataFS struct {
	inner  syncpkg.FS
	secret []byte
	opts   Options
	temp   string
	// ownedTemp 标记 temp 目录由 NewFS 自建（os.MkdirTemp 随机目录），
	// backend.Close 负责清理；显式配置的 TempDir 不清理（调用方所有）。
	ownedTemp bool

	mu      sync.RWMutex
	index   map[string]*metaEntry
	dirs    map[string]struct{} // 已知逻辑目录（子目录发现/进入用）
	dirSegs map[string]string   // 逻辑目录 → 随机容器目录名
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
	ownedTemp := false
	if opts.TempDir == "" {
		// 默认临时目录：os.MkdirTemp 生成随机名（不可预测 + 0700），避免
		// os.TempDir() 下固定可预测的公开可写路径（Sonar go:S5445）。
		dir, err := os.MkdirTemp("", "sproxy-secretdata-")
		if err != nil {
			return nil, fmt.Errorf("secretdata: 创建默认临时目录失败: %w", err)
		}
		opts.TempDir = dir
		ownedTemp = true
	}
	// 显式配置的 TempDir 同样收紧为 0700 并在创建时校验归属。
	if err := os.MkdirAll(opts.TempDir, 0o700); err != nil {
		return nil, fmt.Errorf("secretdata: 创建临时目录 %s 失败: %w", opts.TempDir, err)
	}
	fs := &SecretdataFS{
		inner: inner, secret: opts.Secret, opts: opts, temp: opts.TempDir, ownedTemp: ownedTemp,
		index: map[string]*metaEntry{}, dirs: map[string]struct{}{}, dirSegs: map[string]string{},
	}
	if err := fs.loadIndex(context.Background()); err != nil {
		return nil, err
	}
	return fs, nil
}

// backend 是 ExternalBackend 实现。
type backend struct{ fs *SecretdataFS }

func (b *backend) FS() syncpkg.FS { return b.fs }
func (b *backend) Close() error {
	// 仅清理 NewFS 自建的随机临时目录；显式配置的 TempDir 归调用方所有，不碰。
	if b.fs.ownedTemp {
		return os.RemoveAll(b.fs.temp)
	}
	return nil
}

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

// splitDirPrefix 把 rel 规范化为目录前缀（含尾部 /；根为 ""）。
func splitDirPrefix(rel string) string {
	p := strings.Trim(rel, "/")
	if p == "" {
		return ""
	}
	return p + "/"
}

// entryOf 构造逻辑条目（正斜杠相对路径）。
func entryOf(rel, seg string, size int64, isDir bool) syncpkg.Entry {
	p := path.Join(rel, seg)
	return syncpkg.Entry{Name: seg, Path: p, Size: size, IsDir: isDir}
}

// ListDir 列出逻辑 rel 目录下的直接子项（透明子目录可发现/进入）。
// 索引只存完整逻辑路径文件键；这里对每个路径把下一段作为子项聚合，
// 跨段前缀呈现为目录条目（审查 F-2：ListDir 永不返回子目录）。
func collectChildSegments(paths map[string]*metaEntry, dirs map[string]struct{}, prefix, rel string, entrySize func(*metaEntry) int64) []syncpkg.Entry {
	out := collectIndexSegments(paths, prefix, rel, entrySize)
	return append(out, collectDirSegments(dirs, prefix, rel)...)
}

// collectIndexSegments 从 index 文件键聚合：首段为目录则目录条目，否则文件条目。
func collectIndexSegments(paths map[string]*metaEntry, prefix, rel string, entrySize func(*metaEntry) int64) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for p := range paths {
		if !strings.HasPrefix(p, prefix) || len(p) <= len(prefix) {
			continue
		}
		rest := p[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		out = append(out, entryOf(rel, rest, entrySize(paths[p]), false))
	}
	return out
}

// collectDirSegments 从 dirs 目录键聚合：只贡献目录条目（空/字节目录可见性）。
func collectDirSegments(dirs map[string]struct{}, prefix, rel string) []syncpkg.Entry {
	var out []syncpkg.Entry
	seenDirs := map[string]bool{}
	for d := range dirs {
		if !strings.HasPrefix(d, prefix) || len(d) <= len(prefix) {
			continue
		}
		rest := d[len(prefix):]
		if before, _, has := strings.Cut(rest, "/"); has {
			if !seenDirs[before] {
				out = append(out, entryOf(rel, before, 0, true))
				seenDirs[before] = true
			}
			continue
		}
		if !seenDirs[rest] {
			out = append(out, entryOf(rel, rest, 0, true))
			seenDirs[rest] = true
		}
	}
	return out
}

func (s *SecretdataFS) ListDir(ctx context.Context, rel string) ([]syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	prefix := splitDirPrefix(rel)
	out := collectChildSegments(s.index, s.dirs, prefix, rel, func(e *metaEntry) int64 {
		if e == nil {
			return 0
		}
		return e.size
	})
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // 目录在前
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func (s *SecretdataFS) Stat(ctx context.Context, rel string) (*syncpkg.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := strings.TrimPrefix(rel, "/")
	if _, ok := s.dirs[key]; ok {
		return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: 0, IsDir: true}, nil
	}
	e, ok := s.index[key]
	if !ok {
		return nil, nil
	}
	return &syncpkg.Entry{Name: path.Base(rel), Path: rel, Size: e.size, IsDir: false}, nil
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

// Delete 删除逻辑文件：底层容器内删除分块与文件 meta 后移出索引。
func (s *SecretdataFS) Delete(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.index[key]
	if !ok {
		return nil // 幂等：不存在视为已删
	}
	s.removeVersionMeta(e)
	delete(s.index, key)
	s.pruneDirsLocked()
	return nil
}

// MakeDir 创建逻辑空目录：随机容器目录 + 目录 meta（@ 标记记录逻辑 path），
// 不再提若干层（旧 inner.MakeDir(rel) 会向底层泄漏目录名，违背目录名保密）。
func (s *SecretdataFS) MakeDir(ctx context.Context, rel string) error {
	key := strings.TrimPrefix(rel, "/")
	_, _, _, err := s.ensureContainer(ctx, key, 0)
	return err
}

var _ syncpkg.FS = (*SecretdataFS)(nil)

// ---- 内部实现 ----

// writeFile：读全量明文 → 分块加密到临时 → 上传分块 + 加密文件 meta 到逻辑目录
// 的随机容器 → 全部成功后才原子切换索引（覆盖写中途失败删新留旧，F-2）。
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

	parentDir := parentDirOf(rel)
	container, containerCreated, dmName, err := s.ensureContainer(ctx, parentDir, mtime)
	if err != nil {
		return err
	}

	metaName, metaBlob, merr := s.buildMetaBlob(out)
	if merr != nil {
		return merr
	}
	uploaded := []string{path.Join(container, metaName)}
	if uerr := s.uploadChunks(ctx, container, tmp, mtime, out.ChunkNames, &uploaded); uerr != nil {
		s.rollbackWrite(ctx, container, uploaded, containerCreated, parentDir, dmName)
		return uerr
	}
	if werr := s.inner.WriteFile(ctx, path.Join(container, metaName), bytes.NewReader(metaBlob), int64(len(metaBlob)), mtime); werr != nil {
		s.rollbackWrite(ctx, container, uploaded, containerCreated, parentDir, dmName)
		return fmt.Errorf("secretdata: 上传 meta 失败: %w", werr)
	}

	// 全部上传成功 → 原子切换索引；best-effort 删除旧版本分块/meta。
	s.mu.Lock()
	prev := s.index[rel]
	s.index[rel] = &metaEntry{size: int64(len(data)), mtime: mtime, dirSeg: container, metaName: metaName, meta: out.Meta}
	addDirKeysLocked(s.dirs, rel)
	s.mu.Unlock()
	if prev != nil {
		s.removeVersionMeta(prev)
	}
	return nil
}

// buildMetaBlob 构造文件 meta 加密 blob：独立随机盐 + key，pad 到 metaPadTarget，
// 以 out.MetaName（真实三段名，含 -/_ 标记）为上传目标名。
func (s *SecretdataFS) buildMetaBlob(out *shardseal.EncryptionResult) (string, []byte, error) {
	salt, serr := shardseal.RandSalt()
	if serr != nil {
		return "", nil, serr
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt)
	if kerr != nil {
		return "", nil, kerr
	}
	metaJSON, merr := json.Marshal(out.Meta)
	if merr != nil {
		return "", nil, merr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, metaJSON, s.metaPadTarget())
	if berr != nil {
		return "", nil, berr
	}
	return out.MetaName, blob, nil
}

// uploadChunks 逐块上传到容器目录，并把已上传路径追加到 uploaded（供失败回滚删新留旧）。
func (s *SecretdataFS) uploadChunks(ctx context.Context, container, tmp string, mtime int64, chunkNames []string, uploaded *[]string) error {
	for _, cn := range chunkNames {
		blob, rerr := os.ReadFile(filepath.Join(tmp, cn))
		if rerr != nil {
			return fmt.Errorf("secretdata: 读本地分块 %s 失败: %w", cn, rerr)
		}
		p := path.Join(container, cn)
		if werr := s.inner.WriteFile(ctx, p, bytes.NewReader(blob), int64(len(blob)), mtime); werr != nil {
			return fmt.Errorf("secretdata: 上传分块 %s 失败: %w", cn, werr)
		}
		*uploaded = append(*uploaded, p)
	}
	return nil
}

// rollbackWrite 失败回滚：删除本次已上传的新 meta/分块；若容器为本次新建，一并移除
// 目录 meta 并注销 dirSegs/dirs（删新留旧，F-2）。
func (s *SecretdataFS) rollbackWrite(ctx context.Context, container string, uploaded []string, created bool, parentDir, dmName string) {
	for _, p := range uploaded {
		_ = s.inner.Delete(ctx, p)
	}
	if !created {
		return
	}
	if dmName != "" {
		_ = s.inner.Delete(ctx, path.Join(container, dmName))
	}
	s.mu.Lock()
	delete(s.dirSegs, parentDir)
	delete(s.dirs, parentDir)
	s.mu.Unlock()
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
	// 唯一临时分块目录（审查 F-5：固定 view/view-<hash> 并发读同文件有竞态窗口）。
	chunkLocalDir, err := os.MkdirTemp(s.temp, "view-*")
	if err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return nil, err
	}
	defer os.RemoveAll(chunkLocalDir)
	for _, ci := range e.meta.Chunks {
		rc, rerr := s.inner.OpenRead(ctx, path.Join(e.dirSeg, ci.FileName))
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

// loadIndex：扫描底层根下全部随机容器，解析目录 meta（→ path）与文件 meta（→ rel），
// 重建内存索引（旧卷加载，§10 / 修复 F-1：逻辑键与写路径一致）。
func (s *SecretdataFS) loadIndex(ctx context.Context) error {
	root, err := s.inner.ListDir(ctx, "")
	if err != nil {
		return nil // 底层不存在 → 空卷（首次使用）
	}
	for _, d := range root {
		if !d.IsDir {
			continue
		}
		s.loadContainer(ctx, d.Name)
	}
	return nil
}

// loadContainer 扫描单个随机容器：目录 meta 提供逻辑 path，文件 meta 提供文件根信息。
// 单文件损坏 / 目录 meta 缺失时跳过该容器（旧卷可恢复，容错扫描）。
func (s *SecretdataFS) loadContainer(ctx context.Context, container string) {
	inner, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return
	}
	dirPath := s.loadContainerDirMeta(ctx, container, inner)
	for _, f := range inner {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindFileMeta {
			continue
		}
		s.loadContainerFileMeta(ctx, container, dirPath, f)
	}
}

// loadContainerDirMeta 解析容器的目录 meta（@ → 逻辑 path），登记 dirSegs/dirs。
func (s *SecretdataFS) loadContainerDirMeta(ctx context.Context, container string, inner []syncpkg.Entry) string {
	for _, f := range inner {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindDirMeta {
			continue
		}
		blob, oerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
		if oerr != nil {
			continue
		}
		dm, derr := s.decryptDirMeta(blob)
		if derr != nil {
			continue
		}
		s.mu.Lock()
		s.dirSegs[dm.Path] = container
		if dm.Path != "" {
			s.dirs[dm.Path] = struct{}{}
		}
		addDirKeysLocked(s.dirs, dm.Path)
		s.mu.Unlock()
		return dm.Path
	}
	return ""
}

// loadContainerFileMeta 解密单个文件 meta 并登记索引（逻辑 rel = 目录 meta.path + basename）。
func (s *SecretdataFS) loadContainerFileMeta(ctx context.Context, container, dirPath string, f syncpkg.Entry) {
	blob, oerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
	if oerr != nil {
		return
	}
	mm, merr := s.decryptFileMeta(blob)
	if merr != nil {
		return
	}
	rel := path.Join(dirPath, mm.Original.Name)
	s.mu.Lock()
	s.index[rel] = &metaEntry{size: mm.Original.Size, mtime: f.MTime, dirSeg: container, metaName: f.Name, meta: mm}
	addDirKeysLocked(s.dirs, rel)
	s.mu.Unlock()
}

// readBlob 读取底层文件全部字节（容错：失败返回 error）。
func readBlob(ctx context.Context, inner syncpkg.FS, p string) ([]byte, error) {
	rc, err := inner.OpenRead(ctx, p)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// decryptFileMeta 解密文件 meta blob 并反序列化为 shardseal.Meta。
func (s *SecretdataFS) decryptFileMeta(blob []byte) (*shardseal.Meta, error) {
	raw, err := s.decryptBlob(blob)
	if err != nil {
		return nil, err
	}
	var m shardseal.Meta
	if json.Unmarshal(raw, &m) != nil {
		return nil, fmt.Errorf("secretdata: 文件 meta 反序列化失败")
	}
	if m.Original.Name == "" || len(m.Chunks) == 0 {
		return nil, fmt.Errorf("secretdata: 文件 meta 字段不完整")
	}
	return &m, nil
}

// decryptDirMeta 解密目录 meta blob 并反序列化为 dirMeta。
func (s *SecretdataFS) decryptDirMeta(blob []byte) (*dirMeta, error) {
	raw, err := s.decryptBlob(blob)
	if err != nil {
		return nil, err
	}
	var dm dirMeta
	if json.Unmarshal(raw, &dm) != nil {
		return nil, fmt.Errorf("secretdata: 目录 meta 反序列化失败")
	}
	if dm.Type != "dir" {
		return nil, fmt.Errorf("secretdata: 目录 meta type=%q，应为 dir", dm.Type)
	}
	return &dm, nil
}

// decryptBlob 用 blob 内嵌盐派生 key 解密统一格式 meta blob。
func (s *SecretdataFS) decryptBlob(blob []byte) ([]byte, error) {
	salt, err := shardseal.MetaBlobSalt(blob)
	if err != nil {
		return nil, err
	}
	key, err := shardseal.DeriveKey(s.secret, salt)
	if err != nil {
		return nil, err
	}
	return shardseal.DecryptMetaJSON(key, blob)
}

// ensureContainer 返回 parentDir 逻辑目录的随机容器目录名；不存在则创建容器并写
// 目录 meta（@encrypt，记录逻辑 path）。createdNew 标记本次新建（供失败回滚清理）。
// 持有 s.mu（含底层 I/O，避免并发建容器 TOCTOU）。
func (s *SecretdataFS) ensureContainer(ctx context.Context, parentDir string, mtime int64) (container string, createdNew bool, dmName string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.dirSegs[parentDir]; ok {
		return c, false, "", nil
	}
	c, cerr := shardseal.RandDirName()
	if cerr != nil {
		return "", false, "", cerr
	}
	dirID, iderr := shardseal.RandIDHex()
	if iderr != nil {
		return "", false, "", iderr
	}
	salt, serr := shardseal.RandSalt()
	if serr != nil {
		return "", false, "", serr
	}
	key, kerr := shardseal.DeriveKey(s.secret, salt)
	if kerr != nil {
		return "", false, "", kerr
	}
	dm := dirMeta{Version: 1, Algorithm: s.opts.Algorithm, KDF: "scrypt", Type: "dir", Path: parentDir, MTime: mtimeString(mtime), DirID: dirID}
	dmJSON, merr := json.Marshal(dm)
	if merr != nil {
		return "", false, "", merr
	}
	blob, berr := shardseal.EncryptMetaJSON(key, salt, dmJSON, s.metaPadTarget())
	if berr != nil {
		return "", false, "", berr
	}
	origHex, _ := shardseal.Hash16(dmJSON)
	encHex, _ := shardseal.Hash16(blob)
	name := shardseal.DirMetaName(origHex, dirID, encHex)
	if werr := s.inner.WriteFile(ctx, path.Join(c, name), bytes.NewReader(blob), int64(len(blob)), mtime); werr != nil {
		return "", false, "", fmt.Errorf("secretdata: 写目录 meta %s 失败: %w", name, werr)
	}
	s.dirSegs[parentDir] = c
	if parentDir != "" {
		s.dirs[parentDir] = struct{}{}
	}
	return c, true, name, nil
}

// metaPadTarget 返回文件/目录 meta 加密 pad 目标（整块落盘总长），默认 = Block.Min
// 抖动态：[base, 2×base)，受统一格式 R 地板（最大 192B）约束向下钳制到 ≥192。
func (s *SecretdataFS) metaPadTarget() int {
	base := s.opts.MetaPadBytes
	if base <= 0 {
		base = s.opts.Block.Min
	}
	if base <= 0 {
		base = 192
	}
	t := base + shardseal.RandN(base)
	if t < 192 {
		t = 192
	}
	return int(t)
}

// mtimeString 把 UnixNano 转 RFC3339Nano（目录 meta 的 mtime 字段）。
func mtimeString(mtime int64) string {
	if mtime <= 0 {
		return ""
	}
	return time.Unix(0, mtime).UTC().Format(time.RFC3339Nano)
}

// parentDirOf 返回逻辑文件 rel 所在逻辑目录（根为 ""）。
func parentDirOf(rel string) string {
	d := path.Dir(rel)
	if d == "." || d == "/" {
		return ""
	}
	return strings.Trim(d, "/")
}

// addDirKeysLocked 登记 rel 的全部祖先目录（不含 rel 自身）到 dirs 集合。
// 调用方需持有 s.mu。
func addDirKeysLocked(dirs map[string]struct{}, rel string) {
	p := strings.Trim(rel, "/")
	for i := range p {
		if p[i] != '/' {
			continue
		}
		parent := strings.TrimSuffix(p[:i], "/")
		if parent != "" {
			dirs[parent] = struct{}{}
		}
	}
}

// pruneDirsLocked 删除 dirs 中不再被任何索引路径用作祖先的目录（删除文件后收尾）。
// 调用方需持有 s.mu（Delete 在 Lock 下调用，只清集合不碰底层）。
func (s *SecretdataFS) pruneDirsLocked() {
	if len(s.dirs) == 0 {
		return
	}
	active := map[string]bool{}
	for p := range s.index {
		for d := range s.dirs {
			if strings.HasPrefix(p, d+"/") {
				active[d] = true
			}
		}
	}
	for d := range s.dirs {
		if !active[d] {
			delete(s.dirs, d)
		}
	}
}

// removeVersionMeta 删除旧版本条目的底层分块与文件 meta（覆盖写 / 删除清理，best-effort）。
func (s *SecretdataFS) removeVersionMeta(e *metaEntry) {
	if e == nil || e.meta == nil {
		return
	}
	for _, ci := range e.meta.Chunks {
		_ = s.inner.Delete(context.Background(), path.Join(e.dirSeg, ci.FileName))
	}
	_ = s.inner.Delete(context.Background(), path.Join(e.dirSeg, e.metaName))
}
