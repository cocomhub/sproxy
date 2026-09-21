// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// search_index.go 是「搜索/列表走增量文件索引」的实现（roadmap P0 文件服务）。
//
// 动机：旧实现 Search() 每次请求 filepath.WalkDir 全量扫描 owner 的 user 桶（O(N) 且
// 大量 stat 系统调用），大文件库下搜索与列表性能随文件数线性退化。本文件引入按 owner
// 维度的内存索引：
//
//   - **首次使用全量构建**：进程内某 owner 首次搜索/列表时 WalkDir 一次构建（跳过
//     分块在途临时文件），此后只维护增量；
//   - **写路径增量维护**：upload / 分块 complete / rename / delete / rmdir 成功路径
//     upsert/rename/remove，与 checksum 台账同生命周期；
//   - **索引丢失/损坏可重建**：装配层旁路写（版本恢复）经 `InvalidateIndex(owner)`
//     显式失效，下次访问全量重建；进程重启后天然重建。
//
// 语义保持与旧实现**逐字一致**：子串匹配不区分大小写，只匹配目录/文件**名**（base name）
// 不匹配完整路径；命中目录返回 IsDir 目录条目（name = 相对 user 桶路径，跨卷去重不绑卷）；
// 命中文件 name = 完整相对路径（如 sub/keep_me.txt）；在途临时文件不参与。

import (
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/storage"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// indexEntry 是索引中的单个条目。Key = 相对 user 桶的完整 rel（如 "sub/keep_me.txt"），
// 不含 "user/" 前缀（与 checksum 台账的 "user/"+rel 键、以及旧搜索结果的 name 字段形态
// 分开：name 字段 = 相对 user 桶的完整 rel，ToSlash）。
type indexEntry struct {
	// name 是相对 user 桶的完整 rel（ToSlash），搜索结果的 Name 字段直接取它。
	name string
	// base 是最后一段文件名（目录/文件的本名），子串匹配只对它做。
	base string
	// isDir 表示目录条目（旧语义：目录只列一次、不绑卷、无 size/mtime/checksum）。
	isDir bool
	// size 是文件字节数（目录为 0）。
	size int64
	// modTime 是文件 UnixNano mtime（目录为 0）。
	modTime int64
	// volume 是文件所在卷名（目录为空；多卷聚合语义）。
	volume string
}

// ownerIndex 是一个 owner 的索引快照。同一 owner 的所有卷并入一张表
// （rel 在 owner 逻辑树内唯一——AD-4），volume 字段记录条目物理归属。
type ownerIndex struct {
	entries map[string]*indexEntry // key = 相对 user 桶的完整 rel
}

// searchIndex 是按 owner 分片的内存索引（进程内缓存）。
//
// 并发模型：ownerIndex 在构建/失效后**整体替换**（atomic.Pointer 语义），写路径增量
// upsert/remove 在持锁下修改 map 后替换指针；读路径只读指针（无锁）。这使搜索与写路径
// 并发安全且不互相阻塞（搜索是高频读，写是低频改）。
type searchIndex struct {
	mu      sync.Mutex
	owners  map[string]*ownerIndex
	built   map[string]bool // owner → 是否已全量构建（防重复 WalkDir；失效后重置）
	logger  func() *slog.Logger
	volSet  func() VolumeSet
	tenant  func(volName, owner string) *storage.Tenant
	tenant0 func(owner string) *storage.Tenant
}

// newSearchIndex 构造索引容器。logger/volSet/tenant/tenant0 是 Service 侧能力的注入
// （构建/失效时按当前装配状态取值，避免索引持有过期引用）。
func newSearchIndex(logger func() *slog.Logger, volSet func() VolumeSet,
	tenant func(volName, owner string) *storage.Tenant, tenant0 func(owner string) *storage.Tenant) *searchIndex {
	return &searchIndex{
		owners:  map[string]*ownerIndex{},
		built:   map[string]bool{},
		logger:  logger,
		volSet:  volSet,
		tenant:  tenant,
		tenant0: tenant0,
	}
}

// ensureOwner 返回 owner 的索引快照，首次访问时全量构建。
// owner 租户不可用（非法/根不可用）时返回 nil（调用方按空结果处理，与旧语义一致）。
func (ix *searchIndex) ensureOwner(owner string) *ownerIndex {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.built[owner] {
		return ix.owners[owner]
	}
	// 全量构建：owner 视图逐卷 WalkDir user 桶。
	oi := ix.buildLocked(owner)
	ix.owners[owner] = oi
	ix.built[owner] = true
	return oi
}

// invalidate 使 owner 索引失效（装配层旁路写后调用；下次访问全量重建）。
func (ix *searchIndex) invalidate(owner string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	delete(ix.built, owner)
	delete(ix.owners, owner)
}

// upsert 写路径增量：新增/覆盖一个文件条目，并顺带补父目录链（旧 WalkDir 语义：
// 新建文件到新目录后，该目录在搜索中作为目录条目可见）。
func (ix *searchIndex) upsert(owner, rel string, size, modTime int64, volume string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return // 索引尚未构建（首次搜索会全量构建），无需增量
	}
	name := filepath.ToSlash(rel)
	oi.entries[name] = &indexEntry{
		name: name, base: filepath.Base(name),
		size: size, modTime: modTime, volume: volume,
	}
	ix.ensureParentsLocked(oi, name)
}

