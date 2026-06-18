// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mockserver

import (
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/server"
)

// MockChecksumStore 实现 server.ChecksumStoreIface，内存 map。
type MockChecksumStore struct {
	mu   sync.RWMutex
	data map[string]string

	SetErr    error
	GetErr    error
	DeleteErr error
}

// NewChecksumStore 创建一个空的 MockChecksumStore。
func NewChecksumStore() *MockChecksumStore {
	return &MockChecksumStore{data: make(map[string]string)}
}

// Get 返回指定文件的 checksum。
func (m *MockChecksumStore) Get(filename string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.GetErr != nil {
		return "", false
	}
	v, ok := m.data[filename]
	return v, ok
}

// Set 设置指定文件的 checksum。
func (m *MockChecksumStore) Set(filename, checksum string) {
	if m.SetErr != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[filename] = checksum
}

// Delete 删除指定文件的 checksum。
func (m *MockChecksumStore) Delete(filename string) {
	if m.DeleteErr != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, filename)
}

// Rename 将一条 checksum 记录从 from 迁移到 to。
func (m *MockChecksumStore) Rename(from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[from]; ok {
		m.data[to] = v
		delete(m.data, from)
	}
}

// DeletePrefix 删除指定前缀的所有 checksum 记录。
func (m *MockChecksumStore) DeletePrefix(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.data {
		if strings.HasPrefix(k, prefix) {
			delete(m.data, k)
		}
	}
}

// GetAll 返回全部 checksum 记录的副本。
func (m *MockChecksumStore) GetAll() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]string, len(m.data))
	for k, v := range m.data {
		cp[k] = v
	}
	return cp
}

// Ensure implementation of ChecksumStoreIface.
var _ server.ChecksumStoreIface = (*MockChecksumStore)(nil)
