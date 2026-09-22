// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package syncmgr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/internal/shortid"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ConflictIndex 是冲突索引存储（#461 merge3 冲突登记 → 持久化 + API 查询/解决）。
//
// 内存 map + meta 目录 JSON 原子写（<meta>/conflicts.json）；Record 幂等去重（按 path+ts）；
// Resolve 写回文件后标记 resolved（不再列表出现）。装配层（cmd/sproxy）创建后注入
// syncexec.Executor.ConflictIndex（engine ConflictRecorder 回调）与 Handlers（API 查询）。
type ConflictIndex struct {
	mu     sync.Mutex
	dir    string                   // meta 目录（conflicts.json 所在）
	items  map[string]*ConflictItem // id → 条目（含 resolved）
	nextID int64
}

// ConflictItem 是索引条目（含解决状态与内容快照）。
type ConflictItem struct {
	ID        string   `json:"id"`
	Path      string   `json:"path"`
	HunkCount int      `json:"hunk_count"`
	BaseSHA   string   `json:"base_sha"`
	OursSHA   string   `json:"ours_sha"`
	TheirsSHA string   `json:"theirs_sha"`
	Ours      []string `json:"ours"`
	Theirs    []string `json:"theirs"`
	Timestamp int64    `json:"ts"`
	Resolved  bool     `json:"resolved"` // resolve 后 true（不再列表出现）
	ResolveAt int64    `json:"resolve_at,omitempty"`
}

// NewConflictIndex 构造冲突索引（metaDir 为 meta 目录；加载既有 conflicts.json）。
func NewConflictIndex(metaDir string) (*ConflictIndex, error) {
	if err := os.MkdirAll(metaDir, 0o755); err != nil {
		return nil, fmt.Errorf("冲突索引创建目录: %w", err)
	}
	idx := &ConflictIndex{
		dir:   metaDir,
		items: make(map[string]*ConflictItem),
	}
	if err := idx.load(); err != nil {
		return nil, err
	}
	return idx, nil
}

// Close 落盘（幂等；持久化保证崩溃不丢已登记冲突）。
func (c *ConflictIndex) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.saveLocked()
}

// conflictsPath 返回持久化文件路径。
func (c *ConflictIndex) conflictsPath() string { return filepath.Join(c.dir, "conflicts.json") }

// load 从 conflicts.json 载入（文件不存在 = 空索引，非错误）。
func (c *ConflictIndex) load() error {
	raw, err := os.ReadFile(c.conflictsPath())
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("冲突索引读取: %w", err)
	}
	var items []*ConflictItem
	if err := json.Unmarshal(raw, &items); err != nil {
		return fmt.Errorf("冲突索引解析: %w", err)
	}
	for _, it := range items {
		c.items[it.ID] = it
		if it.ID != "" {
			if _, perr := parseConflictID(it.ID); perr == nil {
				if n := conflictIDSeq(it.ID); n > c.nextID {
					c.nextID = n
				}
			}
		}
	}
	return nil
}

// saveLocked 原子写 conflicts.json（调用方持锁）。
func (c *ConflictIndex) saveLocked() error {
	items := make([]*ConflictItem, 0, len(c.items))
	for _, it := range c.items {
		items = append(items, it)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Timestamp < items[j].Timestamp })
	raw, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return fmt.Errorf("冲突索引序列化: %w", err)
	}
	tmp := c.conflictsPath() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("冲突索引写临时: %w", err)
	}
	if err := os.Rename(tmp, c.conflictsPath()); err != nil {
		return fmt.Errorf("冲突索引原子替换: %w", err)
	}
	return nil
}

// conflictID 生成形如 "cf-<base62>-<seq>" 的短 ID（与任务 ID 同风格）。
func (c *ConflictIndex) conflictID(ts int64) string {
	c.nextID++
	return fmt.Sprintf("cf-%s-%d", shortid.ShortHash(fmt.Sprintf("%d", ts)), c.nextID)
}

// parseConflictID 校验 ID 格式（返回错误 = 非本索引条目 ID）。
func parseConflictID(id string) (string, error) {
	parts := strings.Split(id, "-")
	if len(parts) != 3 || parts[0] != "cf" || parts[1] == "" || parts[2] == "" {
		return "", fmt.Errorf("非法冲突 ID: %s", id)
	}
	return id, nil
}