// ensureParentsLocked 补父目录链（调用方持 ix.mu）：从 name 逐级取父路径，
// 索引中不存在则登记 isDir 目录条目（与全量构建的目录条目同形）。
func (ix *searchIndex) ensureParentsLocked(oi *ownerIndex, name string) {
	dir := filepath.Dir(name)
	for dir != "." && dir != "/" && dir != "" {
		if _, ok := oi.entries[dir]; !ok {
			oi.entries[dir] = &indexEntry{name: dir, base: filepath.Base(dir), isDir: true}
		}
		dir = filepath.Dir(dir)
	}
}

// remove 写路径增量：删除一个文件条目（rmdir/delete）。
func (ix *searchIndex) remove(owner, rel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	delete(oi.entries, filepath.ToSlash(rel))
}

// removePrefix 写路径增量：删除一个目录子树（rmdir）。
func (ix *searchIndex) removePrefix(owner, rel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	prefix := filepath.ToSlash(rel)
	for k := range oi.entries {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(oi.entries, k)
		}
	}
}

// rename 写路径增量：重命名/移动条目（from → to）。
func (ix *searchIndex) rename(owner, from, to string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	fromName := filepath.ToSlash(from)
	toName := filepath.ToSlash(to)
	if e, ok := oi.entries[fromName]; ok {
		delete(oi.entries, fromName)
		e.name = toName
		e.base = filepath.Base(toName)
		oi.entries[toName] = e
		return
	}
	// 目录子树重命名：前缀搬移。
	var moves []struct{ old, new string }
	for k := range oi.entries {
		if strings.HasPrefix(k, fromName+"/") {
			moves = append(moves, struct{ old, new string }{k, toName + strings.TrimPrefix(k, fromName)})
		}
	}
	for _, m := range moves {
		e := oi.entries[m.old]
		delete(oi.entries, m.old)
		e.name = m.new
		e.base = filepath.Base(m.new)
		oi.entries[m.new] = e
	}
}

