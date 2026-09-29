// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// logCleanSyncTmp 是 sync-tmp 目录清理失败告警文案（三条清理路径共用）。
const logCleanSyncTmp = "清理 sync-tmp 失败"

// Engine 编排一次同步。
type Engine struct {
	Concurrency int // 多文件并发数；0 或负数回落 3
	Logger      *slog.Logger
	// ConflictRecorder 是冲突索引登记回调（conflict_policy=merge3 时冲突段写标记文件后调用；
	// 保留败方副本时登记 backup_loser 记录；nil = 不登记）。装配层注入持久化索引。
	ConflictRecorder func(rec ConflictRecord)
	// BackupLoser 覆盖型策略保留败方副本（keep-both 防静默丢失）：覆盖前检测到目标与源
	// **分歧改动**（内容不同）时，旧目标 rename 为 `<dst>.conflict-<ts>` 副本保留 +
	// 登记 ConflictRecord。默认 false（引擎零回归）；装配层（服务端）显式开启。
	BackupLoser bool
	// KeepMax 单目录内 `<name>.conflict-*` 副本数量上限（超过时删最旧，0 = 不限制）。
	KeepMax int
}

// ConflictRecord 是一次三方合并冲突/败方副本的登记信息（供冲突索引持久化）。
// Kind 区分登记类型："" = merge3 文本冲突段；"backup_loser" = 覆盖型策略保留败方副本。
type ConflictRecord struct {
	Path       string   `json:"path"`
	Kind       string   `json:"kind,omitempty"`
	BackupPath string   `json:"backup_path,omitempty"`
	HunkCount  int      `json:"hunk_count"`
	BaseSHA    string   `json:"base_sha"`
	OursSHA    string   `json:"ours_sha"`
	TheirsSHA  string   `json:"theirs_sha"`
	Ours       []string `json:"ours"`
	Theirs     []string `json:"theirs"`
	Timestamp  int64    `json:"ts"`
}

func (e *Engine) concurrency() int {
	if e.Concurrency <= 0 {
		return 3
	}
	return e.Concurrency
}

func (e *Engine) logger() *slog.Logger {
	if e.Logger != nil {
		return e.Logger
	}
	return slog.Default()
}

// Sync 执行一次同步：src/dst 是两侧 FS；job 记录进度与结果（可变引用）。
//
// 流程：枚举 src 树（Recursive 递归、每层过滤 filters、跳过 .__ 内部目录、符号链接按
// job.FollowSymlinks）→ ComputeDiff → 按 Action 执行。单文件错误不会中止整个同步
// （记 ActionError 继续）；ctx 取消时返回 ctx.Err() 并将 job.Status 置为 cancelled。
func (e *Engine) Sync(ctx context.Context, src, dst FS, job *Job) error {
	if err := ctx.Err(); err != nil {
		job.Status = StatusCancelled
		return err
	}
	job.Status = StatusSyncing

	entries, err := WalkEntries(ctx, src, job.Src, job.Recursive, job.FollowSymlinks, job.Filters)
	if err != nil {
		job.Status = StatusFailed
		return fmt.Errorf("枚举源目录失败: %w", err)
	}

	dstStat := func(p string) (*Entry, error) {
		dstPath := joinSlash(job.Dst, stripRootPrefix(p, job.Src))
		return dst.Stat(ctx, dstPath)
	}
	diffs, derr := ComputeDiff(entries, dstStat, job.ConflictPolicy)
	if derr != nil {
		e.logger().Warn("差异计算存在目标 stat 错误（已记录为 error 结果）", "error", derr)
	}

	var mu sync.Mutex
	rec := func(r FileResult) {
		mu.Lock()
		job.Results = append(job.Results, r)
		mu.Unlock()
	}

	fileTransfers := e.collectFileTransfers(ctx, dst, job, diffs, rec)

	// 统计只计文件传输（目录与符号链接不计入进度）
	var bytesTotal int64
	for _, d := range fileTransfers {
		if d.Src != nil {
			bytesTotal += d.Src.Size
		}
	}
	job.Stats.FilesTotal = int64(len(fileTransfers))
	job.Stats.BytesTotal = bytesTotal

	// 多文件之间并发传输（同一文件串行）
	e.transferFiles(ctx, src, dst, job, fileTransfers, rec, &mu)

	// 删除传播（job.DeletePolicy=propagate）：文件传输后枚举 dst 树，删除源已不存在的文件。
	if job.DeletePolicy == DeletePropagate {
		if err := e.propagateDeletes(ctx, src, dst, job, rec); err != nil {
			e.logger().Warn("删除传播执行存在错误（已记录为 error 结果）", "error", err)
		}
	}

	if err := ctx.Err(); err != nil {
		job.Status = StatusCancelled
		return err
	}
	job.Status = StatusCompleted
	return nil
}

