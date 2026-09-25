// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package files

// dup_report.go 是重复文件发现（roadmap 11.5-⑦，设计文档
// docs/designs/2026-09-24-duplicate-finder.md）的领域实现：
//
//   - **台账快照**（`ReportFromLedger`）：复用 DedupStore 台账（checksum → 引用列表），
//     秒级输出同内容文件组；仅 dedup.enabled 时台账存在（否则空报告 + 提示语义）；
//   - **全量扫描**（`ScanVolume`）：遍历卷 user 桶（跳过 symlink 与 meta/ 等非用户桶），
//     逐文件 SHA-256（checksum.Reader，与全仓巡检同源）→ 按 checksum 分组 → 过滤
//     refs<2 → `DuplicateBytes = Σ size×(refs-1)`（可回收空间）。
//
// 数据流：walk → 逐文件哈希 → 分组 → 过滤 → savings。单文件读失败记 `ScanError`
// 继续（报告不因单文件失败中断）；ctx 取消/超时返回已扫部分 + `Truncated=true`；
// 卷根不存在/不可用 fail-fast 明确错误。只读操作：不改文件不动台账。
//
// 硬链接语义：scan 按内容哈希判定，硬链文件内容相同 → 视为重复，与去重语义一致
// （设计文档风险节注明）。

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// DupRef 是重复组内单个文件引用：所在卷 + 租户根内相对路径（user/...）。
type DupRef struct {
	Volume string `json:"volume"`
	Rel    string `json:"rel"`
	// ModTime 是文件修改时间（UnixNano）；台账模式恒 0（台账不存 mtime，扫描模式填充）。
	ModTime int64 `json:"mod_time"`
}

// DupGroup 是同一个 checksum 的重复文件组（refs ≥ 2）。
type DupGroup struct {
	Checksum string   `json:"checksum"`
	Size     int64    `json:"size"`
	Refs     []DupRef `json:"refs"`
}

// ScanError 是扫描单文件失败记录（不中断整体报告）。
type ScanError struct {
	Rel    string `json:"rel"`
	Reason string `json:"reason"`
}

// Report 是重复文件发现报告。
type Report struct {
	Groups         []DupGroup  `json:"groups"`
	ScannedFiles   int         `json:"scanned_files"`
	TotalBytes     int64       `json:"total_bytes"`
	DuplicateBytes int64       `json:"duplicate_bytes"` // 可回收空间 = Σ size×(refs-1)
	Truncated      bool        `json:"truncated,omitempty"`
	Errors         []ScanError `json:"errors,omitempty"`
}

// ScanOptions 是 ScanVolume 的选项。
type ScanOptions struct {
	// Root 是待扫描的卷租户根（Tenant.Root()）。必须非 nil（fail-fast）。
	Root *storage.Root
	// Volume 是引用中记录的卷名（缺省 "" = 单卷/默认卷）。
	Volume string
	// Subdir 是 user 桶内相对子目录（缺省 "" = user 桶根）。
	Subdir string
}

// dupScanSkipDir 是 scan 跳过/收纳的顶层目录判定：只扫 user 桶；meta/cloud/archive/
// chunk/version/trash 与遗留 .__ 魔法目录一律跳过。
func dupScanSkipDir(name string) bool {
	if strings.HasPrefix(name, ".__") || strings.HasPrefix(name, "__") {
		return true
	}
	switch name {
	case "user":
		return false
	case "meta", "cloud", "archive", "chunk", "version", "trash":
		return true
	default:
		// 旧布局平铺文件（无桶结构）：按用户文件计入（与 stats/du 语义一致）。
		return false
	}
}

