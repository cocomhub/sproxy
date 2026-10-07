// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
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
func (h *Handlers) resolveExternalDownload(r *http.Request, owner, rel, filename, explicitVol string) *downloadPath {
	candidates := h.externalCandidates(r, owner, explicitVol)
	// **集群出口环打破（2026-10-05）**：B 态 302 到出口自身时带 `egress_forward=1`，
	// 二次进入强制 A 态（服务端转发）——否则 DirectURL 无条件命中 → 无限 302 环。
	// 只收紧（强制转发），无提权面。
	forceForward := r.URL.Query().Get("egress_forward") == "1"
	// 域侧 rel 形如 user/<name>；剥桶后经 v.ResolveUserLocation + FSPath 统一计算最终键
	// （共享卷自动加 <owner>/ 前缀隔离，独享卷无前缀——评审 M3 + 用户裁定统一入口）。
	stripped := strings.TrimPrefix(rel, "user/")
	for _, v := range candidates {
		be := h.volSet.External(v.Name)
		if be == nil {
			continue
		}
		fsys := be.FS()
		if fsys == nil {
			// 后端已登记但 FS 视图未就绪（评审 Minor：nil 接口解引用 panic 防御）。
			continue
		}
		loc, kerr := v.ResolveUserLocation(owner, stripped)
		if kerr != nil {
			// 路径非法（域侧已校验应不可达）或无权（candidates 已 ACL 过滤）——fail-closed。
			continue
		}
		// **评审 I2 修复（跨 owner 隔离）**：egress 卷代表凭证授的持有侧 owner——本地
		// 请求 owner 必须与 holder_owner 一致。否则默认 ACL 开放下任一本地 owner 经
		// 出口卷剥自己的前缀、按凭证 Owner 读持有侧数据（跨 owner 越权读）。
		// 装配期已强制 holder_owner 必填（NewBackend fail-closed），此处请求时校验。
		if !egressOwnerMatch(v, owner) {
			continue // 非该 owner 请求 egress 卷 → 不命中（404，不泄卷存在性）
		}
		// 基于 locator 操作：FS 键仅在调用 FS 方法时经 FSPath 拼接（唯一拼接点）。
		ownerKey := loc.FSPath()
		e, err := fsys.Stat(r.Context(), ownerKey)
		if err != nil || e == nil || e.IsDir {
			continue // 该卷无此文件/目录 → 下一候选
		}
		// 命中卷：分 A/B/C 态（volumePrivate 见 volumes.go：secretdata 恒 true）。
		src := newExternalSource(fsys, ownerKey, e)
		if resolved := h.externalStateFor(v, forceForward, fsys, r, ownerKey, filename, src); resolved != nil {
			return resolved
		}
	}
	return nil
}

// egressOwnerMatch 判定本地请求 owner 是否匹配 egress 卷的 holder_owner（评审 I2）：
// egress 卷代表凭证授的持有侧 owner，装配期强制 holder_owner 必填；非 egress 卷恒 true。
func egressOwnerMatch(v volume.Volume, owner string) bool {
	if v.Type != clustercred.TypeEgress {
		return true
	}
	ho, _ := v.Extra["holder_owner"].(string)
	return strings.TrimSpace(ho) == owner
}

// egressVisibleView 过滤 owner 可见卷视图：egress 卷**仅对 holder_owner 可见**（评审/
// 用户裁定可见性）——非 holder_owner 的普通用户不可见（连卷名/存在性都不泄露；数据面
// 已由 egressOwnerMatch 堵 404）。holder_owner（凭证授的已授权者）可见并可访问已授权
// 内容（管理员视角 = 凭证 scope 内）。
func egressVisibleView(view []volume.Volume, owner string) []volume.Volume {
	filtered := view[:0]
	for _, v := range view {
		if egressOwnerMatch(v, owner) {
			filtered = append(filtered, v)
		}
	}
	return filtered
}

// externalStateFor 分 A/B/C 态（gocognit 收敛）：私密/egress_forward → A 态服务端转发；
// 明文未私密 → B 态 302 直链（失败回落 A）；返回 nil = 未命中（调用方继续下一候选）。
func (h *Handlers) externalStateFor(v volume.Volume, forceForward bool, fsys syncpkg.FS, r *http.Request, ownerKey, filename string, src files.DownloadSource) *downloadPath {
	if volumePrivate(v) || forceForward {
		// A/C 态：服务端转发（解密/整流）。filename 用用户原始名（无 user/ 前缀）。
		return &downloadPath{filename: filename, volName: v.Name, rel: ownerKey, source: src}
	}
	// B 态：明文外部卷未私密 → 尝试 302 直链；失败回落服务端转发。
	if d, ok, derr := syncpkg.AssertDirectURL(fsys).DirectURL(r.Context(), ownerKey); derr == nil && ok && d != "" {
		// 直链与 source 并存：Download 走 302；Stat 经 source 回元信息。
		return &downloadPath{filename: filename, volName: v.Name, rel: ownerKey, redirectURL: d, source: src}
	}
	// 直链不可得（adapter 不支持 / 定位失败）：回落服务端转发，绝不半截 302。
	return &downloadPath{filename: filename, volName: v.Name, rel: ownerKey, source: src}
}

// externalCandidates 返回 owner 可见的外部卷候选（External 非 nil；显式卷只取指定卷）。
//
// **ACL（评审 I2 修复，2026-10-05）**：显式 ?volume= 分支同样校验 v.Authorize(owner)
// ——不在 owner 视图的卷不命中（404，fail-closed 不泄卷存在性），与本地卷 locateForRead
// 显式分支语义一致。此前仅查 ByName+External 会允许未授权 owner 经 ?volume= 读外部卷。
func (h *Handlers) externalCandidates(r *http.Request, owner, explicitVol string) []volume.Volume {
	if h.volSet == nil {
		return nil
	}
	if explicitVol != "" {
		v, ok := h.volSet.ByName(explicitVol)
		if !ok || h.volSet.External(explicitVol) == nil {
			return nil
		}
		if !v.Authorize(owner) {
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