// dstPathOf 返回 diff 条目在目标 FS 的斜杠路径（去掉源前缀后拼到 job.Dst）。
func (e *Engine) dstPathOf(job *Job, d *DiffEntry) string {
	return joinSlash(job.Dst, stripRootPrefix(d.Path, job.Src))
}

// collectFileTransfers 遍历差异，分发目录/符号链接/跳过/错误结果，返回待传输的文件条目。
func (e *Engine) collectFileTransfers(ctx context.Context, dst FS, job *Job, diffs []DiffEntry, rec func(FileResult)) []*DiffEntry {
	var fileTransfers []*DiffEntry
	for i := range diffs {
		d := &diffs[i]
		if e.dispatchDiffEntry(ctx, dst, job, d, rec) {
			fileTransfers = append(fileTransfers, d)
		}
	}
	return fileTransfers
}

// dispatchDiffEntry 分发单个 diff 条目：符号链接/跳过/错误/目录记结果，
// 常规文件返回 true（调用方计入待传输列表）。
func (e *Engine) dispatchDiffEntry(ctx context.Context, dst FS, job *Job, d *DiffEntry, rec func(FileResult)) bool {
	if d.Src != nil && d.Src.IsSymlink {
		rec(FileResult{Path: e.dstPathOf(job, d), Action: ActionSkippedSymlink})
		return false
	}
	switch d.Action {
	case ActionCreated, ActionUpdated, ActionConflictRenamed:
		if d.Src != nil && d.Src.IsDir {
			e.syncDir(ctx, dst, job, d, e.dstPathOf(job, d), rec)
			return false
		}
		return true
	case ActionSkipped, ActionSkippedConflict:
		if d.Src != nil {
			rec(FileResult{Path: e.dstPathOf(job, d), Action: d.Action, Size: d.Src.Size, MTime: d.Src.MTime, Checksum: d.Src.Checksum})
		}
		return false
	case ActionError:
		rec(FileResult{Path: e.dstPathOf(job, d), Action: ActionError, Error: diffErrString(d)})
		return false
	}
	return false
}

// diffErrString 返回 diff 条目的错误文本（nil Err = 空串）。
func diffErrString(d *DiffEntry) string {
	if d.Err == nil {
		return ""
	}
	return d.Err.Error()
}

// transferFiles 多文件之间并发传输（同一文件串行），每文件错误记 ActionError 继续。
func (e *Engine) transferFiles(ctx context.Context, src, dst FS, job *Job, fileTransfers []*DiffEntry, rec func(FileResult), mu *sync.Mutex) {
	concurrency := e.concurrency()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, d := range fileTransfers {
		// 审查 R1：进入 select 前先查 ctx，避免取消后 select 仍随机选到 sem 分支
		// 启动本已取消的传输（LocalFS 不查 ctx 会写完再返回）。
		if err := ctx.Err(); err != nil {
			rec(FileResult{Path: e.dstPathOf(job, d), Action: ActionError, Error: err.Error()})
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				rec(FileResult{Path: e.dstPathOf(job, d), Action: ActionError, Error: ctx.Err().Error()})
				return
			}
			defer func() { <-sem }()
			e.syncFile(ctx, src, dst, job, d, rec, mu)
		})
	}
	wg.Wait()
}

