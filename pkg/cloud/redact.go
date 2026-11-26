// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// redact.go 是**日志脱敏**（2026-11-26）：任务 URL 可能携带分享 token
// （keepshare `/s/<id>/<token>`、带签名的直链 query 等），直接写日志等于把访问凭据
// 落到磁盘（日志长期留存、可被转发）。
//
// 脱敏口径：
//   - 保留 scheme://host[:port]（排障必需：确认目标站点/端口）；
//   - 路径**只保留第一段**（`/s`、`/api` 这类接口名），其余段一律 `***`；
//     单段路径（常见文件名）原样保留（无凭据语义，且排障有用）；
//   - query / fragment 一律丢弃（签名、token 常在这里）。
package cloud

import (
	"net/url"
	"strings"
)

// redactURLForLog 返回可安全写入日志的 URL 形式。
func redactURLForLog(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "***" // 解析失败：宁可不记，也不泄漏原文
	}
	out := u.Scheme + "://" + u.Host
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case u.Path == "" || u.Path == "/":
		// 无路径
	case len(segs) == 1 && segs[0] != "" && strings.Contains(segs[0], "."):
		// 单段**且像文件名**（含扩展名）：保留，便于排障。
		out += "/" + segs[0]
	case len(segs) == 1 && segs[0] != "":
		// 单段无扩展名：多半是分享 token（如 `/AbCdEf1234567890`）⇒ 掩码。
		out += "/***"
	default:
		out += "/" + segs[0] + "/***" // 多段：只留第一段（接口名），其余视为凭据
	}
	if u.RawQuery != "" || u.Fragment != "" {
		out += "?***"
	}
	return out
}
