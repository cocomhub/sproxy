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
	"encoding/base64"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// trashDefaultTTL 是回收站默认保留期（7d）。
const trashDefaultTTL = 7 * 24 * time.Hour

// trashDeletedSuffix 是 trash 桶文件名的删除标记（恢复时解析原 rel）。
const trashDeletedSuffix = ".__deleted__"

// TrashDeletedSuffix 导出删除标记（server 侧列表解析用）。
func TrashDeletedSuffix() string { return trashDeletedSuffix }

// UnflattenTrashRel 导出扁平条目 → 原 rel 的解码（server 侧列表展示用；与
// RestoreTrash 同解码——B-m3 修复：列表名不得用裸 ReplaceAll，否则含 `_` 的
// 文件名展示错位/歧义）。
func UnflattenTrashRel(flat string) string { return unflattenRel(flat) }

// trashMetaMarker 是 trash 桶内 meta sidecar 条目的文件名标记（软删时随主文件入 trash，
// 供恢复/清理识别——EmptyTrash/CleanupTrash 逐条目删除自然一并清理，无孤儿）。
const trashMetaMarker = ".meta"

// TrashMetaMarker 导出 trash 桶内 meta sidecar 条目标记（server 侧列表解析过滤用）。
func TrashMetaMarker() string { return trashMetaMarker }

// softDeleteToTrash 软删：校验成功后把 quarantine 移到 trash 桶（非 Remove）。
// 在 DeleteFile 的 checksum 匹配分支调用；返回 (trashRel, err)。
// flattenRel / unflattenRel 是 trash 桶条目原 rel 的可逆编解码（D-C1 修复）：
// 顶层扁平命名需把 `/` 转单字符，但 `strings.ReplaceAll(rel,"/","_")` 对文件名含 `_`
// 的 rel（`user/a_b/c.txt`）双射破坏——恢复时 `_`→`/` 会把 a_b 还原成 a/b 两级目录
// （数据错位/409 永久丢失）。可逆规则：`/` → `_`；`_` → `__`（转义）。解码先合并
// `__`→`_` 再单 `_`→`/`，与编码互逆。
// flattenRel 把 user/ 相对路径编码为 trash 内条目路径（用户裁定 2026-10-09：
// **目录镜像 + 文件名编码**）：
//   - 目录段**保留原文**（`trash/user/dir1/dir2/` 镜像 user 树——目录名本来合法、
//     天然不超 NAME_MAX，且恢复时直接拼接、可读）；
//   - 仅**文件名 base64**（字符集 A-Za-z0-9-_，无 `/` 与 `.`——防与 sidecar `.meta`
//     标记、`__deleted__` 后缀、用户真实 `.meta` 文件名冲突）。
//
// C-MAJOR-2 修复：原整条 rel 单文件 base64 深路径（100 段×5 字 → ~670 字符）超
// NAME_MAX(255) → ENAMETOOLONG 软删半途失败。目录镜像后单段恒 ≤255（文件名本身
// 受 ValidSegmentName 约束），无超长风险。
// 例：user/a/b.txt → `user/a/<base64(b.txt)>`。
func flattenRel(rel string) string {
	dir, file := path.Split(rel)
	return strings.TrimSuffix(dir, "/") + "/" + base64.RawURLEncoding.EncodeToString([]byte(file))
}

// unflattenRel 把 trash 条目路径还原为原 rel（与 flattenRel 互逆；文件名段非法编码
// 返回 ""）。目录段原样拼接、仅最后一段 base64 解码。
// E-MAJOR 修复：base64 解码失败（**存量旧编码条目**——升级前用 `_` 扁平时代
// `user_a_b.txt` 形态）回退旧 `_`→`/` 解码——否则升级后旧 trash 条目永久无法恢复 +
// 配额永不释放（EmptyTrash 删磁盘字节后原 rel 配额虚高假 507）。
func unflattenRel(flat string) string {
	dir, file := path.Split(flat)
	b, err := base64.RawURLEncoding.DecodeString(file)
	if err == nil {
		return strings.TrimSuffix(dir, "/") + "/" + string(b)
	}
	// 回退旧编码：`_` 分隔的扁平 rel（`user_a_b.txt` → `user/a/b.txt`；仅文件名段解码）。
	legacy := strings.ReplaceAll(strings.TrimSuffix(dir, "/")+"/"+file, "_", "/")
	legacy = strings.TrimPrefix(legacy, "/")
	if !storage.ValidSegmentName(path.Base(legacy)) {
		return "" // 回退名也非法（非 trash 条目形态）
	}
	return legacy
}