// propagateDeletes 枚举目标树，对「源已不存在」的文件执行删除（幂等）。
// 统计删除数到 job.Stats.FilesDeleted，结果记 ActionDeleted。
func (e *Engine) propagateDeletes(ctx context.Context, src, dst FS, job *Job, rec func(FileResult)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dstEntries, err := WalkEntries(ctx, dst, job.Dst, job.Recursive, job.FollowSymlinks, job.Filters)
	if err != nil {
		return fmt.Errorf("枚举目标目录失败（删除传播）: %w", err)
	}
	srcStat := func(p string) (*Entry, error) {
		srcPath := joinSlash(job.Src, stripRootPrefix(p, job.Dst))
		return src.Stat(ctx, srcPath)
	}
	diffs, derr := ComputeDeleteDiff(dstEntries, srcStat, job.DeletePolicy)
	if derr != nil {
		e.logger().Warn("删除传播差异计算存在源 stat 错误（已记录为 error 结果）", "error", derr)
	}
	for i := range diffs {
		d := &diffs[i]
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.Action == ActionDeleted {
			e.deletePropagated(ctx, dst, job, d, rec)
			continue
		}
		if d.Action == ActionError {
			errMsg := ""
			if d.Err != nil {
				errMsg = d.Err.Error()
			}
			rec(FileResult{Path: e.deleteDstPathOf(job, d), Action: ActionError, Error: errMsg})
		}
	}
	return nil
}

// deleteDstPathOf 返回删除传播 diff 条目在目标 FS 的斜杠路径（去掉目标前缀后拼到 job.Dst）。
func (e *Engine) deleteDstPathOf(job *Job, d *DiffEntry) string {
	return joinSlash(job.Dst, stripRootPrefix(d.Path, job.Dst))
}

// deletePropagated 执行单个文件的删除传播（幂等）：
// 删除前二次 stat 目标，mtime 与枚举时不一致（期间被改）→ 保留目标不删，记 ActionSkippedConflict。
func (e *Engine) deletePropagated(ctx context.Context, dst FS, job *Job, d *DiffEntry, rec func(FileResult)) {
	dstPath := joinSlash(job.Dst, stripRootPrefix(d.Path, job.Dst))
	// 删除传播冲突语义（roadmap P2 无删除传播残余）：双向连续同步下
	// 枚举与删除之间目标可能被修改——删除前二次 stat 目标，mtime 与枚举
	// 时不一致（期间被改）→ **保留目标不删**（删除不覆盖本地修改），
	// 记 ActionSkippedConflict；一致才删（幂等）。
	if cur, serr := dst.Stat(ctx, dstPath); serr == nil {
		if cur.MTime != d.Dst.MTime {
			rec(FileResult{Path: dstPath, Action: ActionSkippedConflict, MTime: cur.MTime})
			return
		}
	} else {
		// 目标已不存在（并发删除/上一轮已删）：视为删除已完成（幂等成功）。
		rec(FileResult{Path: dstPath, Action: ActionDeleted})
		job.Stats.FilesDeleted++
		return
	}
	if err := dst.Delete(ctx, dstPath); err != nil {
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("删除目标失败: %v", err)})
		return
	}
	rec(FileResult{Path: dstPath, Action: ActionDeleted})
	job.Stats.FilesDeleted++
}

