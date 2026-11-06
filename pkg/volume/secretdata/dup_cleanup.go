// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// dup_cleanup.go —— **空落选容器的安全清理**（2026-11-07 用户要求）。
//
// 背景：重复容器组的「落选容器」经实测**全部只有目录 meta + 空子容器、无任何文件 meta**
// ⇒ 是「目录创建中断后重试」留下的**空容器树**，不含用户数据。清理它们可让卷的物理结构
// 与逻辑结构彻底一致。
//
// **安全约束（逐条强制）**：
//  1. 只处理重复容器组的 `others`（**权威容器绝不动**）；
//  2. 清理前逐容器校验**整棵子树无文件 meta**（`KindFileMeta`）—— 只要有一个文件就
//     **跳过并告警**（绝不为"清理"丢数据）；
//  3. 清理前把每个容器的**目录 meta 解密 JSON** 落盘留证（可追溯"删了什么"）；
//  4. **自底向上**删除（叶子先），避免非空目录删除失败；
//  5. 支持 `dryRun`（只报告不删）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// CleanupEmptyDuplicateContainers 清理**空**落选容器。
//
// evidenceDir 非空时把目录 meta 解密 JSON 写该目录留证（同 dup-export 的隔离结构）。
// 返回 (已清理容器数, 因含文件而跳过数, error)。
func (s *SecretdataFS) CleanupEmptyDuplicateContainers(ctx context.Context, evidenceDir string, dryRun bool) (removed, skipped int, err error) {
	groups := s.dupGroupsSnapshot()
	for _, g := range groups {
		for _, o := range g.others {
			did, err := s.cleanupOneEmptyContainer(ctx, g.rel, o, evidenceDir, dryRun)
			if err != nil {
				return removed, skipped, err
			}
			if did {
				removed++
			} else {
				skipped++
			}
		}
	}
	return removed, skipped, nil
}

// cleanupOneEmptyContainer 清理单个落选容器（**空**才删，否则跳过）。返回 (是否清理, error)。
func (s *SecretdataFS) cleanupOneEmptyContainer(ctx context.Context, groupRel, o, evidenceDir string, dryRun bool) (bool, error) {
	ok, reason, cerr := s.containerTreeIsEmpty(ctx, o)
	if cerr != nil {
		return false, cerr
	}
	if !ok {
		slog.Warn("secretdata: 落选容器含**文件**，跳过清理（保留数据，需人工处理）",
			"rel", groupRel, "container", o, "reason", reason)
		return false, nil
	}
	if !dryRun && evidenceDir != "" {
		if eerr := s.writeCleanupEvidence(ctx, groupRel, o, evidenceDir); eerr != nil {
			return false, eerr
		}
	}
	if dryRun {
		slog.Info("[dry-run] 将清理空落选容器", "rel", groupRel, "container", o)
		return true, nil
	}
	if derr := s.deleteContainerTree(ctx, o); derr != nil {
		return false, derr
	}
	slog.Warn("secretdata: 已清理空落选容器（无文件数据；目录 meta 已留证）",
		"rel", groupRel, "container", o)
	return true, nil
}

// containerTreeIsEmpty 校验整棵容器树内**无文件 meta**。返回 (是否为空, 原因, error)。
func (s *SecretdataFS) containerTreeIsEmpty(ctx context.Context, root string) (bool, string, error) {
	nodes, err := s.collectContainerTree(ctx, root)
	if err != nil {
		return false, "", err
	}
	for _, n := range nodes {
		ents, lerr := s.listInner(ctx, n.container)
		if lerr != nil {
			return false, "", lerr
		}
		for _, e := range ents {
			if !e.IsDir && shardseal.ClassifyName(e.Name) == shardseal.KindFileMeta {
				return false, fmt.Sprintf("容器 %s 含文件 meta %s", n.container, e.Name), nil
			}
		}
	}
	return true, "", nil
}

// deleteContainerTree 自底向上删除整棵容器树（叶子先 ⇒ 根目录删时空目录状态）。
func (s *SecretdataFS) deleteContainerTree(ctx context.Context, root string) error {
	nodes, err := s.collectContainerTree(ctx, root)
	if err != nil {
		return err
	}
	for _, node := range slices.Backward(nodes) {
		// **先清空容器内条目**（目录 meta / 空子容器），再删目录本身——否则目录非空删不掉
		// （实测：`The directory is not empty`）。
		ents, lerr := s.listInner(ctx, node.container)
		if lerr != nil {
			return fmt.Errorf("secretdata: 列举待删空容器 %q 失败: %w", node.container, lerr)
		}
		for _, e := range ents {
			if derr := s.inner.Delete(ctx, path.Join(node.container, e.Name)); derr != nil {
				return fmt.Errorf("secretdata: 删除空容器条目 %q 失败: %w", e.Name, derr)
			}
		}
		if derr := s.inner.Delete(ctx, node.container); derr != nil {
			return fmt.Errorf("secretdata: 删除空容器 %q 失败: %w", node.container, derr)
		}
	}
	return nil
}

// writeCleanupEvidence 把落选容器树各容器的**目录 meta 解密 JSON** 落盘留证。
// 尽力而为：单个容器 meta 读取失败只记日志（不阻断清理，留证不是正确性依赖）。
func (s *SecretdataFS) writeCleanupEvidence(ctx context.Context, groupRel, container, evidenceDir string) error {
	nodes, err := s.collectContainerTree(ctx, container)
	if err != nil {
		return err
	}
	base := filepath.Join(evidenceDir, sanitizeExportSegment(groupRel), sanitizeExportSegment(path.Base(container)))
	for _, n := range nodes {
		dm, blobLen, hasFiles, found, derr := s.scanContainerDirMeta(ctx, n.container)
		if derr != nil || !found || dm == nil {
			slog.Warn("secretdata: 清理留证跳过（目录 meta 不可读）", "container", n.container, "error", derr)
			continue
		}
		rec := map[string]any{
			"group_rel":         groupRel,
			"cleanup_container": container,
			"container":         n.container,
			"sub":               n.suffix,
			"dir_meta_blob_len": blobLen,
			"has_file_meta":     hasFiles,
			"dir_meta":          dm,
		}
		b, merr := json.MarshalIndent(rec, "", " ")
		if merr != nil {
			continue
		}
		out := filepath.Join(base, sanitizeExportSegment(n.suffix)+".json")
		if mkErr := os.MkdirAll(filepath.Dir(out), 0o755); mkErr != nil {
			return mkErr
		}
		if werr := os.WriteFile(out, b, 0o600); werr != nil {
			return werr
		}
	}
	return nil
}