// M2 修复：meta sidecar 随主文件一起移到 trash 桶（生命周期一致）——软删保留 meta
// 供恢复；trash 条目随清理一起删（无孤儿）；同 rel 新文件不受影响（其 meta 是新的，
// 不在此路径移动）。
func (s *Service) softDeleteToTrash(ctx context.Context, root *storage.Root, quarRel, rel string, info os.FileInfo) (string, error) {
	// trash 桶相对路径：trash/<分层扁平>.__deleted__<nano>（C-MAJOR-2：逐段 base64 保留
	// 目录层级，单文件名不超 NAME_MAX——整条 rel 编码深路径会 ENAMETOOLONG）。
	flatRel := flattenRel(rel)
	nano := strconv.FormatInt(time.Now().UnixNano(), 10)
	trashRel := trashPrefix + flatRel + trashDeletedSuffix + nano
	// trash 条目父目录逐级创建（分层树：trash/<dir1>/<dir2>/...——rename 目标父目录必须已建）。
	if dir := filepath.ToSlash(filepath.Dir(trashRel)); dir != "." {
		if err := root.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("trash 目录: %w", err)
		}
	}
	if err := atomicRenameRoot(root, quarRel, trashRel); err != nil {
		return "", fmt.Errorf("trash rename: %w", err)
	}
	// meta sidecar 随迁（best-effort：源 meta 不存在则跳过——读路径直算兜底；恢复时
	// 随主文件一起回 meta 桶）。meta 条目与主文件同 flat 目录（+.meta 标记）。
	// **同代 nano**：主文件与 meta 共享同一 nano，恢复时按同代精确配对（P1：此前各
	// 自 time.Now() → 多代软删后恢复取到最旧一代 meta → 主文件/meta 错代不可读）。
	_ = atomicRenameRoot(root, meta.MetaPath(rel), trashPrefix+flatRel+trashMetaMarker+trashDeletedSuffix+nano)
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
	if !strings.HasPrefix(trashRel, trashPrefix) {
		return &HTTPError{Status: 400, Message: "无效的回收站路径"}
	}
	inner := strings.TrimPrefix(trashRel, trashPrefix)
	before, nano, ok := strings.Cut(inner, trashDeletedSuffix)
	if !ok {
		return &HTTPError{Status: 400, Message: "无效的回收站条目"}
	}
	flat := before // 分层扁平（逐段 base64，含 user 段；C-MAJOR-2：保留目录层级防单名超长）
	// flat → 原 rel（逐段可逆解码）。
	origRel := unflattenRel(flat)
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
	// M2 修复：meta sidecar 随主文件一起恢复（软删时已随迁 trash 桶；这里回 meta 桶）。
	// 按**同代 nano** 精确配对（P1：多代软删后不得取到最旧一代 meta）。
	if mt := restoreTrashMetaSuffix(root, flat, nano); mt != "" {
		_ = atomicRenameRoot(root, trashPrefix+flat+trashMetaMarker+trashDeletedSuffix+mt, meta.MetaPath(origRel))
	}
	return nil
}

// restoreTrashMetaSuffix 定位 trash 桶中对应 flat 的 meta 条目并返回其删除后缀段。
// **优先精确匹配同代 nano**（主文件与 meta 软删时共享同一 nano）——多代软删（同 rel 多次
// 删除/重传）时旧实现取「前缀第一个」（最旧）会致主文件/meta 错代不可读（P1）；精确匹配
// 失败时回落首个前缀命中（兼容修复前旧条目，读路径仍可退化兜底）。找不到返回空。
func restoreTrashMetaSuffix(root *storage.Root, flat, nano string) string {
	// flat 可能是 user/<...>/<base>（分层）——meta 条目与主文件同目录，basename 前加 .meta。
	base := path.Base(flat)
	dir := path.Dir(flat)
	if dir == "." || dir == "" {
		dir = "trash"
	} else {
		dir = "trash/" + dir
	}
	trashAbs, ok := root.Abs(dir)
	if !ok {
		return ""
	}
	entries, err := os.ReadDir(trashAbs)
	if err != nil {
		return ""
	}
	prefix := base + trashMetaMarker + trashDeletedSuffix
	want := prefix + nano
	fallback := ""
	for _, e := range entries {
		name := e.Name()
		if name == want {
			return nano
		}
		if fallback == "" && strings.HasPrefix(name, prefix) {
			fallback = strings.TrimPrefix(name, prefix)
		}
	}
	return fallback
}

