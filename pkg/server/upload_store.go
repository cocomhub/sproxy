// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
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
}

// UploadStore 管理分块上传会话的持久化与并发安全。
type UploadStore struct {
	mu         sync.RWMutex
	baseDir    string // <uploadsDir>/.__chunked__/
	sessions   map[string]*ChunkedUploadSession
	persistCh  chan string   // uploadID → 异步持久化
	stopCh     chan struct{} // 关闭后台 goroutine
	stopOnce   sync.Once     // 保证 Stop 幂等
	wg         sync.WaitGroup
	sessionTTL time.Duration // 未完成上传会话的保留时间
	logger     *slog.Logger
}

const (
	chunkedDirName = ".__chunked__"
	chunkFileExt   = ".chunk"
)

// NewUploadStore 创建并启动 UploadStore，同时从磁盘恢复已有 session。
// sessionTTL 指定未完成上传会话的过期时间，默认 24h。
func NewUploadStore(baseDir string, sessionTTL time.Duration, logger *slog.Logger) *UploadStore {
	storeDir := filepath.Join(baseDir, chunkedDirName)
	log := defaultLogger(logger)
	if err := os.MkdirAll(storeDir, 0755); err != nil {
		log.Error("创建分块上传目录失败", "error", err)
	}

	if sessionTTL <= 0 {
		sessionTTL = 24 * time.Hour
	}

	us := &UploadStore{
		baseDir:    storeDir,
		sessions:   make(map[string]*ChunkedUploadSession),
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

	return us
}

// Stop 停止后台 goroutine 并等待结束。多次调用是安全的（幂等）。
func (us *UploadStore) Stop() {
	us.stopOnce.Do(func() {
		close(us.stopCh)
		us.wg.Wait()
	})
}

// CreateSession 创建一个新的分块上传会话，使用客户端提供的 uploadID。
func (us *UploadStore) CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error) {
	if uploadID == "" {
		return nil, fmt.Errorf("upload_id 不能为空")
	}
	now := time.Now()
	session := &ChunkedUploadSession{
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
		ExpiresAt:      now.Add(us.sessionTTL),
	}

	us.logger.Info("创建上传会话", "upload_id", uploadID, "filename", filename,
		"total_size", totalSize, "chunk_size", chunkSize, "total_chunks", totalChunks)

	// 创建会话目录
	sessionDir := filepath.Join(us.baseDir, uploadID)
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		return nil, fmt.Errorf("创建会话目录失败: %w", err)
	}

	// 写入 session.json
	if err := us.writeSessionJSON(session); err != nil {
		os.RemoveAll(sessionDir) // 清理
		return nil, err
	}

	us.mu.Lock()
	us.sessions[uploadID] = session
	us.mu.Unlock()

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
	// 返回副本，避免并发修改
	cp := *s
	cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
	copy(cp.ReceivedChunks, s.ReceivedChunks)
	cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
	copy(cp.ChunkChecksums, s.ChunkChecksums)
	return &cp
}

// GetSessionByFilename 按文件名查找未完成的 session。
func (us *UploadStore) GetSessionByFilename(filename string) *ChunkedUploadSession {
	us.mu.RLock()
	defer us.mu.RUnlock()
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed {
			cp := *s
			cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
			copy(cp.ReceivedChunks, s.ReceivedChunks)
			cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
			copy(cp.ChunkChecksums, s.ChunkChecksums)
			return &cp
		}
	}
	return nil
}

