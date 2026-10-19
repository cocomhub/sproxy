// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

// archive.go 是 **M4 归档原语**（设计 docs/designs/2026-10-19-audit-service.md §5.5 / M4）：
//
//	sealed 段 → 外置归档适配器（Archiver）→ 写内核（外部/加密卷由使用方实现）→ 热段回收
//
// 本文件只提供**与存储无关的编排**：枚举封段、交给适配器、成功后回收本地封段。
// 「写到哪个卷、是否加密、配额如何记」全部由 `Archiver` 实现决定（外置适配器模式，
// 设计 §5.5「外置适配器」）⇒ 审计包不依赖任何存储卷实现。
//
// **护栏（缺一不可）**：
//   - **活跃段绝不归档**：只处理 `Sealed=true` 的段（封段是轮转产物，已不再追加）；
//   - **失败不丢**：适配器报错 ⇒ 本地段**保留**（下轮重试），绝不在未确认落地前删除证据；
//   - **有界保留**：保留最近 `keep` 个封段在本地（便于近期查询/审计自身排障）；
//   - **幂等**：目的地已存在时由适配器返回成功/跳过（本层删除前不会再调一次同一段）。

import (
	"context"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// SegmentArchiver 把**已封（sealed）**的合规段归档到外部目的地（外部卷 / 加密卷 / 远端）。
//
// 契约：
//   - 实现必须**完整消费** r（或返回错误）；返回非空 ref 表示目的地标识（用于读回/审计留痕）；
//   - 实现必须**幂等**：同一段被重复归档时不得产生重复副本（返回既有 ref 即可）；
//   - 实现**不得**在失败时留下半成品（或必须自行清理），因为失败时本层不会删除本地段。
type SegmentArchiver interface {
	Archive(ctx context.Context, seg SegmentInfo, r io.Reader) (ref string, err error)
}

// ArchiveReport 是一次归档运行的汇总。
type ArchiveReport struct {
	Candidates int // 候选（封段）数量
	Archived   int // 成功归档并从本地回收的数量
	Kept       int // 按 keep 保留在本地（未尝试归档）的数量
	Failed     int // 失败（本地段保留，待下轮重试）
	Refs       []string
	Err        error // 首个错误（便于调用方告警）
}

// String 生成一行摘要（日志友好）。
func (r ArchiveReport) String() string {
	return fmt.Sprintf("audit archive: candidates=%d archived=%d kept=%d failed=%d",
		r.Candidates, r.Archived, r.Kept, r.Failed)
}

// ArchiveSealed 把该 Journal 目录下的**封段**交给 a 归档；成功后**删除**本地封段。
//
// keep 是最新保留在本地的最小封段数（<0 视为 0）。语义要点见文件头「护栏」。
func ArchiveSealed(ctx context.Context, j *FileJournal, a SegmentArchiver, keep int) (ArchiveReport, error) {
	var rep ArchiveReport
	if j == nil {
		return rep, fmt.Errorf("audit: ArchiveSealed 需要 Journal")
	}
	if a == nil {
		return rep, fmt.Errorf("audit: ArchiveSealed 需要 SegmentArchiver")
	}
	if keep < 0 {
		keep = 0
	}
	sealed, serr := sealedSegments(j)
	if serr != nil {
		return rep, serr
	}
	rep.Candidates = len(sealed)
	if rep.Candidates == 0 {
		return rep, nil
	}
	if rep.Candidates <= keep {
		rep.Kept = rep.Candidates
		return rep, nil
	}
	todo := sealed[:rep.Candidates-keep]
	rep.Kept = keep
	for _, seg := range todo {
		if cerr := ctx.Err(); cerr != nil {
			rep.Err = cerr
			return rep, cerr
		}
		if aerr := archiveOne(ctx, a, seg, &rep); aerr != nil {
			rep.Failed++
			if rep.Err == nil {
				rep.Err = aerr
			}
		}
	}
	return rep, nil
}

// archiveOne 归档单个封段：成功后**才**删除本地文件（失败保留 ⇒ 不丢证据）。
func archiveOne(ctx context.Context, a SegmentArchiver, seg SegmentInfo, rep *ArchiveReport) error {
	f, err := os.Open(seg.Path)
	if err != nil {
		return fmt.Errorf("audit: 打开封段 %s 失败: %w", seg.Path, err)
	}
	ref, aerr := a.Archive(ctx, seg, f)
	cerr := f.Close()
	if aerr != nil {
		return fmt.Errorf("audit: 归档封段 %s 失败（本地保留）: %w", seg.Path, aerr)
	}
	if cerr != nil {
		return fmt.Errorf("audit: 关闭封段 %s 失败（本地保留）: %w", seg.Path, cerr)
	}
	if rerr := os.Remove(seg.Path); rerr != nil {
		return fmt.Errorf("audit: 归档成功但回收本地封段 %s 失败: %w", seg.Path, rerr)
	}
	rep.Archived++
	if ref != "" {
		rep.Refs = append(rep.Refs, ref)
	}
	return nil
}

// sealedSegments 返回**封段**并按**最旧优先**排序（archiveIndex 大者更旧）。
func sealedSegments(j *FileJournal) ([]SegmentInfo, error) {
	segs, err := j.Segments()
	if err != nil {
		return nil, fmt.Errorf("audit: 枚举段失败: %w", err)
	}
	sealed := make([]SegmentInfo, 0, len(segs))
	for _, s := range segs {
		if s.Sealed {
			sealed = append(sealed, s)
		}
	}
	sort.SliceStable(sealed, func(i, k int) bool {
		return archiveIndex(sealed[i].Path) > archiveIndex(sealed[k].Path)
	})
	return sealed, nil
}

// FSArchiver 把封段写入 **`pkg/sync` 的 FS 能力**（**唯一**通用实现）。
//
// 为什么复用 FS 而不是自造 sink 接口（用户裁定 2026-10-20）：归档与「云端下载 → 转存」同类，
// 目标可能是本地目录 / 外部卷 / 加密卷 / 远端；这些语义已由 `pkg/sync.FS` 统一表达
// （`WriteFile`、可选的 **`WriteIfAbsent`** 幂等写、`Stat`）⇒ 审计包只选能力，不复制实现 ✓。
//
// 幂等：底层实现 `WriteIfAbsent` ⇒ 直接用它（目标存在即不覆盖、返回 written=false）；
// 未实现 ⇒ 退化为 `Stat` + `WriteFile`（大小一致视为已完成），并在两处都保持「不静默覆盖」语义。
type FSArchiver struct {
	FS     syncpkg.FS
	Prefix string           // 空 ⇒ "audit-archive"
	Now    func() time.Time // 可注入（测试确定性）
}

// Archive 实现 SegmentArchiver。
func (a FSArchiver) Archive(ctx context.Context, seg SegmentInfo, r io.Reader) (string, error) {
	if a.FS == nil {
		return "", fmt.Errorf("audit: FSArchiver 需要 pkg/sync.FS（装配层注入）")
	}
	prefix := a.Prefix
	if prefix == "" {
		prefix = "audit-archive"
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	rel := path.Join(prefix, now().UTC().Format("20060102"), filepath.Base(seg.Path))

	// 可选能力查询（仓库既有范式：使用方类型断言查能力，不在调用侧硬编码实现语义）。
	writer, hasWriteIfAbsent := a.FS.(syncpkg.WriteIfAbsent)
	if !hasWriteIfAbsent {
		// 退化路径：先看目标是否已存在且大小一致（幂等），再普通写。
		if e, err := a.FS.Stat(ctx, rel); err == nil && e != nil && seg.Size > 0 && e.Size == seg.Size {
			return rel, nil
		}
		if err := a.FS.WriteFile(ctx, syncpkg.LogicalKey{}, rel, r, seg.Size, 0); err != nil {
			return "", fmt.Errorf("audit: 写归档目标失败: %w", err)
		}
		return rel, nil
	}
	// 首选：既有幂等写能力（目标已存在 ⇒ 不覆盖、written=false）。
	if _, err := writer.WriteIfAbsent(ctx, rel, r, seg.Size, 0); err != nil {
		return "", fmt.Errorf("audit: 写归档目标（WriteIfAbsent）失败: %w", err)
	}
	return rel, nil
}