// buildLocked 全量构建 owner 索引（调用方持 ix.mu）。owner 视图逐卷 WalkDir user 桶，
// 跳过在途临时文件（.inflight-*.part）；目录条目与文件条目分别登记（目录用独立 isDir
// 标记，搜索时目录条目只列一次）。owner 租户不可用返回空索引（非 nil，保证 nil 安全）。
func (ix *searchIndex) buildLocked(owner string) *ownerIndex {
	oi := &ownerIndex{entries: map[string]*indexEntry{}}
	if ix.tenant0 == nil {
		return oi
	}
	baseTnt := ix.tenant0(owner)
	if baseTnt == nil || baseTnt.Root() == nil {
		return oi
	}

	// 单卷（volSet nil）：唯一根 user 桶。
	if ix.volSet == nil || ix.volSet() == nil {
		ix.walkUserRoot(baseTnt.Root(), baseTnt.UserRoot(), "", "", oi)
		return oi
	}
	// 多卷：owner 视图逐卷（默认卷在前）；同 rel 唯一（AD-4），后卷同名条目跳过
	// （目录条目跨卷并存时只记默认卷命中——搜索目录条目不绑卷，无需区分）。
	seen := map[string]bool{}
	for _, v := range volume.AllowedVolumes(ix.volSet().All(), owner) {
		tnt := ix.tenant(v.Name, owner)
		if tnt == nil || tnt.Root() == nil {
			continue
		}
		// 只搜 user 桶存在的卷（只读探测，不创建租户目录）。
		if _, err := tnt.Root().Stat(tnt.UserRoot()); err != nil {
			continue
		}
		ix.walkUserRoot(tnt.Root(), tnt.UserRoot(), v.Name, "", oi)
		_ = seen // 目录条目去重在 walkUserRoot 内经 oi.entries 判定
	}
	return oi
}

