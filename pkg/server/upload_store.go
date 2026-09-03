// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	"github.com/cocomhub/sproxy/pkg/quota"
)

// ChunkedUploadSession 表示一个分块上传会话。
type ChunkedUploadSession struct {
	UploadID       string    `json:"upload_id"`
	Filename       string    `json:"filename"`
	TotalSize      int64     `json:"total_size"`
	ChunkSize      int64     `json:"chunk_size"`
	TotalChunks    int       `json:"total_chunks"`
	ReceivedChunks []bool    `json:"received_chunks"`
	ChunkChecksums []string  `json:"chunk_checksums"`
	FileChecksum   string    `json:"file_checksum"`
	FileModTime    int64     `json:"file_mod_time"` // UnixNano, 0 = unknown
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	Completed      bool      `json:"completed"`

	// TempPath 是任务 4 分块在途整文件的存储根相对路径（user 桶下，如
	// user/.inflight-<hash16>-<upload_id>.part）。init 创建并截断（Truncate(TotalSize)），
	// chunk 经 seek+BoundWriter 直写，complete 校验后 rename 为正式名，会话删除/过期删除。
	// 持久化（重启后可恢复续传；恢复时按内容重新校验分片）。
	TempPath string `json:"temp_path,omitempty"`

	// Reservation 是分块上传的租户配额预留句柄（P4）。init 预留、complete Commit、
	// 会话删除/过期 Release。不持久化（json:"-"），重启后内存预留丢失，由上游对账补齐。
	Reservation *quota.Reservation `json:"-"`
	// StorageMgrReserved 是 storageMgr 回退预留的字节数（P5，quota 未装配时启用）。
	// 与 Reservation 二选一：scope 预留走 Reservation，storageMgr 回退走本字段。
	// 会话删除/过期/完成时按此释放；不持久化（json:"-"），重启后由对账补齐。
	StorageMgrReserved int64 `json:"-"`
}

// UploadStoreIface 定义 UploadStore 的业务接口，方便测试替身。
type UploadStoreIface interface {
	Health() error
	Stop()
	CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error)
	GetSession(uploadID string) *ChunkedUploadSession
	GetSessionByFilename(filename string) *ChunkedUploadSession
	MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error
	AllChunksReceived(uploadID string) bool
	CompleteSession(uploadID string) error
	ChunkFilePath(uploadID string, chunkIndex int) string
	SessionDir(uploadID string) string
	DeleteSession(uploadID string)
	CleanupSessionAfter(uploadID string, delay time.Duration)
	GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error)
	ListSessions() []ChunkedUploadSessionMeta
	LockChunkIO(uploadID string) func()
	LockChunkMerge(uploadID string) func()
	// TODO: 考虑添加 SyncPersistSession/RollbackChunkReceived 方法支持幂等会话持久化
}

// ChunkFileLocker 管理分块文件的并发读写锁。
// 提取为独立导出类型，使 UploadStore 和 MockUploadStore 共享同一份真实锁定逻辑。
type ChunkFileLocker struct {
	fileLocks   map[string]*sync.RWMutex
	fileLocksMu sync.Mutex
}

// NewChunkFileLocker 创建一个新的 ChunkFileLocker。
func NewChunkFileLocker() *ChunkFileLocker {
	return &ChunkFileLocker{fileLocks: make(map[string]*sync.RWMutex)}
}

// LockChunkIO 获取 chunk 文件写入锁（读锁）。
// uploadChunk 在写入 chunk 文件前调用，允许多个 uploadChunk 并发写入不同 chunk。
func (l *ChunkFileLocker) LockChunkIO(uploadID string) func() {
	l.fileLocksMu.Lock()
	f, ok := l.fileLocks[uploadID]
	if !ok {
		f = new(sync.RWMutex)
		l.fileLocks[uploadID] = f
	}
	l.fileLocksMu.Unlock()
	f.RLock()
	return f.RUnlock
}

// LockChunkMerge 获取 chunk 文件合并锁（写锁）。
// mergeOneChunk 在读取 chunk 文件前调用，排他地等待所有正在写入的 chunk 完成后才允许读取，
// 同时阻塞新的 chunk 写入，避免读到不完整的 chunk。
func (l *ChunkFileLocker) LockChunkMerge(uploadID string) func() {
	l.fileLocksMu.Lock()
	f, ok := l.fileLocks[uploadID]
	if !ok {
		f = new(sync.RWMutex)
		l.fileLocks[uploadID] = f
	}
	l.fileLocksMu.Unlock()
	f.Lock()
	return f.Unlock
}

// DeleteLock 删除指定 uploadID 的锁条目，防止内存泄漏。
func (l *ChunkFileLocker) DeleteLock(uploadID string) {
	l.fileLocksMu.Lock()
	delete(l.fileLocks, uploadID)
	l.fileLocksMu.Unlock()
}

