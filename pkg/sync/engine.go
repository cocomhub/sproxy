// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package sync

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Engine 编排一次同步。
type Engine struct {
	Concurrency int // 多文件并发数；0 或负数回落 3
	Logger      *slog.Logger
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
	dstPathOf := func(d *DiffEntry) string {
		return joinSlash(job.Dst, stripRootPrefix(d.Path, job.Src))
	}

	var fileTransfers []*DiffEntry
	for i := range diffs {
		d := &diffs[i]
		if d.Src != nil && d.Src.IsSymlink {
			rec(FileResult{Path: dstPathOf(d), Action: ActionSkippedSymlink})
			continue
		}
		switch d.Action {
		case ActionCreated, ActionUpdated, ActionConflictRenamed:
			if d.Src != nil && d.Src.IsDir {
				e.syncDir(ctx, dst, job, d, dstPathOf(d), rec)
			} else {
				fileTransfers = append(fileTransfers, d)
			}
		case ActionSkipped:
			if d.Src != nil {
				rec(FileResult{Path: dstPathOf(d), Action: ActionSkipped, Size: d.Src.Size, MTime: d.Src.MTime, Checksum: d.Src.Checksum})
			}
		case ActionSkippedConflict:
			if d.Src != nil {
				rec(FileResult{Path: dstPathOf(d), Action: ActionSkippedConflict, Size: d.Src.Size, MTime: d.Src.MTime, Checksum: d.Src.Checksum})
			}
		case ActionError:
			errMsg := ""
			if d.Err != nil {
				errMsg = d.Err.Error()
			}
			rec(FileResult{Path: dstPathOf(d), Action: ActionError, Error: errMsg})
		}
	}

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
	concurrency := e.concurrency()
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, d := range fileTransfers {
		// 审查 R1：进入 select 前先查 ctx，避免取消后 select 仍随机选到 sem 分支
		// 启动本已取消的传输（LocalFS 不查 ctx 会写完再返回）。
		if err := ctx.Err(); err != nil {
			rec(FileResult{Path: dstPathOf(d), Action: ActionError, Error: err.Error()})
			continue
		}
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				rec(FileResult{Path: dstPathOf(d), Action: ActionError, Error: ctx.Err().Error()})
				return
			}
			defer func() { <-sem }()
			e.syncFile(ctx, src, dst, job, d, rec, &mu)
		})
	}
	wg.Wait()

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
		dstPath := joinSlash(job.Dst, stripRootPrefix(d.Path, job.Dst))
		switch d.Action {
		case ActionDeleted:
			if err := dst.Delete(ctx, dstPath); err != nil {
				rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("删除目标失败: %v", err)})
				continue
			}
			rec(FileResult{Path: dstPath, Action: ActionDeleted})
			job.Stats.FilesDeleted++
		case ActionError:
			errMsg := ""
			if d.Err != nil {
				errMsg = d.Err.Error()
			}
			rec(FileResult{Path: dstPath, Action: ActionError, Error: errMsg})
		}
	}
	return nil
}

