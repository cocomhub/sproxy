// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package downloader

import (
	"context"
	"fmt"
	"net/url"
	"path"
	"strings"
)

// KindGeneric 是通用回落解析结果类型（无扩展识别该 URL 时的内置答案）。
const KindGeneric = "generic"

// ResolvedFile 是解析出的单个文件条目（分享内文件 / 单文件资源）。
type ResolvedFile struct {
	ID         string `json:"id,omitempty"`          // 分享内文件 ID（供按文件下载）
	Name       string `json:"name"`                  // 文件名
	Size       int64  `json:"size"`                  // 字节数（未知 = 0）
	Type       string `json:"type,omitempty"`        // 内容类型（video/mp4 等）
	Hash       string `json:"hash,omitempty"`        // 内容哈希（提供方自报，可能为空）
	DirectLink string `json:"direct_link,omitempty"` // 可直连下载的 URL（可能为空/受限）

	// 媒体探测结果（可选；未探测/探测失败时全部为零值/空串）。
	// 由 MediaProber 可选能力在解析时尽力填充，消费方**必须**能接受空值。
	Width      int    `json:"width,omitempty"`      // 视频宽度（像素）
	Height     int    `json:"height,omitempty"`     // 视频高度（像素）
	Resolution string `json:"resolution,omitempty"` // 分辨率档位（如 720p/1080p）
}

// MediaProber 是「媒体探测」可选能力接口：对可访问的媒体 URL 探测分辨率。
//
// 设计取舍：
//   - 探测**不是**解析的必需环节（解析结果里的 width/height 允许为空）；
//   - 实现应**尽力而为**：任何失败都不得让解析整体失败（返回零值即可）；
//   - 由装配层按需注入（未装配 ⇒ 解析结果不含探测字段，行为与旧版一致）。
type MediaProber interface {
	// Probe 返回媒体文件的宽高；无法探测时返回零值与错误（调用方应忽略并留空）。
	Probe(ctx context.Context, url string) (ProbeResult, error)
}

// ProbeResult 是探测结果（零值 = 未知）。
type ProbeResult struct {
	Width  int
	Height int
}

// ResolutionLabel 把宽高映射为常用档位标签（720p/1080p/…）。
// 判据：优先用**高度**（视频档位惯例），高度未知时回落到宽度。
func ResolutionLabel(w, h int) string {
	base := h
	if base <= 0 {
		base = w
	}
	switch {
	case base >= 2000:
		return "2160p"
	case base >= 1400:
		return "1440p"
	case base >= 1000:
		return "1080p"
	case base >= 700:
		return "720p"
	case base >= 450:
		return "480p"
	case base >= 300:
		return "360p"
	case base > 0:
		return "low"
	default:
		return ""
	}
}

// ResolveInfo 是 URL 解析结果（通用组织：单文件用 Filename/Size，多文件用 Files）。
type ResolveInfo struct {
	URL      string         `json:"url"`
	Kind     string         `json:"kind"`               // 扩展名（pikpak_share 等）或 KindGeneric
	Filename string         `json:"filename,omitempty"` // 单文件时的文件名
	Size     int64          `json:"size,omitempty"`     // 单文件时的大小
	Files    []ResolvedFile `json:"files,omitempty"`    // 多文件（分享）清单
}

// URLResolver 是「URL → 资源信息」可选能力接口。
//
// 设计：与 Downloader 同处一个 Registry（同一注册表两种能力），识别语义一致
// （Supports 判定归属；Resolve 返回统一组织的资源信息）。新增分享类型只需注册
// 一个 URLResolver 实现，**无需**新增 HTTP 端点（收敛原则）。
type URLResolver interface {
	// Name 返回解析扩展名称（如 "pikpak"）。
	Name() string
	// Supports 判断是否识别该 URL。
	Supports(rawURL string) bool
	// Resolve 解析该 URL 对应资源信息；错误原样上抛（不静默回落通用结果）。
	Resolve(ctx context.Context, rawURL string) (*ResolveInfo, error)
}

// ResolveURL 全局函数：对 DefaultRegistry 执行通用 URL 解析。
func ResolveURL(ctx context.Context, rawURL string) (*ResolveInfo, error) {
	return DefaultRegistry.ResolveURL(ctx, rawURL)
}

// RegisterResolver 全局函数：向 DefaultRegistry 注册解析扩展。
func RegisterResolver(p Plugin[URLResolver]) {
	DefaultRegistry.RegisterResolver(p)
}

// RegisterResolver 注册一个 URL 解析扩展（与 Downloader 独立命名空间）。
func (r *Registry) RegisterResolver(p Plugin[URLResolver]) {
	r.resolvers.Register(p)
}

