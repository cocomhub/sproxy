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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

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
	// contentTokens 是内容索引（roadmap P2 内容索引残余）：从文本文件
	// 首 4KiB 抽样抽取的小写词元（字母数字段）。空 = 未启用内容索引或
	// 非文本文件。search 命中这些词元也返回该文件。
	contentTokens []string
	// tags 是文件标签（roadmap 11.10-④）：由 POST /api/tags 打标，持久化在
	// <tenant meta>/tags/<sha256(rel)>.json（tagsStore），全量构建/写路径增量
	// 时合并进条目。search?tag= 精确匹配。空 = 未打标。
	tags []string
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
	// content 是内容索引开关（默认 false 零回归）：构建时抽样文本抽取词元。
	content bool
	// 集群同步（roadmap 11.11 方案 A-④）：sync nil = 单节点零回归。
	sync       IndexSync
	dirty      map[string]bool  // 写路径增量置脏（周期统一 Publish）
	applied    appliedRev       // 副本已应用 rev（防乱序覆盖）
	revCounter map[string]int64 // 主节点 per-owner rev（启动置 1）
	nodeIDFn   func() string    // 发布者标识（装配层注入）
}

// newSearchIndex 构造索引容器。logger/volSet/tenant/tenant0 是 Service 侧能力的注入
// （构建/失效时按当前装配状态取值，避免索引持有过期引用）。
func newSearchIndex(logger func() *slog.Logger, volSet func() VolumeSet,
	tenant func(volName, owner string) *storage.Tenant, tenant0 func(owner string) *storage.Tenant,
	content bool) *searchIndex {
	return &searchIndex{
		owners:     map[string]*ownerIndex{},
		built:      map[string]bool{},
		logger:     logger,
		volSet:     volSet,
		tenant:     tenant,
		tenant0:    tenant0,
		content:    content,
		applied:    appliedRev{rev: map[string]int64{}},
		revCounter: map[string]int64{},
	}
}

// ensureOwner 返回 owner 的索引快照，首次访问时优先载入持久化快照（免全量
// WalkDir——roadmap 2.3 P0 持久化增强）；无快照则全量构建并落盘。
// owner 租户不可用（非法/根不可用）时返回 nil（调用方按空结果处理，与旧语义一致）。
func (ix *searchIndex) ensureOwner(owner string) *ownerIndex {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if ix.built[owner] {
		return ix.owners[owner]
	}
	// 持久化快照载入（owner 租户经 tenant0 解析；损坏/缺失回退全量构建）。
	if tnt := ix.tenant0(owner); tnt != nil {
		if entries, ok := loadIndexSnapshot(tnt, owner); ok {
			oi := &ownerIndex{entries: entries}
			ix.owners[owner] = oi
			ix.built[owner] = true
			ix.logger().Debug("索引载入持久化快照", "owner", owner, "entries", len(entries))
			return oi
		}
	}
	// 全量构建：owner 视图逐卷 WalkDir user 桶。
	oi := ix.buildLocked(owner)
	ix.owners[owner] = oi
	ix.built[owner] = true
	// 落盘快照（失败仅日志——快照是加速层）。
	if tnt := ix.tenant0(owner); tnt != nil {
		if err := saveIndexSnapshot(tnt, owner, oi.entries); err != nil {
			ix.logger().Warn("索引快照落盘失败", "owner", owner, "error", err)
		}
	}
	return oi
}

// invalidate 使 owner 索引失效（装配层旁路写后调用；下次访问全量重建）。
// 同时删除持久化快照（防下次 ensureOwner 载入**过期快照**——失效语义要求全量重建）。
func (ix *searchIndex) invalidate(owner string) {
	ix.mu.Lock()
	delete(ix.built, owner)
	delete(ix.owners, owner)
	ix.mu.Unlock()
	if tnt := ix.tenant0(owner); tnt != nil {
		p := indexSnapshotPath(tnt, owner)
		if p != "" {
			_ = os.Remove(p)
		}
	}
}

// cloneOwnerIndexLocked 深拷贝 owner 索引（写路径 copy-on-write 用）：map 本身 + 每条
// entry 值拷贝。**必须深拷贝 entry**：rename 会改 entry 的 name/base 字段，若浅拷贝共享
// 指针，在途 reader 遍历旧 map 时读到的 entry 会被写路径改写（数据竞争）。深拷贝后
// 旧 map 的 entry 指针永不被写（immutable），reader 遍历旧 map 安全。
func cloneOwnerIndexLocked(oi *ownerIndex) *ownerIndex {
	if oi == nil {
		return nil
	}
	ne := make(map[string]*indexEntry, len(oi.entries))
	for k, e := range oi.entries {
		c := *e
		ne[k] = &c
	}
	return &ownerIndex{entries: ne}
}