// syncFile 传输单个文件。
func (e *Engine) syncFile(ctx context.Context, src, dst FS, job *Job, d *DiffEntry, rec func(FileResult), mu *sync.Mutex) {
	srcE := d.Src
	dstPath := joinSlash(job.Dst, stripRootPrefix(d.Path, job.Src))

	tmpPath, abort := e.stageSyncTmp(ctx, dst, job, d, dstPath, rec)
	if abort {
		return
	}
	if d.Action == ActionConflictRenamed {
		// conflict-rename：目标改名保留，再写新文件
		if err := dst.Rename(ctx, dstPath, d.RenameDstTo); err != nil {
			rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("冲突改名失败: %v", err)})
			return
		}
	}

	rc, err := src.OpenRead(ctx, d.Path)
	if err != nil {
		e.restoreTmp(ctx, dst, dstPath, tmpPath)
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("打开源文件失败: %v", err)})
		return
	}
	defer rc.Close() // 统一关闭源 reader（merge3/block 成功路径也关——防 Windows 句柄泄漏）
	// 审查 R5：本地 os 写入假设可靠；远程 HTTPTransport 建议在 WriteFile 后按需校验
	// 目标 checksum，防截断/损坏静默落盘（分块管线 ChunkedUpload 已逐块校验，简单
	// Upload 走 multipart 全量校验，故本地阶段无需额外校验）。
	// 块级增量、三方合并与保留败方副本（详见 syncFileSmartPath）：适用时经该路径
	// 完成传输并返回；不适用则回退下面的整文件复制。
	if tmpPath != "" && d.Action == ActionUpdated && d.Dst != nil && !d.Dst.IsDir {
		f := &fileTransfer{ctx: ctx, src: src, dst: dst, job: job, rec: rec, mu: mu, srcE: srcE, d: d, dstPath: dstPath, tmpPath: tmpPath, srcPath: d.Path}
		if e.syncFileSmartPath(f) {
			return
		}
	}
	werr := dst.WriteFile(ctx, dstPath, rc, srcE.Size, srcE.MTime)
	if werr != nil {
		e.restoreTmp(ctx, dst, dstPath, tmpPath)
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("写入目标失败: %v", werr)})
		return
	}
	if tmpPath != "" {
		if err := dst.Delete(ctx, tmpPath); err != nil {
			e.logger().Warn(logCleanSyncTmp, "path", tmpPath, "error", err)
		}
	}
	e.recordFileDone(job, mu, srcE, dstPath, d.Action, rec)
}

// stageSyncTmp 覆盖型策略（overwrite/lww/merge3）更新时把目标改名到 .sync-tmp，
// 再写新文件、成功后删除 tmp。返回 (tmpPath, abort)：abort=true 表示已记录错误结果，
// 调用方直接返回；tmpPath=="" 表示不适用覆盖改名。
//
// 拒绝用文件覆盖同名目录（审查 I-2）：Rename 目标为 .sync-tmp 会把非空目录整体
// "移走"并残留幽灵目录（os.Remove 删不了非空目录）。类型冲突由 diff 层已显式
// 判定（src 文件 vs dst 目录），此处兜底拒绝并报明确错误。
func (e *Engine) stageSyncTmp(ctx context.Context, dst FS, job *Job, d *DiffEntry, dstPath string, rec func(FileResult)) (string, bool) {
	if d.Action != ActionUpdated || (job.ConflictPolicy != ConflictOverwrite && job.ConflictPolicy != ConflictLWW && job.ConflictPolicy != ConflictMerge3) {
		return "", false
	}
	if d.Dst != nil && d.Dst.IsDir {
		rec(FileResult{Path: dstPath, Action: ActionError, Error: "拒绝用文件覆盖同名目录"})
		return "", true
	}
	// overwrite/lww 覆盖：先把目标改名到 .sync-tmp，再写新文件，成功后删除 tmp。
	// 注意（审查 R4）：.sync-tmp 是保留后缀，源树中若同时含 a.txt 与 a.txt.sync-tmp
	// 可能互相踩踏；case-insensitive 的远程 FS 上更明显，文档已标注。
	tmpPath := dstPath + ".sync-tmp"
	if err := dst.Rename(ctx, dstPath, tmpPath); err != nil {
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("重命名目标到临时名失败: %v", err)})
		return "", true
	}
	return tmpPath, false
}

// recordFileDone 记录单文件传输完成的进度账本与结果。
func (e *Engine) recordFileDone(job *Job, mu *sync.Mutex, srcE *Entry, dstPath string, action Action, rec func(FileResult)) {
	mu.Lock()
	job.Stats.FilesDone++
	// 审查 R2：按源条目 size 记账；源文件在 diff 后、传输中被并发修改时统计会失真
	// （本地 io.Copy 实际写入字节未知）。对进度展示可接受，文档已标注。
	job.Stats.BytesDone += srcE.Size
	mu.Unlock()
	rec(FileResult{Path: dstPath, Action: action, Size: srcE.Size, MTime: srcE.MTime, Checksum: srcE.Checksum})
}

