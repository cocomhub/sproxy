// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package tempclean 统一清理 sproxy 在系统临时目录产生的**孤儿残留**（唯一实现）。
//
// 背景（实测 2026-10-31）：%TEMP% 下累积了数百个 `sproxy-secretdata-*` 空目录、
// `sproxy-sig-body-*`（客户端签名临时文件）与 `multipart-*`（服务端 multipart 落盘文件）。
// 根因分两类：
//   - 由 Close/defer 清理，但**进程被强杀**（WSL 崩溃 / taskkill）或装配失败时不执行；
//   - `r.ParseMultipartForm` 落盘的临时文件 Go 标准库**从不自动删**（此前全仓无 RemoveAll）。
//
// 本包提供「启动 + 周期」两用的清理入口：只删**超龄**且**属于 sproxy 前缀**的直接子项，
// 失败一律忽略（绝不影响主流程）。
package tempclean

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 固定父目录名（与本包外的创建方保持一致）。
const (
	// RootSecretdata 是 secretdata 卷本地临时目录的固定父目录。
	RootSecretdata = "sproxy-secretdata"
	// RootClient 是客户端签名临时文件的固定父目录。
	RootClient = "sproxy-client"
)

// legacyPrefixes 是历史实现散落在系统临时目录根下的前缀（需继续清理）。
var legacyPrefixes = []string{
	RootSecretdata + "-", // 旧实现：<Temp>/sproxy-secretdata-<rand>
	"sproxy-sig-body-",   // 旧实现：<Temp>/sproxy-sig-body-<rand>
	"multipart-",         // Go 标准库 multipart 落盘（RemoveAll 缺失的历史残留）
}

// DefaultMaxAge 是孤儿判定阈值。活跃进程会持续刷新 mtime（写临时文件），
// 取 6h 以避开「长时间运行但恰好空闲」的误删风险。
const DefaultMaxAge = 6 * time.Hour

// DefaultInterval 是周期清理间隔。
const DefaultInterval = time.Hour

// Report 是一次清理的结果（用于日志/指标）。
type Report struct {
	Removed int
	Scanned int
	// FreedBytes 是本次删除的**磁盘字节数**。配额/容量账本对账需要它：残留临时文件
	// 占盘但不被任何卷账本统计，是「账实不符」的来源之一（清理后可据此修正/告警）。
	FreedBytes int64
}

// CleanupStale 清理系统临时目录下**超龄**的 sproxy 残留（best-effort）。
// maxAge <= 0 时用 DefaultMaxAge。只处理系统临时目录的**直接子项**（不递归任意路径），
// 且必须命中已知前缀/固定父目录，避免误删其它程序的文件。
func CleanupStale(maxAge time.Duration) Report {
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	cutoff := time.Now().Add(-maxAge)
	root := os.TempDir()
	rep := Report{}
	entries, err := os.ReadDir(root)
	if err != nil {
		return rep
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		switch {
		case name == RootSecretdata || name == RootClient:
			rep.merge(cleanupDirAged(path, cutoff, true))
		case matchesLegacy(name):
			rep.merge(cleanupDirAged(path, cutoff, false))
		}
	}
	return rep
}

func matchesLegacy(name string) bool {
	for _, p := range legacyPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// cleanupDirAged 清理单一路径：recursive=true 时对其**直接子项**逐个按 mtime 判定
// （用于固定父目录）；false 时按路径自身 mtime 判定（用于历史散落项）。
func cleanupDirAged(path string, cutoff time.Time, recursive bool) Report {
	rep := Report{Scanned: 1}
	if !recursive {
		if fi, err := os.Stat(path); err == nil && fi.ModTime().Before(cutoff) {
			size := dirSize(path)
			if os.RemoveAll(path) == nil {
				rep.Removed++
				rep.FreedBytes += size
			}
		}
		return rep
	}
	children, err := os.ReadDir(path)
	if err != nil {
		return rep
	}
	for _, c := range children {
		p := filepath.Join(path, c.Name())
		fi, ierr := c.Info()
		if ierr != nil || !fi.ModTime().Before(cutoff) {
			continue
		}
		rep.Scanned++
		size := dirSize(p)
		if os.RemoveAll(p) == nil {
			rep.Removed++
			rep.FreedBytes += size
		}
	}
	return rep
}

func (r *Report) merge(o Report) {
	r.Removed += o.Removed
	r.Scanned += o.Scanned
	r.FreedBytes += o.FreedBytes
}

// dirSize 返回路径的磁盘占用（文件=size；目录=递归求和；错误忽略）。
func dirSize(path string) int64 {
	var total int64
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	_ = filepath.WalkDir(path, func(_ string, d os.DirEntry, werr error) error {
		if werr != nil || d.IsDir() {
			return nil
		}
		if info, ierr := d.Info(); ierr == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// Start 周期执行 CleanupStale（立即先跑一次），返回停止函数（幂等）。
// ctx 取消或调用 stop 都会终止；清理失败只记 Warn，不影响调用方。
func Start(ctx context.Context, log *slog.Logger, interval, maxAge time.Duration) (stop func()) {
	if interval <= 0 {
		interval = DefaultInterval
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	runOnce := func() {
		rep := CleanupStale(maxAge)
		if rep.Removed > 0 && log != nil {
			log.Info("tempclean: 已清理孤儿临时项",
				"removed", rep.Removed, "scanned", rep.Scanned, "freed_bytes", rep.FreedBytes)
		}
	}
	runOnce()
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-done:
				return
			case <-t.C:
				runOnce()
			}
		}
	}()
	var once = make(chan struct{}, 1)
	return func() {
		select {
		case once <- struct{}{}:
			close(done)
		default:
		}
	}
}