// syncFile 传输单个文件。
func (e *Engine) syncFile(ctx context.Context, src, dst FS, job *Job, d *DiffEntry, rec func(FileResult), mu *sync.Mutex) {
	srcE := d.Src
	dstPath := joinSlash(job.Dst, stripRootPrefix(d.Path, job.Src))

	var tmpPath string
	if d.Action == ActionUpdated && (job.ConflictPolicy == ConflictOverwrite || job.ConflictPolicy == ConflictLWW) {
		// 拒绝用文件覆盖同名目录（审查 I-2）：Rename 目标为 .sync-tmp 会把非空目录整体
		// "移走"并残留幽灵目录（os.Remove 删不了非空目录）。类型冲突由 diff 层已显式
		// 判定（src 文件 vs dst 目录），此处兜底拒绝并报明确错误。
		if d.Dst != nil && d.Dst.IsDir {
			rec(FileResult{Path: dstPath, Action: ActionError, Error: "拒绝用文件覆盖同名目录"})
			return
		}
		// overwrite/lww 覆盖：先把目标改名到 .sync-tmp，再写新文件，成功后删除 tmp。
		// 注意（审查 R4）：.sync-tmp 是保留后缀，源树中若同时含 a.txt 与 a.txt.sync-tmp
		// 可能互相踩踏；case-insensitive 的远程 FS 上更明显，文档已标注。
		tmpPath = dstPath + ".sync-tmp"
		if err := dst.Rename(ctx, dstPath, tmpPath); err != nil {
			rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("重命名目标到临时名失败: %v", err)})
			return
		}
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
	// 审查 R5：本地 os 写入假设可靠；远程 HTTPTransport 建议在 WriteFile 后按需校验
	// 目标 checksum，防截断/损坏静默落盘（分块管线 ChunkedUpload 已逐块校验，简单
	// Upload 走 multipart 全量校验，故本地阶段无需额外校验）。
	// 块级增量（roadmap 4.3 P2 v1）：overwrite 覆盖时若两端支持 BlockAccessor
	// （本地 FS），对 src 与旧目标（tmpPath）做块 SHA-256 比对 → 只复制差异块到
	// 新文件（省写放大；相同块跳过）。跨 FS 差异传输留 v2（分块基建衔接）。
	if tmpPath != "" && d.Action == ActionUpdated && d.Dst != nil && !d.Dst.IsDir {
		if e.syncFileBlock(ctx, src, dst, dstPath, tmpPath, d.Path, srcE.Size, srcE.MTime) {
			if err := dst.Delete(ctx, tmpPath); err != nil {
				e.logger().Warn("清理 sync-tmp 失败", "path", tmpPath, "error", err)
			}
			mu.Lock()
			job.Stats.FilesDone++
			job.Stats.BytesDone += srcE.Size
			mu.Unlock()
			rec(FileResult{Path: dstPath, Action: d.Action, Size: srcE.Size, MTime: srcE.MTime, Checksum: srcE.Checksum})
			return
		}
		// 块级路径失败（不满足条件/错误）：回退到下面整文件复制。
	}
	werr := dst.WriteFile(ctx, dstPath, rc, srcE.Size, srcE.MTime)
	_ = rc.Close()
	if werr != nil {
		e.restoreTmp(ctx, dst, dstPath, tmpPath)
		rec(FileResult{Path: dstPath, Action: ActionError, Error: fmt.Sprintf("写入目标失败: %v", werr)})
		return
	}
	if tmpPath != "" {
		if err := dst.Delete(ctx, tmpPath); err != nil {
			e.logger().Warn("清理 sync-tmp 失败", "path", tmpPath, "error", err)
		}
	}
	mu.Lock()
	job.Stats.FilesDone++
	// 审查 R2：按源条目 size 记账；源文件在 diff 后、传输中被并发修改时统计会失真
	// （本地 io.Copy 实际写入字节未知）。对进度展示可接受，文档已标注。
	job.Stats.BytesDone += srcE.Size
	mu.Unlock()
	rec(FileResult{Path: dstPath, Action: d.Action, Size: srcE.Size, MTime: srcE.MTime, Checksum: srcE.Checksum})
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
func (e *Engine) syncFileBlock(ctx context.Context, src, dst FS, dstPath, tmpPath, srcPath string, size, mtime int64) bool {
	srcBA, srcOK := src.(BlockAccessor)
	dstBA, dstOK := dst.(BlockAccessor)
	if !srcOK || !dstOK {
		return false // 任一端不支持块访问 → 回退整文件复制（零回归）
	}

	// 打开旧目标（tmpPath 已改名存在）与源文件。
	srcR, srcClose, err := srcBA.OpenReaderAt(ctx, srcPath)
	if err != nil {
		return false
	}
	if srcClose != nil {
		defer srcClose.Close()
	}
	dstR, dstClose, err := dstBA.OpenReaderAt(ctx, tmpPath)
	if err != nil {
		return false // 旧目标不存在/不可读 → 回退整文件（全量覆盖）
	}
	if dstClose != nil {
		defer dstClose.Close()
	}

	// 块比对：src vs 旧目标 → 差异块索引。
	diffs, err := BlockDiff(srcR, dstR, defaultBlockSize)
	if err != nil {
		e.logger().Warn("块级比对失败，回退整文件复制", "path", srcPath, "error", err)
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
	wr, wrClose, err := dstBA.OpenWriterAt(ctx, dstPath, size, mtime)
	if err != nil {
		e.logger().Warn("块级写入打开失败，回退整文件复制", "path", dstPath, "error", err)
		return false
	}
	if wrClose != nil {
		defer wrClose.Close()
	}
	buf := make([]byte, defaultBlockSize)
	// 差异块索引 → 集合（相同块 = 非差异块）。
	diffSet := make(map[int]bool, len(diffs))
	for _, idx := range diffs {
		diffSet[idx] = true
	}
	nBlocks := (size + defaultBlockSize - 1) / defaultBlockSize
	for i := range nBlocks {
		offset := i * defaultBlockSize
		length := defaultBlockSize
		if offset+length > size {
			length = size - offset
		}
		if length <= 0 {
			continue
		}
		if diffSet[int(i)] {
			// 差异块：从源读。
			if _, err := srcR.ReadAt(buf[:length], offset); err != nil {
				e.logger().Warn("块级读源失败，回退整文件复制", "path", srcPath, "offset", offset, "error", err)
				return false
			}
		} else {
			// 相同块：从旧目标读（不读源；跨 FS 时免网络传输）。
			if _, err := dstR.ReadAt(buf[:length], offset); err != nil {
				e.logger().Warn("块级读旧目标失败，回退整文件复制", "path", tmpPath, "offset", offset, "error", err)
				return false
			}
		}
		if _, err := wr.WriteAt(buf[:length], offset); err != nil {
			e.logger().Warn("块级写目标失败，回退整文件复制", "path", dstPath, "offset", offset, "error", err)
			return false
		}
	}
	if err := wrClose.Close(); err != nil {
		return false
	}
	e.logger().Info("块级增量复制完成", "path", srcPath, "diff_blocks", len(diffs), "total_blocks", nBlocks)
	return true
}