// fileTransfer 是单文件智能路径传输（merge3 三方合并 / 块级增量 / 保留败方副本）的共享
// 上下文（S107：收敛 syncFileSmartPath/syncFileMerge3/syncFileBlock/writeBlockDiffs/
// keepLoserIfDivergent 的 ctx/src/dst/job/rec/mu/srcE/d/dstPath/tmpPath/srcPath 参数为
// 结构体；这些值在单文件传输全程不变）。srcE/d 是 diff 条目视图，dstPath 为目标落地路径，
// tmpPath 为覆盖型策略改名的 .sync-tmp 旧目标，srcPath 为源相对路径。
type fileTransfer struct {
	ctx     context.Context // NOSONAR: S8242 — 单文件传输全程不变的共享 ctx（S107 收敛），非请求侧驻留
	src     FS
	dst     FS
	job     *Job
	rec     func(FileResult)
	mu      *sync.Mutex
	srcE    *Entry
	d       *DiffEntry
	dstPath string
	tmpPath string
	srcPath string
}

// syncFileSmartPath 处理 merge3 三方合并与块级增量路径。返回 true 表示已通过本路径完成传输；
// false 表示不适用/出错，调用方回退整文件复制。
//
// 块级增量（roadmap 4.3 P2 v1）：overwrite 覆盖时若两端支持 BlockAccessor
// （本地 FS），对 src 与旧目标（tmpPath）做块 SHA-256 比对 → 只复制差异块到
// 新文件（省写放大；相同块跳过）。跨 FS 差异传输留 v2（分块基建衔接）。
//
// 三方合并（conflict_policy=merge3）：文本文件冲突时以 tmpPath 旧目标为 base、
// src 为 ours 做 diff3 合并；冲突段写标记文件 + 登记索引。二进制回退整文件复制。
//
// 保留败方副本（keep-both 防静默丢失）：覆盖型策略（overwrite/lww/merge3 二进制
// 回退）在覆盖前检测到目标与源**分歧改动**（内容不同）时，旧目标 tmpPath 不删除，
// 改为 `<dst>.conflict-<ts>` 副本保留 + 登记 ConflictRecord；无分歧（仅 mtime 变化）
// 不备份（防噪）。
func (e *Engine) syncFileSmartPath(f *fileTransfer) bool {
	if f.job.ConflictPolicy == ConflictMerge3 {
		if e.syncFileMerge3(f) {
			if err := f.dst.Delete(f.ctx, f.tmpPath); err != nil {
				e.logger().Warn(logCleanSyncTmp, "path", f.tmpPath, "error", err)
			}
			f.mu.Lock()
			f.job.Stats.FilesDone++
			f.job.Stats.BytesDone += f.srcE.Size
			f.mu.Unlock()
			return true
		}
		// merge3 不适用（二进制/读失败）→ 回退整文件复制（下面）。
		// 回退前保留败方：旧目标不再删除（副本化）。
		e.keepLoserIfDivergent(f)
	}
	if e.syncFileBlock(f) {
		if err := f.dst.Delete(f.ctx, f.tmpPath); err != nil {
			e.logger().Warn(logCleanSyncTmp, "path", f.tmpPath, "error", err)
		}
		f.mu.Lock()
		f.job.Stats.FilesDone++
		f.job.Stats.BytesDone += f.srcE.Size
		f.mu.Unlock()
		f.rec(FileResult{Path: f.dstPath, Action: f.d.Action, Size: f.srcE.Size, MTime: f.srcE.MTime, Checksum: f.srcE.Checksum})
		return true
	}
	// 块级路径失败（不满足条件/错误）：回退到下面整文件复制。
	// 整文件复制前同样先保留败方（仅首次；syncFileBlock 未动 tmpPath）。
	e.keepLoserIfDivergent(f)
	return false
}

