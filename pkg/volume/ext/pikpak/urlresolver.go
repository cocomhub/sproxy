// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/media"
)

// URLResolverName 是通用 URL 解析接口下本扩展的名称。
const URLResolverName = "pikpak_share"

// URLResolver 把 PikPak 分享解析能力接入通用 /api/resolve 接口。
//
// 收敛要点（用户裁定 2026-10-23）：**解析能力唯一实现在本包**（ShareResolver /
// API.ListShareRecursive），此处只是薄适配——消费方（sdserver 等）不再重复实现
// 分享列举/直链解析，只做业务筛选。
//
// 双重信息源（互补，均由本包既有实现提供）：
//   - 匿名解析（Resolver）：文件清单 + 每文件匿名直链（大文件直链受分享限制）；
//   - 登录态列举（API.ListShareRecursive）：文件清单（id/name/size/hash），
//     供「按文件转存 + 全 Range 下载」路径使用（无需直链）。
//
// 列举优先用登录态（更完整：含 hash、无分享直链 40-50% 限制），失败回落匿名。
type URLResolver struct {
	api      *API
	resolver *ShareResolver
}

// NewURLResolver 创建解析扩展（api 可为 nil：仅匿名解析）。
func NewURLResolver(api *API, resolver *ShareResolver) *URLResolver {
	if resolver == nil {
		resolver = NewShareResolver(ShareResolverConfig{})
	}
	return &URLResolver{api: api, resolver: resolver}
}

// Name 返回扩展名。
func (r *URLResolver) Name() string { return URLResolverName }

// Supports 判断是否 PikPak 分享 URL（与 PikpakDownloader.Supports 同判据）。
func (r *URLResolver) Supports(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	if !supportedShareHost(strings.ToLower(u.Hostname())) {
		return false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	return len(parts) >= 2 && parts[0] == "s"
}

// Resolve 解析分享内容为统一结构。
func (r *URLResolver) Resolve(ctx context.Context, rawURL string) (*downloader.ResolveInfo, error) {
	src := stripShareTarget(rawURL)
	shareID, err := parseShareID(src)
	if err != nil {
		return nil, err
	}
	info := &downloader.ResolveInfo{URL: rawURL, Kind: URLResolverName}
	// ① 登录态列举优先（含 hash、无分享直链限制）
	if files, ok := r.listOwned(ctx, shareID); ok {
		info.Files = files
		r.fillSingle(info)
		return info, nil
	}
	// ② 匿名解析回落（含匿名直链；大文件直链受限，仅供信息参考/小文件直连）
	files, aerr := r.listAnon(ctx, shareID, src)
	if aerr != nil {
		return nil, aerr
	}
	info.Files = files
	r.fillSingle(info)
	return info, nil
}

// listOwned 登录态递归列举分享文件；无 API 或失败返回 ok=false（回落匿名）。
func (r *URLResolver) listOwned(ctx context.Context, shareID string) ([]downloader.ResolvedFile, bool) {
	if r.api == nil {
		return nil, false
	}
	files, err := r.api.ListShareRecursive(ctx, shareID)
	if err != nil || len(files) == 0 {
		return nil, false
	}
	var out []downloader.ResolvedFile
	for _, f := range files {
		if f.Kind != driveKindFileMeta {
			continue
		}
		out = append(out, downloader.ResolvedFile{
			ID: f.ID, Name: f.Name, Size: f.Size,
			Type: f.MimeType, Hash: f.Hash,
		})
	}
	return out, len(out) > 0
}

// listAnon 匿名解析分享文件（含直链）。
func (r *URLResolver) listAnon(ctx context.Context, shareID, src string) ([]downloader.ResolvedFile, error) {
	meta, err := r.resolver.Resolve(ctx, src)
	if err != nil {
		return nil, fmt.Errorf("pikpak resolve share %s: %w", shareID, err)
	}
	var out []downloader.ResolvedFile
	for _, f := range meta.Files {
		if f.Kind != "" && f.Kind != driveKindFileMeta {
			continue
		}
		out = append(out, downloader.ResolvedFile{
			ID: f.ID, Name: f.Name, Size: f.Size,
			Type:       mediaTypeOf(f.Name),
			Hash:       f.Hash,
			DirectLink: f.DirectLink,
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: share %s has no files", ErrShareNotFound, shareID)
	}
	return out, nil
}

// fillSingle 单文件时同时填 Filename/Size（调用方无需判断 Files 长度）。
func (r *URLResolver) fillSingle(info *downloader.ResolveInfo) {
	if len(info.Files) == 1 {
		info.Filename = info.Files[0].Name
		info.Size = info.Files[0].Size
	}
}

// mediaTypeOf 由文件名推导内容类型（视频/图片/其它；未知返回空串）。
func mediaTypeOf(name string) string {
	lower := strings.ToLower(name)
	switch {
	case media.IsVideoName(name):
		if strings.HasSuffix(lower, ".mkv") {
			return "video/x-matroska"
		}
		if strings.HasSuffix(lower, ".avi") {
			return "video/x-msvideo"
		}
		return "video/mp4"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".srt"):
		return "application/x-subrip"
	}
	return ""
}
