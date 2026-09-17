// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// storage.go 实现百度网盘 Storage 接口（Put/Get/Stat/List/Delete/Copy/Move），
// 参考 cocom/pkg/storage/baidupcs 的稳定性保障：
//   - 上传后 ETag 复核有界重试 ≤ maxPutAttempts（防 Baidu 分片 ETag 语义差异导致无界重传）
//   - 错误分类映射（PCSErrorCategory → 哨兵错误）
//   - 临时文件受控（TempDir），Get 走本地临时文件再返回 Reader
package baidupcs

import (
	"context"
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	pcsapi "github.com/cocomhub/sproxy/pkg/baidupcs/internal"
)

// maxPutAttempts 限制上传后 ETag 复核不匹配时的重试次数。
const maxPutAttempts = 3

// StorageConfig 是百度网盘 Storage 配置。
type StorageConfig struct {
	// Root 是网盘根路径（如 /baidu）；空 = "/"。
	Root string
	// TempDir 是本地临时目录（下载/上传中转）；空 = os.TempDir()。
	TempDir string
	// Adapter 是底层执行器（二进制优先或库）；必填。
	Adapter Adapter
	// Logger 是日志。
	Logger *slog.Logger
}

// Storage 是百度网盘存储后端。
type Storage struct {
	root    string
	temp    string
	adapter Adapter
	log     *slog.Logger
}

// NewStorage 创建百度网盘 Storage。
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
	if cfg.Adapter == nil {
		return nil, fmt.Errorf("%w: adapter is required", ErrInvalidParam)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Storage{root: root, temp: cfg.TempDir, adapter: cfg.Adapter, log: logger}, nil
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
	return path.Join(s.root, strings.TrimPrefix(p, "/")), nil
}

// logicalKey 把网盘路径映射回用户 key（root 前缀剥离）。
func (s *Storage) logicalKey(remote string) (string, error) {
	p, err := sanitizeRemotePath(remote)
	if err != nil {
		return "", err
	}
	if s.root == "/" {
		return strings.TrimPrefix(p, "/"), nil
	}
	if p == s.root {
		return "/", nil
	}
	prefix := s.root + "/"
	if !strings.HasPrefix(p, prefix) {
		return "", fmt.Errorf("%w: remote %q outside root %q", ErrInvalidParam, remote, s.root)
	}
	return strings.TrimPrefix(p, prefix), nil
}

// Put 上传内容到网盘（本地临时文件 + adapter.Upload + ETag 复核有界重试）。
func (s *Storage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	tmp, err := os.CreateTemp(s.temp, filepath.Base(key)+"-*")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)

	hash := md5.New()
	tee := io.TeeReader(r, hash)
	if _, err := io.Copy(tmp, tee); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	localMD5 := fmt.Sprintf("%x", hash.Sum(nil))

	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 1; attempt <= maxPutAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := s.adapter.Upload(ctx, tmpPath, remote, true); err != nil {
			return nil, s.mapErr("put", err)
		}
		meta, statErr := s.Stat(ctx, key)
		if statErr != nil {
			return nil, s.mapErr("put-stat", statErr)
		}
		if meta.ETag == localMD5 || meta.ETag == "" {
			return meta, nil
		}
		lastErr = fmt.Errorf("ETag 不匹配 (local=%s remote=%s)", localMD5, meta.ETag)
		s.log.Warn("baidupcs put ETag 复核失败，重试", "key", key, "attempt", attempt, "error", lastErr)
	}
	return nil, fmt.Errorf("baidupcs put: %v (超过 %d 次)", lastErr, maxPutAttempts)
}