// restoreTmp 在写入失败时恢复原目标（best-effort）。
func (e *Engine) restoreTmp(ctx context.Context, dst FS, dstPath, tmpPath string) {
	if tmpPath == "" {
		return
	}
	// 移除可能的半成品，再改名恢复原目标
	_ = dst.Delete(ctx, dstPath)
	if err := dst.Rename(ctx, tmpPath, dstPath); err != nil {
		e.logger().Warn("恢复原目标失败", "tmp", tmpPath, "dst", dstPath, "error", err)
	}
}

// keepLoserIfDivergent 在覆盖前保留败方副本（keep-both 防静默丢失）：
//
//   - 仅当备份开启（Engine.BackupLoser）且确实存在**分歧改动**时才动作
//     （防噪：无分歧不备份）；
//   - 分歧判定：目标条目（d.Dst）与源条目（srcE）内容不同。Checksum 均可得时用
//     checksum 比较；不可得时回落 mtime（不同 = 双方各自改动过）；size/mtime 均相同
//     视为无分歧。
//   - 动作：tmpPath（旧目标，同步前内容）rename 为 `<dst>.conflict-<ts>` 副本，
//     登记 ConflictRecord{Kind:"backup_loser"}，随后按 KeepMax 修剪同目录旧副本。
//
// 副本化后 tmpPath 不再存在，调用方后续的 `dst.Delete(ctx, tmpPath)` 幂等（本地
// Delete 对不存在路径静默成功；远程 Delete 对 404 视为已删成功）。merge3 二进制
// 回退与块级/整文件覆盖共用本路径。
func (e *Engine) keepLoserIfDivergent(f *fileTransfer) {
	if !e.BackupLoser {
		return
	}
	if f.tmpPath == "" || f.d.Dst == nil || f.d.Dst.IsDir {
		return
	}
	// 分歧判定（防噪）：目标与源内容不同才保留。checksum 可得时直接比较；
	// 不可得回落 mtime（不同 = 双方各自改动过）。
	if entriesSame(f.d.Dst, f.srcE) {
		return // 无分歧（仅 mtime/相同内容）→ 不备份
	}
	if err := f.ctx.Err(); err != nil {
		return
	}
	backupPath := backupNameFor(f.dstPath)
	if err := f.dst.Rename(f.ctx, f.tmpPath, backupPath); err != nil {
		e.logger().Warn("保留败方副本失败（旧目标将被覆盖删除）", "tmp", f.tmpPath, "backup", backupPath, "error", err)
		return
	}
	e.recordLoser(f.dstPath, backupPath, f.d, f.srcE)
	e.pruneBackups(f.ctx, f.dst, f.dstPath)
	e.logger().Info("覆盖型策略保留败方副本", "path", f.dstPath, "backup", backupPath)
}

// recordLoser 登记败方副本冲突记录（装配层注入 ConflictRecorder 时）。
func (e *Engine) recordLoser(dstPath, backupPath string, d *DiffEntry, srcE *Entry) {
	if e.ConflictRecorder == nil {
		return
	}
	baseSHA := ""
	if d.Dst != nil && d.Dst.Checksum != "" {
		baseSHA = d.Dst.Checksum
	}
	e.ConflictRecorder(ConflictRecord{
		Path:       dstPath,
		Kind:       "backup_loser",
		BackupPath: backupPath,
		BaseSHA:    baseSHA,
		OursSHA:    srcE.Checksum,
		Timestamp:  time.Now().UnixNano(),
	})
}

// backupNameFor 生成 `<dst>.conflict-<unixnano>` 副本名（与 conflict_rename 命名
// 对齐）。unixnano 单次同步内唯一（同一文件不会重复备份两次）。
func backupNameFor(dstPath string) string {
	return fmt.Sprintf("%s.conflict-%d", dstPath, time.Now().UnixNano())
}

