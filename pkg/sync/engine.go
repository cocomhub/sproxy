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

	if err := ctx.Err(); err != nil {
		job.Status = StatusCancelled
		return err
	}
	job.Status = StatusCompleted
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