// Get 下载网盘文件到本地临时文件并返回 Reader。
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error) {
	meta, err := s.Stat(ctx, key)
	if err != nil {
		return nil, nil, err
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
	tmp.Close()
	if err := s.adapter.Download(ctx, remote, tmpPath); err != nil {
		os.Remove(tmpPath)
		return nil, nil, s.mapErr("get", err)
	}
	fd, err := os.Open(tmpPath)
	if err != nil {
		os.Remove(tmpPath)
		return nil, nil, err
	}
	return &tempFile{File: fd, path: tmpPath}, meta, nil
}

// Stat 查询网盘文件元信息。
func (s *Storage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	remote, err := s.remotePath(key)
	if err != nil {
		return nil, err
	}
	fd, err := s.adapter.Meta(remote)
	if err != nil {
		return nil, s.mapErr("stat", err)
	}
	if fd == nil {
		return nil, ErrNotFound
	}
	return s.metaFromFileDirectory(fd)
}

// Exists 判断文件是否存在。
func (s *Storage) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.Stat(ctx, key)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return false, err
}

// List 递归列目录。
func (s *Storage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	remote, err := s.remotePath(prefix)
	if err != nil {
		return nil, err
	}
	fds, err := s.adapter.List(remote)
	if err != nil {
		return nil, s.mapErr("list", err)
	}
	out := make([]ObjectMeta, 0, len(fds))
	for _, fd := range fds {
		if fd == nil {
			continue
		}
		meta, err := s.metaFromFileDirectory(fd)
		if err != nil {
			continue
		}
		out = append(out, *meta)
	}
	return out, nil
}

// Delete 删除网盘文件（幂等：不存在视为成功）。
func (s *Storage) Delete(ctx context.Context, key string) error {
	remote, err := s.remotePath(key)
	if err != nil {
		return err
	}
	if err := s.adapter.Delete(remote); err != nil {
		return s.mapErr("delete", err)
	}
	return nil
}

// Copy 网盘内复制。
func (s *Storage) Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	src, err := s.remotePath(srcKey)
	if err != nil {
		return nil, err
	}
	dst, err := s.remotePath(dstKey)
	if err != nil {
		return nil, err
	}
	if err := s.adapter.Copy(&pcsapi.CpMvJSON{From: src, To: dst}); err != nil {
		return nil, s.mapErr("copy", err)
	}
	return s.Stat(ctx, dstKey)
}

// Move 网盘内移动。
func (s *Storage) Move(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	src, err := s.remotePath(srcKey)
	if err != nil {
		return nil, err
	}
	dst, err := s.remotePath(dstKey)
	if err != nil {
		return nil, err
	}
	if err := s.adapter.Move(&pcsapi.CpMvJSON{From: src, To: dst}); err != nil {
		return nil, s.mapErr("move", err)
	}
	return s.Stat(ctx, dstKey)
}

func (s *Storage) metaFromFileDirectory(fd *pcsapi.FileDirectory) (*ObjectMeta, error) {
	key, err := s.logicalKey(fd.Path)
	if err != nil {
		return nil, err
	}
	meta := &ObjectMeta{
		Key:  key,
		Size: fd.Size,
		ETag: fd.MD5,
	}
	if fd.Mtime > 0 {
		meta.ModTime = time.Unix(fd.Mtime, 0).UTC()
	}
	return meta, nil
}

// mapErr 把 Adapter 返回的错误映射为哨兵错误。
func (s *Storage) mapErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var pcsErr *PCSError
	if errors.As(err, &pcsErr) {
		switch pcsErr.Category {
		case ErrCategoryNotFound:
			return fmt.Errorf("baidupcs %s: %w", op, ErrNotFound)
		case ErrCategoryAlreadyExists:
			return fmt.Errorf("baidupcs %s: %w", op, ErrAlreadyExists)
		case ErrCategoryPermissionDenied:
			return fmt.Errorf("baidupcs %s: %w", op, ErrPermission)
		case ErrCategoryTransient:
			return fmt.Errorf("baidupcs %s: %w", op, ErrTransient)
		}
	}
	return err
}

// ObjectMeta 是对象元信息。
type ObjectMeta struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
}

// tempFile 是下载临时文件（Close 时删除）。
type tempFile struct {
	*os.File
	path string
}

func (f *tempFile) Close() error {
	err := f.File.Close()
	os.Remove(f.path)
	return err
}
