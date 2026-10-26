// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package pikpak

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/downloader"
	"github.com/cocomhub/sproxy/pkg/media"
	"github.com/cocomhub/sproxy/pkg/netutil"
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
	client   *http.Client // 出站 HTTP（重定向跟随/匿名解析）；nil = 默认安全 client
}

// NewURLResolver 创建解析扩展（api 可为 nil：仅匿名解析）。
func NewURLResolver(api *API, resolver *ShareResolver) *URLResolver {
	return NewURLResolverWithClient(api, resolver, nil)
}

// NewURLResolverWithClient 创建解析扩展并注入出站 HTTPClient（本地自签/代理等场景）。
// client 为空 = 默认安全 client。
func NewURLResolverWithClient(api *API, resolver *ShareResolver, client *http.Client) *URLResolver {
	if resolver == nil {
		resolver = NewShareResolver(ShareResolverConfig{})
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: netutil.IsolatedTransport()}
	}
	return &URLResolver{api: api, resolver: resolver, client: client}
}

// Name 返回扩展名。
func (r *URLResolver) Name() string { return URLResolverName }

// Supports 判断是否 PikPak 分享 URL（与 parseShareIDWithToken 同判据）。
//
// 两类来源（用户裁定 2026-10-26）：
//   - mypikpak.com/s/<id>[/<token>]（原始分享页）；
//   - keepshare.org|.cc/<id>/magnet:...（磁力镜像，parts[0] 即分享 id，无需 302 跟随）。
//
// 判据必须与 parseShareIDWithToken 一致，否则 resolve 对 keepshare 链接恒回落 generic。
func (r *URLResolver) Supports(rawURL string) bool {
	id, _ := parseShareIDWithToken(rawURL)
	if id == "" {
		return false
	}
	// mypikpak 分享要求 /s/<id> 路径形态（parseShareIDWithToken 已校验）；keepshare 镜像
	// 由该函数内 keepshare 分支返回 id，直接放行。
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "keepshare") {
		return true
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
	// keepshare 镜像：parts[0] 是 keepshare 容器 id 而非 PikPak 分享 id——
	// 需 HTTP 跟随重定向链（keepshare.org→.cc→mypikpak.com/s/<真实id>）拿真实分享 id。
	if strings.Contains(strings.ToLower(hostOf(src)), "keepshare") {
		id, ferr := followKeepshareShareID(ctx, r.httpClient(), src)
		if ferr != nil {
			return nil, fmt.Errorf("keepshare follow %s: %w", src, ferr)
		}
		shareID = id
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

// httpClient 返回解析用 HTTP 客户端（注入的或默认安全）。
func (r *URLResolver) httpClient() *http.Client {
	if r.client != nil {
		return r.client
	}
	return &http.Client{}
}

// followKeepshareShareID 跟随 keepshare 镜像重定向链，返回最终 mypikpak 分享 id。
// keepshare.org/<容器id>/magnet:... → 301 keepshare.cc → 302 mypikpak.com/s/<真实id>。
// 终点非 mypikpak 分享（如 keepshare 控制台页/分享不存在）→ 错误（fail-closed，不猜 id）。
//
// 实现：**手动重定向循环**（最多 5 跳）——Go http.Client 的 CheckRedirect 在拒绝跨域时
// resp.Request.URL 是前一个请求 URL，无法拿到被拒绝的 mypikpak 目标；手动循环每跳检查
// Location 并逐跳重建请求，直到目标主机是 mypikpak 或无可跳转。
func followKeepshareShareID(ctx context.Context, client *http.Client, raw string) (string, error) {
	if client == nil {
		client = &http.Client{}
	}
	cur := raw
	for hop := 0; hop < 5; hop++ {
		if id := shareIDFromFinalURL(cur); id != "" {
			return id, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, cur, nil)
		if err != nil {
			return "", err
		}
		// 干净 client：本函数手动控制重定向（禁用自动跟随避免 CheckRedirect 陷阱）。
		c := &http.Client{Timeout: client.Timeout, Transport: client.Transport}
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			return "", err
		}
		finalURL := ""
		loc := resp.Header.Get("Location")
		_ = resp.Body.Close()
		if loc == "" {
			// 无重定向：当前 URL 即终点
			finalURL = cur
			if id := shareIDFromFinalURL(finalURL); id != "" {
				return id, nil
			}
			return "", fmt.Errorf("keepshare 终点非 PikPak 分享: %s", finalURL)
		}
		ref, rerr := url.Parse(loc)
		if rerr != nil {
			return "", fmt.Errorf("keepshare redirect Location 非法: %q", loc)
		}
		base, _ := url.Parse(cur)
		cur = base.ResolveReference(ref).String()
	}
	return "", fmt.Errorf("keepshare redirect 链超过 5 跳: %s", raw)
}

// hostOf 返回 URL 主机名（小写）。
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// shareIDFromFinalURL 从最终 URL 提取 PikPak 分享 id（mypikpak.com|net/s/<id>[/token]）。
func shareIDFromFinalURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(u.Hostname())
	if !strings.Contains(host, "mypikpak") {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) >= 2 && parts[0] == "s" && parts[1] != "" {
		return parts[1]
	}
	return ""
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
