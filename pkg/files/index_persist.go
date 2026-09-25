// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// index_persist.go 实现搜索索引持久化（roadmap 2.3 P0 增强）：
// 内存增量索引 + 落盘快照（<tenant meta>/index/<owner>.json）——重启载入免全量
// WalkDir；写路径增量后周期保存；失效重建后保存覆盖。快照缺失/损坏 → 回退全量重建。

// indexSnapshotEntry 是快照的导出 DTO（indexEntry 字段未导出，JSON 需导出字段）。
// Tags 字段随 roadmap 11.10-④ 新增：旧格式快照缺该字段 → nil（零回归）。
type indexSnapshotEntry struct {
	Name    string   `json:"name"`
	Base    string   `json:"base"`
	IsDir   bool     `json:"is_dir,omitempty"`
	Size    int64    `json:"size,omitempty"`
	ModTime int64    `json:"mod_time,omitempty"`
	Volume  string   `json:"volume,omitempty"`
	Tags    []string `json:"tags,omitempty"`
}

// indexSnapshotFile 是快照 JSON 结构（entries 键为 rel）。
type indexSnapshotFile struct {
	Entries map[string]*indexSnapshotEntry `json:"entries"`
}

// toSnapshotEntries 内存条目 → 快照 DTO。
func toSnapshotEntries(entries map[string]*indexEntry) map[string]*indexSnapshotEntry {
	out := make(map[string]*indexSnapshotEntry, len(entries))
	for k, e := range entries {
		out[k] = &indexSnapshotEntry{Name: e.name, Base: e.base, IsDir: e.isDir, Size: e.size, ModTime: e.modTime, Volume: e.volume, Tags: e.tags}
	}
	return out
}

// fromSnapshotEntries 快照 DTO → 内存条目。旧格式快照缺 tags 字段 → nil（零回归）。
func fromSnapshotEntries(entries map[string]*indexSnapshotEntry) map[string]*indexEntry {
	out := make(map[string]*indexEntry, len(entries))
	for k, e := range entries {
		out[k] = &indexEntry{name: e.Name, base: e.Base, isDir: e.IsDir, size: e.Size, modTime: e.ModTime, volume: e.Volume, tags: e.Tags}
	}
	return out
}

// indexSnapshotPath 返回 owner 快照文件路径（meta/index/<owner>.json）。
func indexSnapshotPath(t *storage.Tenant, owner string) string {
	abs, ok := t.Root().Abs(filepath.ToSlash(filepath.Join("meta", "index", owner+".json")))
	if !ok {
		return ""
	}
	return abs
}

// saveIndexSnapshot 原子写 owner 索引快照（tmp + rename）。失败返回错误（调用方
// 记日志后继续——快照是加速层，写失败不影响索引功能）。
func saveIndexSnapshot(t *storage.Tenant, owner string, entries map[string]*indexEntry) error {
	if t == nil {
		return nil
	}
	p := indexSnapshotPath(t, owner)
	if p == "" {
		return nil
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(indexSnapshotFile{Entries: toSnapshotEntries(entries)})
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// loadIndexSnapshot 载入 owner 索引快照。缺失/损坏 → (nil, false)（调用方全量重建）。
func loadIndexSnapshot(t *storage.Tenant, owner string) (map[string]*indexEntry, bool) {
	if t == nil {
		return nil, false
	}
	p := indexSnapshotPath(t, owner)
	if p == "" {
		return nil, false
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, false
	}
	var f indexSnapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, false
	}
	if f.Entries == nil {
		return nil, false
	}
	return fromSnapshotEntries(f.Entries), true
}