// conflictIDSeq 提取 ID 的序号段（用于 nextID 恢复）。
func conflictIDSeq(id string) int64 {
	parts := strings.Split(id, "-")
	if len(parts) != 3 {
		return 0
	}
	var n int64
	for _, ch := range parts[2] {
		if ch < '0' || ch > '9' {
			return 0
		}
		n = n*10 + int64(ch-'0')
	}
	return n
}

// Record 登记冲突（幂等去重：同 path+ts 已存在则跳过；返回条目 ID）。
func (c *ConflictIndex) Record(rec syncpkg.ConflictRecord) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 去重：同 path + ts 已有条目（重跑同一次 sync 不重复登记）。
	for _, it := range c.items {
		if it.Path == rec.Path && it.Timestamp == rec.Timestamp {
			return it.ID
		}
	}
	id := c.conflictID(rec.Timestamp)
	c.items[id] = &ConflictItem{
		ID:        id,
		Path:      rec.Path,
		HunkCount: rec.HunkCount,
		BaseSHA:   rec.BaseSHA,
		OursSHA:   rec.OursSHA,
		TheirsSHA: rec.TheirsSHA,
		Ours:      append([]string(nil), rec.Ours...),
		Theirs:    append([]string(nil), rec.Theirs...),
		Timestamp: rec.Timestamp,
	}
	_ = c.saveLocked() // 落盘失败不阻塞登记（下次 Close 再试；可观测性由装配层日志）
	return id
}

// List 返回未解决冲突（按时间升序）；owner 参数保留（当前冲突是全局文件路径，
// 未来可按 owner 过滤——本仓冲突不区分 owner 时传空返回全部）。
func (c *ConflictIndex) List(owner string) []*ConflictItem {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*ConflictItem
	for _, it := range c.items {
		if it.Resolved {
			continue
		}
		out = append(out, it)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp < out[j].Timestamp })
	return out
}

// Get 返回单条（含已 resolved——API 详情可查历史）。
func (c *ConflictIndex) Get(id string) (*ConflictItem, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	if !ok {
		return nil, false
	}
	cp := *it
	return &cp, true
}

// ResolveManual 手动解决：写 content 后标 resolved（调用方负责把 content 写回文件）。
func (c *ConflictIndex) ResolveManual(id, content string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	if !ok {
		return fmt.Errorf("冲突不存在: %s", id)
	}
	_ = content // 手动内容由调用方落盘；此处仅标记
	it.Resolved = true
	it.ResolveAt = time.Now().UnixNano()
	return c.saveLocked()
}

// Resolve 解决冲突：choice = ours|theirs|manual。
// ours/theirs：把对应侧内容（快照行）写回 Path（替换标记文件）；manual：写 content（由调用方提供）。
// 返回写回后的文件内容（调用方落盘）与条目（已标 resolved）。
func (c *ConflictIndex) Resolve(id, choice string) ([]byte, error) {
	c.mu.Lock()
	it, ok := c.items[id]
	if !ok {
		c.mu.Unlock()
		return nil, fmt.Errorf("冲突不存在: %s", id)
	}
	var content []byte
	switch choice {
	case "ours":
		content = []byte(strings.Join(it.Ours, "\n"))
	case "theirs":
		content = []byte(strings.Join(it.Theirs, "\n"))
	case "manual":
		// manual 由调用方通过 content 提供——但本 API 签名不含 content；
		// manual 需调用方先取条目再自行写文件。此处 manual 直接取 ours（保守）
		// 并由调用方覆盖。为防误用，manual 返回错误提示走带 content 的变体。
		c.mu.Unlock()
		return nil, fmt.Errorf("manual 解决需显式内容（调用方自行写文件后调 MarkResolved）")
	default:
		c.mu.Unlock()
		return nil, fmt.Errorf("choice 仅支持 ours|theirs，got %q", choice)
	}
	it.Resolved = true
	it.ResolveAt = time.Now().UnixNano()
	err := c.saveLocked()
	c.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return content, nil
}