// UploadStore 管理分块上传会话的持久化与并发安全。
type UploadStore struct {
	mu         sync.RWMutex
	writeMu    sync.Mutex // 串行化 writeSessionJSON，防止 Windows rename 竞争
	baseDir    string     // 租户 chunk 桶绝对路径（<root>/<owner>/chunk/），会话目录直接位于其下
	sessions   map[string]*ChunkedUploadSession
	locker     *ChunkFileLocker // chunk 文件并发锁
	persistCh  chan string      // uploadID → 异步持久化
	stopCh     chan struct{}    // 关闭后台 goroutine
	stopOnce   sync.Once        // 保证 Stop 幂等
	wg         sync.WaitGroup
	sessionTTL time.Duration // 未完成上传会话的保留时间
	logger     *slog.Logger
	// storageMgr 是 storageMgr 回退预留的释放目标（P5，quota 未装配时由 uploadStoreFor
	// 经 SetStorageMgr 注入；nil = 无回退预留需释放）。
	storageMgr *StorageManager
}

// SetStorageMgr 注入 storageMgr 回退预留的释放目标（P5）。
// quota 未装配（globalPool nil）时 uploadStoreFor 调用；已装配 quota 时无需注入。
func (us *UploadStore) SetStorageMgr(sm *StorageManager) {
	us.mu.Lock()
	us.storageMgr = sm
	us.mu.Unlock()
}

// inflightPrefix 是任务 4 分块在途整文件临时名前缀（user 桶目标同目录）：
// `.inflight-<sha256(正式名)前16hex>-<upload_id>.part`。不以 .__ 开头（避免被
// ValidSegmentName 拒绝），扫描层对不以 .inflight 开头的普通文件按 user 桶配额计入，
// 本前缀命中的在途文件随会话清理/过期删除。
const inflightPrefix = ".inflight-"

// inflightTempName 生成分块在途整文件临时名。name 为存储根相对正式路径（user/...），
// uploadID 为会话 ID；hash 取 sha256(name) 前 8 字节（16 hex）缩短，uploadID 保证
// 同目标多会话唯一。返回的临时名可作为单个路径段（inflightToken + uploadID 均为
// 安全段），散列相同目标不同会话的文件名可区分。
func inflightTempName(name, uploadID string) string {
	h := sha256.Sum256([]byte(name))
	return fmt.Sprintf("%s%s-%s.part", inflightPrefix, hex.EncodeToString(h[:8]), uploadID)
}

