// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

import (
	"context"
	"fmt"
	"path"
	"sync"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// metaAuditSampleCap 是「缺失分块」样例的条数上限（报告体积可控）。
const metaAuditSampleCap = 200

// AuditMeta 只读审计元数据层（**不修改任何数据**）：
//   - file meta：存活 / 墓碑 / 不可解密；
//   - dir meta：可解密 / 不可解密；
//   - **存活 meta 引用的分块是否都还在**（缺失 = 数据缺口，最危险的一类）；
//   - 存活 meta 的分块是否互相冲突（同一分块被多个文件引用属正常去重）。
//
// 为什么需要（2026-11-20）：清理孤儿分块后，容器里仍残留历史 file meta（墓碑）；跑 GC 前
// 必须先确认「存活 meta 引用的分块齐全、meta 可解密」，否则清理会扩大损失。
//
// 复用：解密走 `gcConfirmFileMeta` / `decryptDirMeta`（与 GC 同一判据）；并发走 `runOrphanWorkers`。
// fail-closed：任一容器列举失败 ⇒ 返回错误（不给出可能误导的报告）。
func (s *SecretdataFS) AuditMeta(ctx context.Context) (syncpkg.MetaAuditReport, error) {
	s.mu.RLock()
	if s.writesInFlight > 0 {
		s.mu.RUnlock()
		return syncpkg.MetaAuditReport{}, fmt.Errorf("secretdata: 存在在途写入，拒绝元数据审计（避免读到未提交状态）")
	}
	containers := s.gcContainersLocked()
	owners := s.containerOwnersLocked()
	s.mu.RUnlock()

	// 阶段 1：逐容器轻量列举，缓存名字集合（供阶段 2 的引用存在性判定，避免二次列举）。
	listings, lerr := s.auditListAll(ctx, containers)
	if lerr != nil {
		return syncpkg.MetaAuditReport{}, lerr
	}

	// 阶段 2：解密全部 meta 并校验引用完整性。
	rep := syncpkg.MetaAuditReport{Containers: len(containers)}
	var mu sync.Mutex
	run := func(ctx context.Context, c string) error {
		one := s.auditContainerMetas(ctx, c, listings, owners)
		mu.Lock()
		defer mu.Unlock()
		mergeMetaAudit(&rep, one)
		return one.err
	}
	if err := s.runOrphanWorkers(ctx, len(containers), containers, run); err != nil {
		return syncpkg.MetaAuditReport{}, err
	}
	return rep, nil
}

// metaAuditPart 是单容器的审计片段（并发聚合用）。
type metaAuditPart struct {
	fileMetas     int
	live          int
	tombstones    int
	unreadable    int
	dirMetas      int
	dirUnreadable int
	missing       int
	samples       []string
	owners        []string
	err           error
}

// mergeMetaAudit 把片段并入总报告（调用方持锁）。
func mergeMetaAudit(rep *syncpkg.MetaAuditReport, p metaAuditPart) {
	rep.FileMetas += p.fileMetas
	rep.Live += p.live
	rep.Tombstones += p.tombstones
	rep.Unreadable += p.unreadable
	rep.DirMetas += p.dirMetas
	rep.DirUnreadable += p.dirUnreadable
	rep.MissingChunks += p.missing
	for _, o := range p.owners {
		if len(rep.MissingOwners) >= metaAuditSampleCap {
			break
		}
		rep.MissingOwners = append(rep.MissingOwners, o)
	}
	for _, sm := range p.samples {
		if len(rep.MissingSamples) >= metaAuditSampleCap {
			break
		}
		rep.MissingSamples = append(rep.MissingSamples, sm)
	}
}

// auditListAll 逐容器列举并缓存名字集合（fail-closed：任一失败即返回错误）。
func (s *SecretdataFS) auditListAll(
	ctx context.Context, containers []string,
) (map[string]map[string]struct{}, error) {
	out := make(map[string]map[string]struct{}, len(containers))
	var mu sync.Mutex
	run := func(ctx context.Context, c string) error {
		ents, err := s.listInner(ctx, c)
		if err != nil {
			return fmt.Errorf("%w: 列举容器失败 %s: %v", ErrOrphanAuditUnsafe, c, err)
		}
		set := make(map[string]struct{}, len(ents))
		for _, e := range ents {
			if !e.IsDir {
				set[e.Name] = struct{}{}
			}
		}
		mu.Lock()
		out[c] = set
		mu.Unlock()
		return nil
	}
	if err := s.runOrphanWorkers(ctx, len(containers), containers, run); err != nil {
		return nil, err
	}
	return out, nil
}

// auditContainerMetas 解密单容器的全部 meta 并校验存活 meta 的分块引用。
func (s *SecretdataFS) auditContainerMetas(
	ctx context.Context, c string, listings map[string]map[string]struct{}, owners map[string]string,
) metaAuditPart {
	var p metaAuditPart
	for name := range listings[c] {
		if ctx.Err() != nil {
			p.err = ctx.Err()
			return p
		}
		switch shardseal.ClassifyName(name) {
		case shardseal.KindFileMeta:
			s.auditOneFileMeta(ctx, c, name, listings, owners, &p)
		case shardseal.KindDirMeta:
			s.auditOneDirMeta(ctx, c, name, &p)
		}
	}
	return p
}

// auditOneFileMeta 解密单个 file meta：统计存活/墓碑/损坏，并校验其分块引用是否齐全。
func (s *SecretdataFS) auditOneFileMeta(
	ctx context.Context, c, name string, listings map[string]map[string]struct{},
	owners map[string]string, p *metaAuditPart,
) {
	p.fileMetas++
	mm, err := s.gcConfirmFileMeta(ctx, c, name)
	if err != nil {
		p.unreadable++
		return
	}
	if mm.Deleted {
		p.tombstones++
		return
	}
	p.live++
	dir := c
	if dd, ok := mm.Extra["dedup"].(string); ok && dd != "" {
		dir = dd
	}
	refs := make([]string, 0, len(mm.Chunks)+1)
	for _, ci := range mm.Chunks {
		refs = append(refs, ci.FileName)
	}
	if mm.Parity != nil {
		refs = append(refs, mm.Parity.FileName)
	}
	for _, ref := range refs {
		if _, ok := listings[dir][ref]; ok {
			continue
		}
		p.missing++
		if len(p.samples) < metaAuditSampleCap {
			p.samples = append(p.samples, path.Join(dir, ref))
		}
		if owner := owners[c]; owner != "" && len(p.owners) < metaAuditSampleCap {
			p.owners = append(p.owners, owner)
		}
	}
}

// auditOneDirMeta 解密单个 dir meta（只统计可解密性；目录树由物理嵌套表达，无需交叉校验）。
func (s *SecretdataFS) auditOneDirMeta(ctx context.Context, c, name string, p *metaAuditPart) {
	p.dirMetas++
	blob, err := readBlob(ctx, s.inner, path.Join(c, name))
	if err != nil {
		p.dirUnreadable++
		return
	}
	if _, derr := s.decryptDirMeta(blob); derr != nil {
		p.dirUnreadable++
	}
}
