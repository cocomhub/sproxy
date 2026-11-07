// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package secretdata

// meta_provider.go 实现 meta.Provider 能力接口（用户裁定 2026-10-07：卷自身提供
// 正确 meta 接口的无需 trusted 封装——secretdata 已有 shardseal.Meta 加密 meta，
// 内含整文件 SHA-256 + 每分块 SHA-256/大小/偏移，直接转换即完整 FileMeta）。
//
// 因此装配层对 secretdata 卷**不包** TrustedVolumeFS 装饰器（trusted.Wrap 探测
// Provider 短路），卷自身负责提供与校验 meta。

import (
	"context"
	"fmt"
	"maps"
	"path"
	"strings"

	"github.com/cocomhub/sproxy/pkg/cryptox/shardseal"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
)

// _ 编译期断言：SecretdataFS 实现 meta.Provider（自带 meta，无需 trusted 封装）。
var _ meta.Provider = (*SecretdataFS)(nil)

// FileMeta 返回 rel 对应文件的完整 FileMeta（从卷内 shardseal.Meta 转换）：
//   - 整文件 SHA-256 → TotalSHA256（md5 无——secretdata 无 md5，留空 omitempty）；
//   - 分块 ChunkInfo.OrigSHA256/OrigSize/Offset → ChunkMeta（完整 64 hex）；
//   - ChunkSize 取首块 OrigSize（secretdata 分块不定长——按首块表达；实际校验以
//     每块自己的 SHA256 为准）；
//   - 文件不存在 / 目录 → 明确错误（fail-closed）。
func (s *SecretdataFS) FileMeta(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := strings.TrimPrefix(rel, "/")
	e, ok := s.index[key]
	if !ok {
		return nil, fmt.Errorf("secretdata: FileMeta 文件不存在: %s", rel)
	}
	if e.meta == nil {
		return nil, fmt.Errorf("secretdata: FileMeta 元信息缺失（不可校验）: %s", rel)
	}
	return shardsealMetaToFileMetaBlocklets(e.meta), nil
}

// shardsealMetaToFileMeta 把 shardseal.Meta 转换为通用 FileMeta（字段对齐互转）。
func shardsealMetaToFileMeta(m *shardseal.Meta) *meta.FileMeta {
	fm := &meta.FileMeta{
		Version:     1,
		Size:        m.Original.Size,
		TotalSHA256: m.Original.SHA256,
		Name:        m.Original.Name,
		MTime:       m.Original.MTime,
		CTime:       m.Original.CTime,
		MediaType:   m.Original.MediaType,
		BaseVersion: m.BaseVersion,
		Signature:   m.Signature,
	}
	// m9 修复：Extra 深拷贝（转换结果独立于卷内 metaEntry.meta.Extra 共享 map——
	// 调用方修改返回 FileMeta.Extra 不得污染卷内存态）。
	if m.Extra != nil {
		fm.Extra = make(map[string]any, len(m.Extra))
		maps.Copy(fm.Extra, m.Extra)
	}
	// 分块大小：secretdata 不定长分块——取首块 OrigSize 作 ChunkSize 表达；
	// 实际逐块校验以各块 SHA256 为准（ChunkSize 仅元信息，非校验粒度）。
	// 零分块分支为防御性表达（shardseal 写/载入均拒绝空分块 meta——空文件在本卷
	// 写不进去；此分支仅在外部注入畸形 meta 时产出合法默认值）。
	if len(m.Chunks) > 0 {
		fm.ChunkSize = m.Chunks[0].OrigSize
	} else {
		fm.ChunkSize = meta.ChunkSizeForSize(0)
	}
	for i, c := range m.Chunks {
		fm.Chunks = append(fm.Chunks, meta.ChunkMeta{
			Index:  i,
			Offset: c.Offset,
			Size:   c.OrigSize,
			SHA256: c.OrigSHA256,
		})
	}
	return fm
}