// ResolverFor 返回第一个 Supports 该 URL 的解析扩展（无则 ok=false）。
// 用于装配自检/测试（不经网络确认扩展已注册到位）。
func (r *Registry) ResolverFor(rawURL string) (URLResolver, bool) {
	if r.resolvers == nil {
		return nil, false
	}
	for _, name := range r.resolvers.Names() {
		res, _ := r.resolvers.Get(name)
		if res != nil && res.Supports(rawURL) {
			return res, true
		}
	}
	return nil, false
}

// ResolveURL 按注册顺序查找第一个 Supports 的解析扩展并执行解析；
// 无扩展匹配时返回通用回落结果（文件名由 URL 推导、大小未知）。
func (r *Registry) ResolveURL(ctx context.Context, rawURL string) (*ResolveInfo, error) {
	info, err := r.resolveURLInner(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	r.probeResolved(ctx, info)
	return info, nil
}

func (r *Registry) resolveURLInner(ctx context.Context, rawURL string) (*ResolveInfo, error) {
	if info, ok, err := r.resolveWithExtensions(ctx, rawURL); err != nil {
		return nil, err
	} else if ok {
		return info, nil
	}
	return genericResolve(rawURL), nil
}

// maxProbeFilesPerResolve 限制单次解析最多探测几个文件。
// 理由：探测是**外部进程调用**（ffprobe），一次分享可能有上百个文件；
// 不限制会让解析延迟不可控。只取前 N 个视频（分享清单一般正片在前）。
const maxProbeFilesPerResolve = 8

// probeResolved 对解析结果里的视频条目**尽力**填充分辨率（可选能力）。
//
// 不变量：
//   - 未装配 prober ⇒ 完全不变（与旧版行为一致）；
//   - 单个文件探测失败 ⇒ **忽略**，不影响其它文件与整体解析结果；
//   - 只探测看起来是视频、且有 DirectLink 的条目（无直链无法探测）。
func (r *Registry) probeResolved(ctx context.Context, info *ResolveInfo) {
	if r.mediaProber == nil || info == nil || len(info.Files) == 0 {
		return
	}
	n := 0
	for i := range info.Files {
		f := &info.Files[i]
		if n >= maxProbeFilesPerResolve {
			break
		}
		if f.DirectLink == "" || !looksLikeVideo(*f) {
			continue
		}
		n++
		res, err := r.mediaProber.Probe(ctx, f.DirectLink)
		if err != nil {
			continue // 尽力而为：失败留空
		}
		f.Width, f.Height = res.Width, res.Height
		f.Resolution = ResolutionLabel(res.Width, res.Height)
	}
}

// looksLikeVideo 判定条目是否像视频（看类型或扩展名）。
func looksLikeVideo(f ResolvedFile) bool {
	t := strings.ToLower(f.Type)
	if strings.HasPrefix(t, "video/") {
		return true
	}
	name := strings.ToLower(f.Name)
	for _, ext := range []string{".mp4", ".mkv", ".avi", ".mov", ".wmv", ".ts", ".m2ts", ".flv", ".webm", ".mpg", ".mpeg", ".iso", ".rmvb"} {
		if strings.HasSuffix(name, ext) {
			return true
		}
	}
	return false
}

// resolveWithExtensions 遍历已注册解析扩展：返回 (result, matched, err)。
// 首个 Supports 的实现负责解析；解析失败错误原样上抛（不静默回落通用结果）。
func (r *Registry) resolveWithExtensions(ctx context.Context, rawURL string) (*ResolveInfo, bool, error) {
	if r.resolvers == nil {
		return nil, false, nil
	}
	for _, name := range r.resolvers.Names() {
		res, _ := r.resolvers.Get(name)
		if !res.Supports(rawURL) {
			continue
		}
		info, err := res.Resolve(ctx, rawURL)
		if err != nil {
			return nil, false, fmt.Errorf("resolve url via %s: %w", name, err)
		}
		if info == nil {
			continue
		}
		if info.URL == "" {
			info.URL = rawURL
		}
		if info.Kind == "" {
			info.Kind = name
		}
		return info, true, nil
	}
	return nil, false, nil
}

// genericResolve 是通用回落：从 URL 路径推导文件名，大小/类型未知。
func genericResolve(rawURL string) *ResolveInfo {
	name := ""
	if u, err := url.Parse(strings.TrimSpace(rawURL)); err == nil {
		name = path.Base(u.Path)
		if name == "." || name == "/" {
			name = ""
		}
	}
	info := &ResolveInfo{URL: rawURL, Kind: KindGeneric, Filename: name}
	if name != "" {
		info.Files = []ResolvedFile{{Name: name}}
	}
	return info
}
