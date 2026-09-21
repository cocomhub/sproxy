// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bufio"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// audit_store.go 是审计落盘存储（roadmap §2 P1：audit.persist_dir 可选落盘）。
//
// 设计：AuditStore 是「全量内存列表 + append-only JSON lines 日志」双写：
//   - Append 追加事件到内存（TS 升序追加尾部）并原子 append 到 <persist_dir>/audit.log；
//   - 启动时 NewAuditStore 扫描日志载入历史（重启可查，验收核心）；
//   - Recent 按 AuditFilter 过滤 + limit 返回（TS 倒序，最新在前，与 AuditRing 同语义）；
//   - 落盘格式：每行一个 AuditEvent JSON（可机器回放 / 日志 collector 消费）。
//
// 安全边界：落盘目录由配置显式指定（默认空 = 关闭零回归）；日志为明文 JSON（审计行
// 本就不含密钥/凭据——RecordAudit 只录 action/actor/mesh/object/result/detail/ts）。
// Append 永不阻塞业务：写盘失败记日志并跳过（审计落盘是尽力而为，绝不影响操作面）。

// AuditStore 是审计落盘存储（内存全量 + append-only 日志双写，thread-safe）。
type AuditStore struct {
	mu      sync.RWMutex
	events  []AuditEvent // 内存全量（TS 升序追加；查询时倒序返回）
	logPath string
	logger  *slog.Logger
	file    *os.File // append-only 句柄（nil = 未打开，懒打开）
}

// NewAuditStore 打开（或创建）审计日志并载入历史。
// logPath 为空 → 返回 nil（关闭）；打开失败返回错误（装配层记日志降级为 ring-only）。
func NewAuditStore(logPath string, logger *slog.Logger) (*AuditStore, error) {
	if logPath == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	st := &AuditStore{logPath: logPath, logger: logger}
	if err := st.load(); err != nil {
		return nil, err
	}
	return st, nil
}

// load 扫描日志载入历史事件（启动恢复；日志不存在视为空历史）。
func (s *AuditStore) load() error {
	f, err := os.Open(s.logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // 首次启动：无历史
		}
		return err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 1<<20) // 大行防御（单事件 JSON 应远小于 1 MiB）
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev AuditEvent
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			s.logger.Warn("审计日志行解析失败（跳过）", "error", err.Error())
			continue
		}
		if ev.TS.IsZero() {
			continue // 无时间戳的坏行跳过
		}
		s.events = append(s.events, ev)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	// 载入后按 TS 升序稳定（日志行已按写入顺序，但防御性排序）。
	sortAuditByTS(s.events)
	return nil
}

// Append 追加一条审计事件：内存记录 + 原子 append 落盘。
// 写盘失败记日志并继续（尽力而为，绝不阻断业务）。
func (s *AuditStore) Append(evt AuditEvent) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evt)
	if err := s.appendLine(evt); err != nil {
		s.logger.Error("审计落盘失败", "error", err.Error(), "action", evt.Action)
		return err
	}
	return nil
}

// appendLine 把事件以 JSON line 追加到日志（原子 append，O_APPEND + 单次 Write）。
// 懒打开文件句柄（首次写时建目录 + 打开 append 模式）。
func (s *AuditStore) appendLine(evt AuditEvent) error {
	if s.file == nil {
		if err := os.MkdirAll(filepath.Dir(s.logPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(s.logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		s.file = f
	}
	b, err := json.Marshal(evt)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = s.file.Write(b)
	return err
}

// Recent 按过滤条件返回至多 limit 条事件（TS 倒序，最新在前）。
// limit<=0 用默认兜底 defaultRecentLimit（与 AuditRing 同款防御）。
func (s *AuditStore) Recent(limit int, f AuditFilter) []AuditEvent {
	if s == nil {
		return []AuditEvent{}
	}
	if limit <= 0 {
		limit = defaultRecentLimit
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]AuditEvent, 0, min(limit, len(s.events)))
	for i := len(s.events) - 1; i >= 0 && len(out) < limit; i-- {
		if auditMatches(s.events[i], f) {
			out = append(out, s.events[i])
		}
	}
	return out
}

// Len 返回当前事件总数。
func (s *AuditStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.events)
}

// Close 关闭审计日志文件句柄（优雅停服 flush）。
func (s *AuditStore) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file != nil {
		err := s.file.Close()
		s.file = nil
		return err
	}
	return nil
}

// sortAuditByTS 按 TS 升序稳定排序（载入恢复用；Append 天然升序）。
func sortAuditByTS(evs []AuditEvent) {
	for i := 1; i < len(evs); i++ {
		for j := i; j > 0 && evs[j].TS.Before(evs[j-1].TS); j-- {
			evs[j], evs[j-1] = evs[j-1], evs[j]
		}
	}
}

// splitLinesRaw 按换行拆分（含末尾空行处理，供测试断言 JSON lines 格式）。
func splitLinesRaw(s string) []string {
	return strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
}