// shardsealMetaToFileMetaBlocklets 把 shardseal.Meta 转成**blocklet 粒度**的 FileMeta
// （2026-10-25 性能修复）：校验分块 = 块内 blocklet（通常 256KiB）而非整个加密块
// （1-200MB 随机）。
//
// 背景：`verifyReadSeeker.verifyChunkAhead` 对「从分块中间起始」的 Range 请求做
// **整块回读式**先验。secretdata 的加密块平均 72MB（此 2.1GB 文件 29 块），而
// `/download/chunk` 请求 4MiB ⇒ 每次回读 72MB = **18 倍放大**（实测 20 倍、2.1s/chunk）。
// 按 blocklet 切分后回读代价降到 blocklet 大小（≤ 4MiB）⇒ 放大 ≤1.06 倍。
//
// 兼容：旧 meta 无 `OrigSHA256Full`（blocklet 完整 sha256）→ 回退用 `OrigSHA256`
// （64-bit 截断，仍可探测损坏）与加密块粒度（保持旧行为）。
// 仅 Used 段入索引（padding 段不承载用户内容，且 Offsets 不连续）。
func shardsealMetaToFileMetaBlocklets(m *shardseal.Meta) *meta.FileMeta {
	fm := shardsealMetaToFileMeta(m)
	// 仅当**全部**有效 blocklet 都有完整 sha256 时才改用 blocklet 粒度（否则混用粒度
	// 会让 FileMeta.Chunks 覆盖/顺序校验失败——Validate 要求连续覆盖 [0,Size)）。
	if !allBlockletsHaveFullHash(m) {
		return fm
	}
	chunks := make([]meta.ChunkMeta, 0, len(m.Chunks)*8)
	idx := 0
	for _, c := range m.Chunks {
		for _, bl := range c.Blocklets {
			if !bl.Used || bl.Size <= 0 {
				continue
			}
			chunks = append(chunks, meta.ChunkMeta{
				Index: idx, Offset: bl.Offset, Size: bl.Size, SHA256: bl.OrigSHA256Full,
			})
			idx++
		}
	}
	if len(chunks) == 0 {
		return fm
	}
	// 防御：blocklet 必须连续覆盖 [0, Size)（否则 Validate 会拒，回退块粒度）。
	var next int64
	for _, c := range chunks {
		if c.Offset != next {
			return fm
		}
		next = c.Offset + c.Size
	}
	if next != fm.Size {
		return fm
	}
	fm.Chunks = chunks
	// ChunkSize 表达粒度（maxMetaChunks 上限已由写侧分块数保证；blocklet 数 ≤ 349525）。
	if n := len(chunks); n > 0 && n <= maxMetaChunksBlocklets {
		fm.ChunkSize = chunks[0].Size
		return fm
	}
	// 超出 FileMeta 分块数上限 ⇒ 回退块粒度（安全降级，仍比不校验强）。
	fm.Chunks = shardsealMetaToFileMeta(m).Chunks
	return fm
}

// maxMetaChunksBlocklets 与 meta.maxMetaChunks 同口径（64MiB / 192B）。
const maxMetaChunksBlocklets = (64 << 20) / 192

// allBlockletsHaveFullHash 报告全部有效 blocklet 是否都有完整 sha256。
func allBlockletsHaveFullHash(m *shardseal.Meta) bool {
	for _, c := range m.Chunks {
		for _, bl := range c.Blocklets {
			if !bl.Used || bl.Size <= 0 {
				continue
			}
			if len(bl.OrigSHA256Full) != 64 {
				return false
			}
		}
	}
	return true
}

// _ 编译期断言：secretdata 仍实现 syncpkg.FS（不因 Provider 改变）。
var _ syncpkg.FS = (*SecretdataFS)(nil)

