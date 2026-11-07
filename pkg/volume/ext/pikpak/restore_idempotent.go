// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// restore_idempotent.go 是「转存幂等（复用既有副本）」的**唯一实现**：
// 旧下载器（PikpakDownloader.restoreAndDownload）与 hybrid
// （HybridDownloader.restoreAndLink / restoreForAccount）共用同一判据。
//
// 收敛原因（用户裁定 2026-10-27）：此前只有 hybrid 有幂等前置（findInPackShared），
// 旧下载器每次都 RestoreShare —— 匿名路径失败 fallback 到旧下载器时，同一文件
// 多次提交会累积同名副本（实测同一 GCID 3 份），撑爆免费账号 6GB 空间。
//
// 判据（与 hybrid 原有逻辑一致，零回归）：
//  1. 优先「Pack From Shared」文件夹内 size/name 匹配（转存副本固定落这里，不全盘 walk）；
//  2. miss 回退 FindInDrive 全盘（强判据：hash 双方都有且一致才复用）；
//  3. hash 双方都有且不等 → 拒绝复用（防内容错配）；
//  4. hash 缺失 → 降级按「同名+同大小」复用（弱判据，防撑爆空间）。
//
// 返回值 second 为是否「restore 副本」（位于 Pack From Shared）——调用方据此
// 决定是否登记 AutoDelete（用户自有同内容文件不可删，NH-P1）。
package pikpak

import (
	"context"
	"fmt"
	"strings"

	"github.com/cocomhub/sproxy/pkg/downloader"
)

// restoreIdempotentAPI 是幂等查找所需的最小 API 面（hybrid/旧下载器各自持有的 *API 都满足）。
type restoreIdempotentAPI interface {
	// List 列出指定文件夹下条目（根 = ""）。
	List(ctx context.Context, parentID string) ([]FileMeta, error)
	// FindByID 按 ID 查文件元信息（parent 定位用）。
	FindByID(ctx context.Context, id string) (*FileMeta, error)
	// ListRecursive 递归列出网盘全部（FindInDrive 兜底用）。
	ListRecursive(ctx context.Context, parentID string) ([]FileMeta, error)
}

// findRestoreCopy 在网盘里找「已存在的同名同大小（hash 可选一致）转存副本」。
// 返回 (FileMeta, isRestoreCopy, ok)：ok=false = 未找到（调用方走 RestoreShare）。
// isRestoreCopy=true = 该文件位于「Pack From Shared」（restore 副本，可登记 AutoDelete）；
// false = 用户自有同内容文件（只复用、绝不登记删除）。
func findRestoreCopy(ctx context.Context, api restoreIdempotentAPI, wantName string, wantSize int64, wantHash string) (*FileMeta, bool, bool) {
	if existing, found := findInPackShared(ctx, api, wantName, wantSize); found && existing != nil {
		return existing, isRestoreCopyFolder(ctx, api, existing), true
	}
	// 全盘兜底：**强判据**——hash 双方都必须存在且相等才复用。
	// 与 pack 路径的弱判据（同名同大小）不同：全盘无法区分「上次遗留的 restore 副本」
	// 与「用户自己网盘里同大小的无关文件」，只认内容哈希（防内容错配 + 防误登记删除）。
	all, err := api.ListRecursive(ctx, "")
	if err != nil {
		return nil, false, false
	}
	if wantHash == "" {
		return nil, false, false // 目标无 hash → 全盘兜底不可用（宁走 restore）
	}
	candidates := []*FileMeta{findFileBySize(all, wantSize), findFileByName(all, wantName)}
	for _, f := range candidates {
		if f == nil || f.Hash == "" || f.Hash != wantHash {
			continue
		}
		return f, isRestoreCopyFolder(ctx, api, f), true
	}
	return nil, false, false
}

// findInPackShared 在「Pack From Shared」文件夹内查找转存副本（不全盘 walk）。
func findInPackShared(ctx context.Context, api restoreIdempotentAPI, wantName string, wantSize int64) (*FileMeta, bool) {
	packID, err := packSharedFolderID(ctx, api)
	if err != nil || packID == "" {
		return nil, false
	}
	files, err := api.List(ctx, packID)
	if err != nil {
		return nil, false
	}
	// size 精确（强判据）优先；回退 name 子串（弱判据，hash 缺失时兜底）。
	if f := packFileBySize(files, wantSize); f != nil {
		return f, true
	}
	if f := packFileByName(files, wantName); f != nil {
		return f, true
	}
	return nil, false
}

// packSharedFolderID 在网盘根目录找「Pack From Shared」文件夹（转存副本固定落这里）。
func packSharedFolderID(ctx context.Context, api restoreIdempotentAPI) (string, error) {
	root, err := api.List(ctx, "")
	if err != nil {
		return "", err
	}
	for i := range root {
		if root[i].Kind == driveKindFolder && strings.EqualFold(root[i].Name, "Pack From Shared") {
			return root[i].ID, nil
		}
	}
	return "", nil
}

