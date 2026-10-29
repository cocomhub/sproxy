// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// mutation_index.go 把 `storage.Root` 的结构性变异事件接到**搜索索引**上
// （设计 2026-10-28-volume-state-maintenance §4.1 的本地卷落地，V3）。
//
// 为什么要这样做：#874 的根因是「写 user 桶但绕经写内核」的路径必须**记得**手动维护派生
// 状态（`RecordBypassWrite` / `IndexMove`）。现在结构性变异统一从 Root 观察 ⇒ 转存/归档/
// 跨卷搬移/备份恢复等旁路写自动进索引，**不需要新代码登记**。
//
// 边界：
//   - 只维护 `user/` 桶（meta/version/chunk/archive/cloud 等是内部桶，不进用户可见视图）；
//   - 索引未构建时本函数零成本（`searchIndex.upsert*` 内部即早退）——不因观察者而拖慢写路径；
//   - 内容写入（`OpenFile` 关闭）不发结构事件：size/checksum 由写内核维护，本文件只在
//     结构性变化（mkdir/rename/link/remove/chtimes）时刷新条目。
package files

import (
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// userBucket 是用户可见文件桶（索引只覆盖它）。
const userBucket = "user"

// ApplyRootMutation 消费一次 `storage.Root` 结构性变异（装配层注入为 Root 观察者）。
// 非 `user/` 桶、非法 owner、索引未构建时安全早退。
func (s *Service) ApplyRootMutation(m storage.Mutation) {
	if s == nil || s.index == nil {
		return
	}
	switch m.Op {
	case storage.MutationRename:
		if m.IsDir {
			s.applyDirRename(m)
			break
		}
		// 源与目标可能跨桶（如转存临时区 → user 桶）：两侧各自判定。
		s.applyMutationPath(m, m.Rel, mutationDropOnly)
		s.applyMutationPath(m, m.Rel2, mutationUpsert)
	case storage.MutationLink:
		s.applyMutationPath(m, m.Rel2, mutationUpsert)
	case storage.MutationWrite:
		// 内容写完成（卷 FS 写面显式声明）：按落盘真值刷新条目（size/mtime 会比结构性事件更新）。
		s.applyMutationPath(m, m.Rel, mutationUpsert)
	case storage.MutationRemove, storage.MutationRemoveAll:
		s.applyMutationPath(m, m.Rel, mutationDropOnly)
	default: // Mkdir / Chtimes：登记或刷新条目
		s.applyMutationPath(m, m.Rel, mutationUpsert)
	}
}

type mutationAction int

const (
	mutationUpsert mutationAction = iota
	mutationDropOnly
)

// applyDirRename 是**目录** rename 的投影：删源前缀 + 按真实磁盘重建目标子树
// （`storage.MutationRename` 只有单条语义，子条目必须由消费方递归重建）。
func (s *Service) applyDirRename(m storage.Mutation) {
	fromOwner, fromRel, ok := splitUserBucketPath(m.Rel)
	if !ok {
		return
	}
	toOwner, toRel, ok2 := splitUserBucketPath(m.Rel2)
	if !ok2 || toOwner != fromOwner {
		return
	}
	s.index.removePrefix(fromOwner, fromRel)
	s.index.rebuildSubtree(toOwner, toRel, m.Tag, m.Root, s.rt.tenantOf(toOwner))
}

// applyMutationPath 把一个卷根命名空间路径翻译为 (owner, user 相对路径) 后落到索引。
func (s *Service) applyMutationPath(m storage.Mutation, rel string, act mutationAction) {
	owner, userRel, ok := splitUserBucketPath(rel)
	if !ok {
		return
	}
	if act == mutationDropOnly {
		// 目录/子树删除必须按**前缀**清理（设计 2026-10-30 D1）：空目录走 `MutationRemove`
		// 且 IsDir=true；rename 目录同理。只删单条会残留子树条目（搜到已不存在文件）。
		if m.Op == storage.MutationRemoveAll || m.IsDir {
			s.index.removePrefix(owner, userRel)
			return
		}
		s.index.remove(owner, userRel)
		return
	}
	// 目录创建：无需 Stat（目录条目无 size/mtime）。
	if m.Op == storage.MutationMkdir {
		s.index.upsertDir(owner, userRel)
		return
	}
	entry, ok := mutationEntry(m, rel)
	if !ok {
		return
	}
	if entry.isDir {
		s.index.upsertDir(owner, userRel)
		return
	}
	s.index.upsert(owner, userRel, entry.size, entry.modTime, m.Tag, m.Root, rel)
}

// mutationEntry 从卷根 Stat 出条目属性（加密卷用 LogicalSize 取明文长度）。
type mutationEntryInfo struct {
	size    int64
	modTime int64
	isDir   bool
}

func mutationEntry(m storage.Mutation, rel string) (mutationEntryInfo, bool) {
	if m.Root == nil {
		return mutationEntryInfo{}, false
	}
	fi, err := m.Root.Stat(rel)
	if err != nil {
		// 目标可能已被后续操作删除（rename 连发）——尽力而为，索引由下次全量构建兜底。
		return mutationEntryInfo{}, false
	}
	info := mutationEntryInfo{modTime: fi.ModTime().UnixNano(), isDir: fi.IsDir()}
	if !info.isDir {
		info.size = fi.Size()
		if logical, lerr := m.Root.LogicalSize(rel); lerr == nil {
			info.size = logical // at-rest 加密卷：索引记明文长度（用户可见口径）
		}
	}
	return info, true
}

// splitUserBucketPath 把 `<owner>/user/<rel>` 拆成 (owner, rel)。
// 只接受恰好落在 user 桶下的路径；owner 段须是合法租户名。
func splitUserBucketPath(fullRel string) (owner, rel string, ok bool) {
	raw := strings.ReplaceAll(fullRel, "\\", "/")
	// 拒绝 `..` / `.` 段（纵深防御：rel 来自我方 Root，但归一化会静默吃掉越界段）。
	for seg := range strings.SplitSeq(raw, "/") {
		if seg == ".." || seg == "." {
			return "", "", false
		}
	}
	p := strings.TrimPrefix(path.Clean("/"+raw), "/")
	if p == "" {
		return "", "", false
	}
	owner, rest, found := strings.Cut(p, "/")
	if !found || !storage.ValidSegmentName(owner) {
		return "", "", false
	}
	rel, found = strings.CutPrefix(rest, userBucket+"/")
	if !found || rel == "" {
		return "", "", false
	}
	return owner, rel, true
}