// EmptyTrash 清空回收站（删除全部 trash 桶文件）。
// B-MAJOR-3 修复：删除条目时按**原 rel** 释放配额——软删时主文件/meta 配额保留
// （可恢复），清空即永久删除，磁盘字节释放但配额键仍在原 user/meta 路径 → 必须
// ReleaseUsage 对称（否则 owner Scope 永久虚高直至重启）。
// C-MAJOR-2：trash 条目为**分层树**（逐段 base64 保留目录层级）——递归遍历清理。
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
	// 递归清理（分层树：目录先递归删内容，文件删除条目 + 释放配额）。
	err := filepath.WalkDir(trashAbs, func(absPath string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil // 目录内容由子条目处理，目录自身由 RemoveAll 收尾
		}
		entryRel := trashEntryRel(trashAbs, absPath)
		info, ierr := os.Stat(absPath)
		if ierr == nil {
			s.releaseTrashEntryQuota(owner, root, entryRel, info.Size())
		}
		_ = root.Remove(entryRel)
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// 删除后清空可能残留的空目录（分层树剪枝）。
	_ = os.RemoveAll(trashAbs)
	return nil
}

// trashEntryRel 把 WalkDir 的绝对路径映射为「含 trash/ 前缀的完整条目相对路径」
// （releaseTrashEntryQuota 的 TrimPrefix 契约）。
func trashEntryRel(trashAbs, absPath string) string {
	rel, err := filepath.Rel(trashAbs, absPath)
	if err != nil {
		return trashPrefix + filepath.ToSlash(filepath.Base(absPath))
	}
	return trashPrefix + filepath.ToSlash(rel)
}

// releaseTrashEntryQuota 按 trash 条目名还原原 rel 并释放对应配额（B-MAJOR-3）：
// 主条目 `trash/<flat>.__deleted__` → 原 rel=user/...（ReleaseUsage user 桶）；
// meta 条目 `trash/<flat>.meta.__deleted__` → meta/<origRel>.meta（ReleaseUsage meta 桶）。
// 解析失败（flat 编码异常）跳过——配额由 reconcile 自愈。
// B-C1 修复：入参 entryName 是**含 trash/ 前缀的完整条目路径**（与软删命名一致）——
// 原实现传裸 e.Name() 与 guard HasPrefix("trash/") 不匹配 → 恒早退（配额释放死代码）。
// 调用方（EmptyTrash/CleanupTrash）传 `trashPrefix + e.Name()`。
func (s *Service) releaseTrashEntryQuota(owner string, root *storage.Root, entryName string, size int64) {
	if size <= 0 {
		return
	}
	inner := strings.TrimPrefix(entryName, trashPrefix)
	before, _, ok := strings.Cut(inner, trashDeletedSuffix)
	if !ok {
		return
	}
	// meta 条目（软删随迁命名 <flat>.meta.__deleted__）→ 释放 meta/<origRel>.meta。
	if strings.HasSuffix(before, trashMetaMarker) {
		flat := strings.TrimSuffix(before, trashMetaMarker)
		origRel := unflattenRel(flat)
		mrel := meta.MetaPath(origRel)
		if scope := s.rt.quotaScope(owner, mrel); scope != nil {
			scope.ReleaseUsage(size)
		}
		return
	}
	// 主条目 → 释放 user 桶原 rel。
	origRel := unflattenRel(before)
	if scope := s.rt.quotaScope(owner, origRel); scope != nil {
		scope.ReleaseUsage(size)
	}
}

// CleanupTrash 清理过期回收站条目（保留期 ttl；0 = 立即全部清理）。
// 返回清理条数。按 mtime 判定（软删时的 mtime 由 rename 保持）。
// C-MAJOR-2：trash 条目为**分层树**——递归遍历，过期条目（文件）删除 + 释放配额。
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
	now := time.Now()
	cleaned := 0
	err := filepath.WalkDir(trashAbs, s.cleanupTrashWalk(owner, root, trashAbs, ttl, now, &cleaned))
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return cleaned, nil
}

// cleanupTrashWalk 是 CleanupTrash 的 WalkDir 回调（S107 拆分降 gocognit）：过期文件
// 删除 + 释放原 rel 配额（与 EmptyTrash 对称）；目录跳过（内容由子条目处理）。
func (s *Service) cleanupTrashWalk(owner string, root *storage.Root, trashAbs string, ttl time.Duration, now time.Time, cleaned *int) fs.WalkDirFunc {
	return func(absPath string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, serr := os.Stat(absPath)
		if serr != nil {
			return nil
		}
		if ttl > 0 && now.Sub(info.ModTime()) <= ttl {
			return nil // 未过期
		}
		entryRel := trashEntryRel(trashAbs, absPath)
		s.releaseTrashEntryQuota(owner, root, entryRel, info.Size())
		if root.Remove(entryRel) == nil {
			*cleaned++
		}
		return nil
	}
}
