// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/clustercred"
	"github.com/cocomhub/sproxy/pkg/files"
	"github.com/cocomhub/sproxy/pkg/files/meta"
	syncpkg "github.com/cocomhub/sproxy/pkg/sync"
	"github.com/cocomhub/sproxy/pkg/volume"
	"github.com/cocomhub/sproxy/pkg/volume/trusted"
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
	// fileMeta 是可选的 FileMeta 读取器（装配层注入：fsys 为可信卷——secretdata
	// Provider 或 trusted.Wrap 装饰器——时非 nil）。Open 后包装 VerifyReadSeeker
	// 逐分块校验（下载路径比对 meta，异常 fail-closed——与本地卷同级的信任保证）。
	fileMeta func(ctx context.Context, rel string) (*meta.FileMeta, error)
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
// 信任保证演进：fsys 为可信卷（装配层注入 fileMeta 读取器）→ 包装 VerifyReadSeeker
// 逐分块校验（下载比对 meta，静默损坏 fail-closed 不发损坏内容）。
func (s *externalDownloadSource) Open(ctx context.Context) (files.SeekReadCloser, error) {
	// 无 RangeReader 的外部卷（如 S3 网关后端）：退**整流 seeker**（P1：此前直接报错 →
	// /download 恒 500，本函数注释承诺的「调用方退整流 200」并不存在）。
	var seeker files.SeekReadCloser
	// **窗口对齐 maxRangeSpan（2026-10-24 实测优化）**：外部卷默认窗口 256KiB ⇒ 8MiB
	// 的 /download/chunk 要串行重开 32 次窗口（每次 OpenRangeRead + 段定位 + 解密），
	// 实测 chunk 中位 20s。提升到 4MiB（= 百度 CDN 单次 Range 上限，也是 blocklet
	// 段跨度的自然上界）：8MiB chunk 只需 2 次窗口，本地 staging 命中时几乎无额外开销。
	// seek 仍按窗口重开（4MiB 粒度对播放首帧足够）。
	win := int64(4 << 20)
	if rs, err := syncpkg.NewRangeSeekerWithHint(ctx, s.fs, s.rel, s.size, win); err == nil {
		seeker = rs
	} else {
		seeker = syncpkg.NewStreamSeeker(ctx, s.fs, s.rel, s.size)
	}
	if s.fileMeta != nil {
		if fm, ferr := s.fileMeta(ctx, s.rel); ferr == nil && fm != nil {
			// meta 缺失/读取失败 → 直通（不误报）；meta 可用 → 逐分块校验。
			// **SkipChunkAhead（2026-10-25 性能修复）**：外部卷读路径自带逐段校验
			// （secretdata：窗口密文 sha256 + 逐 blocklet GCM；trusted 装饰器：meta
			// 逐分块流式校验）—— 的「从分块中间起始回读整块」纯属
			// 冗余（实测 2.1GB / 4MiB 请求 × 72MB 块 = 18 倍放大）。保留流式 consume
			// 逐块校验与 EOF 总哈希校验（本包装器仍提供），只跳过回读式先验。
			v := meta.VerifyReadSeekerWithOpts(seeker, fm, meta.VerifyOpts{SkipChunkAhead: true})
			if vc, ok := v.(files.SeekReadCloser); ok {
				return vc, nil
			}
		} else if ferr != nil {
			// 可观测性（第 5 轮对抗评审 P1）：sidecar 缺失是升级后的预期状态（Debug），
			// 而**读失败/畸形**必须 Warn——否则运维无法区分「无校验能力」与「已校验」。
			if os.IsNotExist(ferr) {
				slog.Default().Debug("外部卷下载不经服务端校验（sidecar 缺失）", "path", s.rel)
			} else {
				slog.Default().Warn("外部卷 sidecar 读取失败，降级为不经服务端校验", "path", s.rel, "error", ferr.Error())
			}
		}
	}
	return seeker, nil
}

// Checksum 实现 files.ChecksumSource：经 meta.Provider 取**明文**整文件 SHA-256。
// secretdata 的 meta 置 `Original.SHA256` = 明文哈希（与客户端收到的解密字节一致）；
// trusted.Wrap 的 sidecar 记录值同语义。无 meta 能力/读取失败/无记录值 → ok=false
// （调用方保持旧行为：checksum 空，不伪造）。
func (s *externalDownloadSource) Checksum(ctx context.Context) (string, bool) {
	if s.fileMeta == nil {
		return "", false
	}
	fm, err := s.fileMeta(ctx, s.rel)
	if err != nil || fm == nil || fm.TotalSHA256 == "" {
		return "", false
	}
	return fm.TotalSHA256, true
}

// _ 编译期断言：外部读取源实现可选 checksum 能力（Stat 据此填 checksum）。
var _ files.ChecksumSource = (*externalDownloadSource)(nil)

// newExternalSource 构造服务端读取源（从 sync.Entry 元信息填充）。skipVerify=true
// （trusted_volume.skip_verify）时**不注入** meta 读取器 —— 外部卷下载与本地卷下载、
// 云转存读回同口径地尊重该开关（P3 对抗评审：此前外部卷下载无效）。
func newExternalSource(fsys syncpkg.FS, rel string, e *syncpkg.Entry, skipVerify bool) *externalDownloadSource {
	var fm func(ctx context.Context, rel string) (*meta.FileMeta, error)
	if !skipVerify {
		// 装配层注入 FileMeta 读取器（fsys 为可信卷——secretdata Provider / trusted
		// Wrap 装饰器——时经 meta.Provider 探测）；无 meta 能力 → nil（直通）。此时该读路径
		// **不提供校验**（sidecar 缺失=零校验，读路径不重算 meta）。
		fm = fileMetaReaderFor(fsys)
	}
	return &externalDownloadSource{
		fs:       fsys,
		rel:      rel,
		size:     e.Size,
		modTime:  time.Unix(0, e.MTime),
		fileMeta: fm,
	}
}

