// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// ChecksumStore 在 uploads 目录下维护一个 .checksums.json 侧边文件，
// 持久化每个文件的 SHA-256 摘要，供 upload/download/delete 操作复用。
type ChecksumStore struct {
	mu        sync.RWMutex
	storePath string
	checksums map[string]string // filename -> sha256 hex
	logger    *slog.Logger
}

// NewChecksumStore 创建 ChecksumStore，从 uploadsDir/.checksums.json 加载已有记录。
func NewChecksumStore(uploadsDir string, logger *slog.Logger) *ChecksumStore {
	storePath := filepath.Join(uploadsDir, ".checksums.json")
	cs := &ChecksumStore{
		storePath: storePath,
		checksums: make(map[string]string),
		logger:    defaultLogger(logger),
	}

	data, err := os.ReadFile(storePath)
	if err != nil {
		if !os.IsNotExist(err) {
			cs.logger.Warn("读取 checksum 存储文件失败", "path", storePath, "error", err)
		}
		return cs
	}

	if len(data) == 0 {
		return cs
	}

	if err := json.Unmarshal(data, &cs.checksums); err != nil {
		cs.logger.Warn("解析 checksum 存储文件失败，将使用空存储", "path", storePath, "error", err)
		cs.checksums = make(map[string]string)
	}
	return cs
}

// Get 查询指定文件的 checksum。
func (cs *ChecksumStore) Get(filename string) (string, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	v, ok := cs.checksums[filename]
	return v, ok
}

// Set 写入一条 checksum 记录并持久化到磁盘。
func (cs *ChecksumStore) Set(filename, checksum string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.checksums[filename] = checksum
	if err := cs.saveLocked(); err != nil {
		cs.logger.Error("checksum 存储持久化失败", "op", "set", "filename", filename, "error", err)
	}
}

// Delete 删除指定文件的 checksum 记录并持久化。
func (cs *ChecksumStore) Delete(filename string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	delete(cs.checksums, filename)
	if err := cs.saveLocked(); err != nil {
		cs.logger.Error("checksum 存储持久化失败", "op", "delete", "filename", filename, "error", err)
	}
}

// DeletePrefix 删除指定前缀的所有 checksum 记录（用于目录删除）。
func (cs *ChecksumStore) DeletePrefix(prefix string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for key := range cs.checksums {
		if strings.HasPrefix(key, prefix) {
			delete(cs.checksums, key)
		}
	}
	if err := cs.saveLocked(); err != nil {
		cs.logger.Error("checksum 存储持久化失败", "op", "deletePrefix", "prefix", prefix, "error", err)
	}
}

// GetAll 返回全部 checksum 记录的副本（filename -> sha256）。
func (cs *ChecksumStore) GetAll() map[string]string {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	result := make(map[string]string, len(cs.checksums))
	maps.Copy(result, cs.checksums)
	return result
}

// saveLocked 必须在持有 cs.mu 的情况下调用：
// 先写入临时文件再用 os.Rename 原子替换，避免进程中途崩溃导致 .checksums.json 损坏。
func (cs *ChecksumStore) saveLocked() error {
	data, err := json.MarshalIndent(cs.checksums, "", "  ")
	if err != nil {
		return err
	}
	tmp := cs.storePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, cs.storePath)
}
