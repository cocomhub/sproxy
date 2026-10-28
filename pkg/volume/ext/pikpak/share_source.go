// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// share_source.go 是「分享源归一」的**唯一实现**：把外部镜像表单的分享 URL 归一为
// 可直接定位分享的形态。此前只有只读解析路径（URLResolver.Resolve）做了 keepshare
// 跟随，下载路径（PikpakDownloader/HybridDownloader）直接 postMessage parseShareID，
// 把 keepshare 容器 id 当分享 id 用 → NOT_FOUND（"解析通、下载必挂"）。
//
// 归一则收敛到本文件，三个下载器（pikpak / hybrid / resolver）共用同一实现。
package pikpak

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cocomhub/sproxy/pkg/netutil"
)

// ShareSourceNormalizer 把分享 URL 归一为「可直接定位分享」的形态。
//
// 归一规则：
//   - keepshare.org|.cc/<容器id>/magnet:...（磁力镜像）→ 跟随 301→302 重定向链拿真实
//     PikPak 分享 id，产出 https://mypikpak.com/s/<真实id>（保留 spx_target 目标文件参数）；
//   - mypikpak.com|.net/s/<id>[/<token>] → 原样返回（已是权威形态）。
//
// 为什么必须归一：keepshare 的 parts[0] 是**容器 id**而非 PikPak 分享 id
// （实测：keepshare 容器 id → 真实 mypikpak 分享 id），用容器 id 调 share/detail 恒 NOT_FOUND。
type ShareSourceNormalizer struct {
	client *http.Client // 出站 HTTP（重定向跟随）；nil = 默认安全 client
}

// NewShareSourceNormalizer 创建归一器；client 为空时用默认安全传输。
func NewShareSourceNormalizer(client *http.Client) *ShareSourceNormalizer {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second, Transport: netutil.IsolatedTransport()}
	}
	return &ShareSourceNormalizer{client: client}
}

// NormalizeShareSource 归一分享 URL；非分享 URL / 非镜像形态原样返回（零回归）。
// keepshare 归一失败（分享失效/终点非 PikPak）→ 错误（fail-closed，不拿容器 id 冒充）。
func (n *ShareSourceNormalizer) NormalizeShareSource(ctx context.Context, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw, nil
	}
	if !isKeepshareMirror(raw) {
		return raw, nil // 已是 mypikpak 权威形态或非分享 URL
	}
	// 归一时先剔除 spx_target（它只描述目标文件，不参与分享定位；follow 后原样回填）。
	target := parseShareTarget(raw)
	id, err := followKeepshareShareID(ctx, n.client, stripShareTarget(raw))
	if err != nil {
		return "", fmt.Errorf("keepshare 归一失败: %w", err)
	}
	out := "https://mypikpak.com/s/" + id
	if target != "" {
		out += "?spx_target=" + url.QueryEscape(target)
	}
	return out, nil
}

// isKeepshareMirror 判断是否为 keepshare 镜像 URL（host 属 keepshare 且路径非空）。
func isKeepshareMirror(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if !strings.Contains(strings.ToLower(u.Hostname()), "keepshare") {
		return false
	}
	return strings.Trim(u.Path, "/") != ""
}
