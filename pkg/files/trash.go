// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// trash.go 是回收站/软删除（roadmap P2 回收站）：
//
//   - DeleteFile SoftDelete=true：checksum 校验成功后把 quarantine 重命名到 trash 桶
//     （FeatureRel("trash", rel+".__deleted__<nano>")，文件名保留原 rel 供恢复解析）。
//   - RestoreTrash / EmptyTrash / CleanupTrash：恢复 / 清空 / TTL 清理（保留期）。
//   - 语义：软删不释放配额（防误删后可恢复）；恢复回 user/ 原路径。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/storage"
)

// trashDefaultTTL 是回收站默认保留期（7d）。
const trashDefaultTTL = 7 * 24 * time.Hour

// trashDeletedSuffix 是 trash 桶文件名的删除标记（恢复时解析原 rel）。
const trashDeletedSuffix = ".__deleted__"

// TrashDeletedSuffix 导出删除标记（server 侧列表解析用）。
func TrashDeletedSuffix() string { return trashDeletedSuffix }

// SoftDelete 软删：校验成功后把 quarantine 移到 trash 桶（非 Remove）。
// 在 DeleteFile 的 checksum 匹配分支调用；返回 (trashRel, err)。
func (s *Service) softDeleteToTrash(ctx context.Context, root *storage.Root, quarRel, rel string, info os.FileInfo) (string, error) {
	// trash 桶相对路径：trash/<rel 斜杠转点>.__deleted__<nano>（顶层扁平，防嵌套目录）。
	flatRel := strings.ReplaceAll(rel, "/", "_")
	trashRel := "trash/" + flatRel + trashDeletedSuffix + strconv.FormatInt(time.Now().UnixNano(), 10)
	// trash 桶顶层目录确保存在（rename 目标目录必须已建）。
	if err := root.MkdirAll("trash", 0o755); err != nil {
		return "", fmt.Errorf("trash 目录: %w", err)
	}
	if err := atomicRenameRoot(root, quarRel, trashRel); err != nil {
		return "", fmt.Errorf("trash rename: %w", err)
	}
	_ = info // 保留（未来恢复时用大小）
	return trashRel, nil
}

// RestoreTrash 恢复回收站文件回 user/ 原路径（trashRel 形如 trash/<orig>.__deleted__<nano>）。
func (s *Service) RestoreTrash(ctx context.Context, owner, trashRel string) error {
	owner = normalizeOwner(owner)
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		return &HTTPError{Status: 400, Message: "无效的路径"}
	}
	root := tnt.Root()
	// 解析原 rel：去掉 trash/ 前缀 + __deleted__ 后缀。
	if !strings.HasPrefix(trashRel, "trash/") {
		return &HTTPError{Status: 400, Message: "无效的回收站路径"}
	}
	inner := strings.TrimPrefix(trashRel, "trash/")
	before, _, ok := strings.Cut(inner, trashDeletedSuffix)
	if !ok {
		return &HTTPError{Status: 400, Message: "无效的回收站条目"}
	}
	flat := before // 形如 user_a.txt（_ 分隔原 rel 段）
	// flat → 原 rel（下划线恢复为斜杠；只转文件名字符，原 rel 的 / 全转 _）。
	origRel := strings.ReplaceAll(flat, "_", "/")
	if !strings.HasPrefix(origRel, "user/") {
		return &HTTPError{Status: 400, Message: "无效的回收站条目"}
	}
	// 目标已存在 → 409。
	if _, err := root.Stat(origRel); err == nil {
		return &HTTPError{Status: 409, Message: "目标文件已存在"}
	}
	// 父目录确保存在。
	if dir := filepath.ToSlash(filepath.Dir(origRel)); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("恢复目录: %w", err)
		}
	}
	if err := atomicRenameRoot(root, trashRel, origRel); err != nil {
		if os.IsNotExist(err) {
			return &HTTPError{Status: 404, Message: "回收站条目不存在"}
		}
		return fmt.Errorf("恢复失败: %w", err)
	}
	return nil
}

// EmptyTrash 清空回收站（删除全部 trash 桶文件）。
func (s *Service) EmptyTrash(ctx context.Context, owner string) error {
	owner = normalizeOwner(owner)
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		return &HTTPError{Status: 400, Message: "无效的路径"}
	}
	root := tnt.Root()
	trashAbs, ok := root.Abs("trash")
	if !ok {
		return nil
	}
	entries, err := os.ReadDir(trashAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		_ = root.Remove("trash/" + filepath.ToSlash(e.Name()))
	}
	return nil
}

// CleanupTrash 清理过期回收站条目（保留期 ttl；0 = 立即全部清理）。
// 返回清理条数。按 mtime 判定（软删时的 mtime 由 rename 保持）。
func (s *Service) CleanupTrash(ctx context.Context, owner string, ttl time.Duration) (int, error) {
	owner = normalizeOwner(owner)
	tnt := s.rt.tenantOf(owner)
	if tnt == nil || tnt.Root() == nil {
		return 0, nil
	}
	root := tnt.Root()
	trashAbs, ok := root.Abs("trash")
	if !ok {
		return 0, nil
	}
	entries, err := os.ReadDir(trashAbs)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	now := time.Now()
	cleaned := 0
	for _, e := range entries {
		full := filepath.Join(trashAbs, e.Name())
		info, serr := os.Stat(full)
		if serr != nil {
			continue
		}
		if ttl <= 0 || now.Sub(info.ModTime()) > ttl {
			if rerr := root.Remove("trash/" + filepath.ToSlash(e.Name())); rerr == nil {
				cleaned++
			}
		}
	}
	return cleaned, nil
}