// ReportFromLedger 从 dedup 台账（per-tenant）生成重复报告（instant，仅 dedup 启用时
// 台账非空）。refs≥2 才输出。**台账不存文件 size/mtime**（dedupRef 仅 volume+rel），
// 且签名无 Root 入参（设计文档固定），故 Size/DuplicateBytes 在台账模式为 0——
// 可回收空间的精确值走 scan 模式（文档局限，测试按此断言）。
// 台账为 nil（未启用/未装配）返回空报告（nil error）——沿用 dedup 关闭时台账为空的语义。
func ReportFromLedger(ds *DedupStore) (*Report, error) {
	rep := &Report{Groups: []DupGroup{}}
	if ds == nil {
		return rep, nil
	}
	snap := ds.exportSnapshot()
	for checksum, refs := range snap {
		if len(refs) < 2 {
			continue // refs<2 过滤（变异点：漏过滤 → 唯一文件成组 → 红）
		}
		rep.Groups = append(rep.Groups, DupGroup{
			Checksum: checksum,
			Refs:     refs,
		})
	}
	// 组按 checksum 排序（稳定输出，与 scan 同规则）。
	sortDupGroups(rep.Groups)
	return rep, nil
}

// ScanVolume 遍历卷 user 桶并生成重复报告。
func ScanVolume(ctx context.Context, _ any, opts ScanOptions) (*Report, error) {
	rep := &Report{Groups: []DupGroup{}}
	if opts.Root == nil {
		return nil, errors.New("duplicate scan: 卷根未装配（Root 不能为 nil）")
	}
	root := opts.Root
	userRoot := "user"
	if opts.Subdir != "" {
		if err := validateDupSubdir(opts.Subdir); err != nil {
			return nil, fmt.Errorf("duplicate scan: 子目录非法: %w", err)
		}
		userRoot = filepath.ToSlash(filepath.Join("user", opts.Subdir))
	}
	groups := map[string]*DupGroup{}
	var walk func(rel string) error
	walk = func(rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := root.ReadDir(rel)
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			name := e.Name()
			child := filepath.ToSlash(filepath.Join(rel, name))
			if e.IsDir() {
				if dupScanSkipDir(name) {
					continue
				}
				if err := walk(child); err != nil {
					return err
				}
				continue
			}
			// symlink 不跟随（DirEntry.Type()&ModeSymlink）：不 stat 目标，不计入扫描。
			if e.Type()&os.ModeSymlink != 0 {
				continue
			}
			info, ierr := e.Info()
			if ierr != nil {
				rep.Errors = append(rep.Errors, ScanError{Rel: child, Reason: ierr.Error()})
				continue
			}
			size := info.Size()
			f, oerr := root.Open(child)
			if oerr != nil {
				rep.Errors = append(rep.Errors, ScanError{Rel: child, Reason: oerr.Error()})
				continue
			}
			cs, herr := checksum.Reader(f)
			_ = f.Close()
			if herr != nil {
				rep.Errors = append(rep.Errors, ScanError{Rel: child, Reason: herr.Error()})
				continue
			}
			rep.ScannedFiles++
			rep.TotalBytes += size
			g, ok := groups[cs]
			if !ok {
				g = &DupGroup{Checksum: cs, Size: size}
				groups[cs] = g
			}
			g.Refs = append(g.Refs, DupRef{Volume: opts.Volume, Rel: child, ModTime: info.ModTime().UnixNano()})
		}
		return nil
	}
	if err := walk(userRoot); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			rep.Truncated = true
			return rep, nil
		}
		return nil, err
	}
	for _, g := range groups {
		if len(g.Refs) < 2 {
			continue
		}
		rep.Groups = append(rep.Groups, *g)
		rep.DuplicateBytes += g.Size * int64(len(g.Refs)-1)
	}
	// 组按 checksum 排序（稳定输出）。
	sortDupGroups(rep.Groups)
	return rep, nil
}

// sortDupGroups 按 checksum 字典序稳定排序组（scan 与 ledger 共用，输出可 diff）。
func sortDupGroups(groups []DupGroup) {
	for i := range groups {
		for j := i + 1; j < len(groups); j++ {
			if groups[j].Checksum < groups[i].Checksum {
				groups[i], groups[j] = groups[j], groups[i]
			}
		}
	}
}

// validateDupSubdir 是子目录校验的窄校验（拒绝穿越/绝对路径；subdir 由调用方已归一，
// 此处按 pathguard 语义轻量拒绝 .. / 绝对路径）。
func validateDupSubdir(p string) error {
	if p == "" {
		return nil
	}
	if strings.Contains(p, "..") {
		return errors.New("非法路径")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
		return errors.New("非法路径")
	}
	return nil
}
