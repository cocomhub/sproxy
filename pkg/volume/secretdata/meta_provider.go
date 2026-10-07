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
	return shardsealMetaToFileMeta(e.meta), nil
}

// shardsealMetaToFileMeta 把 shardseal.Meta 转换为通用 FileMeta（字段对齐互转）。
func shardsealMetaToFileMeta(m *shardseal.Meta) *meta.FileMeta {
	fm := &meta.FileMeta{
		Version:     1,
		Size:        m.Original.Size,
		TotalSHA256: m.Original.SHA256,
		Name:        m.Original.Name,
		MTime:       m.Original.MTime,
		BaseVersion: m.BaseVersion,
		Signature:   m.Signature,
	}
	// m9 修复：Extra 深拷贝（转换结果独立于卷内 metaEntry.meta.Extra 共享 map——
	// 调用方修改返回 FileMeta.Extra 不得污染卷内存态）。
	if m.Extra != nil {
		fm.Extra = make(map[string]any, len(m.Extra))
		for k, v := range m.Extra {
			fm.Extra[k] = v
		}
	}
	// 分块大小：secretdata 不定长分块——取首块 OrigSize 作 ChunkSize 表达；
	// 实际逐块校验以各块 SHA256 为准（ChunkSize 仅元信息，非校验粒度）。
	// m2 修复：零字节文件无分块（Chunks 空）→ ChunkSize 取默认 1MiB
	// （ChunkSizeForSize(0)）——空文件是合法产物（touch/.gitkeep），
	// ChunkSize=0 会被 meta.Validate 拒（"分块大小非法"），须给默认表达。
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

// _ 编译期断言：secretdata 仍实现 syncpkg.FS（不因 Provider 改变）。
var _ syncpkg.FS = (*SecretdataFS)(nil)