// isInflightTempName 判断 name 是否为分块在途临时文件
// （.inflight-<hash16>-<upload_id>.part，inflightTempName 命中的完整形态）。
// 列表/搜索按整临时名过滤（服务端内部在途文件，对外不可见），避免与用户可创建的同名前缀
// 普通文件（如 <id>.part 形式的用户文件）误拦——严格校验整个临时名形态而非仅前缀。
func isInflightTempName(name string) bool {
	rest, ok := strings.CutPrefix(name, inflightPrefix)
	if !ok {
		return false
	}
	// 形态：<16hex>-<uploadID>.part（uploadID 为合法段名，可为多段拼接前的裸名）。
	token, idPart, hasPart := strings.Cut(rest, "-")
	if !hasPart || len(token) != 16 || !strings.HasSuffix(idPart, ".part") {
		return false
	}
	for _, c := range token {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// NewUploadStore 创建并启动 UploadStore，同时从磁盘恢复已有 session。
// baseDir 是租户 chunk 桶的绝对路径（<root>/<owner>/chunk/，经 Tenant.Root().Abs("chunk")
// 派生）；会话目录直接位于 baseDir 下（<baseDir>/<uploadID>/）。不再拼接魔法目录。
// sessionTTL 指定未完成上传会话的过期时间，默认 24h。
func NewUploadStore(baseDir string, sessionTTL time.Duration, logger *slog.Logger) (*UploadStore, error) {
	log := defaultLogger(logger)
	if err := os.MkdirAll(baseDir, 0755); err != nil {
		return nil, fmt.Errorf("创建分块上传目录失败: %w", err)
	}

	if sessionTTL < 0 {
		// 负 TTL：保留原值，用于测试"已过期"场景。
		// ExpiresAt = now.Add(negative) 保证为过去时间，cleanupExpired 可立即清理。
	} else if sessionTTL == 0 {
		sessionTTL = 24 * time.Hour
	}

	us := &UploadStore{
		baseDir:    baseDir,
		sessions:   make(map[string]*ChunkedUploadSession),
		locker:     NewChunkFileLocker(),
		persistCh:  make(chan string, 64),
		stopCh:     make(chan struct{}),
		sessionTTL: sessionTTL,
		logger:     log,
	}
	us.recoverSessions()

	// 启动持久化 goroutine
	us.wg.Add(1)
	go us.persistLoop()

	// 启动过期清理 goroutine（每 5 分钟）
	us.wg.Add(1)
	go us.cleanupLoop()

	return us, nil
}

// MustNewUploadStore 创建 UploadStore，失败时 panic。
// 仅用于 handlers.go 等无法优雅处理错误的位置。
func MustNewUploadStore(baseDir string, sessionTTL time.Duration, logger *slog.Logger) *UploadStore {
	us, err := NewUploadStore(baseDir, sessionTTL, logger)
	if err != nil {
		logger = defaultLogger(logger)
		logger.Error("创建 UploadStore 失败", "error", err)
		panic("创建 UploadStore 失败: " + err.Error())
	}
	return us
}

// Health 返回 UploadStore 的健康状态。
// 检查后台 goroutine 是否仍在运行。
func (us *UploadStore) Health() error {
	select {
	case <-us.stopCh:
		return fmt.Errorf("UploadStore 已停止")
	default:
	}
	return nil
}

// Stop 停止后台 goroutine 并等待结束。
//
// 优雅停止流程（draining）：
//  1. 关闭 stopCh 通知 cleanupLoop 和 fallback goroutine 退出，同时阻止新的持久化请求
//  2. 关闭 persistCh（不再接受新请求）
//  3. 排空 persistCh：处理所有已入列的持久化请求
//  4. 等待 wg 完成
//
// 多次调用是安全的（幂等）。
func (us *UploadStore) Stop() {
	us.stopOnce.Do(func() {
		// 1. 先关闭 stopCh，通知所有后台 goroutine 退出
		//    同时确保 MarkChunkReceived / CompleteSession 不会再向 persistCh 发送新请求
		close(us.stopCh)

		// 2. 关闭 persistCh，persistLoop 将在消费完当前请求后退出
		close(us.persistCh)

		// 3. 排空 persistCh：处理所有已入列的持久化请求（受 wg 追踪）
		us.wg.Go(func() {
			for uploadID := range us.persistCh {
				us.persistSession(uploadID)
			}
		})

		// 4. 等待所有 goroutine 完成
		us.wg.Wait()
	})
}

// newSession 创建 ChunkedUploadSession 对象（不持久化）。
func newSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64, sessionTTL time.Duration) *ChunkedUploadSession {
	now := time.Now()
	return &ChunkedUploadSession{
		UploadID:       uploadID,
		Filename:       filename,
		TotalSize:      totalSize,
		ChunkSize:      chunkSize,
		TotalChunks:    totalChunks,
		ReceivedChunks: make([]bool, totalChunks),
		ChunkChecksums: make([]string, totalChunks),
		FileChecksum:   fileChecksum,
		FileModTime:    fileModTime,
		CreatedAt:      now,
		ExpiresAt:      now.Add(sessionTTL),
	}
}

// saveNewSession 创建会话目录、持久化 session.json，并将 session 注册到内存 map。
// 调用方需保证不在持锁状态（writeSessionJSON 内部会持 writeMu 做 I/O）。
func (us *UploadStore) saveNewSession(session *ChunkedUploadSession) error {
	sessionDir := filepath.Join(us.baseDir, session.UploadID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return fmt.Errorf("创建会话目录失败: %w", err)
	}
	if err := us.writeSessionJSON(session); err != nil {
		os.RemoveAll(sessionDir)
		return err
	}
	us.mu.Lock()
	us.sessions[session.UploadID] = session
	us.mu.Unlock()
	return nil
}

// CreateSession 创建一个新的分块上传会话，使用客户端提供的 uploadID。
func (us *UploadStore) CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error) {
	if uploadID == "" {
		return nil, fmt.Errorf("upload_id 不能为空")
	}

	session := newSession(uploadID, filename, totalSize, chunkSize, totalChunks, fileChecksum, fileModTime, us.sessionTTL)

	us.logger.Info("创建上传会话", "upload_id", uploadID, "file_name", filename,
		"total_size", totalSize, "chunk_size", chunkSize, "total_chunks", totalChunks)

	if err := us.saveNewSession(session); err != nil {
		return nil, err
	}

	return session, nil
}

// GetSession 返回指定 upload_id 的会话副本。
func (us *UploadStore) GetSession(uploadID string) *ChunkedUploadSession {
	us.mu.RLock()
	defer us.mu.RUnlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return nil
	}
	return copySession(s)
}

// ChunkedUploadSessionMeta 是分块上传会话的紧凑元信息（不含 bitmap），用于列表展示。
type ChunkedUploadSessionMeta struct {
	UploadID      string `json:"upload_id"`
	Filename      string `json:"filename"`
	TotalSize     int64  `json:"total_size"`
	ChunkSize     int64  `json:"chunk_size"`
	TotalChunks   int    `json:"total_chunks"`
	ReceivedCount int    `json:"received_count"`
	MissingCount  int    `json:"missing_count"`
	FileChecksum  string `json:"file_checksum"`
	FileModTime   int64  `json:"file_mod_time"` // UnixNano, 0 = unknown
	ExpiresAt     int64  `json:"expires_at"`
	Completed     bool   `json:"completed"`
}

