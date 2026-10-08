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
	"os"
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
// flattenRel 把 user/ 相对路径编码为 trash 顶层扁平条目名（**严格双射**）。
// B-C2/B-C3 修复：原 `/`→`_`、`_`→`__` 编码对「段首下划线」不可逆（`user/_a/b` 与
// `user/a_/b` 碰撞/错位 → 软删永久不可恢复）；且 flat 含字面 `_` 与 `.meta` 标记、
// 用户真实 `.meta` 文件名空间冲突。改用 base64 RawURLEncoding（字符集 A-Za-z0-9-_，
// 不含 `/` 与 `.`）：任何 rel 唯一映射、无 `_`/`.` 歧义、与 sidecar `.meta` 标记天然
// 隔离（base64 产物永不以 `.` 结尾；用户 rel 的 flat 也永不含 `.`）。
func flattenRel(rel string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(rel))
}

// unflattenRel 把扁平条目还原为原 rel（与 flattenRel 互逆；非法编码返回 ""）。
func unflattenRel(flat string) string {
	b, err := base64.RawURLEncoding.DecodeString(flat)
	if err != nil {
		return ""
	}
	return string(b)
}

// M2 修复：meta sidecar 随主文件一起移到 trash 桶（生命周期一致）——软删保留 meta
// 供恢复；trash 条目随清理一起删（无孤儿）；同 rel 新文件不受影响（其 meta 是新的，
// 不在此路径移动）。
func (s *Service) softDeleteToTrash(ctx context.Context, root *storage.Root, quarRel, rel string, info os.FileInfo) (string, error) {
	// trash 桶相对路径：trash/<rel 扁平编码>.__deleted__<nano>（顶层扁平，防嵌套目录）。
	flatRel := flattenRel(rel)
	trashRel := trashPrefix + flatRel + trashDeletedSuffix + strconv.FormatInt(time.Now().UnixNano(), 10)
	// trash 桶顶层目录确保存在（rename 目标目录必须已建）。
	if err := root.MkdirAll("trash", 0o755); err != nil {
		return "", fmt.Errorf("trash 目录: %w", err)
	}
	if err := atomicRenameRoot(root, quarRel, trashRel); err != nil {
		return "", fmt.Errorf("trash rename: %w", err)
	}
	// meta sidecar 随迁（best-effort：源 meta 不存在则跳过——读路径直算兜底；恢复时
	// 随主文件一起回 meta 桶）。meta 条目与主文件用同一 flat 前缀（+.meta 标记）。
	_ = atomicRenameRoot(root, meta.MetaPath(rel), trashPrefix+flatRel+trashMetaMarker+trashDeletedSuffix+strconv.FormatInt(time.Now().UnixNano(), 10))
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
	before, _, ok := strings.Cut(inner, trashDeletedSuffix)
	if !ok {
		return &HTTPError{Status: 400, Message: "无效的回收站条目"}
	}
	flat := before // 形如 user_a__b_c.txt（_ 分隔原 rel 段；`__` 是文件名中的 `_` 转义）
	// flat → 原 rel（可逆解码：`__`→`_`、单 `_`→`/`——D-C1 修复：文件名含 `_` 的 rel
	// 不再被还原成多级目录）。
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
	if mt := restoreTrashMetaSuffix(root, flat); mt != "" {
		_ = atomicRenameRoot(root, trashPrefix+flat+trashMetaMarker+trashDeletedSuffix+mt, meta.MetaPath(origRel))
	}
	return nil
}

// restoreTrashMetaSuffix 定位 trash 桶中对应 flat 的 meta 条目（软删随迁命名
// trash/<flat>.meta.__deleted__<nano>）并返回其删除后缀段。找不到返回空（读路径直算兜底）。
// 实现：遍历 trash 桶顶层，匹配前缀 <flat>.meta.__deleted__。
func restoreTrashMetaSuffix(root *storage.Root, flat string) string {
	trashAbs, ok := root.Abs("trash")
	if !ok {
		return ""
	}
	entries, err := os.ReadDir(trashAbs)
	if err != nil {
		return ""
	}
	prefix := flat + trashMetaMarker + trashDeletedSuffix
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, flat+trashMetaMarker+trashDeletedSuffix)
		}
	}
	return ""
}

// EmptyTrash 清空回收站（删除全部 trash 桶文件）。
// B-MAJOR-3 修复：删除条目时按**原 rel** 释放配额——软删时主文件/meta 配额保留
// （可恢复），清空即永久删除，磁盘字节释放但配额键仍在原 user/meta 路径 → 必须
// ReleaseUsage 对称（否则 owner Scope 永久虚高直至重启）。
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
		rel := filepath.Join(trashAbs, e.Name())
		info, ierr := os.Stat(rel)
		if ierr == nil {
			// B-C1 修复：传含 trash/ 前缀的完整条目路径（releaseTrashEntryQuota 的
			// TrimPrefix 契约）——裸 e.Name() 会让 guard 恒早退（配额释放死代码）。
			s.releaseTrashEntryQuota(owner, root, trashPrefix+filepath.ToSlash(e.Name()), info.Size())
		}
		_ = root.Remove(trashPrefix + filepath.ToSlash(e.Name()))
	}
	return nil
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
			// B-MAJOR-3：清空即永久删除——先释放原 rel 配额（与 EmptyTrash 对称）。
			// B-C1 修复：传含 trash/ 前缀完整条目路径（TrimPrefix 契约）。
			s.releaseTrashEntryQuota(owner, root, trashPrefix+filepath.ToSlash(e.Name()), info.Size())
			if root.Remove(trashPrefix+filepath.ToSlash(e.Name())) == nil {
				cleaned++
			}
		}
	}
	return cleaned, nil
}
