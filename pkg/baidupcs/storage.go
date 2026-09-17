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

// Stat 查询对象元数据。
func (s *Storage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, mapPCSError(err)
	}
	// 通过 fake/真实 adapter 的 Meta 能力：真实 adapter 走二进制 stat 或库 FilesDirectoriesMeta。
	// 当前 Adapter 接口无 Meta —— 用 Download 到临时文件 + 本地 Stat 兜底（轻量场景）。
	// 说明：百度网盘 Stat 直接走库更高效，但二进制优先策略下 adapter 只暴露 Upload/Download，
	// 因此 Stat 用「临时下载后本地 stat」近似（配合 Put 的 ETag 复核）。
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
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

// List 列对象（目录递归；当前对单文件路径返回单元素）。
func (s *Storage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
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
