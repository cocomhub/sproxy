// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// maxIndexSnapshotBytes 是快照读取上界（防被注入超大 JSON 的 OOM：快照是加速层，宁可重建）。
// 与 meta sidecar 同口径（8MiB 级索引；超限即视为损坏 → 全量重建）。
const maxIndexSnapshotBytes = 8 << 20

// index_persist.go 实现搜索索引持久化（roadmap 2.3 P0 增强）：
// 内存增量索引 + 落盘快照（<tenant>/meta/index/<owner>.json）——重启载入免全量
// WalkDir；写路径增量后周期保存；失效重建后保存覆盖。快照缺失/损坏 → 回退全量重建。
//
// **快照经 storage.Root 读写**（此前是裸 os I/O + 绝对路径）：索引条目含**用户可见文件名**
// （name/base/rel），裸 I/O 会让它在 at-rest 加密卷上**明文落盘**（加密承诺对文件名落空），
// 并绕过**封印**（整卷封印下仍能写入被封装区间）。改经 Root 后：加密卷透明加密落盘、封印路径
// 写入恒拒（`ErrSealed` → 保存失败仅日志，快照是加速层，不影响索引功能）。
// 读取用 `OpenDecrypted`：历史上的明文快照（本改动前写入）在加密卷上解密失败 → 视为
// 「无快照」回退全量重建（**无兼容迁移**：用户裁定尚未上线；重建数据可完全再生成）。

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
//
// Gen / Epoch 是**失效代际**（审计 2026-10-29）：快照只在代际匹配时可信。
//   - Gen：per-owner 代际，`InvalidateIndex(owner)` 递增（旁路写/单卷 refresh）；
//   - 全局失效（`InvalidateAllIndexes`）由删除全部快照实现（跨进程有效）。
//
// 为什么要代际而不是「删快照」：删除是 best-effort（权限/占用/崩溃窗口都可能失败），
// 一旦失败旧快照会被重新载入 ⇒ 失效静默失效。带代际后即使快照复活也必然重建。
// 旧快照缺这两个字段 ⇒ 0；初始代际也是 0 ⇒ 向后兼容（首次部署不强制重建）。
type indexSnapshotFile struct {
	Entries map[string]*indexSnapshotEntry `json:"entries"`
	Gen     uint64                         `json:"gen,omitempty"`
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

// indexSnapshotRel 返回 owner 快照的**租户根相对路径**（meta/index/<owner>.json）。
func indexSnapshotRel(owner string) string {
	return filepath.ToSlash(filepath.Join("meta", "index", owner+".json"))
}

// saveIndexSnapshot 原子写 owner 索引快照（tmp + rename）。失败返回错误（调用方
// 记日志后继续——快照是加速层，写失败不影响索引功能）。
func saveIndexSnapshot(t *storage.Tenant, owner string, entries map[string]*indexEntry, gen uint64) error {
	if t == nil || t.Root() == nil {
		return nil
	}
	root := t.Root()
	rel := indexSnapshotRel(owner)
	if dir := filepath.ToSlash(filepath.Dir(rel)); dir != "." && dir != "" {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.Marshal(indexSnapshotFile{Entries: toSnapshotEntries(entries), Gen: gen})
	if err != nil {
		return err
	}
	// 经 Root 的加密写（非加密卷 = 普通写）+ tmp→rename 原子替换（`AtomicRename` 处理
	// Windows「目标已存在」语义）。
	tmpRel := rel + ".tmp"
	w, werr := root.OpenFileEncrypted(tmpRel, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if werr != nil {
		return werr
	}
	if _, cerr := w.Write(data); cerr != nil {
		_ = w.Close()
		_ = root.Remove(tmpRel)
		return cerr
	}
	if cerr := w.Close(); cerr != nil {
		_ = root.Remove(tmpRel)
		return cerr
	}
	if rerr := atomicRenameRoot(root, tmpRel, rel); rerr != nil {
		_ = root.Remove(tmpRel)
		return rerr
	}
	return nil
}

// loadIndexSnapshot 载入 owner 索引快照。缺失/损坏 → (nil, false)（调用方全量重建）。
func loadIndexSnapshot(t *storage.Tenant, owner string) (map[string]*indexEntry, uint64, bool) {
	if t == nil || t.Root() == nil {
		return nil, 0, false
	}
	rc, err := t.Root().OpenDecrypted(indexSnapshotRel(owner))
	if err != nil {
		return nil, 0, false // 缺失/不可读/（加密卷上的）历史明文快照 → 全量重建
	}
	data, rerr := io.ReadAll(io.LimitReader(rc, maxIndexSnapshotBytes+1))
	_ = rc.Close()
	if rerr != nil || int64(len(data)) > maxIndexSnapshotBytes {
		return nil, 0, false
	}
	var f indexSnapshotFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, 0, false
	}
	if f.Entries == nil {
		return nil, 0, false
	}
	return fromSnapshotEntries(f.Entries), f.Gen, true
}