// walkUserRoot 递归遍历 user 桶，把文件/目录登记进 oi。
// volume 为空 = 单卷（旧装配）。dirRel 是相对 user 桶的路径前缀（"" = 根）。
func (ix *searchIndex) walkUserRoot(root *storage.Root, userRoot, volume, dirRel string, oi *ownerIndex) {
	rel := userRoot
	if dirRel != "" {
		rel = userRoot + "/" + dirRel
	}
	entries, err := root.ReadDir(rel)
	if err != nil {
		return // 目录不可读（正常空目录返回空）；静默跳过与旧 searchWalkDirCallback 同语义
	}
	for _, e := range entries {
		name := e.Name()
		if IsInflightTempName(name) {
			continue // 在途临时文件不参与（与列表/搜索旧语义一致）
		}
		child := dirRel
		if child != "" {
			child += "/"
		}
		child += name
		key := filepath.ToSlash(child)
		if e.IsDir() {
			// 目录条目：跨卷并存时只登记一次（isDir 条目不绑卷，搜索去重语义）。
			if _, exists := oi.entries[key]; !exists {
				oi.entries[key] = &indexEntry{name: key, base: name, isDir: true}
			}
			ix.walkUserRoot(root, userRoot, volume, child, oi)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		oi.entries[key] = &indexEntry{
			name: key, base: name,
			size: info.Size(), modTime: info.ModTime().UnixNano(), volume: volume,
		}
	}
}

// search 按 q（已小写）在 owner 索引中匹配，返回与旧 Search 逐字一致的 ListResult：
// 文件条目 name = 完整相对路径（ToSlash），带 size/mtime/checksum/volume；目录条目
// IsDir=true、name = 相对 user 桶路径、Volume 空。结果排序：先文件后目录？——**与旧语义
// 一致**：旧实现按 WalkDir 序遍历（先父后子、按目录深度优先），本实现按 name 字典序
// 稳定排序（可预测、与 List 的 name asc 排序观感一致；契约测试不锁序）。
func (ix *searchIndex) search(owner, qLower string, csMap map[string]string) []FileInfo {
	// 先目录后文件、按 name 字典序：与 List 的 sortFileEntries(name asc) 对齐。
	out := ix.searchLocked(owner, qLower, csMap)
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // 目录在前
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// searchLocked 是 search 的底层实现（不含排序，供 list 复用目录/文件条目构造逻辑）。
func (ix *searchIndex) searchLocked(owner, qLower string, csMap map[string]string) []FileInfo {
	oi := ix.ensureOwner(owner)
	if oi == nil {
		return nil
	}
	out := make([]FileInfo, 0, 8) // 恒非 nil：空结果也是空切片（ListResult.Files 契约）
	for _, e := range oi.entries {
		if !strings.Contains(strings.ToLower(e.base), qLower) {
			continue
		}
		if e.isDir {
			out = append(out, FileInfo{Name: e.name, IsDir: true})
			continue
		}
		fi := FileInfo{Name: e.name, Size: e.size, ModTime: e.modTime, Volume: e.volume}
		if cs, ok := csMap["user/"+e.name]; ok {
			fi.Checksum = cs
		}
		out = append(out, fi)
	}
	return out
}

// list 按 owner 索引列出目录的直接子项（roadmap P0 验收另一半：列表走索引）。
//
// dirRel 是相对 user 桶的目录路径（"" = 根）；volFilter 非空时只列该卷文件条目
// （目录条目不绑卷，始终列出——与旧多卷聚合 List 的 seenDirs 语义一致）。
//
// 返回条目与旧 List 逐字一致的形状：目录条目 name = basename、IsDir=true、无 size/mtime/
// checksum/volume；文件条目 name = basename、带 size/mtime/checksum/volume。
//
// 索引未构建（owner 租户不可用）时返回 nil（调用方按空结果处理，与旧语义一致）。
//
// 语义要点：
//   - 直接子项 = key 前缀 dirRel/ 且不含更深的 '/'（一层）；
//   - 目录条目来自索引的 isDir 条目（构建时跨卷去重、不绑卷）——列表天然只列一次；
//   - 文件条目带 volume（构建时登记）；volFilter 非空时过滤；
//   - 在途临时文件在构建时已跳过 → 天然排除；
//   - 排序由调用方 sortFileEntries 处理（本方法不排序）。
func (ix *searchIndex) list(owner, dirRel, volFilter string, csMap map[string]string) []FileInfo {
	oi := ix.ensureOwner(owner)
	if oi == nil {
		return nil
	}
	prefix := dirRel
	if prefix != "" {
		prefix += "/"
	}
	out := make([]FileInfo, 0, 8) // 恒非 nil：空结果也是空切片（ListResult.Files 契约）
	for key, e := range oi.entries {
		if prefix != "" {
			if !strings.HasPrefix(key, prefix) {
				continue
			}
			rest := key[len(prefix):]
			if rest == "" || strings.Contains(rest, "/") {
				continue // 目录自身或更深层子项 → 非直接子项
			}
		} else if strings.Contains(key, "/") {
			continue // 根目录只列直接子项
		}
		if e.isDir {
			out = append(out, FileInfo{Name: e.base, IsDir: true})
			continue
		}
		if volFilter != "" && e.volume != volFilter {
			continue
		}
		fi := FileInfo{Name: e.base, Size: e.size, ModTime: e.modTime, Volume: e.volume}
		if cs, ok := csMap["user/"+key]; ok {
			fi.Checksum = cs
		}
		out = append(out, fi)
	}
	return out
}

// indexForService 把 Service 侧能力注入索引容器（Service 构造时调用）。
func (s *Service) indexForService() {
	s.index = newSearchIndex(
		func() *slog.Logger { return s.rt.logger() },
		func() VolumeSet { return s.rt.volSet() },
		func(volName, owner string) *storage.Tenant { return s.rt.volumeTenant(volName, owner) },
		func(owner string) *storage.Tenant { return s.rt.tenantOf(owner) },
	)
}

// InvalidateIndex 使 owner 的文件索引失效（装配层旁路写——版本恢复等不经领域写路径的
// 落盘——后调用；下次搜索/列表全量重建）。暴露给装配层（pkg/server）使用。
func (s *Service) InvalidateIndex(owner string) {
	if s.index != nil {
		s.index.invalidate(owner)
	}
}