// ListSessions 返回所有（含已完成）会话的元信息快照，并按上传 ID 升序排列（输出确定）。
// 持锁期间从 sessions map 拷贝元信息（不含 bitmap），并在锁内完成计数，
// 避免读取时被 MarkChunkReceived 改写造成数据竞争。
// 注：已完成会话在 complete 后 CleanupSessionAfter 删除前仍在 sessions map 中，列表交由调用方过滤。
func (us *UploadStore) ListSessions() []ChunkedUploadSessionMeta {
	us.mu.RLock()
	meta := make([]ChunkedUploadSessionMeta, 0, len(us.sessions))
	for _, s := range us.sessions {
		m := ChunkedUploadSessionMeta{
			UploadID:      s.UploadID,
			Filename:      s.Filename,
			TotalSize:     s.TotalSize,
			ChunkSize:     s.ChunkSize,
			TotalChunks:   s.TotalChunks,
			ReceivedCount: countReceived(s.ReceivedChunks),
			MissingCount:  s.TotalChunks - countReceived(s.ReceivedChunks),
			FileChecksum:  s.FileChecksum,
			FileModTime:   s.FileModTime,
			ExpiresAt:     s.ExpiresAt.UnixNano(),
			Completed:     s.Completed,
		}
		meta = append(meta, m)
	}
	us.mu.RUnlock()
	slices.SortFunc(meta, func(a, b ChunkedUploadSessionMeta) int { return strings.Compare(a.UploadID, b.UploadID) })
	return meta
}

// GetSessionByFilename 按文件名查找未完成的 session。
// per-tenant UploadStore 实例天然只含本租户会话，无需 owner 过滤。
func (us *UploadStore) GetSessionByFilename(filename string) *ChunkedUploadSession {
	us.mu.RLock()
	defer us.mu.RUnlock()
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed {
			return copySession(s)
		}
	}
	return nil
}

// MarkChunkReceived 标记指定分块为已接收并持久化。
func (us *UploadStore) MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error {
	us.mu.Lock()

	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.Unlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	if chunkIndex < 0 || chunkIndex >= s.TotalChunks {
		us.mu.Unlock()
		return fmt.Errorf("chunk_index %d 超出范围 [0, %d)", chunkIndex, s.TotalChunks)
	}

	s.ReceivedChunks[chunkIndex] = true
	s.ChunkChecksums[chunkIndex] = checksum

	received := countReceived(s.ReceivedChunks)
	total := s.TotalChunks
	us.mu.Unlock()

	us.logger.Debug("chunk 已接收", "upload_id", uploadID, "chunk_index", chunkIndex,
		"checksum", shortid.ShortHash(checksum), "received", received, "total", total)

	// 异步持久化（检查 UploadStore 是否已停止）
	select {
	case <-us.stopCh:
		// 已停止，丢弃持久化请求
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			// 通道满时异步持久化，受 wg 追踪
			us.wg.Go(func() {
				us.persistSession(uploadID)
			})
		}
	}
	return nil
}

// ClearChunksReceived 把指定分块索引置为未接收（bitmap 清位 + checksum 记录清空），
// 并持久化。任务 5 用：全文件校验失败后按 mismatch 逐分片清位——精确恢复客户端
// 需重传的接收态（坏分片需重传 seek 覆盖，skipped 分片不受影响）。
// index 越界/会话不存在时返回错误（不 panic）。
func (us *UploadStore) ClearChunksReceived(uploadID string, indices []int) error {
	ss := slices.Clone(indices)
	slices.Sort(ss)
	us.mu.Lock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.Unlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	for _, i := range ss {
		if i < 0 || i >= s.TotalChunks {
			us.mu.Unlock()
			return fmt.Errorf("chunk_index %d 超出范围 [0, %d)", i, s.TotalChunks)
		}
		s.ReceivedChunks[i] = false
		s.ChunkChecksums[i] = ""
	}
	us.mu.Unlock()

	select {
	case <-us.stopCh:
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			us.wg.Go(func() { us.persistSession(uploadID) })
		}
	}
	return nil
}

// AllChunksReceived 检查是否所有分块都已接收。
func (us *UploadStore) AllChunksReceived(uploadID string) bool {
	us.mu.RLock()
	defer us.mu.RUnlock()
	s, ok := us.sessions[uploadID]
	if !ok {
		return false
	}
	if s.Completed {
		return true
	}
	for _, received := range s.ReceivedChunks {
		if !received {
			return false
		}
	}
	return true
}

