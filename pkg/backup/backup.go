// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package backup 提供通用备份引擎（源任意 sync.FS → 目标任意 sync.FS）：
// 全量/增量（manifest 比较 size+mtime）+ 每文件可选校验 + 单文件错误隔离与重试 + 报告。
// P1 只做纯函数引擎层，不涉及路由/CLI/装配（见
// docs/designs/2026-09-24-backup-remote-volume.md 的片划分）。
package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// ManifestRel 是目标卷内 manifest 的相对路径（设计文档：<backup>/manifest.json）。
const ManifestRel = "backup/manifest.json"

// Options 控制一次备份。
type Options struct {
	Concurrent int      // 并发传输文件数；<=0 回落默认 4
	Verify     bool     // 传输后对目标 Stat 校验 size（防截断/静默损坏落盘）
	MaxRetries int      // 单文件写入失败重试次数（指数退避 100ms×2ⁿ；总尝试 = 1+MaxRetries）
	Exclude    []string // 排除 glob 模式（path.Match 语义，对齐 pkg/sync 的 ParseFilters）
}

// concurrency 归一化并发数（默认 4）。
func (o Options) concurrency() int {
	if o.Concurrent <= 0 {
		return 4
	}
	return o.Concurrent
}

// FileError 是单文件失败记录（不中断整体备份）。
type FileError struct {
	Path string
	Err  error
}

// Report 是一次备份的结果报告。
type Report struct {
	Files     int         // 成功传输的文件数
	Bytes     int64       // 成功传输的字节数
	Skipped   int         // 因 manifest 命中（size+mtime 相同）或符号链接而跳过的文件数
	Failed    int         // 失败文件数（含校验失败与 manifest 写失败）
	Errors    []FileError // 失败明细
	Truncated bool        // ctx 取消/超时：仅部分完成
}

// manifestEntry 是 manifest 中单文件条目（rel → size+mtime）。
type manifestEntry struct {
	Size  int64 `json:"size"`
	MTime int64 `json:"mtime"`
}

// manifestFile 是目标卷 <backup>/manifest.json 的结构。
type manifestFile struct {
	Files map[string]manifestEntry `json:"files"`
}

// readManifest 读取目标现有 manifest；不存在 → 空；损坏 → Warn + 全量重扫（不中断备份）。
func readManifest(ctx context.Context, dst syncpkg.FS) map[string]manifestEntry {
	rc, err := dst.OpenRead(ctx, ManifestRel)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			slog.Warn("备份 manifest 读取失败，按全量重扫", "path", ManifestRel, "error", err)
		}
		return map[string]manifestEntry{}
	}
	defer rc.Close()
	var m manifestFile
	if err := json.NewDecoder(rc).Decode(&m); err != nil {
		slog.Warn("备份 manifest 损坏，按全量重扫", "path", ManifestRel, "error", err)
		return map[string]manifestEntry{}
	}
	if m.Files == nil {
		m.Files = map[string]manifestEntry{}
	}
	return m.Files
}

// writeManifest 原子写 manifest（tmp 写入 + Rename 替换，对齐 DedupStore.save 模式）。
func writeManifest(ctx context.Context, dst syncpkg.FS, files map[string]manifestEntry) error {
	m := manifestFile{Files: files}
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("序列化 manifest 失败: %w", err)
	}
	tmp := ManifestRel + ".tmp"
	if err := dst.WriteFile(ctx, tmp, bytes.NewReader(data), int64(len(data)), 0); err != nil {
		return fmt.Errorf("写 manifest 临时文件失败: %w", err)
	}
	if err := dst.Rename(ctx, tmp, ManifestRel); err != nil {
		return fmt.Errorf("原子替换 manifest 失败: %w", err)
	}
	return nil
}