// BlockletMode 返回 rel 对应文件写路径固化的 blocklet 模式（"video-keyframe"/"fixed"）。
// 供本地工具（sclient sealfile info）与装配层断言关键帧分块是否生效；
// 文件不存在 / 目录 → 明确错误（fail-closed）。
func (s *SecretdataFS) BlockletMode(ctx context.Context, rel string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := strings.TrimPrefix(rel, "/")
	e, ok := s.index[key]
	if !ok {
		return "", fmt.Errorf("secretdata: BlockletMode 文件不存在: %s", rel)
	}
	if e.meta == nil {
		return "", fmt.Errorf("secretdata: BlockletMode 元信息缺失: %s", rel)
	}
	mode := e.meta.Block.BlockletMode
	if mode == "" {
		mode = shardseal.BlockletModeFixed
	}
	return mode, nil
}

// UpdateMetaExtra 更新已落盘 meta 的 Extra（旁路记录能力——供 damaged 源完整性标记写入
// 目标卷 meta；D-MAJOR-2 修复：secretdata 走 Provider 短路不包装饰器，须自带该能力）。
// 实现：锁内克隆当前 shardseal.Meta → 合并 extra → 重新加密写新 blob（metaName 由
// blob 内容派生，Extra 变更后换名）→ 更新索引 metaName/meta → 锁外删旧 blob。
func (s *SecretdataFS) UpdateMetaExtra(ctx context.Context, rel string, extra map[string]any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	key := strings.TrimPrefix(rel, "/")
	e, ok := s.index[key]
	if !ok || e.meta == nil {
		s.mu.RUnlock()
		return fmt.Errorf("secretdata: UpdateMetaExtra 文件不存在: %s", rel)
	}
	nm := cloneMeta(e.meta)
	nm.Extra = cloneExtra(nm.Extra)
	// 合并模式（用户裁定 2026-10-26：默认新增而非覆盖；replace 整表替换；delete 删键）。
	// 模式经 ctx 承载（syncpkg.MetaUpdateModeFrom）——不扩 UpdateMetaExtra 签名。
	switch syncpkg.MetaUpdateModeFrom(ctx) {
	case syncpkg.MetaUpdateReplace:
		nm.Extra = cloneExtra(extra)
	case syncpkg.MetaUpdateDelete:
		for k := range extra {
			delete(nm.Extra, k)
		}
	default: // merge（默认，含零值）：同名键更新、其余保留
		maps.Copy(nm.Extra, extra)
	}
	dirSeg, oldName, mt := e.dirSeg, e.metaName, e.mtime
	s.mu.RUnlock()

	name, blob, merr := s.encryptMetaBlob(nm)
	if merr != nil {
		return fmt.Errorf("secretdata: 更新 meta 加密失败: %w", merr)
	}
	metaPath := path.Join(dirSeg, name)
	if werr := s.uploadBlob(ctx, metaPath, blob, s.blobMTime(mt)); werr != nil {
		_ = s.inner.Delete(ctx, metaPath)
		return fmt.Errorf("secretdata: 更新 meta 上传失败: %w", werr)
	}
	// 锁内提交新 metaName；并发覆盖（条目已被他人改写）→ 删本写 blob 返回错误（不覆盖）。
	s.mu.Lock()
	cur, ok := s.index[key]
	if !ok || cur.metaName != oldName || cur.meta != e.meta {
		s.mu.Unlock()
		_ = s.inner.Delete(ctx, metaPath)
		return fmt.Errorf("secretdata: 更新 meta 并发冲突（条目已被改写）: %s", rel)
	}
	// copy-on-write（P2 并发）：**不得**就地改已发布条目的 meta/metaName——读路径（OpenRangeRead/
	// openRead）先取 *metaEntry 指针、RUnlock 后才解引用，就地改会与之构成 data race（Go UB，
	// -race 必报）。换成新条目，旧指针对在途读者保持不可变。
	next := *cur
	next.metaName = name
	next.meta = nm
	s.index[key] = &next
	s.mu.Unlock()
	// 锁外删旧 blob（best-effort：残留由 GC 上收）。
	_ = s.inner.Delete(context.Background(), path.Join(dirSeg, oldName))
	return nil
}

// cloneExtra 深拷贝 Extra map（防共享污染）。
func cloneExtra(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(m))
	maps.Copy(out, m)
	return out
}