// CompleteSession 标记会话为已完成。
func (us *UploadStore) CompleteSession(uploadID string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	s, ok := us.sessions[uploadID]
	if !ok {
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	if s.Completed {
		return fmt.Errorf("upload_id %s 已完成", uploadID)
	}

	s.Completed = true
	us.logger.Info("上传会话已完成", "upload_id", uploadID, "file_name", s.Filename,
		"received", countReceived(s.ReceivedChunks), "total", s.TotalChunks)
	select {
	case <-us.stopCh:
		// 已停止，不触发持久化
	default:
		select {
		case us.persistCh <- uploadID:
		default:
			us.wg.Go(func() {
				us.persistSession(uploadID)
			})
		}
	}
	return nil
}

// ChunkFilePath 曾是任务 4 前独立 .chunk 文件的路径推导；改造后分块直写整临时文件，
// 不再存在 per-chunk 文件，方法已删除（ChunkFileLocker 仍保留供遗留锁域使用）。

// SessionDir 返回会话目录路径。
func (us *UploadStore) SessionDir(uploadID string) string {
	return filepath.Join(us.baseDir, uploadID)
}

// DeleteSession 删除会话目录并清理在途临时文件、释放预留，最后清理 fileLocks 条目防内存泄漏。
func (us *UploadStore) DeleteSession(uploadID string) {
	us.mu.Lock()
	s := us.sessions[uploadID]
	delete(us.sessions, uploadID)
	us.mu.Unlock()

	var tempRel string
	if s != nil {
		tempRel = s.TempPath
		// P4 配额：清理会话时释放未落地的预留。已完成会话的预留已被 complete Commit
		// 消费（Commit 原子生效一次），此 Release 为空操作；未完成会话则归还 reserved。
		// P5：storageMgr 回退预留（quota 未装配时）同样在此释放（与 Reservation 二选一）。
		if s.Reservation != nil {
			s.Reservation.Release()
		} else if s.StorageMgrReserved > 0 {
			if us.storageMgr != nil {
				us.storageMgr.Release(s.StorageMgrReserved, CategoryChunked)
			}
			s.StorageMgrReserved = 0
		}
	}

	// 任务 4：删除 in-flight 临时文件（在 user 桶，独立于 session 目录；tempRel 来自
	// session.TempPath，由 init/恢复写入，经路径安全校验后删除）。
	if s != nil && tempRel != "" {
		if abs, ok := us.tempAbsPath(tempRel); ok {
			if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
				us.logger.Warn("删除在途临时文件失败", "upload_id", uploadID, "error", err)
			}
		}
	}

	us.locker.DeleteLock(uploadID)

	dir := filepath.Join(us.baseDir, uploadID)
	if err := os.RemoveAll(dir); err != nil {
		us.logger.Warn("删除会话目录失败", "upload_id", uploadID, "error", err)
	}
}

// LockChunkIO 获取 chunk IO 读锁（任务 4：并发分段 seek 直写整临时文件，锁域按 uploadID
// 划分，避免同会话 bitmap 更新/MarkChunkReceived 与 complete 读全文件间的竞态；读锁允许多
// 分片并发写，complete 用 LockChunkMerge 写锁排他读整文件）。
func (us *UploadStore) LockChunkIO(uploadID string) func() {
	return us.locker.LockChunkIO(uploadID)
}

// LockChunkMerge 获取 chunk 合并写锁（排他）。
// complete 在读取整临时文件前调用，等待所有正在写入的分片完成后才允许读取，
// 同时阻塞新的分片写入，避免读到不完整的临时文件。
func (us *UploadStore) LockChunkMerge(uploadID string) func() {
	return us.locker.LockChunkMerge(uploadID)
}

// persistLoop 异步持久化 goroutine。
//
// 使用 for-range 从 persistCh 消费，当 persistCh 被关闭时自动退出。
func (us *UploadStore) persistLoop() {
	defer us.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			us.logger.Error("persistLoop panic", "panic", r)
		}
	}()
	for uploadID := range us.persistCh {
		us.persistSession(uploadID)
	}
}

// persistSession 将指定 session 持久化到磁盘。
// 在持锁状态下深拷贝 session（含 ReceivedChunks / ChunkChecksums 两个 slice），
// 然后在释放锁后再做 JSON marshal / 写文件，避免 marshal 期间被 MarkChunkReceived 改写 slice 造成 data race。
func (us *UploadStore) persistSession(uploadID string) {
	us.mu.RLock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.RUnlock()
		return
	}
	snapshot := copySession(s)
	us.mu.RUnlock()

	if err := us.writeSessionJSON(snapshot); err != nil {
		us.logger.Error("持久化 session 失败", "upload_id", uploadID, "error", err)
	}
}

// CopySession 返回 session 的深拷贝（含两个 slice），调用方需保证持锁或持有稳定副本。
func copySession(s *ChunkedUploadSession) *ChunkedUploadSession {
	cp := *s
	cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
	copy(cp.ReceivedChunks, s.ReceivedChunks)
	cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
	copy(cp.ChunkChecksums, s.ChunkChecksums)
	return &cp
}

