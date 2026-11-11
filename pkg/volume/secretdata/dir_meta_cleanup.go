// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
)

// CleanupStaleDirMeta 删除容器内**非权威**的目录 meta blob（同名目录只保留一个）。
//
// 背景（用户裁定 2026-11-10）：「记录失败事件审计重试能力，后续定期重试处理，**最终必须成功**」。
// 目录改名/移动是「先写新 meta、后删旧 meta」两步；第二步失败（客户端断开、远端 5xx）⇒ 容器内
// 并存两个目录 meta ⇒ `sclient volume explain` 报 `dir_meta_ambiguous`。此前只有一条 WARN 日志、
// **没有任何自动修复入口**；本方法是该问题的**领域侧执行器能力**（供 `pkg/server/issues` 的
// 重试执行器调用），使「重试直到成功」真正可行。
//
// 仲裁判据（与加载期 `scanContainerDirMetaBlob` **同一套**，不得分叉）：
// `Version`（改名/移动单调递增，权威信号）→ `mtime`（兼容旧数据）→ `Name`（纯兜底确定性）。
//
// 安全语义（fail-closed，宁可不删也绝不误删）：
//   - **只处理** `shardseal.KindDirMeta` 条目（绝不碰文件 meta / 分块 / 容器目录）；
//   - **任何** dir-meta 解不开（密钥不符/损坏）⇒ **不删任何**并返回错误
//     （否则可能把唯一可解的那份删掉，造成不可恢复的逻辑树丢失）；
//   - 多个候选的 `DirID` **不一致** ⇒ **不删任何**并返回错误（语义上不属于同一次改名的残留，
//     需人工判断）；
//   - **幂等**：只有 0 或 1 个 dir-meta（含"已被前一次重试删干净"）⇒ `(0, nil)` 视为成功，
//     因此重试天然可重复执行。
//
// 返回 (removed, err)：removed = 实际删除的 blob 数。
// metaCleanupCand 是清理用的目录 meta 候选（只保留仲裁所需字段）。
type metaCleanupCand struct {
	name    string
	mtime   int64
	version int
	dirID   string
}

// collectCleanupCands 收集容器内全部**可解密**的目录 meta 候选，并返回解不开的个数。
//
// 解不开的个数由调用方用于 **fail-closed 判定**（无法确知"保留哪一份"时不删任何）。
func (s *SecretdataFS) collectCleanupCands(ctx context.Context, container string) ([]metaCleanupCand, int, error) {
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return nil, 0, fmt.Errorf("secretdata: 列容器 %s 失败: %w", container, err)
	}
	var cands []metaCleanupCand
	unreadable := 0
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindDirMeta {
			continue
		}
		blob, rerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
		if rerr != nil {
			unreadable++
			continue
		}
		dm, derr := s.decryptDirMeta(blob)
		if derr != nil || dm == nil {
			unreadable++
			continue
		}
		cands = append(cands, metaCleanupCand{name: f.Name, mtime: f.MTime, version: dm.Version, dirID: dm.DirID})
	}
	return cands, unreadable, nil
}

// sortCandsAuthoritativeFirst 按**与加载期完全相同**的仲裁把候选降序排列（[0] = 权威），
// 并校验所有候选 `DirID` 一致（不一致 ⇒ 不属于同一次改名的残留，交由人工判断）。
func sortCandsAuthoritativeFirst(container string, cands []metaCleanupCand) error {
	for _, c := range cands[1:] {
		if c.dirID != cands[0].dirID {
			return fmt.Errorf("secretdata: 容器 %s 存在 DirID 不同的多个目录 meta（%s vs %s），拒绝自动清理（需人工判断）",
				container, cands[0].dirID, c.dirID)
		}
	}
	slices.SortStableFunc(cands, func(a, b metaCleanupCand) int {
		if a.version != b.version {
			if a.version > b.version {
				return -1
			}
			return 1
		}
		if a.mtime != b.mtime {
			if a.mtime > b.mtime {
				return -1
			}
			return 1
		}
		return strings.Compare(a.name, b.name)
	})
	return nil
}

func (s *SecretdataFS) CleanupStaleDirMeta(ctx context.Context, container string) (int, error) {
	if s == nil {
		return 0, errors.New("secretdata: CleanupStaleDirMeta 需要非空接收者")
	}
	if container == "" {
		return 0, errors.New("secretdata: CleanupStaleDirMeta 需要非空容器路径")
	}
	cands, unreadable, err := s.collectCleanupCands(ctx, container)
	if err != nil {
		return 0, err
	}
	// 解不开的 meta 必须阻塞清理（fail-closed）：无法确知"保留哪一份"。
	if unreadable > 0 {
		return 0, fmt.Errorf("secretdata: 容器 %s 有 %d 个目录 meta 无法解密，拒绝清理（fail-closed，需人工核对密钥）",
			container, unreadable)
	}
	if len(cands) <= 1 {
		return 0, nil // 无残留（或已清理干净）⇒ 幂等成功
	}
	if serr := sortCandsAuthoritativeFirst(container, cands); serr != nil {
		return 0, serr
	}
	removed := 0
	for _, c := range cands[1:] {
		if derr := s.inner.Delete(ctx, path.Join(container, c.name)); derr != nil && !errors.Is(derr, os.ErrNotExist) {
			return removed, fmt.Errorf("secretdata: 删除非权威目录 meta %s/%s 失败: %w", container, c.name, derr)
		}
		removed++
	}
	return removed, nil
}

// StaleDirMetaCount 报告容器内**非权威**目录 meta 的数量（0 = 无残留；只读，不修改）。
//
// 用途：供调度/审查判断"是否还需要重试"（0 ⇒ 事项可解除）。语义与 `CleanupStaleDirMeta`
// 完全一致（同一仲裁与同一 fail-closed 判据），只是不执行删除。
func (s *SecretdataFS) StaleDirMetaCount(ctx context.Context, container string) (int, error) {
	if s == nil {
		return 0, errors.New("secretdata: StaleDirMetaCount 需要非空接收者")
	}
	entries, err := s.inner.ListDir(ctx, container)
	if err != nil {
		return 0, fmt.Errorf("secretdata: 列容器 %s 失败: %w", container, err)
	}
	n := 0
	for _, f := range entries {
		if f.IsDir || shardseal.ClassifyName(f.Name) != shardseal.KindDirMeta {
			continue
		}
		blob, rerr := readBlob(ctx, s.inner, path.Join(container, f.Name))
		if rerr != nil {
			return 0, fmt.Errorf("secretdata: 读目录 meta %s/%s 失败: %w", container, f.Name, rerr)
		}
		if _, derr := s.decryptDirMeta(blob); derr != nil {
			return 0, fmt.Errorf("secretdata: 目录 meta %s/%s 无法解密（fail-closed）: %w", container, f.Name, derr)
		}
		n++
	}
	if n <= 1 {
		return 0, nil
	}
	return n - 1, nil
}
