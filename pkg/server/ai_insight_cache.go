// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

// ai_insight_cache.go 是 AI 文件洞察的落盘缓存（roadmap 11.9-⑤）：
//
//   - key = (owner, rel, kind)；value = {mtime, data}；
//   - mtime 未变命中缓存（零 LLM 调用）；文件变更 → 失效；
//   - 落盘 `<meta>/insight/<owner>.bin`（gob），原子写（tmp+Rename）。
//
// 缓存损坏 → 忽略 + 重新生成（幂等，设计 §4）。

import (
	"bytes"
	"context"
	"encoding/gob"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// insightEntry 是单条缓存（落盘 DTO）。
type insightEntry struct {
	Mtime int64  `gob:"mtime"`
	Data  []byte `gob:"data"`
}

// InsightCache 按 owner 分片的 AI 洞察结果缓存。
type InsightCache struct {
	mu      sync.RWMutex
	dir     string                              // <meta>/insight
	byOwner map[string]map[string]*insightEntry // owner → (rel+"\x00"+kind) → entry
	logger  *slog.Logger
}

// newInsightCache 构造缓存（dir = <tenant meta> 根；落盘在 dir/insight/）。
func newInsightCache(dir string, logger *slog.Logger) *InsightCache {
	if logger == nil {
		logger = slog.Default()
	}
	c := &InsightCache{
		dir:     filepath.Join(dir, "insight"),
		byOwner: make(map[string]map[string]*insightEntry),
		logger:  logger,
	}
	_ = os.MkdirAll(c.dir, 0o755)
	// 构造时载入全部 owner 落盘缓存（新实例可见既有快照）。
	if entries, err := os.ReadDir(c.dir); err == nil {
		for _, e := range entries {
			if !e.IsDir() && filepath.Ext(e.Name()) == ".bin" {
				c.loadOwner(strings.TrimSuffix(e.Name(), ".bin"))
			}
		}
	}
	return c
}

// cacheKey 构造 owner 内键（rel + kind 复合）。
func cacheKey(rel, kind string) string { return rel + "\x00" + kind }

// Get 读取缓存；ok=false = 未命中或 mtime 已变。
func (c *InsightCache) Get(ctx context.Context, owner, rel, kind string, mtime int64) ([]byte, int64, bool, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries := c.byOwner[owner]
	if entries == nil {
		return nil, 0, false, nil
	}
	e := entries[cacheKey(rel, kind)]
	if e == nil {
		return nil, 0, false, nil
	}
	if e.Mtime != mtime {
		return nil, 0, false, nil // mtime 变 → 失效（变异点）
	}
	return e.Data, e.Mtime, true, nil
}

// Put 写缓存并落盘（原子写）。
func (c *InsightCache) Put(ctx context.Context, owner, rel, kind string, mtime int64, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byOwner[owner] == nil {
		c.byOwner[owner] = make(map[string]*insightEntry)
	}
	c.byOwner[owner][cacheKey(rel, kind)] = &insightEntry{Mtime: mtime, Data: data}
	return c.persistLocked(owner)
}

// persistLocked 序列化单 owner 缓存并原子落盘。
func (c *InsightCache) persistLocked(owner string) error {
	entries := c.byOwner[owner]
	if entries == nil {
		return nil
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(entries); err != nil {
		return fmt.Errorf("insight cache encode: %w", err)
	}
	path := filepath.Join(c.dir, owner+".bin")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return fmt.Errorf("insight cache write: %w", err)
	}
	return os.Rename(tmp, path)
}

// loadOwner 载入单 owner 落盘缓存（损坏 → 忽略）。
func (c *InsightCache) loadOwner(owner string) {
	path := filepath.Join(c.dir, owner+".bin")
	data, err := os.ReadFile(path)
	if err != nil {
		return // 不存在/不可读 = 空
	}
	var entries map[string]*insightEntry
	if err := gob.NewDecoder(bytes.NewReader(data)).Decode(&entries); err != nil {
		c.logger.Warn("insight cache 损坏，忽略", "owner", owner, "error", err)
		return
	}
	c.mu.Lock()
	c.byOwner[owner] = entries
	c.mu.Unlock()
}