// PersistNow 同步持久化指定 session（如 init 写入 tempPath 后立即落盘，供重启恢复）。
// 复用 copySession 快照 + writeSessionJSON；与异步 persistSession 语义一致。
func (us *UploadStore) PersistNow(uploadID string) error {
	us.mu.RLock()
	s, ok := us.sessions[uploadID]
	if !ok {
		us.mu.RUnlock()
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	snapshot := copySession(s)
	us.mu.RUnlock()
	return us.writeSessionJSON(snapshot)
}

// writeSessionJSON 原子写入 session.json。
// 使用 writeMu 串行化写入，防止 Windows 上 os.CreateTemp + os.Rename 并发竞争。
func (us *UploadStore) writeSessionJSON(s *ChunkedUploadSession) error {
	us.writeMu.Lock()
	defer us.writeMu.Unlock()

	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("序列化 session 失败: %w", err)
	}
	dir := filepath.Join(us.baseDir, s.UploadID)
	if err = os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	finalPath := filepath.Join(dir, "session.json")
	tmpFile, err := os.CreateTemp(dir, "session.json.tmp.*")
	if err != nil {
		return fmt.Errorf("创建临时文件失败: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		tmpFile.Close()
		os.Remove(tmpPath)
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := tmpFile.Close(); err != nil {
		os.Remove(tmpPath)
		return fmt.Errorf("关闭临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		// Windows cannot rename over an existing file；先删除已存在的目标文件再重试。
		// 若重试仍失败，回退到 os.WriteFile 覆盖写入，避免"目标文件已被删除但新文件未写入"
		// 造成数据丢失（此时目标文件已不存在）。
		removeErr := os.Remove(finalPath)
		if removeErr == nil || os.IsNotExist(removeErr) {
			if os.Rename(tmpPath, finalPath) == nil {
				return nil
			}
			// 回退：直接覆盖写入目标路径（writeMu 已串行化，不会并发竞争同一文件）。
			if writeErr := os.WriteFile(finalPath, data, 0644); writeErr == nil {
				os.Remove(tmpPath)
				return nil
			}
		}
		os.Remove(tmpPath)
		return fmt.Errorf("重命名失败: %w", err)
	}
	return nil
}

// cleanupLoop 周期性清理过期 session。
func (us *UploadStore) cleanupLoop() {
	defer us.wg.Done()
	defer func() {
		if r := recover(); r != nil {
			us.logger.Error("cleanupLoop panic", "panic", r)
		}
	}()
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-us.stopCh:
			return
		case <-ticker.C:
			us.cleanupExpired()
		}
	}
}

// cleanupExpired 清理过期未完成的 session。
// 先持锁收集过期 ID，释放锁后再逐一删除临时文件 / os.RemoveAll，避免持锁执行 I/O。
func (us *UploadStore) cleanupExpired() {
	type expiredItem struct {
		id                 string
		tempRel            string
		reservation        *quota.Reservation
		storageMgrReserved int64
	}
	var expired []expiredItem

	us.mu.Lock()
	now := time.Now()
	for id, s := range us.sessions {
		if !s.Completed && now.After(s.ExpiresAt) {
			us.logger.Info("清理过期上传会话", "upload_id", id, "file_name", s.Filename, "expires_at", s.ExpiresAt)
			delete(us.sessions, id)
			expired = append(expired, expiredItem{id: id, tempRel: s.TempPath, reservation: s.Reservation, storageMgrReserved: s.StorageMgrReserved})
		}
	}
	us.mu.Unlock()

	for _, item := range expired {
		// P4 配额：过期会话（从未完成）归还预留，避免 chunk 字节长期挂账。
		// P5：storageMgr 回退预留（quota 未装配时）同样释放（与 Reservation 二选一）。
		if item.reservation != nil {
			item.reservation.Release()
		} else if item.storageMgrReserved > 0 && us.storageMgr != nil {
			us.storageMgr.Release(item.storageMgrReserved, CategoryChunked)
		}
		// 任务 4：删除 in-flight 临时文件（在 user 桶，独立于 session 目录）。
		if item.tempRel != "" {
			if abs, ok := us.tempAbsPath(item.tempRel); ok {
				if err := os.Remove(abs); err != nil && !os.IsNotExist(err) {
					us.logger.Warn("删除过期会话临时文件失败", "upload_id", item.id, "error", err)
				}
			}
		}
		us.locker.DeleteLock(item.id)
		dir := filepath.Join(us.baseDir, item.id)
		if err := os.RemoveAll(dir); err != nil {
			us.logger.Warn("清理过期会话目录失败", "upload_id", item.id, "error", err)
		}
	}
}

// CleanupSessionAfter 在指定延迟后清理 session 目录。
// 受 UploadStore.wg 追踪，支持通过 stopCh 提前中止。
func (us *UploadStore) CleanupSessionAfter(uploadID string, delay time.Duration) {
	us.wg.Go(func() {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
			us.DeleteSession(uploadID)
		case <-us.stopCh:
			return
		}
	})
}

// recoverSessions 从磁盘恢复未完成的 session。
// per-tenant baseDir（租户 chunk 桶）下直接是会话目录，单层恢复即可。
func (us *UploadStore) recoverSessions() {
	entries, err := os.ReadDir(us.baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		us.logger.Warn("读取分块上传目录失败", "error", err)
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		uploadID := entry.Name()
		sessionDir := filepath.Join(us.baseDir, uploadID)
		sessionPath := filepath.Join(sessionDir, "session.json")

		data, err := os.ReadFile(sessionPath)
		if err != nil {
			us.logger.Warn("读取 session.json 失败，跳过", "upload_id", uploadID, "error", err)
			continue
		}

		us.restoreSession(uploadID, sessionDir, data)
	}
}