// MarkChunkReceived 标记指定分块为已接收并持久化。
func (us *UploadStore) MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error {
	us.mu.Lock()
	defer us.mu.Unlock()

	s, ok := us.sessions[uploadID]
	if !ok {
		return fmt.Errorf("upload_id 不存在: %s", uploadID)
	}
	if chunkIndex < 0 || chunkIndex >= s.TotalChunks {
		return fmt.Errorf("chunk_index %d 超出范围 [0, %d)", chunkIndex, s.TotalChunks)
	}

	s.ReceivedChunks[chunkIndex] = true
	s.ChunkChecksums[chunkIndex] = checksum

	us.logger.Debug("chunk 已接收", "upload_id", uploadID, "chunk_index", chunkIndex,
		"checksum", shortHash(checksum), "received", countReceived(s.ReceivedChunks), "total", s.TotalChunks)

	// 异步持久化
	select {
	case us.persistCh <- uploadID:
	default:
		// 通道满时同步持久化
		go us.persistSession(uploadID)
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
	us.logger.Info("上传会话已完成", "upload_id", uploadID, "filename", s.Filename,
		"received", countReceived(s.ReceivedChunks), "total", s.TotalChunks)
	select {
	case us.persistCh <- uploadID:
	default:
		go us.persistSession(uploadID)
	}
	return nil
}

// ChunkFilePath 返回指定分块的文件路径。
func (us *UploadStore) ChunkFilePath(uploadID string, chunkIndex int) string {
	return filepath.Join(us.baseDir, uploadID, chunkIndexFilename(chunkIndex))
}

// SessionDir 返回会话目录路径。
func (us *UploadStore) SessionDir(uploadID string) string {
	return filepath.Join(us.baseDir, uploadID)
}

// DeleteSession 删除会话目录及所有分块文件。
func (us *UploadStore) DeleteSession(uploadID string) {
	us.mu.Lock()
	delete(us.sessions, uploadID)
	us.mu.Unlock()

	dir := filepath.Join(us.baseDir, uploadID)
	if err := os.RemoveAll(dir); err != nil {
		us.logger.Warn("删除会话目录失败", "upload_id", uploadID, "error", err)
	}
}

// persistLoop 异步持久化 goroutine。
func (us *UploadStore) persistLoop() {
	defer us.wg.Done()
	for {
		select {
		case <-us.stopCh:
			return
		case uploadID := <-us.persistCh:
			us.persistSession(uploadID)
		}
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
	snapshot := *s
	snapshot.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
	copy(snapshot.ReceivedChunks, s.ReceivedChunks)
	snapshot.ChunkChecksums = make([]string, len(s.ChunkChecksums))
	copy(snapshot.ChunkChecksums, s.ChunkChecksums)
	us.mu.RUnlock()

	if err := us.writeSessionJSON(&snapshot); err != nil {
		us.logger.Error("持久化 session 失败", "upload_id", uploadID, "error", err)
	}
}

// writeSessionJSON 原子写入 session.json。
func (us *UploadStore) writeSessionJSON(s *ChunkedUploadSession) error {
	data, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("序列化 session 失败: %w", err)
	}
	dir := filepath.Join(us.baseDir, s.UploadID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("创建目录失败: %w", err)
	}
	tmpPath := filepath.Join(dir, "session.json.tmp")
	finalPath := filepath.Join(dir, "session.json")
	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, finalPath); err != nil {
		return fmt.Errorf("重命名失败: %w", err)
	}
	return nil
}

// cleanupLoop 周期性清理过期 session。
func (us *UploadStore) cleanupLoop() {
	defer us.wg.Done()
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
func (us *UploadStore) cleanupExpired() {
	us.mu.Lock()
	defer us.mu.Unlock()

	now := time.Now()
	for id, s := range us.sessions {
		if !s.Completed && now.After(s.ExpiresAt) {
			us.logger.Info("清理过期上传会话", "upload_id", id, "filename", s.Filename, "expires_at", s.ExpiresAt)
			delete(us.sessions, id)
			dir := filepath.Join(us.baseDir, id)
			if err := os.RemoveAll(dir); err != nil {
				us.logger.Warn("清理过期会话目录失败", "upload_id", id, "error", err)
			}
		}
	}
}

// recoverSessions 从磁盘恢复未完成的 session。
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

		var session ChunkedUploadSession
		if err := json.Unmarshal(data, &session); err != nil {
			us.logger.Warn("解析 session.json 失败，跳过", "upload_id", uploadID, "error", err)
			continue
		}

		// 已过期的跳过（后续由 cleanupExpired 清理）
		if time.Now().After(session.ExpiresAt) {
			continue
		}

		// 已完成的跳过（保留供 complete 查询）
		if session.Completed {
			us.sessions[uploadID] = &session
			continue
		}

		// 恢复：扫描磁盘上的 .chunk 文件，与 bitmap 对齐
		us.reconcileChunks(&session, sessionDir)
		us.sessions[uploadID] = &session
		us.logger.Info("恢复上传会话", "upload_id", uploadID, "filename", session.Filename,
			"received", countReceived(session.ReceivedChunks), "total", session.TotalChunks)
	}
}

