// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// checksum_rebuild.go 实现「checksum 台账自愈 / 一次性导入」（设计
// 2026-10-28-volume-state-maintenance §15.1 步骤 ①+②）。
//
// 背景：per-tenant 台账 `meta/checksums.json` 是**整体重写**的单个 JSON——丢失/损坏/被裁掉时，
// list/search 的 checksum 会整片变空（文件还在，校验能力却没了）。而对象 meta sidecar
// （`<bucket>/<rel>.meta` 的 `TotalSHA256`）是**跟随对象**的权威值。
//
// 本文件的做法：当台账为空时，逐卷遍历本地卷 `user/` 桶，从 sidecar 读回 `TotalSHA256` 批量
// 回填台账（一个 owner 只尝试一次，避免列表路径反复全量遍历）。台账恢复后读路径仍是「一次快照」，
// 不为每个条目做额外 I/O。
package files

import (
	"context"
	"encoding/json"
	"errors"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// ErrChecksumStoreUnavailable 表示台账未装配（未注入 ChecksumLedger）。装配层据此回 503。
var ErrChecksumStoreUnavailable = errors.New("checksum 台账不可用")

// RebuildChecksums 从对象 meta sidecar 重建 owner 的 checksum 台账（一次性导入）。
//
// 返回回填条数。未装配 meta 能力（无 sidecar 可读）/台账不可用 ⇒ (0, nil)/错误。
// **幂等**：已存在的台账条目以 sidecar 为权威覆盖（sidecar 是跟随对象的真值）。
func (s *Service) RebuildChecksums(ctx context.Context, owner string) (int, error) {
	owner = normalizeOwner(owner)
	if !s.rt.fileMetaEnabled() {
		return 0, nil
	}
	policy := s.rt.fileMeta
	cs := s.rt.checksumStore(owner)
	if cs == nil {
		return 0, ErrChecksumStoreUnavailable
	}
	vs := s.rt.volSet()
	if vs == nil {
		return 0, nil
	}
	total := 0
	for _, v := range vs.All() {
		if !vs.HasLocalRoot(v.Name) {
			continue // 外部/远端卷无本地 sidecar（其 list 直接读卷 meta）
		}
		root := vs.Root(v.Name)
		if root == nil {
			continue
		}
		// **按卷收集、按卷写入**（§15.1.1）：跨卷收进一个 map 会让同名文件互相覆盖。
		found := make(map[string]string)
		collectMetaChecksums(ctx, policy, root, owner, found)
		if len(found) == 0 {
			continue
		}
		writeCollectedChecksums(cs, v.Name, found)
		total += len(found)
	}
	return total, nil
}

// collectMetaChecksums 遍历**卷根命名空间**里的 `<owner>/user/` 子树，把每个文件的 sidecar
// `TotalSHA256` 收进 out（键 = 用户桶相对路径 `user/<rel>`，与写内核/旁路写登记的台账键空间一致）。
func collectMetaChecksums(ctx context.Context, policy FileMetaPolicy, root *storage.Root, owner string, out map[string]string) {
	stack := []string{owner + "/" + userBucket}
	for len(stack) > 0 {
		dir := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		entries, err := root.ReadDir(dir)
		if err != nil {
			continue // 目录不存在/不可读：跳过（自愈尽力而为）
		}
		for _, e := range entries {
			rel := path.Join(dir, e.Name())
			if e.IsDir() {
				stack = append(stack, rel)
				continue
			}
			collectOneMetaChecksum(ctx, policy, root, owner, rel, e.Name(), out)
		}
	}
}

// collectOneMetaChecksum 读取单个文件的 sidecar 并写入 out（非内容文件/无 sidecar/损坏均跳过）。
func collectOneMetaChecksum(ctx context.Context, policy FileMetaPolicy, root *storage.Root, owner, rel, name string, out map[string]string) {
	if meta.IsMetaName(name) {
		return // sidecar 自身不是用户内容
	}
	raw, err := policy.ReadMetaRaw(ctx, root, meta.MetaPath(rel))
	if err != nil {
		return // 无 sidecar / 损坏：该文件不可自愈
	}
	var fm meta.FileMeta
	if json.Unmarshal(raw, &fm) != nil || fm.TotalSHA256 == "" {
		return
	}
	// 台账键口径：剥掉 owner 前缀 ⇒ `user/<rel>`。
	if key, ok := strings.CutPrefix(rel, owner+"/"); ok {
		out[key] = fm.TotalSHA256
	}
}

// checksumRebuildAllowed 报告该 owner 是否还能尝试自动重建（首次返回 true 并占位）。
func (s *Service) checksumRebuildAllowed(owner string) bool {
	_, loaded := s.checksumRebuild.LoadOrStore(owner, struct{}{})
	return !loaded
}

// writeCollectedChecksums 按卷批量写入重建结果（后端按可选扩展接口选择：
// 卷维度键用 VolumeBatchWriter，单卷用 BatchWriter——集群后端两者均已实现）。
func writeCollectedChecksums(cs checksum.ChecksumStoreIface, vol string, found map[string]string) {
	if vol != "" {
		if w, ok := cs.(checksum.VolumeBatchWriter); ok {
			w.SetManyInVolume(vol, found)
		}
		return
	}
	if w, ok := cs.(checksum.BatchWriter); ok {
		w.SetMany(found)
	}
}
