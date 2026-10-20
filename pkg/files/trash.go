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
	"github.com/cocomhub/sproxy/pkg/volume"
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
	// meta sidecar 随迁（best-effort：源 meta 不存在则跳过——读侧不校验；恢复时
	// 随主文件一起回 meta 桶）。meta 条目与主文件同 flat 目录（+.meta 标记）。
	// **同代 nano**：主文件与 meta 共享同一 nano，恢复时按同代精确配对（P1：此前各
	// 自 time.Now() → 多代软删后恢复取到最旧一代 meta → 主文件/meta 错代不可读）。
	_ = atomicRenameRoot(root, meta.MetaPath(rel), trashPrefix+flatRel+trashMetaMarker+trashDeletedSuffix+nano)
	_ = info // 保留（未来恢复时用大小）
	return trashRel, nil
}

// RestoreTrash 恢复回收站文件回 user/ 原路径（trashRel 形如 trash/<orig>.__deleted__<nano>）。
func (s *Service) RestoreTrash(ctx context.Context, owner, trashRel string) error {
	return s.RestoreTrashIn(ctx, owner, "", trashRel)
}

// TrashVolumeRoot 是回收站操作的**单卷视图**（卷名 + 该 owner 的租户根）。
type TrashVolumeRoot struct {
	Volume string
	Root   *storage.Root
}

// TrashRoots 返回回收站操作应遍历的卷视图（用户裁定 2026-10-19 ③：**各卷独立 + 汇总展示**）：
//
//   - volume != "" ⇒ 只该卷（须为**本地根形态**且 owner 可访问；否则明确报错，不静默回落）；
//   - volume == "" ⇒ **汇总**：owner 允许的**全部本地卷**（默认卷在前）。
//
// 为什么必须遍历各卷：软删落**文件所在卷**的 trash（`softDeleteToTrash(root=<文件所在卷>)`），
// 而此前列表/恢复/清空只认默认卷 ⇒ 其它卷（user 卷/外部卷）的条目**不可见、不可恢复**（记档缺陷）。
func (s *Service) TrashRoots(owner, volumeName string) ([]TrashVolumeRoot, error) {
	owner = normalizeOwner(owner)
	if volumeName != "" {
		return s.trashRootForVolume(owner, volumeName)
	}
	return s.trashRootsAll(owner)
}

// trashRootForVolume 解析**指定卷**的回收站视图（须本地根形态且 owner 可访问；否则明确报错，
// 不静默回落默认卷——回落会让调用方以为操作的是目标卷）。
func (s *Service) trashRootForVolume(owner, volumeName string) ([]TrashVolumeRoot, error) {
	vs := s.rt.volSet()
	if vs != nil && !vs.HasLocalRoot(volumeName) {
		return nil, &HTTPError{Status: 400, Message: "该卷不支持回收站（非本地卷）"}
	}
	tnt := s.rt.volumeTenant(volumeName, owner)
	if tnt == nil || tnt.Root() == nil {
		return nil, &HTTPError{Status: 404, Message: "卷不存在或无权访问"}
	}
	return []TrashVolumeRoot{{Volume: volumeName, Root: tnt.Root()}}, nil
}

// trashRootsAll 解析**汇总**视图：默认卷在前（单卷客户端看到的顺序不变）+ owner 允许的其余
// 本地卷（按声明序去重）。
func (s *Service) trashRootsAll(owner string) ([]TrashVolumeRoot, error) {
	vs := s.rt.volSet()
	var out []TrashVolumeRoot
	seen := map[string]bool{}
	add := func(name string, root *storage.Root) {
		if root == nil || seen[name] {
			return
		}
		seen[name] = true
		out = append(out, TrashVolumeRoot{Volume: name, Root: root})
	}
	if tnt := s.rt.tenantOf(owner); tnt != nil && tnt.Root() != nil {
		add(volumeNameForTenant(vs, tnt), tnt.Root())
	}
	if vs != nil {
		for _, v := range volume.AllowedVolumes(vs.All(), owner) {
			if !vs.HasLocalRoot(v.Name) {
				continue // 外部/封装卷无本地根 ⇒ 无 trash 桶可遍历
			}
			if tnt := s.rt.volumeTenant(v.Name, owner); tnt != nil {
				add(v.Name, tnt.Root())
			}
		}
	}
	if len(out) == 0 {
		return nil, &HTTPError{Status: 400, Message: "无效的路径"}
	}
	return out, nil
}

