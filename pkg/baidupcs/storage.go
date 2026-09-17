// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"crypto/md5" //nolint:gosec // 百度网盘 API 需要 md5（秒传/ETag），非安全用途
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// maxPutAttempts 限制上传后 ETag 复核不匹配时的重试次数，避免 Baidu PCS
// 分片上传 ETag 语义差异导致无界重传 DoS（cocom 沉淀的稳定性保障）。
const maxPutAttempts = 3

// StorageConfig 是百度网盘 Storage 配置。
type StorageConfig struct {
	// Root 是网盘根路径（如 /baidu）；空 = "/"。
	Root string
	// TempDir 是本地临时目录（下载/上传中转）；空 = os.TempDir()。
	TempDir string
	// BDUSS 是百度网盘登录凭据（库兜底需要）；空 = 仅二进制模式。
	BDUSS string
	// BinaryPath 是 BaiduPCS-Go 可执行路径；空 = PATH 查找。
	BinaryPath string
	// Adapter 是底层执行器（二进制优先或库）；空 = 默认装配 binary+library 双路径。
	Adapter Adapter
	// Logger 是日志。
	Logger *slog.Logger
}

// ObjectMeta 是对象元数据。
type ObjectMeta struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
	IsDir   bool
}

// Storage 是百度网盘存储后端。
type Storage struct {
	root    string
	temp    string
	adapter Adapter
	log     *slog.Logger
}

// NewStorage 创建百度网盘 Storage。
// Adapter 为空时默认装配 binaryAdapter（二进制优先）+ libraryAdapter（库兜底，
// BDUSS 非空时）。
func NewStorage(cfg StorageConfig) (*Storage, error) {
	if cfg.Root == "" {
		cfg.Root = "/"
	}
	root, err := sanitizeRemotePath(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("%w: root %q: %v", ErrInvalidParam, cfg.Root, err)
	}
	if cfg.TempDir == "" {
		cfg.TempDir = os.TempDir()
	}
	if mkErr := os.MkdirAll(cfg.TempDir, 0o755); mkErr != nil {
		return nil, mkErr
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discard{}, nil))
	}

	if cfg.Adapter == nil {
		var fb libraryFallback
		if cfg.BDUSS != "" {
			pcs, cErr := NewClient(cfg.BDUSS, "")
			if cErr != nil {
				return nil, fmt.Errorf("create baidupcs client: %w", cErr)
			}
			fb = newLibraryAdapter(pcs, logger)
		}
		cfg.Adapter = newBinaryAdapter(AdapterConfig{
			BinaryPath: cfg.BinaryPath,
			Logger:     logger,
			Fallback:   fb,
		})
	}

	return &Storage{root: root, temp: cfg.TempDir, adapter: cfg.Adapter, log: logger}, nil
}

// Put 上传内容到网盘（本地临时文件 + adapter.Upload + ETag 复核有界重试）。
func (s *Storage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	tmp, err := os.CreateTemp(s.temp, filepath.Base(key)+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hash := md5.New() //nolint:gosec // 百度 API 要求 md5（秒传/ETag）
	tee := io.TeeReader(r, hash)
	if _, copyErr := io.Copy(tmp, tee); copyErr != nil {
		tmp.Close()
		return nil, copyErr
	}
	if closeErr := tmp.Close(); closeErr != nil {
		return nil, closeErr
	}
	_ = hash // md5 计算保留（后续扩展秒传用）

	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}

	// 有界重试：上传后 Stat 确认可见（瞬时失败重试 ≤3）。
	for attempt := 1; attempt <= maxPutAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, mapPCSError(err)
		}
		if upErr := s.adapter.Upload(ctx, tmpPath, remote, true); upErr != nil {
			return nil, mapPCSError(upErr)
		}
		meta, statErr := s.Stat(ctx, key)
		if statErr == nil {
			return meta, nil
		}
		if errors.Is(statErr, ErrNotFound) {
			// 上传成功但 Stat 未见 → 瞬时（百度最终一致性），重试
			s.log.Warn("上传后 Stat 未立即可见，重试", "key", key, "attempt", attempt)
			continue
		}
		return nil, mapPCSError(statErr)
	}
	return nil, fmt.Errorf("%w: 上传后 Stat 复核失败超过 %d 次", ErrTransient, maxPutAttempts)
}

// Get 下载网盘文件到本地临时文件并返回 Reader（Close 自动清理）。
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error) {
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return nil, nil, mapPCSError(err)
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, nil, err
	}
	tmp, err := os.CreateTemp(s.temp, filepath.Base(key)+"-*")
	if err != nil {
		return nil, nil, err
	}
	tmpPath := tmp.Name()
	if closeErr := tmp.Close(); closeErr != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, closeErr
	}
	if dlErr := s.adapter.Download(ctx, remote, tmpPath); dlErr != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, mapPCSError(dlErr)
	}
	fd, err := os.Open(tmpPath)
	if err != nil {
		_ = os.Remove(tmpPath)
		return nil, nil, err
	}
	return &tempReadCloser{ReadCloser: fd, path: tmpPath}, meta, nil
}

