// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// FileSink 是 Sink 的文件实现：每个作用域独立文件 <dir>/<scopeID>.audit.log，
// 每行一行 JSON 编码的 Row，O_APPEND 追加写、Append 后行尾 sync 落盘、Close 幂等。
// Recent 读回文件逐行解析并按 Filter 过滤，供云下载任务后续查询。
type FileSink struct {
	mu   sync.Mutex
	file *os.File
	path string
}

// NewScopeSink 创建每任务独立文件审计 sink：<dir>/<scopeID>.audit.log，每行一行 JSON Row。
// 目录不存在时以 0700 创建；文件以 O_CREATE|O_WRONLY|O_APPEND 打开，Unix 权限 0600。
func NewScopeSink(dir, scopeID string) (*FileSink, error) {
	if dir == "" {
		return nil, errors.New("audit: NewScopeSink: empty dir")
	}
	if scopeID == "" {
		return nil, errors.New("audit: NewScopeSink: empty scopeID")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit: NewScopeSink: mkdir %q: %w", dir, err)
	}
	path := filepath.Join(dir, scopeID+".audit.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: NewScopeSink: open %q: %w", path, err)
	}
	return &FileSink{file: f, path: path}, nil
}

// Append 追加一行 JSON Row + '\n'，写后行尾 sync 落盘。Close 之后调用返回错误。
func (s *FileSink) Append(r Row) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return errors.New("audit: FileSink: append after close")
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("audit: FileSink: marshal row: %w", err)
	}
	b = append(b, '\n')
	if _, err := s.file.Write(b); err != nil {
		return fmt.Errorf("audit: FileSink: write: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("audit: FileSink: sync: %w", err)
	}
	return nil
}

// Recent 读回作用域文件逐行解析为 Row，按 Filter 精确相等过滤后按写入序返回。
// 空 Filter 返回全部行；文件缺失/不可读或解析失败的行静默跳过（查询不阻塞追加）。
func (s *FileSink) Recent(f Filter) []Row {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	file, err := os.Open(s.path)
	if err != nil {
		return nil
	}
	defer file.Close()

	var rows []Row
	sc := bufio.NewScanner(file)
	// Meta 可能携带较大负载，放宽 Scanner 缓冲上限。
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Row
		if err := json.Unmarshal(line, &r); err != nil {
			continue
		}
		if matchFilter(r, f) {
			rows = append(rows, r)
		}
	}
	return rows
}

// Close 幂等关闭底层文件；已关闭时返回 nil。
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("audit: FileSink: close: %w", err)
	}
	s.file = nil
	return nil
}

// Path 返回该作用域日志文件的完整路径。
func (s *FileSink) Path() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.path
}

// matchFilter 按 Filter 精确相等筛选 Row；字段为空表示不筛选。
func matchFilter(r Row, f Filter) bool {
	if f.Type != "" && r.Type != f.Type {
		return false
	}
	if f.Level != "" && r.Level != f.Level {
		return false
	}
	if f.Dim != "" && r.Dim != f.Dim {
		return false
	}
	if f.Step != "" && r.Step != f.Step {
		return false
	}
	return true
}
