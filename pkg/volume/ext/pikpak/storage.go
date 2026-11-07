// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// StorageConfig 是 PikPak 网盘卷 Storage 配置。
type StorageConfig struct {
	// Cli 是官方 CLI 包装（转存/下载用）。
	Cli *Cli
	// API 是 REST 客户端（列表/删除）。
	API *API
	// Root 是卷根路径（对 PikPak 无目录层级概念，保留字段对齐接口）。
	Root string
	// TempDir 是本地临时目录（下载中转）；空 = 用户缓存目录下的随机命名子目录（同
	// newPikpakStagingDir 的 S5443 考量，不用公开可写的 os.TempDir()）。
	TempDir string
	// AutoDelete 下载完成后是否删除网盘转存文件。
	AutoDelete bool
	// Logger 日志。
	Logger *slog.Logger
}

// ObjectMeta 是网盘对象元数据（对齐 baidupcs.ObjectMeta）。
type ObjectMeta struct {
	Key     string
	Size    int64
	ETag    string
	ModTime time.Time
	IsDir   bool
}

// StorageAPI 是 PikPak 网盘卷的最小存储接口（对齐 baidupcs.StorageAPI）。
type StorageAPI interface {
	Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error)
	Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error)
	Stat(ctx context.Context, key string) (*ObjectMeta, error)
	List(ctx context.Context, prefix string) ([]ObjectMeta, error)
	Delete(ctx context.Context, key string) error
	Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error)
}

// Storage 是 PikPak 网盘卷后端。
// Key 语义：转存文件用其网盘文件 ID 作为 key（PikPak 无路径层级）。
type Storage struct {
	cli            *Cli
	api            *API
	root           string
	temp           string
	autoDelete     bool
	log            *slog.Logger
	commandFactory func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// NewStorage 创建 PikPak 网盘卷 Storage。
func NewStorage(cfg StorageConfig) (*Storage, error) {
	if cfg.Cli == nil {
		return nil, fmt.Errorf("pikpak storage: cli required")
	}
	if cfg.API == nil {
		return nil, fmt.Errorf("pikpak storage: api required")
	}
	if cfg.TempDir == "" {
		var err error
		if cfg.TempDir, err = newPikpakStagingDir("tmp"); err != nil {
			return nil, err
		}
	}
	// 仅 owner 可写：下载中转不含共享需求（S5445 收紧；默认落用户缓存目录下的随机命名
	// 子目录而非公开可写的 os.TempDir()，S5443）。
	if err := os.MkdirAll(cfg.TempDir, 0o700); err != nil {
		return nil, err
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	factory := exec.CommandContext
	if cfg.Cli != nil && cfg.Cli.commandFactory != nil {
		factory = cfg.Cli.commandFactory
	}
	return &Storage{cli: cfg.Cli, api: cfg.API, root: cfg.Root, temp: cfg.TempDir, autoDelete: cfg.AutoDelete, log: log, commandFactory: factory}, nil
}

var _ StorageAPI = (*Storage)(nil)

// Put 上传内容到网盘（PikPak 不支持任意上传路径；用分享转存场景时直接经 CLI 下载）。
// 当前实现：把内容写到临时文件并返回错误提示（PikPak 无通用上传 API —— 上传是
// Connected Apps 官方通道，本模块不实现；Put 仅占位满足接口，返回 ErrUnsupported）。
func (s *Storage) Put(ctx context.Context, key string, r io.Reader) (*ObjectMeta, error) {
	return nil, fmt.Errorf("%w: pikpak Put not supported (use share restore or CLI download)", ErrUnsupported)
}

// Get 下载网盘文件（按 key=文件ID）到本地流。
func (s *Storage) Get(ctx context.Context, key string) (io.ReadCloser, *ObjectMeta, error) {
	files, err := s.api.ListRecursive(ctx, "")
	if err != nil {
		return nil, nil, err
	}
	var target *FileMeta
	for i := range files {
		if files[i].ID == key {
			target = &files[i]
			break
		}
	}
	if target == nil {
		return nil, nil, fmt.Errorf("%w: file id %s", ErrFileNotFound, key)
	}
	// 用 CLI 下载到临时文件，返回 Reader
	tmp := filepath.Join(s.temp, key+".download")
	// 防御性重建：s.temp 可能被并发进程启动时的过期清扫（sweepStalePikpakStaging）删除，
	// 使用时 MkdirAll 幂等重建，避免「目录被清扫 → 本 Get 假失败」（MkdirAll 已存在即无操作）。
	if mkdirErr := os.MkdirAll(s.temp, 0o700); mkdirErr != nil {
		return nil, nil, fmt.Errorf("pikpak: 重建暂存目录失败: %w", mkdirErr)
	}
	_ = os.Remove(tmp)
	cmd := s.commandFactory(ctx, s.cli.bin, "download", key, "-o", tmp)
	if out, cerr := cmd.CombinedOutput(); cerr != nil {
		return nil, nil, fmt.Errorf("pikpak cli download %s: %w (%s)", key, cerr, truncate(string(out), 200))
	}
	f, err := os.Open(tmp)
	if err != nil {
		return nil, nil, err
	}
	meta := &ObjectMeta{Key: key, Size: target.Size, ModTime: time.Now()}
	return &cleanupReadCloser{f: f, path: tmp}, meta, nil
}

// Stat 查询文件元数据。
func (s *Storage) Stat(ctx context.Context, key string) (*ObjectMeta, error) {
	files, err := s.api.ListRecursive(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range files {
		if files[i].ID == key {
			return &ObjectMeta{Key: key, Size: files[i].Size, IsDir: files[i].Kind == "drive#folder"}, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrFileNotFound, key)
}

// List 列出网盘根目录（prefix 保留对齐；PikPak 无路径层级 → 忽略 prefix）。
func (s *Storage) List(ctx context.Context, prefix string) ([]ObjectMeta, error) {
	files, err := s.api.ListRecursive(ctx, "")
	if err != nil {
		return nil, err
	}
	out := make([]ObjectMeta, 0, len(files))
	for i := range files {
		out = append(out, ObjectMeta{
			Key:   files[i].ID,
			Size:  files[i].Size,
			IsDir: files[i].Kind == "drive#folder",
		})
	}
	return out, nil
}

// Delete 删除网盘文件（移到回收站）。
func (s *Storage) Delete(ctx context.Context, key string) error {
	return s.api.Delete(ctx, []string{key})
}

// Copy 复制网盘文件（PikPak 无跨路径复制语义；同一文件不可复制 → 返回错误）。
func (s *Storage) Copy(ctx context.Context, srcKey, dstKey string) (*ObjectMeta, error) {
	return nil, fmt.Errorf("%w: pikpak copy not supported", ErrUnsupported)
}

// cleanupReadCloser 关闭时删除临时文件。
type cleanupReadCloser struct {
	f    *os.File
	path string
}

func (c *cleanupReadCloser) Read(p []byte) (int, error) { return c.f.Read(p) }
func (c *cleanupReadCloser) Close() error {
	err := c.f.Close()
	_ = os.Remove(c.path)
	return err
}