// restoreSession 从磁盘恢复单个会话（解析 + 过期/完成判断 + 临时文件分片校验）。
func (us *UploadStore) restoreSession(uploadID, sessionDir string, data []byte) {
	var session ChunkedUploadSession
	if err := json.Unmarshal(data, &session); err != nil {
		us.logger.Warn("解析 session.json 失败，跳过", "upload_id", uploadID, "error", err)
		return
	}
	// 已过期的跳过（后续由 cleanupExpired 清理）
	if time.Now().After(session.ExpiresAt) {
		return
	}
	// 已完成的跳过（保留供 complete 查询）
	if session.Completed {
		us.sessions[uploadID] = &session
		return
	}
	// 任务 4：按临时名（user 桶在途整文件）逐分片重算校验，校准 bitmap——
	// 内容与 checksum 表匹配的分片保留，不匹配/缺失的分片需重传（bitmap 置 false）。
	if session.TempPath != "" {
		us.verifyTempChunks(&session)
	}
	us.sessions[uploadID] = &session
	us.logger.Info("恢复上传会话", "upload_id", uploadID, "file_name", session.Filename,
		"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)
}

// tempAbsPath 把在途临时文件的存储根相对路径（user/...，相对本租户根）派生为绝对路径。
// baseDir 恒为 <storage_root>/<owner>/chunk（per-tenant store 只归属一个租户），
// 其父目录即租户根；再拼 user/ 桶相对段。非 user/ 桶（被篡改/异常）返回 ok=false。
func (us *UploadStore) tempAbsPath(tempRel string) (string, bool) {
	if !strings.HasPrefix(tempRel, "user/") {
		return "", false
	}
	tenantRoot := filepath.Dir(us.baseDir)
	abs := filepath.Join(tenantRoot, filepath.FromSlash(tempRel))
	// 纵深防御：clean 后必须仍在本租户根内（防 session.json 篡改逃逸）。
	clean := filepath.Clean(abs)
	if clean != tenantRoot && !strings.HasPrefix(clean, tenantRoot+string(filepath.Separator)) {
		return "", false
	}
	return clean, true
}

// verifyTempChunks 打开在途临时文件，逐分片按 checksum 表重算校验：
// 匹配保留 bitmap，不匹配清除（需重传）。临时文件不存在/打不开时全部清除。
func (us *UploadStore) verifyTempChunks(session *ChunkedUploadSession) {
	abs, ok := us.tempAbsPath(session.TempPath)
	if !ok {
		us.logger.Warn("恢复会话临时文件路径非法，分片全部重传", "upload_id", session.UploadID)
		clear(session.ReceivedChunks)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		us.logger.Warn("恢复会话临时文件不可读，分片全部重传", "upload_id", session.UploadID, "error", err)
		clear(session.ReceivedChunks)
		return
	}
	defer f.Close()

	chunkSize := session.ChunkSize
	for i := 0; i < session.TotalChunks; i++ {
		offset := int64(i) * chunkSize
		want := session.ChunkChecksums[i]
		if want == "" {
			// 无 checksum 记录：无法校验，视为需重传（旧会话/异常状态）。
			session.ReceivedChunks[i] = false
			continue
		}
		// 该分片的实际长度（末片短于 chunk_size），与写侧 chunkLenAt 一致。
		length := chunkSize
		if remaining := session.TotalSize - offset; remaining < chunkSize {
			length = remaining
		}
		ok, err := us.verifyChunkChecksum(f, offset, length, want)
		if err != nil {
			us.logger.Warn("临时文件分片校验失败，需重传", "upload_id", session.UploadID, "chunk_index", i, "error", err)
			session.ReceivedChunks[i] = false
			continue
		}
		if !ok {
			session.ReceivedChunks[i] = false
		}
	}
}

// verifyChunkChecksum 从 f 的 offset 起计算 length 字节的 SHA-256 并与 want 比较。
// 与 pkg/testutil.ChecksumAt 语义一致（分片限定校验工具层次：testutil 为通用无状态版，
// 此处为 server 内部带 logger 版？——实际 ChecksumAt 已够用，但保留本地实现避免与
// testutil 的强耦合，且返回错误经其中间态语义一致）。
func (us *UploadStore) verifyChunkChecksum(f *os.File, offset, length int64, want string) (bool, error) {
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return false, err
	}
	lim := io.LimitReader(f, length)
	h := sha256.New()
	if _, err := io.Copy(h, lim); err != nil {
		return false, err
	}
	return hex.EncodeToString(h.Sum(nil)) == want, nil
}

