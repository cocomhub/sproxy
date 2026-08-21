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
	"time"
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

// cloudDirName 是云端下载文件的存储子目录名。
const cloudDirName = ".__cloud__"

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
	userFileCount atomic.Int64              // 用户文件数量（不含内部目录），由 ScanAndRecalculate 更新
	lastScanTime  atomic.Pointer[time.Time] // 最近一次全量扫描完成时间
	logger        *slog.Logger
	scanMu        sync.RWMutex
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
}

// NewStorageManager 创建存储管理器，启动时自动扫描目录统计大小。
func NewStorageManager(dir string, maxBytes int64, _ ChecksumStoreIface, logger *slog.Logger) *StorageManager {
	sm := &StorageManager{
		uploadsDir: dir,
		logger:     defaultLogger(logger),
		stopCh:     make(chan struct{}),
	}
	sm.maxBytes.Store(maxBytes)
	_ = sm.ScanAndRecalculate()

	// 每 30 分钟定期扫描校准
	sm.wg.Add(1)
	go sm.periodicScan()

	return sm
}

// TryReserve 原子检查并预留空间。成功时累加对应分类和总使用量。
// 返回 ErrStorageFull 表示超出上限；maxBytes=0 时不限制。
func (s *StorageManager) TryReserve(size int64, cat StorageCategory) error {
	if size <= 0 {
		return nil
	}
	// 使用 label break 避免内层循环 CAS 成功后回到外层再 CAS 一次导致双倍计数。
outer:
	for {
		max := s.maxBytes.Load()
		current := s.totalUsage.Load()
		if max > 0 && current+size > max {
			return ErrStorageFull
		}
		if s.totalUsage.CompareAndSwap(current, current+size) {
			break
		}
		// CAS 失败，其他 goroutine 修改了 totalUsage，指数退避重试
		backoff := 1
		for {
			time.Sleep(time.Duration(backoff) * time.Microsecond)
			backoff *= 2
			if backoff > 64 {
				backoff = 64
			}
			// 重新加载 current 和 max
			current = s.totalUsage.Load()
			max = s.maxBytes.Load()
			if max > 0 && current+size > max {
				return ErrStorageFull
			}
			if s.totalUsage.CompareAndSwap(current, current+size) {
				break outer
			}
		}
	}
	s.addCategory(cat, size)
	return nil
}

// Release 释放已占用的空间。
func (s *StorageManager) Release(size int64, cat StorageCategory) {
	if size <= 0 {
		return
	}
	s.totalUsage.Add(-size)
	s.addCategory(cat, -size)
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
	// atomic 读取本身就线程安全，且无跨字段一致性需求，无需 scanMu 保护。
	// 移除 scanMu 锁避免与 ScanAndRecalculate 的写锁争用。
	return map[StorageCategory]int64{
		CategoryUserFiles: s.userFilesSize.Load(),
		CategoryChunked:   s.chunkedSize.Load(),
		CategoryVersions:  s.versionsSize.Load(),
		CategoryCloud:     s.cloudSize.Load(),
	}
}

// FileCount 返回当前已扫描的用户文件数量（不含内部目录）。
// 由 ScanAndRecalculate 在每次全量扫描时更新。
func (s *StorageManager) FileCount() int {
	return int(s.userFileCount.Load())
}

// Clear 重置所有计数器为零。仅用于测试。
func (s *StorageManager) Clear() {
	s.userFileCount.Store(0)
	s.userFilesSize.Store(0)
	s.chunkedSize.Store(0)
	s.versionsSize.Store(0)
	s.cloudSize.Store(0)
	s.totalUsage.Store(0)
}

// ScanAndRecalculate 全量扫描上传目录，重新统计各分类文件大小和用户文件数量。
func (s *StorageManager) ScanAndRecalculate() error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	var userFiles, chunked, versions, cloud int64
	var userFileCount int64

	// 解析符号链接，确保扫描的是真实路径
	realDir, err := filepath.EvalSymlinks(s.uploadsDir)
	if err != nil {
		realDir = s.uploadsDir
	}

	err = filepath.WalkDir(realDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			base := filepath.Base(path)
			// 内部存储目录（.__chunked__、.__versions__、.__cloud__、.__cloud_archives__）
			// 需要进入统计；其他 .__ 开头的元数据目录跳过。
			// 注意：__cloud_archives__ 归档文件必须计入 cloud 分类（下方
			// case strings.HasPrefix(rel, cloudArchiveDirName+"/") 依赖此处不跳过），
			// 否则 max_storage_bytes 配额会漏计归档文件。
			if strings.HasPrefix(base, ".__") &&
				base != chunkedDirName && base != versionsDirName && base != cloudDirName &&
				base != cloudArchiveDirName {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 跳过无法读取的文件
		}
		rel, err := filepath.Rel(realDir, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		size := info.Size()

		switch {
		case strings.HasPrefix(rel, chunkedDirName+"/"):
			chunked += size
		case strings.HasPrefix(rel, versionsDirName+"/"):
			versions += size
		case strings.HasPrefix(rel, cloudDirName+"/"):
			cloud += size
		case strings.HasPrefix(rel, downloadsDirName+"/"):
			cloud += size
		case strings.HasPrefix(rel, cloudArchiveDirName+"/"):
			cloud += size
		default:
			userFiles += size
			userFileCount++
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
	s.userFileCount.Store(userFileCount)

	now := time.Now()
	s.lastScanTime.Store(&now)

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

// scanOnce 执行一次全量扫描，校准存储计数器。返回 (before, after, error)。
func (s *StorageManager) scanOnce() (int64, int64, error) {
	before := s.totalUsage.Load()
	if err := s.ScanAndRecalculate(); err != nil {
		return before, 0, err
	}
	after := s.totalUsage.Load()
	return before, after, nil
}

// periodicScan 每 30 分钟执行一次全量扫描，校准存储计数器。
func (s *StorageManager) periodicScan() {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("periodic scan panicked, restarting", "recover", r)
			// 先释放原始计数，再启动新 goroutine
			s.wg.Done()
			// 检查是否已停止，避免 goroutine 泄漏
			select {
			case <-s.stopCh:
				return
			default:
			}
			// 延迟重启，避免频繁 panic 导致 CPU 空转
			time.Sleep(10 * time.Second)
			s.wg.Add(1)
			go s.periodicScan()
			return
		}
		s.wg.Done()
	}()
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			before, after, err := s.scanOnce()
			if err != nil {
				s.logger.Warn("periodic storage scan failed", "error", err)
				continue
			}
			if before != after {
				s.logger.Info("storage usage recalibrated by periodic scan",
					"before", before, "after", after, "delta", after-before)
			}
			now := time.Now()
			s.lastScanTime.Store(&now)
		case <-s.stopCh:
			return
		}
	}
}

// LastScanTime 返回最近一次全量扫描完成时间。
func (s *StorageManager) LastScanTime() *time.Time {
	return s.lastScanTime.Load()
}

func (s *StorageManager) Stop() {
	s.stopOnce.Do(func() {
		close(s.stopCh)
	})
	s.wg.Wait()
}