// Run 执行一次备份：walk 源（ListDir 递归）→ manifest 比对（size+mtime 相同 skip）
// → OpenRead → dst.WriteFile（失败按 MaxRetries 指数退避重试）→ Verify 时对端 Stat
// 校验 size → 更新 manifest（原子写）→ 报告。
//
// 单文件失败不中止整体（记 Failed + FileError 继续）；ctx 取消/超时返回已拷贝部分
// 并标记 Truncated。符号链接跳过（按逻辑文件备份，见设计文档风险节）。
func Run(ctx context.Context, src, dst syncpkg.FS, opts Options) (*Report, error) {
	rep := &Report{}
	if err := ctx.Err(); err != nil {
		rep.Truncated = true
		return rep, nil
	}
	filters := syncpkg.ParseFilters(nil, opts.Exclude)
	entries, err := syncpkg.WalkEntries(ctx, src, "", true, false, filters)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			rep.Truncated = true
			return rep, nil
		}
		return nil, fmt.Errorf("枚举源失败: %w", err)
	}

	prev := readManifest(ctx, dst)
	next := map[string]manifestEntry{}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, opts.concurrency())
	w := &backupWorker{ctx: ctx, src: src, dst: dst, opts: opts, rep: rep, mu: &mu, prev: prev, next: next, sem: sem, wg: &wg}

	for i := range entries {
		e := entries[i]
		if !w.processBackupEntry(&e) {
			break
		}
	}
	wg.Wait()

	// ctx 取消/超时：无论各 worker 走了哪条路径（sem 抢占后 transferFile 返回 ctx 错误，
	// 或 select 走 ctx.Done 分支），统一标记 Truncated 并返回已拷贝部分。
	mu.Lock()
	if ctx.Err() != nil {
		rep.Truncated = true
	}
	mu.Unlock()
	if rep.Truncated {
		return rep, nil
	}
	if merr := writeManifest(ctx, dst, next); merr != nil {
		recordBackupResult(rep, &mu, 0, 1, FileError{Path: ManifestRel, Err: merr})
	}
	return rep, nil
}

// recordBackupResult 原子累加报告计数并追加单文件错误明细。
func recordBackupResult(rep *Report, mu *sync.Mutex, files, failed int, fe FileError) {
	mu.Lock()
	rep.Files += files
	rep.Failed += failed
	if fe.Path != "" || fe.Err != nil {
		rep.Errors = append(rep.Errors, fe)
	}
	mu.Unlock()
}

// manifestHit 判断 manifest 命中（size+mtime 相同 → 增量跳过）。
func manifestHit(prev map[string]manifestEntry, path string, size, mtime int64) (manifestEntry, bool) {
	pe, ok := prev[path]
	if !ok || pe.Size != size || pe.MTime != mtime {
		return manifestEntry{}, false
	}
	return pe, true
}

// backupWorker 是备份遍历的共享工作上下文（S107：收敛 processBackupEntry 等 11 参数
// 为结构体；ctx/src/dst/opts/rep/mu/next/sem/wg 在遍历全程不变）。
type backupWorker struct {
	//nolint:containedctx // S8242 已评估：单次 Run 作用域共享 ctx（遍历生命周期），非请求作用域——与下行 NOSONAR 双抑制
	ctx  context.Context // NOSONAR: S8242 — 单次 Run 的作用域共享 ctx（遍历生命周期），S107 收敛后的字段，非请求作用域
	src  syncpkg.FS
	dst  syncpkg.FS
	opts Options
	rep  *Report
	mu   *sync.Mutex
	prev map[string]manifestEntry
	next map[string]manifestEntry
	sem  chan struct{}
	wg   *sync.WaitGroup
}

// processBackupEntry 处理单个枚举条目：空目录补建 / 符号链接跳过 / manifest 命中跳过 /
// 其余异步传输。返回 false 表示 ctx 已取消（调用方终止遍历）。
func (w *backupWorker) processBackupEntry(e *syncpkg.Entry) bool {
	ctx := w.ctx
	if err := ctx.Err(); err != nil {
		w.mu.Lock()
		w.rep.Truncated = true
		w.mu.Unlock()
		return false
	}
	if e.IsDir {
		// 空目录：在目标补建（WriteFile 已隐式创建父目录，这里只处理叶子空目录）。
		if derr := w.dst.MakeDir(ctx, e.Path); derr != nil {
			recordBackupResult(w.rep, w.mu, 0, 1, FileError{Path: e.Path, Err: fmt.Errorf("创建目录失败: %w", derr)})
		}
		return true
	}
	if e.IsSymlink {
		w.mu.Lock()
		w.rep.Skipped++
		w.mu.Unlock()
		return true
	}
	if pe, ok := manifestHit(w.prev, e.Path, e.Size, e.MTime); ok {
		// manifest 命中（size+mtime 相同）→ 增量跳过；条目保留到新 manifest。
		// 必须持锁：并发 worker goroutine 也会写 w.next（不持锁 = concurrent map writes
		// → 运行时 fatal 直接终止进程）。
		w.mu.Lock()
		w.next[e.Path] = pe
		w.rep.Skipped++
		w.mu.Unlock()
		return true
	}
	w.startBackupTransfer(e)
	return true
}