// pruneBackups 修剪同目录 `<name>.conflict-*` 副本数量至 KeepMax（0 = 不限制）。
// 删除最旧（按名字节序 = unixnano 时间序）。best-effort：删失败仅告警。
func (e *Engine) pruneBackups(ctx context.Context, dst FS, dstPath string) {
	maxKeep := e.KeepMax
	if maxKeep <= 0 {
		return
	}
	dir := path.Dir(dstPath)
	name := path.Base(dstPath)
	entries, err := dst.ListDir(ctx, dir)
	if err != nil {
		e.logger().Warn("修剪副本：列目录失败", "dir", dir, "error", err)
		return
	}
	var backups []string
	prefix := name + ".conflict-"
	for _, ent := range entries {
		if strings.HasPrefix(ent.Name, prefix) {
			backups = append(backups, ent.Name)
		}
	}
	if len(backups) <= maxKeep {
		return
	}
	sort.Strings(backups) // 名字节序 = 时间序（unixnano 定宽）
	excess := len(backups) - maxKeep
	for i := range excess {
		rel := joinSlash(dir, backups[i])
		if err := dst.Delete(ctx, rel); err != nil {
			e.logger().Warn("修剪副本失败", "backup", rel, "error", err)
		}
	}
}

// syncDir 处理目录条目（仅空目录在 SyncEmptyDirs 开启时创建）。
func (e *Engine) syncDir(ctx context.Context, dst FS, job *Job, d *DiffEntry, dstPath string, rec func(FileResult)) {
	if !job.SyncEmptyDirs {
		rec(FileResult{Path: dstPath, Action: ActionSkipped})
		return
	}
	switch d.Action {
	case ActionConflictRenamed:
		if err := dst.Rename(ctx, dstPath, d.RenameDstTo); err != nil {
			rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("冲突改名失败: %v", err)})
			return
		}
	case ActionUpdated:
		// 目录覆盖文件：先删除冲突文件
		if d.Dst != nil && !d.Dst.IsDir {
			if err := dst.Delete(ctx, dstPath); err != nil {
				rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("删除冲突文件失败: %v", err)})
				return
			}
		}
	}
	if err := dst.MakeDir(ctx, dstPath); err != nil {
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("创建目录失败: %v", err)})
		return
	}
	rec(FileResult{Path: dstPath, Action: d.Action})
}

// syncFileBlock 块级增量复制：src 与旧目标（tmpPath，overwrite 已改名移走）做
// 块 SHA-256 比对，只把差异块写入 dstPath（新文件，预分配 src 大小）。
//
// 返回 true = 块级路径成功完成；false = 不满足块级条件（任一端非 BlockAccessor
// 或旧目标不存在）或块级出错——调用方回退整文件复制。
//
// 复用分块基建：块校验和算法（blockChecksum SHA-256）与分块上传 ChunkChecksums
// 一致；块大小默认 1MiB（与分块上传 chunkSize 语义同粒度）。跨 FS（远程目标）
// 的差异块传输（服务端按块表只收差异块）留 v2，见 blockdiff.go 头注释。
func (e *Engine) syncFileBlock(f *fileTransfer) bool {
	srcBA, srcOK := f.src.(BlockAccessor)
	dstBA, dstOK := f.dst.(BlockAccessor)
	if !srcOK || !dstOK {
		return false // 任一端不支持块访问 → 回退整文件复制（零回归）
	}

	// 打开旧目标（tmpPath 已改名存在）与源文件。
	srcR, srcClose, err := srcBA.OpenReaderAt(f.ctx, f.srcPath)
	if err != nil {
		return false
	}
	if srcClose != nil {
		defer srcClose.Close()
	}
	dstR, dstClose, err := dstBA.OpenReaderAt(f.ctx, f.tmpPath)
	if err != nil {
		return false // 旧目标不存在/不可读 → 回退整文件（全量覆盖）
	}
	if dstClose != nil {
		defer dstClose.Close()
	}

	// 块比对：src vs 旧目标 → 差异块索引。
	diffs, err := BlockDiff(srcR, dstR, defaultBlockSize)
	if err != nil {
		e.logger().Warn("块级比对失败，回退整文件复制", "path", f.srcPath, "error", err)
		return false
	}

	// 无差异（内容一致，仅 mtime 等变化）：仍需建 dstPath 并保留内容——回退
	// 整文件复制（BlockDiff 相同 = 无需写，但 overwrite 语义要求新文件存在）。
	if len(diffs) == 0 {
		// 全相同：直接复制整文件成本低（等价旧内容），回退走原路径最稳妥。
		return false
	}

	// 差异块写入新文件：预分配 src 大小后，**相同块从旧目标拷入、差异块从源拷入**——
	// 相同块不读源（省源读取/网络；跨 FS 时即「只传差异块」的基础）。
	wr, wrClose, err := dstBA.OpenWriterAt(f.ctx, f.dstPath, f.srcE.Size, f.srcE.MTime)
	if err != nil {
		e.logger().Warn("块级写入打开失败，回退整文件复制", "path", f.dstPath, "error", err)
		return false
	}
	if wrClose != nil {
		// 失败路径由 defer 兜底关闭；成功后置 nil 防双 Close（f.Close 第二次报 ErrInvalid）。
		defer func() {
			if wrClose != nil {
				_ = wrClose.Close()
			}
		}()
	}
	if !e.writeBlockDiffs(f, srcR, dstR, wr, diffs) {
		return false
	}
	if err := wrClose.Close(); err != nil {
		return false
	}
	wrClose = nil // 已显式关闭：defer 跳过（防双 Close）
	nBlocks := (f.srcE.Size + defaultBlockSize - 1) / defaultBlockSize
	e.logger().Info("块级增量复制完成", "path", f.srcPath, "diff_blocks", len(diffs), "total_blocks", nBlocks)
	return true
}

