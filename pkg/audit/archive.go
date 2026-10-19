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

// SegmentSink 是归档**目的地写能力**（由装配层用 `pkg/sync.FS` 适配：LocalFS / 外部卷 / 加密卷
// / 云端都用同一套 `WriteFile` 语义 ⇒ 审计包**不依赖**任何存储实现，也不重复实现拷贝 ✗）。
//
// 与 `pkg/sync.FS.WriteFile(ctx, key, path, r, size, mtime)` 同形（去掉 LogicalKey/幂等上下文，
// 这两个由装配层适配时补齐）。
type SegmentSink interface {
	// Put 写入整个对象（覆盖语义由实现决定）；size 为声明字节数。
	Put(ctx context.Context, path string, r io.Reader, size int64) error
	// Size 返回已有对象大小；exists=false 表示不存在（归档侧据此**幂等跳过**）。
	Size(ctx context.Context, path string) (n int64, exists bool, err error)
}

// SinkArchiver 是把封段写入 `SegmentSink` 的归档实现（**唯一的通用实现**）。
//
// 幂等：目标已存在且大小一致 ⇒ 直接返回既有 ref（不重复写）；否则覆盖写。
// 路径：`<prefix>/<UTC 日期>/<段文件名>`（同一段名在同一天恒定 ⇒ 幂等可判定 ✓）。
type SinkArchiver struct {
	Sink   SegmentSink
	Prefix string           // 空 ⇒ 默认 "audit-archive"
	Now    func() time.Time // 可注入（测试确定性）
}

// Archive 实现 SegmentArchiver。
func (a SinkArchiver) Archive(ctx context.Context, seg SegmentInfo, r io.Reader) (string, error) {
	if a.Sink == nil {
		return "", fmt.Errorf("audit: SinkArchiver 需要 SegmentSink（装配层用 pkg/sync.FS 适配）")
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
	if n, exists, err := a.Sink.Size(ctx, rel); err != nil {
		return "", fmt.Errorf("audit: 查询归档目标失败: %w", err)
	} else if exists && seg.Size > 0 && n == seg.Size {
		return rel, nil // 幂等：已有完整副本
	}
	if err := a.Sink.Put(ctx, rel, r, seg.Size); err != nil {
		return "", fmt.Errorf("audit: 写入归档目标失败: %w", err)
	}
	return rel, nil
}