// RestoreTrashIn 按卷恢复回收站条目：volume != "" ⇒ 只在该卷；volume == "" ⇒ **跨卷定位**
// （条目只存在于软删时所在的卷）。
//
// 定位判据是**先存在性探测**（`trashEntryExists`）而非「尝试后按错误分类」：底层
// `Root.AtomicRename` 的「源不存在」会被其**重试层包装**成不可用 `os.IsNotExist` 分类的形态
// （实测文案「重命名失败（已达最大重试次数 5）: … cannot find the file specified」）⇒ 依赖错误
// 分类会把「该卷没有此条目」误判为真实故障（500）。存在性探测与底层错误措辞解耦，确定且可诊断。
func (s *Service) RestoreTrashIn(ctx context.Context, owner, volumeName, trashRel string) error {
	roots, rerr := s.TrashRoots(owner, volumeName)
	if rerr != nil {
		return rerr
	}
	for _, vr := range roots {
		if !s.trashEntryExists(vr.Root, trashRel) {
			continue
		}
		if err := s.restoreTrashAt(ctx, vr.Root, trashRel); err != nil {
			s.rt.recordFileAudit(ctx, "trash_restore", trashRel, auditResultError, "volume="+vr.Volume)
			return err
		}
		s.rt.recordFileAudit(ctx, "trash_restore", trashRel, auditResultSuccess, "volume="+vr.Volume)
		return nil
	}
	return &HTTPError{Status: 404, Message: "回收站条目不存在"}
}

// trashEntryExists 报告该卷的 trash 桶里是否存在该条目（跨卷定位判据，见 RestoreTrashIn）。
func (s *Service) trashEntryExists(root *storage.Root, trashRel string) bool {
	if root == nil || trashRel == "" {
		return false
	}
	_, err := root.Stat(trashRel)
	return err == nil
}