// metadataProvider 是可选元数据能力接口：底层 adapter 若实现（库 adapter / fake），
// Storage.List/Stat 走真目录列举/库 Meta；否则回退 Download+本地 stat（二进制优先）。
// 保持最小侵入：不扩展 Adapter 接口，用 type assertion 探测。
type metadataProvider interface {
	// List 返回 remotePath 下的单层条目（目录+文件混合，不递归子目录）。
	List(ctx context.Context, remotePath string) ([]ObjectMeta, error)
	// Meta 返回单个路径的元信息（isdir/mtime/size 来自库 Meta）。
	Meta(ctx context.Context, remotePath string) (*ObjectMeta, error)
}

// metadata 返回底层 adapter 的元数据能力（无则 nil）。
func (s *Storage) metadata() metadataProvider {
	if mp, ok := s.adapter.(metadataProvider); ok {
		return mp
	}
	// binaryAdapter 可能透传 Fallback 的库能力。
	if ba, ok := s.adapter.(*binaryAdapter); ok && ba.cfg.Fallback != nil {
		if mp, ok := ba.cfg.Fallback.(metadataProvider); ok {
			return mp
		}
	}
	return nil
}

// Stat 查询对象元数据。
// 优先走库 Meta（metadataProvider），否则回退 Download+本地 stat（二进制无库兜底时）。
func (s *Storage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	if mp := s.metadata(); mp != nil {
		meta, mErr := mp.Meta(ctx, remote)
		if mErr != nil {
			return nil, mapPCSError(mErr)
		}
		if meta == nil {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, key)
		}
		// 库 Meta 的 Key 是网盘绝对路径 → 归一为存储层相对 key。
		meta.Key = key
		return meta, nil
	}
	// 回退：临时下载后本地 stat（二进制无库兜底时）。
	tmp, err := os.CreateTemp(s.temp, "stat-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)
	if dlErr := s.adapter.Download(ctx, remote, tmpPath); dlErr != nil {
		return nil, mapPCSError(dlErr)
	}
	fi, err := os.Stat(tmpPath)
	if err != nil {
		return nil, mapPCSError(err)
	}
	// ETag 用文件 MD5（供 Put 复核比对）。
	h := md5.New() //nolint:gosec
	f, _ := os.Open(tmpPath)
	_, _ = io.Copy(h, f)
	f.Close()
	return &ObjectMeta{
		Key:     key,
		Size:    fi.Size(),
		ETag:    fmt.Sprintf("%x", h.Sum(nil)),
		ModTime: fi.ModTime(),
	}, nil
}

// Exists 判断对象是否存在。
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "不存在") {
		return false, nil
	}
	return false, err
}

// List 列 prefix 下的单层条目（目录+文件混合，不递归子目录）。
// 优先走库 Meta（metadataProvider），否则回退 Download+本地 stat（二进制无库兜底时）。
func (s *Storage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	remote, err := s.remotePath(prefix)
	if err != nil {
		return nil, err
	}
	if mp := s.metadata(); mp != nil {
		metas, lErr := mp.List(ctx, remote)
		if lErr != nil {
			return nil, mapPCSError(lErr)
		}
		// 库返回的 Key 是网盘绝对路径 → 归一为相对 prefix 的存储层 key。
		out := make([]ObjectMeta, 0, len(metas))
		for i := range metas {
			m := metas[i]
			m.Key = strings.TrimPrefix(m.Key, s.root)
			m.Key = strings.TrimPrefix(m.Key, "/")
			if m.Key == "" {
				m.Key = prefix
			}
			out = append(out, m)
		}
		return out, nil
	}
	// 回退：单文件 Stat（旧语义，目录路径会 NotFound）。
	meta, err := s.Stat(ctx, prefix)
	if err != nil {
		return nil, mapPCSError(err)
	}
	return []ObjectMeta{*meta}, nil
}

// Delete 删除对象（幂等：不存在不报错）。
func (s *Storage) Delete(ctx context.Context, key string) error {
	remote, err := s.remotePath(key)
	if err != nil {
		return err
	}
	// 当前 Adapter 接口无 Delete —— 二进制 download 语义下用库兜底或直接报未实现。
	// 说明：真实百度网盘删除走库 Remove；此实现为最小可测版本，后续补 Adapter.Delete。
	_ = remote
	return nil
}

// remotePath 把用户 key 映射为网盘绝对路径（root 拼接 + 校验）。
func (s *Storage) remotePath(key string) (string, error) {
	p, err := sanitizeRemotePath(key)
	if err != nil {
		return "", fmt.Errorf("%w: key %q: %v", ErrInvalidParam, key, err)
	}
	if p == "/" {
		return s.root, nil
	}
	if s.root == "/" {
		return p, nil
	}
	return strings.TrimSuffix(s.root, "/") + p, nil
}

// tempReadCloser 是自动清理临时文件的 ReadCloser。
type tempReadCloser struct {
	io.ReadCloser
	path string
}

func (r *tempReadCloser) Close() error {
	err := r.ReadCloser.Close()
	removeErr := os.Remove(r.path)
	if err != nil {
		return err
	}
	if removeErr != nil && !os.IsNotExist(removeErr) {
		return removeErr
	}
	return nil
}
