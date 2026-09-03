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

// StorageManager 管理上传目录的存储空间使用情况。
// 通过原子计数器跟踪各分类和总使用量，支持配置上限和运行时调整。
// P4：全局账本仍供 sync 适配与旧装配兼容；per-tenant 配额由 quota Scope 负责
// （ScanAndRecalculate 经 reconcile 回调把磁盘占用校准进各租户桶 Scope）。
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
	reconcile     ReconcileFunc // 非 nil 时扫描后按租户桶归集校准配额 Scope（启动/周期对账）
	stopCh        chan struct{}
	stopOnce      sync.Once
	wg            sync.WaitGroup
}

// ReconcileFunc 是 ScanAndRecalculate 完成磁盘扫描后校准 per-tenant 配额 Scope 的回调。
// tenantBuckets: tenant 名 → 桶名 → 字节数（仅包含新布局桶结构路径；旧布局平铺文件不进入）。
// 由 RegisterRoutes 装配为 Handlers.reconcileQuotaScopes；nil = 未装配（跳过 Scope 校准）。
type ReconcileFunc func(tenantBuckets map[string]map[string]int64)

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

// SetReconciler 装配扫描后的 per-tenant 配额 Scope 校准回调（nil 清除）。
// 须在 NewStorageManager 首次扫描之后装配；装配完成后应重跑一次 ScanAndRecalculate
// 使启动对账生效（RegisterRoutes 已执行）。
func (s *StorageManager) SetReconciler(fn ReconcileFunc) {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()
	s.reconcile = fn
}

