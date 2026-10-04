// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/cocomhub/sproxy/pkg/files"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
)

// externalDownloadSource 是外部卷的服务端读取源（files.DownloadSource 实现，
// 2026-10-05 通用文件获取）：secretdata 加密卷（解密转发）与私密明文外部卷共用。
//
// Open 返回 RangeSeeker（把底层 FS 的 OpenRangeRead 适配为 io.ReadSeeker 喂
// http.ServeContent）：播放器 Range 请求只拉含目标区间的数据段——secretdata 走
// 块定位→blocklet 段解密（视频关键帧随机访问顶点），baidupcs 走 dlink Range GET。
// 底层无 RangeReader → NewRangeSeeker 报错，调用方退整流 200（零回归）。
type externalDownloadSource struct {
	fs      syncpkg.FS // 外部卷后端（SecretdataFS / StorageFS 等）
	rel     string     // 卷内相对路径
	size    int64      // 逻辑文件大小（Stat 已知）
	modTime time.Time  // 元信息 ModTime
}

// externalFileInfo 是 fs.FileInfo 的最小实现（size/modtime；供「不能下载目录」判定）。
type externalFileInfo struct {
	name    string
	size    int64
	modTime time.Time
}

func (f externalFileInfo) Name() string       { return f.name }
func (f externalFileInfo) Size() int64        { return f.size }
func (f externalFileInfo) Mode() fs.FileMode  { return 0o600 }
func (f externalFileInfo) ModTime() time.Time { return f.modTime }
func (f externalFileInfo) IsDir() bool        { return false }
func (f externalFileInfo) Sys() any           { return nil }

// Stat 实现 files.DownloadSource：返回文件元信息（size/modtime）。
func (s *externalDownloadSource) Stat(ctx context.Context) (fs.FileInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return externalFileInfo{name: s.rel, size: s.size, modTime: s.modTime}, nil
}

// Open 实现 files.DownloadSource：返回 RangeSeeker（io.ReadSeeker 适配 OpenRangeRead）。
// 底层无 RangeReader → 报错（调用方退整流 200）。
func (s *externalDownloadSource) Open(ctx context.Context) (files.SeekReadCloser, error) {
	return syncpkg.NewRangeSeeker(ctx, s.fs, s.rel, s.size)
}

// newExternalSource 构造服务端读取源（从 sync.Entry 元信息填充）。
func newExternalSource(fsys syncpkg.FS, rel string, e *syncpkg.Entry) *externalDownloadSource {
	return &externalDownloadSource{
		fs:      fsys,
		rel:     rel,
		size:    e.Size,
		modTime: time.Unix(0, e.MTime),
	}
}

// resolveExternalDownload 解析外部卷的下载路径（2026-10-05 通用文件获取）。
//
// 现状 `/download` 链路（resolveDownloadPathDefault → locateForRead → volumeTenant
// → Set.Tenant）对外部卷（secretdata/baidupcs）恒 nil——它们没有 *storage.Root。
// 本函数在本地定位未命中后，遍历 owner 可见的外部卷（External 非 nil），对命中文件
// 分 A/B/C 态：
//
//   - A 态（secretdata 加密卷恒走）：服务端解密转发（source，RangeSeeker 喂 ServeContent）
//   - B 态（明文外部卷 && direct_link:true）：302 直链（dlink，流量不经服务端）；
//     dlink 定位失败 → 回落 A（graceful，不 500）
//   - C 态（明文外部卷 && 默认私密）：服务端转发（source）
//
// 返回 nil = 未命中任何外部卷（调用方走既有 404/回落默认租户语义）。
func (h *Handlers) resolveExternalDownload(r *http.Request, owner, rel, explicitVol string) *downloadPath {
	candidates := h.externalCandidates(r, owner, explicitVol)
	for _, v := range candidates {
		be := h.volSet.External(v.Name)
		fsys := be.FS()
		e, err := fsys.Stat(r.Context(), rel)
		if err != nil || e == nil || e.IsDir {
			continue // 该卷无此文件/目录 → 下一候选
		}
		// 命中卷：分 A/B/C 态（volumePrivate 见 volumes.go：secretdata 恒 true）。
		src := newExternalSource(fsys, rel, e)
		if volumePrivate(v) {
			// A/C 态：服务端转发（解密/整流）。
			return &downloadPath{filename: rel, volName: v.Name, rel: rel, source: src}
		}
		// B 态：明文外部卷未私密 → 尝试 302 直链；失败回落服务端转发。
		if d, ok, derr := syncpkg.AssertDirectURL(fsys).DirectURL(r.Context(), rel); derr == nil && ok && d != "" {
			// 直链与 source 并存：Download 走 302；Stat 经 source 回元信息。
			return &downloadPath{filename: rel, volName: v.Name, rel: rel, redirectURL: d, source: src}
		}
		// 直链不可得（adapter 不支持 / 定位失败）：回落服务端转发，绝不半截 302。
		return &downloadPath{filename: rel, volName: v.Name, rel: rel, source: src}
	}
	return nil
}

// externalCandidates 返回 owner 可见的外部卷候选（External 非 nil；显式卷只取指定卷）。
func (h *Handlers) externalCandidates(r *http.Request, owner, explicitVol string) []volume.Volume {
	if h.volSet == nil {
		return nil
	}
	if explicitVol != "" {
		v, ok := h.volSet.ByName(explicitVol)
		if !ok || h.volSet.External(explicitVol) == nil {
			return nil
		}
		return []volume.Volume{v}
	}
	var out []volume.Volume
	for _, v := range h.volSet.All() {
		if !v.Authorize(owner) {
			continue
		}
		if h.volSet.External(v.Name) == nil {
			continue
		}
		out = append(out, v)
	}
	return out
}
