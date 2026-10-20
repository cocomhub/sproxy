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
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
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
		if aerr := archiveOne(ctx, j, a, seg, &rep); aerr != nil {
			rep.Failed++
			if rep.Err == nil {
				rep.Err = aerr
			}
		}
	}
	return rep, nil
}

// archiveOne 归档单个封段：成功后**才**删除本地文件（失败保留 ⇒ 不丢证据）。
//
// 先经 `Journal.QuarantineSegment` 改名为独占名（锁内），再打开/上传/删除该隔离名 ⇒
// 归档期间的轮转无法把我们的目标搬走或替换（评审 R13 P2 的 TOCTOU）。失败时尽力放回原名，
// 放不回也**绝不删除**（宁可留下带 `.archiving.` 后缀的文件，也不丢证据）。
func archiveOne(ctx context.Context, j *FileJournal, a SegmentArchiver, seg SegmentInfo, rep *ArchiveReport) error {
	orig := seg.Path
	q, qerr := j.QuarantineSegment(orig)
	if qerr != nil {
		return fmt.Errorf("audit: 隔离封段 %s 失败（本轮跳过，下轮重试）: %w", orig, qerr)
	}
	// 注意：`seg.Path` 保持**原名**（归档器据它做命名/身份，便于幂等与对账）；
	// 只有**读取**用隔离名 ⇒ 归档期间轮转不会搬走/替换我们的目标文件。
	f, err := os.Open(q)
	if err != nil {
		_ = os.Rename(q, orig)
		return fmt.Errorf("audit: 打开封段 %s 失败: %w", orig, err)
	}
	ref, aerr := a.Archive(ctx, seg, f)
	cerr := f.Close()
	if aerr != nil {
		_ = os.Rename(q, orig) // 尽力放回原名（供下轮重试）
		return fmt.Errorf("audit: 归档封段 %s 失败（本地保留）: %w", orig, aerr)
	}
	if cerr != nil {
		_ = os.Rename(q, orig)
		return fmt.Errorf("audit: 关闭封段 %s 失败（本地保留）: %w", orig, cerr)
	}
	if rerr := os.Remove(q); rerr != nil {
		return fmt.Errorf("audit: 归档成功但回收隔离封段 %s 失败: %w", q, rerr)
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
		prefix = DefaultArchivePrefix
	}
	now := a.Now
	if now == nil {
		now = time.Now
	}
	dir := path.Join(prefix, now().UTC().Format("20060102"))
	base := filepath.Base(seg.Path)

	// ① 先写**唯一临时对象**（纳秒后缀），避免并发/重入互相覆盖。
	tmpRel := path.Join(dir, "."+base+"."+strconv.FormatInt(now().UnixNano(), 36))
	h := sha256.New()
	if err := a.FS.WriteFile(ctx, syncpkg.LogicalKey{}, tmpRel, io.TeeReader(r, h), seg.Size, 0); err != nil {
		_ = a.FS.Delete(ctx, tmpRel)
		return "", fmt.Errorf("audit: 写归档临时对象失败: %w", err)
	}

	// ② **内容寻址**命名：`<basename>.<sha256:16>`。
	//
	// 为什么不能沿用「日期 + basename」：封段名恒为 `audit.log.N`，而轮转会**复用序号**
	// （`.1` 被搬成 `.2`、随后又产生新的 `.1`）⇒ 同一天内同名不同内容必然撞名；而
	// `WriteIfAbsent` 对已存在返回 `(false, nil)` ⇒ 旧实现把「没写成功」当成功，调用方随即
	// 删除本地封段 = **审计证据静默丢失** ✗（评审 R13 P0）。
	// 内容寻址下：同内容 ⇒ 同名（幂等 ✓）；异内容 ⇒ 不同名（绝不覆盖 ✓）。
	sum := hex.EncodeToString(h.Sum(nil))
	finalRel := path.Join(dir, base+"."+sum[:16])
	if err := a.FS.Rename(ctx, tmpRel, finalRel); err != nil {
		// 目标已存在（Windows rename 不覆盖）：同名即同内容 ⇒ 幂等成功。
		_ = a.FS.Delete(ctx, tmpRel)
		if e, serr := a.FS.Stat(ctx, finalRel); serr == nil && e != nil {
			return finalRel, nil
		}
		return "", fmt.Errorf("audit: 归档目标 rename 失败: %w", err)
	}
	return finalRel, nil
}

// DefaultArchivePrefix 是冷段在归档卷内的默认前缀（**唯一定义处**；装配层复用本常量）。
// 门禁 R41 保证该字面量只出现在归档实现/装配点，防止把它挂到用户可见面（证据隔离）。
const DefaultArchivePrefix = "audit-archive"
