// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

// defaultMetaCacheQuotaBytes 是精简 meta 缓存（L1a）目录的**默认磁盘配额**：2 GiB。
//
// 为什么必须有（实测 2026-11-13）：本地 metacache 曾增长到 **5.29 GB / 9,489 个文件**（无上限），
// 且单文件最大 86.98 MB —— 不设限会持续吃掉本地磁盘。
//
// 语义（遵守「配置默认值必须安全」）：**默认非零**，即默认即生效；把它设为负值才表示不限制
// （配额本身就提供安全默认，无需 `disable_*` 反向语义）。
const defaultMetaCacheQuotaBytes = 2 << 30

// metaCacheEviction 描述一次配额淘汰的结果。
type metaCacheEviction struct {
	RemovedFiles int
	FreedBytes   int64
}

// planMetaCacheEviction 是**纯函数**：给定现有文件（路径 + 大小 + mtime）与配额，
// 返回应删除的文件（**最旧优先**，按 mtime 升序）直到总量落到配额内。
//
// 抽为纯函数以便单测（无需真实磁盘）：配额语义是「超了就删最旧的」，与 LRU 不同 ——
// 缓存是**可重建**的加速器，按时间淘汰足够且无需维护访问序（零额外状态）。
func planMetaCacheEviction(files []metaCacheFile, quota int64) []metaCacheFile {
	if quota <= 0 {
		return nil // 负值 = 不限制
	}
	var total int64
	for _, f := range files {
		total += f.Size
	}
	if total <= quota {
		return nil
	}
	// 最旧优先（mtime 升序）；同 mtime 用路径保证确定性（便于测试与复现）。
	sort.Slice(files, func(i, j int) bool {
		if files[i].ModTime == files[j].ModTime {
			return files[i].Path < files[j].Path
		}
		return files[i].ModTime < files[j].ModTime
	})
	var out []metaCacheFile
	for _, f := range files {
		if total <= quota {
			break
		}
		out = append(out, f)
		total -= f.Size
	}
	return out
}

// metaCacheFile 是配额扫描的输入项。
type metaCacheFile struct {
	Path    string
	Size    int64
	ModTime int64
}

// enforceMetaCacheQuota 扫描缓存目录，超配额则按**最旧优先**删除；失败静默（缓存是加速器，
// 绝不影响主流程）。返回淘汰结果（供日志/测试）。
func (s *SecretdataFS) enforceMetaCacheQuota() metaCacheEviction {
	var res metaCacheEviction
	dir := s.opts.MetaCacheDir
	quota := s.opts.MetaCacheQuotaBytes.Bytes()
	if quota == 0 {
		quota = defaultMetaCacheQuotaBytes // 零值 ⇒ 用安全默认（默认即生效）
	}
	if dir == "" || quota < 0 {
		return res // 未装配 / 显式不限制（负值）
	}
	var files []metaCacheFile
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return nil
		}
		files = append(files, metaCacheFile{Path: p, Size: info.Size(), ModTime: info.ModTime().UnixNano()})
		return nil
	})
	for _, f := range planMetaCacheEviction(files, quota) {
		if rerr := os.Remove(f.Path); rerr == nil {
			res.RemovedFiles++
			res.FreedBytes += f.Size
		}
	}
	return res
}