// reconcileChunks 扫描磁盘上的 chunk 文件与 bitmap 对齐（处理 crash 后 bitmap 未持久化的情况）。
func (us *UploadStore) reconcileChunks(session *ChunkedUploadSession, sessionDir string) {
	chunkEntries, err := os.ReadDir(sessionDir)
	if err != nil {
		return
	}

	// 收集磁盘上存在的 .chunk 文件索引
	diskChunks := make(map[int]bool)
	for _, e := range chunkEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), chunkFileExt) {
			continue
		}
		name := strings.TrimSuffix(e.Name(), chunkFileExt)
		var idx int
		if _, err := fmt.Sscanf(name, "%05d", &idx); err == nil && idx >= 0 && idx < session.TotalChunks {
			diskChunks[idx] = true
		}
	}

	// 对齐 bitmap：磁盘上有、bitmap 为 false → 置 true（但不清除 checksum，因为无法恢复）
	for idx := range diskChunks {
		if !session.ReceivedChunks[idx] {
			session.ReceivedChunks[idx] = true
			// checksum 无法恢复，留空；下次 /upload/complete 时若客户端要求校验则会失败
		}
	}
}

func chunkIndexFilename(index int) string {
	return fmt.Sprintf("%05d%s", index, chunkFileExt)
}

func countReceived(bitmap []bool) int {
	count := 0
	for _, b := range bitmap {
		if b {
			count++
		}
	}
	return count
}

// shortHash 截取 SHA-256 的前 12 位用于日志显示。
func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// GetOrCreateSession 根据 uploadID 或文件名查找已有未完成的 session，或创建新 session。
func (us *UploadStore) GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error) {
	us.mu.Lock()
	defer us.mu.Unlock()

	// 按 uploadID 查找
	if uploadID != "" {
		if s, ok := us.sessions[uploadID]; ok && !s.Completed {
			us.logger.Info("找到可续传的 session", "upload_id", s.UploadID, "filename", s.Filename)
			cp := *s
			cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
			copy(cp.ReceivedChunks, s.ReceivedChunks)
			cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
			copy(cp.ChunkChecksums, s.ChunkChecksums)
			return &cp, true, nil
		}
	}

	// 按文件名查找（兼容旧版本 / 无 upload_id 场景）
	for _, s := range us.sessions {
		if s.Filename == filename && !s.Completed && s.FileChecksum == fileChecksum && s.TotalSize == totalSize {
			us.logger.Info("找到可续传的 session（按文件名匹配）", "upload_id", s.UploadID, "filename", filename)
			cp := *s
			cp.ReceivedChunks = make([]bool, len(s.ReceivedChunks))
			copy(cp.ReceivedChunks, s.ReceivedChunks)
			cp.ChunkChecksums = make([]string, len(s.ChunkChecksums))
			copy(cp.ChunkChecksums, s.ChunkChecksums)
			return &cp, true, nil
		}
	}

	// 创建新 session
	if uploadID == "" {
		return nil, false, fmt.Errorf("upload_id 不能为空")
	}
	now := time.Now()
	session := &ChunkedUploadSession{
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
		ExpiresAt:      now.Add(us.sessionTTL),
	}

	us.logger.Info("创建上传会话", "upload_id", uploadID, "filename", filename,
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