// writeBlockDiffs 把差异块写入新文件：差异块从源读、相同块从旧目标读（相同块不读源，
// 省源读取/网络；跨 FS 时即「只传差异块」的基础）。任一读写失败返回 false（调用方回退整文件复制）。
func (e *Engine) writeBlockDiffs(f *fileTransfer, srcR, dstR io.ReaderAt, wr io.WriterAt, diffs []int) bool {
	size := f.srcE.Size
	buf := make([]byte, defaultBlockSize)
	// 差异块索引 → 集合（相同块 = 非差异块）。
	diffSet := make(map[int]bool, len(diffs))
	for _, idx := range diffs {
		diffSet[idx] = true
	}
	nBlocks := (size + defaultBlockSize - 1) / defaultBlockSize
	for i := range nBlocks {
		offset := i * defaultBlockSize
		length := blockLen(offset, size)
		if length <= 0 {
			continue
		}
		if !e.readDiffBlock(buf[:length], offset, diffSet[int(i)], f.srcPath, f.tmpPath, srcR, dstR) {
			return false
		}
		if _, err := wr.WriteAt(buf[:length], offset); err != nil {
			e.logger().Warn("块级写目标失败，回退整文件复制", "path", f.dstPath, "offset", offset, "error", err)
			return false
		}
	}
	return true
}

// blockLen 返回偏移 offset 处一块的实际长度（末块可能不足 defaultBlockSize）。
func blockLen(offset, size int64) int64 {
	length := defaultBlockSize
	if offset+length > size {
		length = size - offset
	}
	return length
}

// readDiffBlock 读取单个块到 buf：差异块从源读、相同块从旧目标读。失败返回 false。
func (e *Engine) readDiffBlock(buf []byte, offset int64, isDiff bool, srcPath, tmpPath string, srcR, dstR io.ReaderAt) bool {
	if isDiff {
		// 差异块：从源读。
		if _, err := srcR.ReadAt(buf, offset); err != nil {
			e.logger().Warn("块级读源失败，回退整文件复制", "path", srcPath, "offset", offset, "error", err)
			return false
		}
		return true
	}
	// 相同块：从旧目标读（不读源；跨 FS 时免网络传输）。
	if _, err := dstR.ReadAt(buf, offset); err != nil {
		e.logger().Warn("块级读旧目标失败，回退整文件复制", "path", tmpPath, "offset", offset, "error", err)
		return false
	}
	return true
}