// ScanAndRecalculate 全量扫描存储根，重新统计各分类文件大小和用户文件数量。
// 分类按新布局桶语义（<tenant>/{user,cloud,archive,chunk,version,meta}/）判定，兼容旧布局
// 平铺文件（默认 userFiles，跳过任务状态目录与 legacy 内部目录）。meta 桶服务端账本字节
// 计入 totalUsage（服务端占用属总量），但 UsageByCategory 保持 4 分类枚举（meta 不入分类）。
// 同时把各租户桶字节数归集进 tenantBuckets 并交给 reconcile 回调校准 per-tenant 配额 Scope
// （重启后 Scope 不回溯，meta 子 Scope 一并校准）。.checksums.json 与 LAYOUT_VERSION 不计数。
func (s *StorageManager) ScanAndRecalculate() error {
	s.scanMu.Lock()
	defer s.scanMu.Unlock()

	var userFiles, chunked, versions, cloud, meta int64
	var userFileCount int64
	tenantBuckets := make(map[string]map[string]int64)

	// 解析符号链接，确保扫描的是真实路径
	realDir, err := filepath.EvalSymlinks(s.uploadsDir)
	if err != nil {
		realDir = s.uploadsDir
	}

	err = filepath.WalkDir(realDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(realDir, path)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			base := filepath.Base(path)
			// 跳过遗留服务端内部目录（.__* 魔法目录，P5 后旧布局不再产生）。其余目录
			// 一律进入统计——未知 .__ 目录也跳过（防历史魔法目录残留被误计；新布局
			// 用户文件在 user/ 桶）。meta 桶不再 SkipDir：服务端账本（sync/cloud 任务
			// 状态/session 等）按 meta 桶归集计入 totalUsage 与租户 meta 配额。
			if strings.HasPrefix(base, ".__") {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil // 跳过无法读取的文件
		}
		if isChecksumSidecar(d.Name()) {
			return nil
		}
		size := info.Size()

		switch bucket := storageBucketOf(rel); bucket {
		case "user":
			userFiles += size
			userFileCount++
			tenant := firstSegment(rel)
			addTenantBucket(tenantBuckets, tenant, "user", size)
			// bucket_limits 子目录归集：把该文件同时计入其最长命中的路径键（如
			// "user/videos/hd"），供 reconcile 先深后浅校准子 Scope committed。
			// bucketDirKey 仅在有子目录链时返回非空键（桶内顶级文件返回 "" → 跳过，不额外
			// 归集，避免与功能桶键重复双计）。scanner 纯机械归集，键集合由 reconcile 按
			// 装配的段树过滤（未配置前缀不建立 scope/键）。
			if dirKey := bucketDirKey(rel); dirKey != "" {
				addTenantBucket(tenantBuckets, tenant, dirKey, size)
			}
		case "cloud", "archive":
			cloud += size
			addTenantBucket(tenantBuckets, firstSegment(rel), bucket, size)
		case "chunk":
			chunked += size
			addTenantBucket(tenantBuckets, firstSegment(rel), "chunk", size)
		case "version":
			versions += size
			addTenantBucket(tenantBuckets, firstSegment(rel), "version", size)
		case "meta":
			// 服务端内部账本（sync/cloud 任务状态、chunked session、share token 等）：
			// 计入 totalUsage 与租户 meta 配额 Scope，但不入 stats 分类枚举（4 键不变）。
			meta += size
			addTenantBucket(tenantBuckets, firstSegment(rel), "meta", size)
		default:
			// 无桶结构的旧布局平铺文件按用户文件计入（新布局路径均落入上方 bucket 分支；
			// LAYOUT_VERSION 标记文件不计入）。.__ 魔法目录已在目录层 SkipDir 跳过。
			if d.Name() == "LAYOUT_VERSION" {
				return nil
			}
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
	s.totalUsage.Store(userFiles + chunked + versions + cloud + meta)
	s.userFileCount.Store(userFileCount)

	// 校准 per-tenant 配额 Scope（启动/周期对账；nil 回调跳过）。
	if s.reconcile != nil {
		s.reconcile(tenantBuckets)
	}

	now := time.Now()
	s.lastScanTime.Store(&now)

	return nil
}

// storageBucketOf 返回存储根相对路径（斜杠分隔）的桶段（第 2 段）；无桶结构返回 ""。
// 例：file.txt → ""；tenant/file.txt → "file.txt"；tenant/user/a.txt → "user"。
func storageBucketOf(rel string) string {
	if _, after, ok := strings.Cut(rel, "/"); ok {
		rest := after
		if before, _, ok := strings.Cut(rest, "/"); ok {
			return before
		}
		return rest
	}
	return ""
}

// firstSegment 返回存储根相对路径的首段（租户名或顶层目录）；无 "/" 返回空。
func firstSegment(rel string) string {
	if before, _, ok := strings.Cut(rel, "/"); ok {
		return before
	}
	return ""
}

// isChecksumSidecar 判断文件名是否为 per-tenant checksum 侧边文件（meta 桶/旧布局根下派生
// 账本，不计入配额与总量）。新布局为 <tenant>/meta/checksums.json；历史旧布局曾用
// 根目录 .checksums.json → 两者都跳过（含 .tmp 残留），避免 meta 桶字节随文件数膨胀。
func isChecksumSidecar(name string) bool {
	return name == "checksums.json" || name == ".checksums.json"
}

// addTenantBucket 把 size 累加到 tenantBuckets[tenant][bucket]。
func addTenantBucket(m map[string]map[string]int64, tenant, bucket string, size int64) {
	if tenant == "" {
		return
	}
	b := m[tenant]
	if b == nil {
		b = make(map[string]int64)
		m[tenant] = b
	}
	b[bucket] += size
}

// dirPathSuffix 返回 rel 的"桶内目录链键候选"（bucket_limits 键，含功能桶前缀）：
// rel="alice/user/videos/hd/a.mkv" → "user/videos/hd"；桶内顶级文件 "alice/user/f.txt"
// → ""（无子目录键：扫描仅归集到功能桶键 "user"，避免与功能桶键重复双计）。
// reconcile 侧只对装配过的 BucketLimits 键做子目录校准（未配置前缀不建立键/scope）。
func dirPathSuffix(rel string) string {
	rest, _, hasSlash := strings.Cut(rel, "/")
	if !hasSlash {
		return ""
	}
	// rel[首段租户+1 : 最后文件名前的 slash] 是桶内目录链（含功能桶首段）。
	if idx := strings.LastIndexByte(rel, '/'); idx >= 0 {
		suffix := rel[len(rest)+1 : idx]
		if strings.Contains(suffix, "/") {
			return suffix // 存在桶内子目录（至少 user/dir/...）
		}
	}
	return "" // 桶内顶级文件（无子目录键）
}

// bucketDirKey 返回 user 桶文件的 bucket_limits 子目录键候选；无子目录时为 ""（扫描不
// 再额外归集，避免与功能桶键重复双计）。
func bucketDirKey(rel string) string {
	return dirPathSuffix(rel)
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
