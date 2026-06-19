// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockserver

import (
	"fmt"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
)

// MockUploadStore 内存版 UploadStore，实现 server.UploadStoreIface 全部方法。
type MockUploadStore struct {
	mu       sync.RWMutex
	sessions map[string]*server.ChunkedUploadSession
}

// NewUploadStore 创建一个空的 MockUploadStore。
func NewUploadStore() *MockUploadStore {
	return &MockUploadStore{sessions: make(map[string]*server.ChunkedUploadSession)}
}

// CreateSession 创建新的分块上传会话。
func (m *MockUploadStore) CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*server.ChunkedUploadSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.sessions[uploadID]; ok {
		return nil, fmt.Errorf("session already exists: %s", uploadID)
	}

	now := time.Now()
	s := &server.ChunkedUploadSession{
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
		ExpiresAt:      now.Add(24 * time.Hour),
	}
	m.sessions[uploadID] = s
	return s, nil
}

// GetSession 返回指定 uploadID 的会话，不存在时返回 nil。
func (m *MockUploadStore) GetSession(uploadID string) *server.ChunkedUploadSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[uploadID]
}

// GetSessionByFilename 按文件名查找未完成的会话，返回第一个匹配项。
func (m *MockUploadStore) GetSessionByFilename(filename string) *server.ChunkedUploadSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Filename == filename && !s.Completed {
			return s
		}
	}
	return nil
}

// MarkChunkReceived 标记指定分块为已接收。
func (m *MockUploadStore) MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[uploadID]
	if !ok {
		return fmt.Errorf("session not found: %s", uploadID)
	}
	if chunkIndex < 0 || chunkIndex >= s.TotalChunks {
		return fmt.Errorf("chunk_index %d out of range [0, %d)", chunkIndex, s.TotalChunks)
	}

	s.ReceivedChunks[chunkIndex] = true
	s.ChunkChecksums[chunkIndex] = checksum
	return nil
}

// AllChunksReceived 检查是否所有分块都已接收。
func (m *MockUploadStore) AllChunksReceived(uploadID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()

	s, ok := m.sessions[uploadID]
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

// CompleteSession 将会话标记为已完成。
func (m *MockUploadStore) CompleteSession(uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[uploadID]
	if !ok {
		return fmt.Errorf("session not found: %s", uploadID)
	}
	if s.Completed {
		return fmt.Errorf("session already completed: %s", uploadID)
	}
	s.Completed = true
	return nil
}

// GetOrCreateSession 查找已有会话或创建新会话。
// 返回 (session, found, error)，found=true 表示找到已有会话。
func (m *MockUploadStore) GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*server.ChunkedUploadSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 按 uploadID 查找
	if uploadID != "" {
		if s, ok := m.sessions[uploadID]; ok && !s.Completed {
			return s, true, nil
		}
	}

	// 按文件名匹配，跳过已完成的和不匹配的文件
	for _, s := range m.sessions {
		if s.Filename == filename && !s.Completed && s.FileChecksum == fileChecksum && s.TotalSize == totalSize {
			return s, true, nil
		}
	}

	// 创建新会话
	if uploadID == "" {
		return nil, false, fmt.Errorf("upload_id cannot be empty")
	}

	now := time.Now()
	session := &server.ChunkedUploadSession{
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
		ExpiresAt:      now.Add(24 * time.Hour),
	}
	m.sessions[uploadID] = session
	return session, false, nil
}

// ChunkFilePath 返回分块文件的路径（mock 返回空字符串）。
func (m *MockUploadStore) ChunkFilePath(uploadID string, chunkIndex int) string {
	return ""
}

// SessionDir 返回会话目录路径（mock 返回空字符串）。
func (m *MockUploadStore) SessionDir(uploadID string) string {
	return ""
}

// DeleteSession 删除指定 uploadID 的会话。
func (m *MockUploadStore) DeleteSession(uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, uploadID)
}

// CleanupSessionAfter 在指定延迟后清理会话。
func (m *MockUploadStore) CleanupSessionAfter(uploadID string, delay time.Duration) {
	time.AfterFunc(delay, func() {
		m.DeleteSession(uploadID)
	})
}

// Stop 停止后台任务（mock 空实现，无后台 goroutine 需要停止）。
func (m *MockUploadStore) Stop() {
	// No-op: mock has no background goroutines or resources to release.
}

// Health 返回存储健康状态（mock 始终健康）。
func (m *MockUploadStore) Health() error { return nil }

// LockChunkIO 获取 chunk IO 锁的模拟实现：始终不阻塞，返回空操作函数。
func (m *MockUploadStore) LockChunkIO(uploadID string) func() {
	// No-op: in-memory mock does not need chunk-level locking.
	return func() {}
}

// LockChunkMerge 获取 chunk 合并锁的模拟实现：始终不阻塞，返回空操作函数。
func (m *MockUploadStore) LockChunkMerge(uploadID string) func() {
	// No-op: in-memory mock does not need merge-level locking.
	return func() {}
}

// Ensure interface compliance.
var _ server.UploadStoreIface = (*MockUploadStore)(nil)