// startBackupTransfer 异步传输单个文件：抢占并发信号量 → transferFile → 记录结果。
func (w *backupWorker) startBackupTransfer(e *syncpkg.Entry) {
	rel := e.Path
	w.wg.Go(func() {
		select {
		case w.sem <- struct{}{}:
		case <-w.ctx.Done():
			w.mu.Lock()
			w.rep.Truncated = true
			w.mu.Unlock()
			return
		}
		defer func() { <-w.sem }()
		if ferr := transferFile(w.ctx, w.src, w.dst, rel, e.Size, e.MTime, w.opts); ferr != nil {
			recordBackupResult(w.rep, w.mu, 0, 1, FileError{Path: rel, Err: ferr})
			return
		}
		w.mu.Lock()
		w.rep.Files++
		w.rep.Bytes += e.Size
		w.next[rel] = manifestEntry{Size: e.Size, MTime: e.MTime}
		w.mu.Unlock()
	})
}

// transferFile 传输单个文件：OpenRead → WriteFile（按 MaxRetries 指数退避重试）→
// Verify 时对端 Stat 校验 size。返回错误即该文件失败。
//
// 校验失败不重试：写入本身已成功，重试只会重复同一失败（篡改/持续损坏场景无意义）。
func transferFile(ctx context.Context, src, dst syncpkg.FS, rel string, size, mtime int64, opts Options) error {
	if err := transferWithRetry(ctx, src, dst, rel, size, mtime, opts.MaxRetries); err != nil {
		return err
	}
	if opts.Verify {
		return verifyTransferred(ctx, dst, rel, size)
	}
	return nil
}

// transferWithRetry 带指数退避重试的写入循环：总尝试 = 1+MaxRetries。
func transferWithRetry(ctx context.Context, src, dst syncpkg.FS, rel string, size, mtime int64, maxRetries int) error {
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if werr := writeOnce(ctx, src, dst, rel, size, mtime); werr != nil {
			lastErr = werr
			if attempt == maxRetries {
				break
			}
			if werr := retryBackoffWait(ctx, attempt); werr != nil {
				return werr
			}
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return fmt.Errorf("写入目标失败: %w", lastErr)
	}
	return nil
}

// retryBackoffWait 指数退避等待（100ms×2ⁿ），ctx 取消时立即返回停止原因。
func retryBackoffWait(ctx context.Context, attempt int) error {
	d := time.Duration(100<<uint(attempt)) * time.Millisecond // 100ms×2ⁿ
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// verifyTransferred Verify 校验：对端 Stat 比对 size（防截断/静默损坏落盘）。
func verifyTransferred(ctx context.Context, dst syncpkg.FS, rel string, size int64) error {
	se, verr := dst.Stat(ctx, rel)
	if verr != nil {
		return fmt.Errorf("校验失败: %w", verr)
	}
	var got int64
	if se != nil {
		got = se.Size
	}
	if got != size {
		return fmt.Errorf("校验失败（目标大小 %d != 源 %d）", got, size)
	}
	return nil
}

// writeOnce 单次写入尝试：每次重试重新打开源（前一次失败可能已耗尽 reader）。
func writeOnce(ctx context.Context, src, dst syncpkg.FS, rel string, size, mtime int64) error {
	rc, err := src.OpenRead(ctx, rel)
	if err != nil {
		return fmt.Errorf("打开源文件失败: %w", err)
	}
	defer rc.Close()
	return dst.WriteFile(ctx, rel, rc, size, mtime)
}
