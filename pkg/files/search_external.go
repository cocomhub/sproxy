// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_external.go 实现**外部卷的搜索**（设计 2026-10-30-derived-state-projection D4 定案）：
// 用户裁定「基于缓存能力提供全卷缓存实现搜索，支持对有权限访问的所有卷搜索并聚合结果」。
//
// 数据源与缓存：
//   - 本地卷：`searchIndex`（内存增量索引，O(条目数) 内存遍历）；
//   - 外部卷：`VolumeViewLister.ListDirView` —— 装配层的**唯一目录列举入口**，其视图链含
//     `volumestate.CacheFS`（TTL=0 永不过期 + 写穿/显式 refresh）⇒ 首次遍历付出远端成本，
//     之后命中内存缓存；`volume refresh` 可强制感知进程外改动。
//
// 上限（必须，避免远端大桶拖死请求/内存爆）：
//   - 深度 `searchExternalMaxDepth`；
//   - 单请求总条目预算 `searchExternalMaxEntries`（跨全部外部卷共享）；
//   - 超时 `searchExternalTimeout`（ctx 派生；客户端断开亦取消）。
//
// 失败隔离：单卷/单目录列举失败 ⇒ Warn + 跳过（不使整体搜索失败）；本地卷结果不受影响。
// 权限：只遍历 `volume.AllowedVolumes`（ACL 视图）∩ 可选 `?volume=` 过滤。

import (
	"context"
	"errors"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/volume"
)

const (
	searchExternalMaxDepth   = 16
	searchExternalMaxEntries = 50000
	searchExternalTimeout    = 10 * time.Second
)

// searchBudget 是单请求的外部卷遍历预算（条目数）。
type searchBudget struct {
	used      int
	max       int
	truncated bool
}

// take 消耗一个条目；预算耗尽返回 false（并标记 truncated）。
func (b *searchBudget) take() bool {
	if b.used >= b.max {
		b.truncated = true
		return false
	}
	b.used++
	return true
}

// searchExternalVolumes 遍历 owner 可见的**外部卷**（`HasLocalRoot=false`），按 base name
// 子串匹配（不区分大小写）返回聚合结果；目录条目只记一次（Volume 空）。
//
// `seenDirs` 由调用方预填**本地卷**的目录命中项 ⇒ 跨本地/外部卷的目录去重与 List 语义一致。
func (s *Service) searchExternalVolumes(ctx context.Context, lister VolumeViewLister,
	owner, volFilter, qLower string, seenDirs map[string]bool,
) []FileInfo {
	vs := s.rt.volSet()
	if vs == nil || lister == nil {
		return nil
	}
	vols := volume.AllowedVolumes(vs.All(), owner)
	if volFilter != "" {
		v, ok := vs.ByName(volFilter)
		if !ok || !v.Authorize(owner) {
			return nil
		}
		vols = []volume.Volume{v}
	}
	ctx, cancel := context.WithTimeout(ctx, searchExternalTimeout)
	defer cancel()

	budget := &searchBudget{max: searchExternalMaxEntries}
	var out []FileInfo
	for _, v := range vols {
		if vs.HasLocalRoot(v.Name) {
			continue // 本地卷由 searchIndex 覆盖
		}
		if budget.truncated {
			break
		}
		s.walkVolumeView(ctx, lister, v.Name, owner, "", qLower, 0, seenDirs, budget, &out)
	}
	if budget.truncated {
		s.rt.logger().Warn("外部卷搜索达到上限，结果被截断",
			"owner", normalizeOwner(owner), "entries", budget.used, "max_entries", searchExternalMaxEntries)
	}
	return out
}

// walkVolumeView 递归列举一个外部卷目录并收集命中项（深度/预算受限）。
func (s *Service) walkVolumeView(ctx context.Context, lister VolumeViewLister,
	vol, owner, dir, qLower string, depth int, seenDirs map[string]bool,
	budget *searchBudget, out *[]FileInfo,
) {
	if depth > searchExternalMaxDepth {
		budget.truncated = true
		return
	}
	entries, supported, err := lister.ListDirView(ctx, vol, owner, dir)
	if !supported {
		return
	}
	if err != nil {
		s.logVolumeViewError(vol, dir, err)
		return
	}
	for _, e := range entries {
		if !budget.take() {
			return
		}
		if s.collectViewHit(vol, dir, qLower, e, seenDirs, out) {
			s.walkVolumeView(ctx, lister, vol, owner, joinViewRel(dir, e.Name), qLower, depth+1, seenDirs, budget, out)
		}
	}
}

// collectViewHit 处理单个视图条目：命中则入结果；返回「是否是可下探的目录」。
// 目录**无论是否命中**都要下探（子项可能命中）；在途临时文件不参与（也不下探）。
func (s *Service) collectViewHit(vol, dir, qLower string, e ExternalEntry,
	seenDirs map[string]bool, out *[]FileInfo,
) bool {
	if IsInflightTempName(e.Name) {
		return false
	}
	rel := joinViewRel(dir, e.Name)
	if strings.Contains(strings.ToLower(e.Name), qLower) {
		if e.IsDir {
			if !seenDirs[rel] {
				seenDirs[rel] = true
				*out = append(*out, FileInfo{Name: rel, IsDir: true})
			}
		} else {
			*out = append(*out, FileInfo{Name: rel, Size: e.Size, ModTime: e.MTime, Volume: vol, Checksum: e.Checksum})
		}
	}
	return e.IsDir
}

// joinViewRel 拼接视图相对路径（dir 空 ⇒ 条目自身名）。
func joinViewRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// logVolumeViewError 记录目录列举失败（不存在/取消/超时属正常，不告警）。
func (s *Service) logVolumeViewError(vol, dir string, err error) {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	s.rt.logger().Warn("外部卷搜索列举失败（跳过该目录）", "volume", vol, "dir", dir, "error", err)
}

// sortSearchResults 统一搜索结果的排序（目录在前 → name asc → volume asc），与
// `searchIndex.search` 的既有排序口径一致（合并外部卷结果后需重排）。
func sortSearchResults(results []FileInfo) {
	sort.Slice(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.IsDir != b.IsDir {
			return a.IsDir
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Volume < b.Volume
	})
}