// restoreTrashAt 在给定卷根上恢复条目（原 RestoreTrash 主体）。
func (s *Service) restoreTrashAt(ctx context.Context, root *storage.Root, trashRel string) error {
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
// **只接受同代 nano 精确匹配**（主文件与 meta 软删时共享同一 nano）。
//
// 不要回落「首个前缀命中」：条目名不区分「sidecar」与「名字以 .meta 结尾的用户文件」——
// `user/x` 的 sidecar 软删为 `x.meta.__deleted__<n1>`，而用户文件 `user/x.meta` 软删为
// `x.meta.__deleted__<n3>`，**两者共享同一前缀**；回落会把另一个文件的回收站条目搬进 meta
// 桶（该文件永久不可恢复，数据丢失），或搬入上一代 sidecar（恢复后文件哈希失配 → 下载
// 恒 fail-closed）。精确未命中就不搬 meta（等价于「无 meta」，读路径直通，安全）。
func restoreTrashMetaSuffix(root *storage.Root, flat, nano string) string {
	if nano == "" {
		return ""
	}
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
	want := base + trashMetaMarker + trashDeletedSuffix + nano
	for _, e := range entries {
		if e.Name() == want {
			return nano
		}
	}
	return ""
}

// EmptyTrash 清空回收站（删除全部 trash 桶文件）。
// B-MAJOR-3 修复：删除条目时按**原 rel** 释放配额——软删时主文件/meta 配额保留
// （可恢复），清空即永久删除，磁盘字节释放但配额键仍在原 user/meta 路径 → 必须
// ReleaseUsage 对称（否则 owner Scope 永久虚高直至重启）。
// C-MAJOR-2：trash 条目为**分层树**（逐段 base64 保留目录层级）——递归遍历清理。
func (s *Service) EmptyTrash(ctx context.Context, owner string) error {
	return s.EmptyTrashIn(ctx, owner, "", false)
}

// EmptyTrashIn 清空回收站：volume != "" ⇒ 只清该卷；all ⇒ **汇总清空全部卷**；
// 二者皆无 ⇒ 只清**默认卷**（既有语义，零回归——破坏性默认不静默扩大）。
func (s *Service) EmptyTrashIn(ctx context.Context, owner, volumeName string, all bool) error {
	if volumeName == "" && !all {
		tnt := s.rt.tenantOf(normalizeOwner(owner))
		if tnt == nil || tnt.Root() == nil {
			return &HTTPError{Status: 400, Message: "无效的路径"}
		}
		vol := volumeNameForTenant(s.rt.volSet(), tnt)
		if err := s.emptyTrashAt(owner, vol, tnt.Root()); err != nil {
			s.rt.recordFileAudit(ctx, "trash_empty", owner, auditResultError, "volume="+vol)
			return err
		}
		s.rt.recordFileAudit(ctx, "trash_empty", owner, auditResultSuccess, "volume="+vol+" scope=default")
		return nil
	}
	roots, rerr := s.TrashRoots(owner, volumeName)
	if rerr != nil {
		return rerr
	}
	for _, vr := range roots {
		if err := s.emptyTrashAt(owner, vr.Volume, vr.Root); err != nil {
			s.rt.recordFileAudit(ctx, "trash_empty", owner, auditResultError, "volume="+vr.Volume)
			return err
		}
		s.rt.recordFileAudit(ctx, "trash_empty", owner, auditResultSuccess, "volume="+vr.Volume+" all=true")
	}
	return nil
}

// emptyTrashAt 清空给定卷根的 trash 桶（原 EmptyTrash 主体；volumeName 供配额释放同口径）。
func (s *Service) emptyTrashAt(owner, volumeName string, root *storage.Root) error {
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
		// 释放量必须与**预留量**同口径（用户可见逻辑长度，非磁盘密文长度）——见
		// trashEntryReleaseSize；文件仍存在（下面才 Remove）故 LogicalSize 可取。
		s.releaseTrashEntryQuota(owner, volumeName, root, entryRel)
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
func (s *Service) releaseTrashEntryQuota(owner, volName string, root *storage.Root, entryName string) {
	size := trashEntryReleaseSize(root, entryName)
	if size <= 0 {
		return
	}
	inner := strings.TrimPrefix(entryName, trashPrefix)
	before, _, ok := strings.Cut(inner, trashDeletedSuffix)
	if !ok {
		return
	}
	// **释放金额与写入/结算同源**（独立复审「释放≠结算」类根治，2026-10-18）：条目此刻仍**存在于
	// trash 路径**（下面才 Remove）⇒ 可对**其自身路径**取卷自报实测足迹（`PhysFootprint` 按 rel
	// 查索引，软删改名后索引随迁）。此前只用公式折算 ⇒ 对 `footprint ≠ 公式` 的卷（secretdata 的
	// parity + meta pad）**少释放** ⇒ 幽灵占用/假 507。能力缺失/条目不在索引 ⇒ 回落原公式。
	// meta 条目（软删随迁命名 <flat>.meta.__deleted__）→ 释放 meta/<origRel>.meta。
	if before0, ok0 := strings.CutSuffix(before, trashMetaMarker); ok0 {
		flat := before0
		origRel := unflattenRel(flat)
		mrel := meta.MetaPath(origRel)
		if scope := s.rt.quotaScope(owner, mrel); scope != nil {
			// meta 侧写入按 **Stat 物理长度**记账（见 write_ops.go 的 metaSize 注释）⇒ 回落值为
			// 原始 size（**不再**折算），足迹能力可用时以实测为准。
			scope.ReleaseUsage(s.releaseChargeFor(volName, owner, entryName, size))
		}
		return
	}
	// 主条目 → 释放 user 桶原 rel。
	origRel := unflattenRel(before)
	if scope := s.rt.quotaScope(owner, origRel); scope != nil {
		scope.ReleaseUsage(s.releaseChargeFor(volName, owner, entryName, physicalCharge(root, size)))
	}
}

// trashEntryReleaseSize 返回回收站条目应按**用户可见逻辑长度**释放的字节数。
//
// 为什么不能用 `os.Stat(条目).Size()`：at-rest 加密卷上磁盘字节是**密文**（容器头部 + 每分块
// tag/padding 膨胀），而写路径预留、软删保留的配额都是**逻辑长度** ⇒ 按磁盘字节释放会让 owner
// 配额漂移——密文 > 逻辑时**多释放**（配额虚低 → 超限写入通过），反之留幽灵占用（假 507）。
// `Root.LogicalSize` 与写路径同口径（明文长度）。
//
// 取不到（文件已被并发删除、非加密卷路径异常）→ 返回 0 = **宁少放不多放**：释放不足偏向
// 「配额偏高」，可由对账自愈；过度释放会让超限写入通过，属不安全方向。
func trashEntryReleaseSize(root *storage.Root, rel string) int64 {
	if root == nil {
		return 0
	}
	size, err := root.LogicalSize(rel)
	if err != nil || size < 0 {
		return 0
	}
	return size
}

// CleanupTrash 清理过期回收站条目（保留期 ttl；0 = 立即全部清理）。
// 返回清理条数。按 mtime 判定（软删时的 mtime 由 rename 保持）。
// C-MAJOR-2：trash 条目为**分层树**——递归遍历，过期条目（文件）删除 + 释放配额。
func (s *Service) CleanupTrash(ctx context.Context, owner string, ttl time.Duration) (int, error) {
	return s.CleanupTrashIn(ctx, owner, "", false, ttl)
}

// CleanupTrashIn 按卷 TTL 清理（语义同 EmptyTrashIn：volume 指定卷 / all 全部卷 / 皆无 = 默认卷）。
func (s *Service) CleanupTrashIn(ctx context.Context, owner, volumeName string, all bool, ttl time.Duration) (int, error) {
	if volumeName == "" && !all {
		tnt := s.rt.tenantOf(normalizeOwner(owner))
		if tnt == nil || tnt.Root() == nil {
			return 0, nil
		}
		return s.cleanupTrashAt(owner, volumeNameForTenant(s.rt.volSet(), tnt), tnt.Root(), ttl)
	}
	roots, rerr := s.TrashRoots(owner, volumeName)
	if rerr != nil {
		return 0, rerr
	}
	total := 0
	for _, vr := range roots {
		n, err := s.cleanupTrashAt(owner, vr.Volume, vr.Root, ttl)
		if err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// cleanupTrashAt 在给定卷根上做 TTL 清理（原 CleanupTrash 主体）。
func (s *Service) cleanupTrashAt(owner, volumeName string, root *storage.Root, ttl time.Duration) (int, error) {
	trashAbs, ok := root.Abs("trash")
	if !ok {
		return 0, nil
	}
	now := time.Now()
	cleaned := 0
	err := filepath.WalkDir(trashAbs, s.cleanupTrashWalk(owner, volumeName, root, trashAbs, ttl, now, &cleaned))
	if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	return cleaned, nil
}

// cleanupTrashWalk 是 CleanupTrash 的 WalkDir 回调（S107 拆分降 gocognit）：过期文件
// 删除 + 释放原 rel 配额（与 EmptyTrash 对称）；目录跳过（内容由子条目处理）。
func (s *Service) cleanupTrashWalk(owner, volName string, root *storage.Root, trashAbs string, ttl time.Duration, now time.Time, cleaned *int) fs.WalkDirFunc {
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
		s.releaseTrashEntryQuota(owner, volName, root, entryRel)
		if root.Remove(entryRel) == nil {
			*cleaned++
		}
		return nil
	}
}