// findMismatchChunks 对临时文件逐分片 seek 重算校验，返回与保存 checksum 表不一致的
// 分片索引（升序）。与恢复期 verifyTempChunks 相同的带长度语义（offset=i*ChunkSize、
// length=chunkLenAt）：临时文件缺失/不可读/读取错误时全部标为 mismatch（客户端整文件重传）。
// 仅对已接收分片（ChunkChecksums[i] 非空）做比对；未接收分片不支持（complete 前置
// AllChunksReceived 已保证全接收，此处防御性跳过空 checksum 分片）。
// 任务 5 I-2：重叠/越界写坏单个分片 → 该单片被精确识别为 mismatch（而非泛化 400）。
func (us *UploadStore) findMismatchChunks(session *ChunkedUploadSession) []int {
	abs, ok := us.tempAbsPath(session.TempPath)
	if !ok {
		us.logger.Warn("complete 校验临时文件路径非法，全部分片视为 mismatch", "upload_id", session.UploadID)
		return allMismatchIndices(session)
	}
	f, err := os.Open(abs)
	if err != nil {
		us.logger.Warn("complete 校验临时文件不可读，全部分片视为 mismatch", "upload_id", session.UploadID, "error", err)
		return allMismatchIndices(session)
	}
	defer f.Close()

	chunkSize := session.ChunkSize
	var mismatch []int
	for i := 0; i < session.TotalChunks; i++ {
		want := session.ChunkChecksums[i]
		if want == "" {
			continue // 未接收分片：complete 前置已保证全接收，此处防御性跳过
		}
		ok, err := us.verifyChunkChecksum(f, int64(i)*chunkSize, chunkLenAt(session, i), want)
		if err != nil {
			us.logger.Warn("complete 校验临时文件分片失败，全部分片视为 mismatch",
				"upload_id", session.UploadID, "chunk_index", i, "error", err)
			return allMismatchIndices(session)
		}
		if !ok {
			us.logger.Warn("complete 校验分片不匹配（需重传）",
				"upload_id", session.UploadID, "chunk_index", i)
			mismatch = append(mismatch, i)
		}
	}
	return mismatch
}

// allMismatchIndices 返回全部分片索引（0..TotalChunks-1，升序）。
func allMismatchIndices(session *ChunkedUploadSession) []int {
	out := make([]int, session.TotalChunks)
	for i := range out {
		out[i] = i
	}
	return out
}

// countReceived 返回 bitmap 中已置位的数量。
func countReceived(bitmap []bool) int {
	count := 0
	for _, b := range bitmap {
		if b {
			count++
		}
	}
	return count
}

// GetOrCreateSession 根据 uploadID 或文件名查找已有未完成的 session，或创建新 session。
func (us *UploadStore) GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error) {
	us.mu.Lock()
	defer us.mu.Unlock()

	// 按 uploadID 查找
	if uploadID != "" {
		if s, ok := us.sessions[uploadID]; ok && !s.Completed {
			// 审查 F4：按 key 复用前强制校验 filename/checksum/大小一致——否则攻击者可
			// 预置同 key 会话（伪造 owner 前缀或碰撞）劫持本次续传，篡改目标文件名。
			if s.Filename != filename || s.FileChecksum != fileChecksum || s.TotalSize != totalSize {
				us.logger.Warn("upload_id 冲突且元数据不符，拒绝复用旧会话",
					"upload_id", uploadID, "old_file", s.Filename, "new_file", filename)
				return nil, false, fmt.Errorf("upload_id 已存在但文件元数据不一致")
			}
			us.logger.Info("找到可续传的 session", "upload_id", s.UploadID, "file_name", s.Filename)
			return copySession(s), true, nil
		}
	}

	// 按文件名查找（兼容旧版本 / 无 upload_id 场景）
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed && s.FileChecksum == fileChecksum && s.TotalSize == totalSize {
			us.logger.Info("找到可续传的 session（按文件名匹配）", "upload_id", s.UploadID, "file_name", filename)
			return copySession(s), true, nil
		}
	}

	// 创建新 session
	if uploadID == "" {
		return nil, false, fmt.Errorf("upload_id 不能为空")
	}
	session := newSession(uploadID, filename, totalSize, chunkSize, totalChunks, fileChecksum, fileModTime, us.sessionTTL)

	us.logger.Info("创建上传会话", "upload_id", uploadID, "file_name", filename,
		"total_size", totalSize, "chunk_size", chunkSize, "total_chunks", totalChunks)

	// 创建会话目录
	sessionDir := filepath.Join(us.baseDir, uploadID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, false, fmt.Errorf("创建会话目录失败: %w", err)
	}
	if err := us.writeSessionJSON(session); err != nil {
		os.RemoveAll(sessionDir)
		return nil, false, err
	}

	us.sessions[uploadID] = session
	return session, false, nil
}

// MissingChunks 返回缺失的分块索引列表。
func MissingChunks(session *ChunkedUploadSession) []int {
	var missing []int
	for i, received := range session.ReceivedChunks {
		if !received {
			missing = append(missing, i)
		}
	}
	return missing
}