// upsert 写路径增量：新增/覆盖一个文件条目，并顺带补父目录链（旧 WalkDir 语义：
// 新建文件到新目录后，该目录在搜索中作为目录条目可见）。
//
// 并发模型（审查 P1 修复）：**copy-on-write**——持锁深拷贝 entries → 修改副本 →
// 替换 ix.owners[owner] 指针。reader（searchLocked/list/saveAll）遍历的是替换前的
// 旧 map（immutable，永不被写），并发安全无 runtime fatal。
func (ix *searchIndex) upsert(owner, rel string, size, modTime int64, volume string, root *storage.Root, fullRel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return // 索引尚未构建（首次搜索会全量构建），无需增量
	}
	relKey := filepath.ToSlash(rel)
	newOI := cloneOwnerIndexLocked(oi)
	name := relKey
	var tokens []string
	if ix.content && root != nil {
		// 写路径内容索引：从已落盘文件抽样正文词元（与全量构建同源）。
		// fullRel 是含 user 桶前缀的租户根相对路径（OpenDecrypted 需要）。
		tokens = ix.sampleTokens(root, fullRel)
	}
	e := &indexEntry{
		name: name, base: filepath.Base(name),
		size: size, modTime: modTime, volume: volume, contentTokens: tokens,
	}
	// 覆盖写会重建条目：从 tagsStore 重新合并标签（打标不随内容覆盖丢失）。
	if ix.tenant0 != nil {
		if tnt := ix.tenant0(owner); tnt != nil {
			e.tags = loadTagsFromStore(tnt, name)
		}
	}
	newOI.entries[name] = e
	ix.ensureParentsLocked(newOI, name)
	ix.owners[owner] = newOI
	ix.markDirtyLocked(owner)
}

// upsertDir 写路径增量：mkdir 后登记目录条目（父目录链顺带补全）。
// 与 upsert 区别：只登记目录（不覆盖文件条目），目录条目 isDir=true（无 size/mtime/checksum）。
// 并发模型同 upsert：copy-on-write 替换指针。
func (ix *searchIndex) upsertDir(owner, rel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return // 索引尚未构建（首次搜索会全量构建），无需增量
	}
	newOI := cloneOwnerIndexLocked(oi)
	name := filepath.ToSlash(rel)
	if _, ok := newOI.entries[name]; !ok {
		newOI.entries[name] = &indexEntry{name: name, base: filepath.Base(name), isDir: true}
	}
	ix.ensureParentsLocked(newOI, name)
	ix.owners[owner] = newOI
	ix.markDirtyLocked(owner)
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
// 并发模型同 upsert：copy-on-write 替换指针。
func (ix *searchIndex) remove(owner, rel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	newOI := cloneOwnerIndexLocked(oi)
	delete(newOI.entries, filepath.ToSlash(rel))
	ix.owners[owner] = newOI
	ix.markDirtyLocked(owner)
}

// removePrefix 写路径增量：删除一个目录子树（rmdir）。
// 并发模型同 upsert：copy-on-write 替换指针。顺带删除子树内全部标签。
func (ix *searchIndex) removePrefix(owner, rel string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	newOI := cloneOwnerIndexLocked(oi)
	prefix := filepath.ToSlash(rel)
	var removed []string
	for k := range newOI.entries {
		if k == prefix || strings.HasPrefix(k, prefix+"/") {
			delete(newOI.entries, k)
			removed = append(removed, k)
		}
	}
	ix.owners[owner] = newOI
	ix.markDirtyLocked(owner)
	if tnt := ix.tenant0(owner); tnt != nil {
		for _, k := range removed {
			deleteTagsFromStore(tnt, k)
		}
	}
}

// rename 写路径增量：重命名/移动条目（from → to）。
// 并发模型同 upsert：copy-on-write 替换指针（深拷贝保证 entry 字段不被在途 reader 读改）。
func (ix *searchIndex) rename(owner, from, to string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return
	}
	newOI := cloneOwnerIndexLocked(oi)
	fromName := filepath.ToSlash(from)
	toName := filepath.ToSlash(to)
	tnt := ix.tenant0(owner)
	movedTags := func(oldKey, newKey string) {
		if tnt == nil {
			return
		}
		if tags := loadTagsFromStore(tnt, oldKey); len(tags) > 0 {
			_ = saveTagsToStore(tnt, newKey, tags)
			deleteTagsFromStore(tnt, oldKey)
		}
	}
	if e, ok := newOI.entries[fromName]; ok {
		delete(newOI.entries, fromName)
		e.name = toName
		e.base = filepath.Base(toName)
		newOI.entries[toName] = e
		ix.owners[owner] = newOI
		ix.markDirtyLocked(owner)
		movedTags(fromName, toName)
		return
	}
	// 目录子树重命名：前缀搬移。
	var moves []struct{ old, new string }
	for k := range newOI.entries {
		if strings.HasPrefix(k, fromName+"/") {
			moves = append(moves, struct{ old, new string }{k, toName + strings.TrimPrefix(k, fromName)})
		}
	}
	for _, m := range moves {
		e := newOI.entries[m.old]
		delete(newOI.entries, m.old)
		e.name = m.new
		e.base = filepath.Base(m.new)
		newOI.entries[m.new] = e
		movedTags(m.old, m.new)
	}
	ix.owners[owner] = newOI
}

