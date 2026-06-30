// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"io/fs"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
)

// ErrStorageFull 存储空间已满，拒绝写入。
var ErrStorageFull = errors.New("storage quota exceeded")

// StorageCategory 表示存储空间分类。
type StorageCategory int

const (
	CategoryUserFiles StorageCategory = iota
	CategoryChunked
	CategoryVersions
	CategoryCloud
)

// StorageManager 管理上传目录的存储空间使用情况。
// 通过原子计数器跟踪各分类和总使用量，支持配置上限和运行时调整。
type StorageManager struct {
	uploadsDir    string
	maxBytes      atomic.Int64
	userFilesSize atomic.Int64
	chunkedSize   atomic.Int64
	versionsSize  atomic.Int64
	cloudSize     atomic.Int64
	totalUsage    atomic.Int64
	checksumStore ChecksumStoreIface
	logger        *slog.Logger
	scanMu        sync.Mutex
}

// NewStorageManager 创建存储管理器，启动时自动扫描目录统计大小。
func NewStorageManager(dir string, maxBytes int64, cs ChecksumStoreIface, logger *slog.Logger) *StorageManager {
	sm := &StorageManager{
		uploadsDir:    dir,
		checksumStore: cs,
		logger:        logger,
	}
	sm.maxBytes.Store(maxBytes)
	_ = sm.ScanAndRecalculate()
	return sm
}

// TryReserve 原子检查并预留空间。成功时累加对应分类和总使用量。
// 返回 ErrStorageFull 表示超出上限；maxBytes=0 时不限制。
func (s *StorageManager) TryReserve(size int64, cat StorageCategory) error {
	if size <= 0 {
		return nil
	}
	max := s.maxBytes.Load()
	for {
		current := s.totalUsage.Load()
		if max > 0 && current+size > max {
			return ErrStorageFull
		}
		if s.totalUsage.CompareAndSwap(current, current+size) {
			break
		}
		// CAS 失败，其他 goroutine 修改了 totalUsage，重试
	}
	s.addCategory(cat, size)
	return nil
}

// Release 释放已占用的空间。
func (s *StorageManager) Release(size int64, cat StorageCategory) {
	if size <= 0 {
		return
	}
	s.addCategory(cat, -size)
	s.totalUsage.Add(-size)
}

// SetMaxBytes 运行时动态调整存储上限。
func (s *StorageManager) SetMaxBytes(n int64) {
	s.maxBytes.Store(n)
}

// MaxBytes 返回当前存储上限。
func (s *StorageManager) MaxBytes() int64 {
	return s.maxBytes.Load()
}

// Usage 返回当前总使用量。
func (s *StorageManager) Usage() int64 {
	return s.totalUsage.Load()
}

// UsageByCategory 返回各分类的使用量。
func (s *StorageManager) UsageByCategory() map[StorageCategory]int64 {
	return map[StorageCategory]int64{
		CategoryUserFiles: s.userFilesSize.Load(),
		CategoryChunked:   s.chunkedSize.Load(),
		CategoryVersions:  s.versionsSize.Load(),
		CategoryCloud:     s.cloudSize.Load(),
	}
}

// Clear 重置所有计数器为零。仅用于测试。
func (s *StorageManager) Clear() {
	s.userFilesSize.Store(0)
	s.chunkedSize.Store(0)
	s.versionsSize.Store(0)
	s.cloudSize.Store(0)
	s.totalUsage.Store(0)
}

// ScanAndRecalculate 全量扫描上传目录，重新统计各分类文件大小。
func (s *StorageManager) ScanAndRecalculate() error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	var userFiles, chunked, versions, cloud int64

	err := filepath.WalkDir(s.uploadsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			// 跳过内部元数据目录
			if strings.HasPrefix(base, ".__") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 跳过无法读取的文件
		}
		rel, err := filepath.Rel(s.uploadsDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		size := info.Size()

		switch {
		case strings.HasPrefix(rel, "__chunked__/"):
			chunked += size
		case strings.HasPrefix(rel, "__versions__/"):
			versions += size
		case strings.HasPrefix(rel, "__cloud__/"):
			cloud += size
		default:
			// 跳过元数据文件
			if strings.HasPrefix(filepath.Base(path), ".") {
				return nil
			}
			userFiles += size
		}
		return nil
	})

	if err != nil {
		return err
	}

	s.userFilesSize.Store(userFiles)
	s.chunkedSize.Store(chunked)
	s.versionsSize.Store(versions)
	s.cloudSize.Store(cloud)
	s.totalUsage.Store(userFiles + chunked + versions + cloud)

	return nil
}

func (s *StorageManager) addCategory(cat StorageCategory, delta int64) {
	switch cat {
	case CategoryUserFiles:
		s.userFilesSize.Add(delta)
	case CategoryChunked:
		s.chunkedSize.Add(delta)
	case CategoryVersions:
		s.versionsSize.Add(delta)
	case CategoryCloud:
		s.cloudSize.Add(delta)
	}
}
