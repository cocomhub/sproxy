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

// ResolveURL 按注册顺序查找第一个 Supports 的解析扩展并执行解析；
// 无扩展匹配时返回通用回落结果（文件名由 URL 推导、大小未知）。
func (r *Registry) ResolveURL(ctx context.Context, rawURL string) (*ResolveInfo, error) {
	if info, ok, err := r.resolveWithExtensions(ctx, rawURL); err != nil {
		return nil, err
	} else if ok {
		return info, nil
	}
	return genericResolve(rawURL), nil
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