// buildLocked 全量构建 owner 索引（调用方持 ix.mu）。owner 视图逐卷 WalkDir user 桶，
// 跳过在途临时文件（.inflight-*.part）；目录条目与文件条目分别登记（目录用独立 isDir
// 标记，搜索时目录条目只列一次）。owner 租户不可用返回空索引（非 nil，保证 nil 安全）。
// 构建后从 tagsStore 合并标签（meta/tags 由默认卷权威持有；见 walkUserRoot 的 tagTnt）。
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
		ix.walkUserRoot(baseTnt.Root(), baseTnt.UserRoot(), "", "", baseTnt, oi)
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
		ix.walkUserRoot(tnt.Root(), tnt.UserRoot(), v.Name, "", baseTnt, oi)
		_ = seen // 目录条目去重在 walkUserRoot 内经 oi.entries 判定
	}
	return oi
}

// walkUserRoot 递归遍历 user 桶，把文件/目录登记进 oi。
// volume 为空 = 单卷（旧装配）。dirRel 是相对 user 桶的路径前缀（"" = 根）。
// tagTnt 是默认卷权威租户（标签 store 落点；nil = 跳过标签合并，零回归）。
func (ix *searchIndex) walkUserRoot(root *storage.Root, userRoot, volume, dirRel string, tagTnt *storage.Tenant, oi *ownerIndex) {
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
			ix.walkUserRoot(root, userRoot, volume, child, tagTnt, oi)
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		tokens := ix.sampleTokens(root, userRoot+"/"+child)
		e := &indexEntry{
			name: key, base: name,
			size: info.Size(), modTime: info.ModTime().UnixNano(), volume: volume,
			contentTokens: tokens,
		}
		// 全量构建时从 tagsStore 合并标签（索引只是缓存，store 是权威）。
		if tagTnt != nil {
			e.tags = loadTagsFromStore(tagTnt, key)
		}
		oi.entries[key] = e
	}
}