// fileMetaReaderFor 探测 fsys 的 meta 能力并返回 FileMeta 读取闭包（无能力 → nil）。
func fileMetaReaderFor(fsys syncpkg.FS) func(ctx context.Context, rel string) (*meta.FileMeta, error) {
	if pv, ok := fsys.(meta.Provider); ok {
		return pv.FileMeta
	}
	// trusted.Wrap 装饰器也实现 meta.Provider（FileMeta 读 sidecar）——上面断言已覆盖。
	return nil
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
		fsys, ownerKey, e, ok := h.resolveCandidateVolume(r, v, owner, stripped)
		if !ok {
			continue // 该候选不可用（无后端 / 路径无权 / 无此文件）→ 下一候选
		}
		// 命中卷：分 A/B/C 态 + 直连下载阈值。
		if resolved := h.resolveHitVolume(v, forceForward, fsys, r, ownerKey, filename, e); resolved != nil {
			return resolved
		}
	}
	return nil
}

// resolveCandidateVolume 解析单个候选卷的读视图与命中的 owner 键/条目（gocognit 收敛，
// 2026-10-23）：后端存在性 → FS 包装（Guard+Wrap）→ owner 键映射（唯一入口）→ egress
// 跨 owner 校验（评审 I2）→ Stat 命中判定。任一步失败返回 ok=false（调用方继续下一候选）。
func (h *Handlers) resolveCandidateVolume(r *http.Request, v volume.Volume, owner, stripped string) (syncpkg.FS, string, *syncpkg.Entry, bool) {
	be := h.volSet.External(v.Name)
	if be == nil {
		return nil, "", nil, false
	}
	raw := be.FS()
	if raw == nil {
		// 后端已登记但 FS 视图未就绪（nf 安全：Guard 恒非 nil，必须在 Wrap 前判裸 FS）。
		return nil, "", nil, false
	}
	fsys := trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts()))
	// 键 = 用户可见路径经**唯一映射入口**（externalVolumeKey：ResolveUserLocation +
	// FSPath，恒 `<owner>/user/<rel>`）——与上传/删除/列举/转存同口径。
	ownerKey, kOK := externalVolumeKey(v, owner, stripped)
	if !kOK {
		// 路径非法（域侧已校验应不可达）或无权（candidates 已 ACL 过滤）——fail-closed。
		return nil, "", nil, false
	}
	// **评审 I2 修复（跨 owner 隔离）**：egress 卷代表凭证授的持有侧 owner——本地请求
	// owner 必须与 holder_owner 一致（否则任一本地 owner 可经出口卷跨 owner 越权读）。
	if !egressOwnerMatch(v, owner) {
		return nil, "", nil, false // 不命中（404，不泄卷存在性）
	}
	e, err := fsys.Stat(r.Context(), ownerKey)
	if err != nil || e == nil || e.IsDir {
		return nil, "", nil, false // 该卷无此文件/目录
	}
	return fsys, ownerKey, e, true
}

// resolveHitVolume 解析「命中卷」的下载路径：先按 A/B/C 态（302 直链 / 服务端转发），
// 再尝试直连下载（direct_download 阈值内）。返回 nil = 该候选不提供路径（调用方继续下一候选）。
func (h *Handlers) resolveHitVolume(v volume.Volume, forceForward bool, fsys syncpkg.FS, r *http.Request, ownerKey, filename string, e *syncpkg.Entry) *downloadPath {
	src := newExternalSource(fsys, ownerKey, e, h.verifySkipped())
	if resolved := h.externalStateFor(v, forceForward, fsys, r, ownerKey, filename, src); resolved != nil {
		return resolved
	}
	return h.externalStateFor(v, forceForward, fsys, r, ownerKey, filename, src)
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
	// C4 修复（P0，完整性承诺）：302 直链让客户端直接从后端取字节、绕过本服务端的
	// `VerifyReadSeeker` ⇒ 仅当**本服务端不承诺完整性**时才允许：
	//   ① 显式 `trusted_volume.skip_verify`（可观测地放弃校验）；
	//   ② **出口卷**（`clustercred.TypeEgress`）：其 302 指向出口节点自身，二次进入由
	//      `egress_forward=1` 强制 A 态 → 校验仍在服务端（出口侧）完成，未绕过承诺。
	// 其余情形（默认校验开启的普通外部卷）强制 A 态服务端转发；被抑制的直链显式告警一次
	// （禁静默降级：安全开关生效状态必须可观测）。
	directAllowed := h.verifySkipped() || v.Type == clustercred.TypeEgress
	if volumePrivate(v) || forceForward || !directAllowed {
		if !volumePrivate(v) && !forceForward && !directAllowed {
			h.warnDirectLinkSuppressed(v.Name)
		}
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

// warnDirectLinkSuppressed 对「配置了 direct_link 但因完整性承诺被强制服务端转发」的卷
// 告警一次（每卷一次，避免按请求刷屏）：运维必须能看到「直链未生效」而非静默降级。
func (h *Handlers) warnDirectLinkSuppressed(volName string) {
	if _, loaded := h.directLinkWarned.LoadOrStore(volName, struct{}{}); loaded {
		return
	}
	h.logger.Warn("外部卷 direct_link 被抑制：服务端完整性校验开启，改为服务端转发", "volume", volName,
		"hint", "如需 302 直链（放弃服务端校验）请显式设置 trusted_volume.skip_verify=true")
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
