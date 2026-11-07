// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"os"
	"strings"

	"github.com/cocomhub/sproxy/pkg/quota"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// s3_part_ledger.go —— S3 multipart **在途 part** 的双账本记账（chunk 桶 Scope + 卷容量池）。
//
// 背景（设计文档 2026-10-11-enclosure-volume.md §12.1 第 1 项）：此前只在
// CompleteMultipartUpload 时按合计大小预留/结算，**在途 part**（已落 chunk 桶磁盘、
// 但尚未 complete）完全不计账——单个会话最多 MaxParts × 64MiB，在
// MultipartOrphanTTL（24h）孤儿清理窗口内可反复超配额写盘，且 owner 的 chunk 桶
// Scope 与卷容量池都看不到这部分占用。
//
// 方案是**无状态**的（不引入跨请求预留句柄，重启即由磁盘实况兜底）：part 上传成功即
// 提交进双账本；part 被删除时按 stat 释放。三条不变量：
//
//  1. **写成功才计**：先 TryReserve（强制额度）再 Commit；任一侧失败都回滚已取得的一侧
//     （不允许只入一半账）；
//  2. **删除才释放**：只有 root.Remove 成功之后才释放该 part 的记账——complete / abort /
//     sweepStaleMultipart 三处同口径；
//  3. **可重试失败不释放**：complete 失败时保留的 parts 仍在磁盘上占空间 ⇒ 记账必须保留
//     （否则少计，等于用失败重试兑换超额写入）。
//
// 与 complete 时结算的关系：complete 成功路径先把**目标文件**按合计大小结算
// （`s3CompleteSettle`），同时逐 part 删除即逐 part 释放 ⇒ 净账 = 目标文件，不双计。
type PartLedger struct {
	Scope *quota.Scope
	Pool  *quota.Pool
}

// active 报告是否有任一侧账本可用；两侧都未装配（配额/卷集未启用）时所有方法退化为
// no-op，保持零回归。
func (l *PartLedger) Active() bool {
	return l != nil && (l.Scope != nil || l.Pool != nil)
}

// newS3PartLedger 解析某会话 part 所在（**chunk 桶**）的双账本：owner 的 chunk 桶 Scope
// （键与 files 包写路径同构：首段桶名）+ part 物理所在卷的容量池。owner 为空或未装配
// 配额时返回 no-op 句柄（调用方无需分支）。
func (s *Service) newS3PartLedger(owner string, tnt *storage.Tenant) *PartLedger {
	l := &PartLedger{}
	if owner == "" {
		return l // owner 未知（旧格式 meta 等）→ 不猜账本，退化为 no-op
	}
	l.Scope = s.d.QuotaScopeFor(owner, storage.ChunkPrefix)
	l.Pool = s.d.VolumePoolForTenant(tnt)
	return l
}

// PartReservation 是 part 上传的双账本预留句柄（两侧各自至多一次 Commit/Release，
// 由 quota.Reservation 内部 CAS 保证幂等）。
type PartReservation struct {
	ScopeRes *quota.Reservation
	PoolRes  *quota.Reservation
}

// release 放弃预留（回滚未提交的额度）；nil 安全。
func (r *PartReservation) Release() {
	if r == nil {
		return
	}
	if r.ScopeRes != nil {
		r.ScopeRes.Release()
	}
	if r.PoolRes != nil {
		r.PoolRes.Release()
	}
}

// reserve 为 size 字节预留双账本额度；任一侧拒绝（配额/卷容量不足）时回滚已取得的
// 另一侧并返回 nil——调用方按 507 InsufficientStorage 处理。
func (l *PartLedger) Reserve(size int64) *PartReservation {
	if !l.Active() || size <= 0 {
		return &PartReservation{}
	}
	r := &PartReservation{}
	if l.Scope != nil {
		rr, err := l.Scope.TryReserve(size)
		if err != nil {
			return nil
		}
		r.ScopeRes = rr
	}
	if l.Pool != nil {
		rr, err := l.Pool.TryReserve(size)
		if err != nil {
			r.Release()
			return nil
		}
		r.PoolRes = rr
	}
	return r
}

// commit 把预留转为该 part 的已确认占用（part 已成功落盘）。
func (r *PartReservation) Commit(l *PartLedger, size int64) {
	if r == nil || !l.Active() || size <= 0 {
		return
	}
	if r.ScopeRes != nil {
		r.ScopeRes.Commit(size)
	}
	if r.PoolRes != nil {
		r.PoolRes.Commit(size)
	}
}

// releaseCommitted 释放 n 字节已确认占用（**仅在 part 删除成功后调用**）。两侧各自钳 0，
// 重复调用安全（不会把账本压成负数）。
func (l *PartLedger) ReleaseCommitted(n int64) {
	if !l.Active() || n <= 0 {
		return
	}
	if l.Scope != nil {
		l.Scope.ReleaseUsage(n)
	}
	if l.Pool != nil {
		l.Pool.ReleaseCommitted(n)
	}
}

// removePartReleasing 删除单个 part（partRel）并释放其记账：大小在**删除前**取
// （root.Stat），只有 Remove 成功才释放。返回是否删除成功。
//
// 失败不释放是刻意的：删除失败意味着字节仍在盘上，释放会造成少计。
func (l *PartLedger) removePartReleasing(root *storage.Root, partRel string) bool {
	size := int64(0)
	if st, err := root.Stat(partRel); err == nil && st != nil {
		size = st.Size()
	}
	if err := root.Remove(partRel); err != nil {
		return false
	}
	l.ReleaseCommitted(size)
	return true
}

// removeSessionParts 删除会话 uploadID 的全部 part（entries 为 chunk 桶目录项），
// 逐个「删成功即释放」。返回删除成功的 part 数与字节合计（供日志与测试）。
//
// 用量取自目录项（e.Info().Size()）——与上传时提交的大小同源（均为 part 文件字节数）。
// 任一 Remove 失败只跳过（best-effort，与清理语义一致）：不释放该 part 的记账。
func (l *PartLedger) removeSessionParts(root *storage.Root, entries []os.DirEntry, uploadID string) (int, int64) {
	prefix := MultipartPartPrefix + uploadID + storage.PartSuffix
	deleted, bytes := 0, int64(0)
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) {
			continue
		}
		size := int64(0)
		if info, err := e.Info(); err == nil && info != nil {
			size = info.Size()
		}
		if err := root.Remove(storage.ChunkPrefix + name); err != nil {
			continue
		}
		l.ReleaseCommitted(size)
		deleted++
		bytes += size
	}
	return deleted, bytes
}