func packFileBySize(files []FileMeta, wantSize int64) *FileMeta {
	if wantSize <= 0 {
		return nil
	}
	for i := range files {
		f := &files[i]
		if f.Kind == driveKindFile && f.Size == wantSize {
			return f
		}
	}
	return nil
}

func packFileByName(files []FileMeta, wantName string) *FileMeta {
	if wantName == "" {
		return nil
	}
	for i := range files {
		f := &files[i]
		if f.Kind == driveKindFile && strings.Contains(f.Name, wantName) {
			return f
		}
	}
	return nil
}

// isRestoreCopyFolder 判断文件是否位于「Pack From Shared」（restore 副本）。
func isRestoreCopyFolder(ctx context.Context, api restoreIdempotentAPI, f *FileMeta) bool {
	if f == nil || f.ParentID == "" {
		return false
	}
	parent, err := api.FindByID(ctx, f.ParentID)
	if err != nil {
		return false
	}
	return parent.Kind == driveKindFolder && strings.EqualFold(parent.Name, "Pack From Shared")
}

// restoreCopyCheck 是「检查并复用」的返回（供调用方落 lease/直链）。
type restoreCopyCheck struct {
	File          *FileMeta
	IsRestoreCopy bool // 是否位于 Pack From Shared（可登记 AutoDelete）
	OK            bool // 命中既有副本（无需 RestoreShare）
}

// checkRestoreCopy 幂等检查统一入口（旧下载器 + hybrid 共用）。
// log 用于 hash 拒绝告警（nil 静默）。
//
// 加固（2026-10-27 实测）：命中后**重新 FindByID 确认文件仍存在**——pack 列表可能
// 返回陈旧条目（用户刚清理），直接复用死 id 会致 CLI 下载失败。确认失败 → 保守
// 不命中（调用方走 RestoreShare 正确路径）。
func checkRestoreCopy(ctx context.Context, api restoreIdempotentAPI, target *downloader.ResolvedFile) restoreCopyCheck {
	if target == nil || target.Name == "" {
		return restoreCopyCheck{}
	}
	f, isRC, ok := findRestoreCopy(ctx, api, target.Name, target.Size, target.Hash)
	if !ok || f == nil {
		return restoreCopyCheck{}
	}
	// hash 双方都有且不等 → 拒绝（不同文件，防内容错配）。
	if target.Hash != "" && f.Hash != "" && f.Hash != target.Hash {
		return restoreCopyCheck{}
	}
	// 命中后确认仍存在（FindByID 直查，不依赖列表陈旧快照）。
	live, err := api.FindByID(ctx, f.ID)
	if err != nil || live == nil {
		return restoreCopyCheck{}
	}
	return restoreCopyCheck{File: live, IsRestoreCopy: isRC, OK: true}
}

// locateRestoredFileIn 定位转存产物（**唯一实现**，旧下载器与 hybrid 共用）。
//
// 为什么必须处理文件夹：RestoreShare 对**单文件**转存返回的是「Pack From Shared」
// 文件夹 id（实测 2026-10-27），不是文件 id——直接拿它做 CLI 下载/取直链会报
// "no download URL available"。此前 hybrid 有此分支、旧下载器没有（两份实现漂移），
// 导致 fallback 到旧下载器必失败。
//
// 判据：fid 是文件夹 → 列其内容，优先视频文件（wantName 精确优先），否则第一个文件。
func locateRestoredFileIn(ctx context.Context, api restoreIdempotentAPI, fid, wantName string) (*FileMeta, error) {
	df, err := api.FindByID(ctx, fid)
	if err != nil {
		return nil, fmt.Errorf("locate restored %s: %w", fid, err)
	}
	if df.Kind != driveKindFolder {
		return df, nil
	}
	files, err := api.List(ctx, fid)
	if err != nil {
		return nil, fmt.Errorf("list restored folder %s: %w", fid, err)
	}
	f, ok := pickFileFromFolder(files, wantName)
	if !ok {
		return nil, fmt.Errorf("no file in restored folder %s", fid)
	}
	return f, nil
}

// pickFileFromFolder 在转存文件夹内挑文件：目标名精确 → 视频优先 → 第一个。
func pickFileFromFolder(files []FileMeta, wantName string) (*FileMeta, bool) {
	for i := range files {
		if files[i].Kind == driveKindFile && files[i].Name == wantName {
			return &files[i], true
		}
	}
	for i := range files {
		if files[i].Kind == driveKindFile && hasVideoExt(files[i].Name) {
			return &files[i], true
		}
	}
	for i := range files {
		if files[i].Kind == driveKindFile {
			return &files[i], true
		}
	}
	return nil, false
}