// search 按 q（已小写）在 owner 索引中匹配，返回与旧 Search 逐字一致的 ListResult：
// 文件条目 name = 完整相对路径（ToSlash），带 size/mtime/checksum/volume；目录条目
// IsDir=true、name = 相对 user 桶路径、Volume 空。结果排序：先文件后目录？——**与旧语义
// 一致**：旧实现按 WalkDir 序遍历（先父后子、按目录深度优先），本实现按 name 字典序
// 稳定排序（可预测、与 List 的 name asc 排序观感一致；契约测试不锁序）。
func (ix *searchIndex) search(owner, qLower, tag string, csMap map[string]string) []FileInfo {
	// 先目录后文件、按 name 字典序：与 List 的 sortFileEntries(name asc) 对齐。
	out := ix.searchLocked(owner, qLower, tag, csMap)
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir // 目录在前
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// searchLocked 是 search 的底层实现（不含排序，供 list 复用目录/文件条目构造逻辑）。
// tag 非空时要求 entry.tags 精确含该标签（与 q 的 base/contentTokens 匹配 AND 组合）。
func (ix *searchIndex) searchLocked(owner, qLower, tag string, csMap map[string]string) []FileInfo {
	oi := ix.ensureOwner(owner)
	if oi == nil {
		return nil
	}
	out := make([]FileInfo, 0, 8) // 恒非 nil：空结果也是空切片（ListResult.Files 契约）
	for _, e := range oi.entries {
		// 标签过滤（roadmap 11.10-④）：tag 非空且 entry 不含该标签 → 跳过。
		// 精确匹配（设计文档：精确匹配 + q 仍走 base/contentTokens）。
		if tag != "" && !tagsContain(e.tags, tag) {
			continue
		}
		// q 非空时仍走既有 base/contentTokens 匹配；q 为空（仅按 tag 过滤）放行。
		if qLower != "" && !strings.Contains(strings.ToLower(e.base), qLower) {
			if !ix.content || !tokensContain(e.contentTokens, qLower) {
				continue
			}
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

// setTags 写路径增量：替换一个文件条目的标签（COW 替换指针，与 upsert 同并发模型）。
// 变异②：原地改 entry.tags（非 COW）→ 在途 reader 数据竞争（并发测试 -race 红）。
func (ix *searchIndex) setTags(owner, rel string, tags []string) {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	oi := ix.owners[owner]
	if oi == nil {
		return // 索引尚未构建（首次搜索会全量构建），无需增量
	}
	newOI := cloneOwnerIndexLocked(oi)
	key := filepath.ToSlash(rel)
	if e, ok := newOI.entries[key]; ok {
		// 替换切片引用（不改旧切片——clone 已深拷贝 entry，旧 map 的 entry 永不被写）。
		e.tags = tags
	}
	ix.owners[owner] = newOI
	ix.markDirtyLocked(owner)
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

// saveAll 保存全部已构建 owner 的快照（幂等：未构建的跳过；供装配层周期调用）。
// 返回保存的 owner 数（日志/测试断言用）。
func (ix *searchIndex) saveAll() int {
	ix.mu.Lock()
	owners := make([]string, 0, len(ix.built))
	for o := range ix.built {
		owners = append(owners, o)
	}
	// 快照语义（审查 P1 同族）：每个 owner 的 entries 在锁内**深拷贝快照**后再解锁写盘。
	// 此前解锁后直接遍历 oi.entries（map）——写路径 copy-on-write 替换指针后，旧指针
	// 虽不再被写，但**替换瞬间在途的 saveAll 已持有旧指针**，若写路径旧实现原地改会崩；
	// 现写路径已全改 copy-on-write，旧 map 永不变，但为防御未来回归仍取深拷贝快照。
	snapshots := make(map[string]*ownerIndex, len(owners))
	for _, o := range owners {
		if oi := ix.owners[o]; oi != nil {
			snapshots[o] = cloneOwnerIndexLocked(oi)
		}
	}
	ix.mu.Unlock()
	saved := 0
	for _, o := range owners {
		oi := snapshots[o]
		if oi == nil {
			continue
		}
		tnt := ix.tenant0(o)
		if tnt == nil {
			continue
		}
		if err := saveIndexSnapshot(tnt, o, oi.entries); err != nil {
			ix.logger().Warn("索引快照周期保存失败", "owner", o, "error", err)
			continue
		}
		saved++
	}
	return saved
}

// indexForService 把 Service 侧能力注入索引容器（Service 构造时调用）。
func (s *Service) indexForService() {
	s.index = newSearchIndex(
		func() *slog.Logger { return s.rt.logger() },
		func() VolumeSet { return s.rt.volSet() },
		func(volName, owner string) *storage.Tenant { return s.rt.volumeTenant(volName, owner) },
		func(owner string) *storage.Tenant { return s.rt.tenantOf(owner) },
		s.rt.contentIndexEnabled(),
	)
}

// InvalidateIndex 使 owner 的文件索引失效（装配层旁路写——版本恢复等不经领域写路径的
// 落盘——后调用；下次搜索/列表全量重建）。暴露给装配层（pkg/server）使用。
func (s *Service) InvalidateIndex(owner string) {
	if s.index != nil {
		s.index.invalidate(owner)
	}
}

// sampleTokens 内容索引抽样（roadmap P2）：content 开关关闭或文件 >4MiB 时返回 nil
// （大文件只索引头部采样——避免全量读入）。读取首 4KiB，抽取字母数字词元（小写，
// ≥2 字符）。失败（非文本/不可读）静默返回 nil。
func (ix *searchIndex) sampleTokens(root *storage.Root, rel string) []string {
	if !ix.content {
		return nil
	}
	rc, err := root.OpenDecrypted(rel)
	if err != nil {
		return nil
	}
	defer rc.Close()
	buf := make([]byte, 4<<10)
	n, _ := io.ReadFull(rc, buf)
	if n <= 0 {
		return nil
	}
	// 采样文本近似（二进制文件可能含任意字节——按字母数字段切分，全数字/过短段丢弃）。
	var toks []string
	cur := strings.Builder{}
	flush := func() {
		if cur.Len() >= 2 {
			toks = append(toks, strings.ToLower(cur.String()))
		}
		cur.Reset()
	}
	for _, b := range buf[:n] {
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') {
			cur.WriteByte(b)
		} else {
			flush()
		}
	}
	flush()
	return toks
}

// tokensContain 判断词元列表是否含 q 的子串（小写匹配）。
func tokensContain(tokens []string, qLower string) bool {
	for _, t := range tokens {
		if strings.Contains(t, qLower) {
			return true
		}
	}
	return false
}

// timeNow 是当前时间（独立函数便于测试注入/替换）。
func timeNow() time.Time { return time.Now() }
