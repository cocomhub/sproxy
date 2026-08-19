// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cloudfilename 提供云端下载文件名生成与清理规则。
// 服务端 (pkg/server) 与客户端 (cmd/sclient) 共用同一套规则，
// 保证 URL → 默认保存文件名的推导结果双端一致（wget 行为）。
package cloudfilename

import (
	"net/url"
	"strings"
)

// DefaultFromURL 遵循 wget 行为从 URL 推导默认文件名：
//   - 路径末尾为 / 时使用 "index.html"（如 /xx/?a=v → index.html?a=v）
//   - 查询参数（? 后的 raw query）直接附加到文件名后
//   - 路径最后一段做百分号解码
//
// 无效 URL 或非绝对 URL（无 host）返回 "download"。
// 返回值未做路径穿越清理，调用方应自行调用 Safe。
func DefaultFromURL(rawURL string) string {
	parsed, err := url.Parse(rawURL)
	// 无效 URL 或非绝对 URL（无 host）返回 "download"
	if err != nil || parsed.Host == "" {
		return "download"
	}
	path := parsed.Path
	// 去掉末尾的 /（保留一个用于判断结尾）
	trimmed := strings.TrimSuffix(path, "/")
	if trimmed == "" {
		// 纯 / 路径
		name := "index.html"
		if parsed.RawQuery != "" {
			name += "?" + parsed.RawQuery
		}
		return name
	}
	// 取最后一段
	var name string
	lastSlash := strings.LastIndex(trimmed, "/")
	if lastSlash >= 0 {
		name = trimmed[lastSlash+1:]
	} else {
		name = trimmed
	}
	// 百分号解码（路径语义：不把 + 解码为空格，与 wget / JS decodeURIComponent 一致）
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}
	// 如果原路径以 / 结尾（trimmed 比 path 短），使用 index.html
	if len(path) > len(trimmed) {
		name = "index.html"
	}
	// 查询参数附加在文件名后
	if parsed.RawQuery != "" {
		name += "?" + parsed.RawQuery
	}
	if name == "" {
		return "download"
	}
	return name
}

// Safe 清理文件名中的路径分隔符，防止路径穿越。
// 替换 \ / ? : < > | " * 与 NUL 为下划线，去除首尾空白与点。
// 清理后为空时返回 "download"。
func Safe(name string) string {
	name = strings.ReplaceAll(name, "\x00", "")
	name = strings.NewReplacer(
		"\\", "_",
		"/", "_",
		"?", "_",
		":", "_",
		"<", "_",
		">", "_",
		"|", "_",
		"\"", "_",
		"*", "_",
	).Replace(name)
	name = strings.Trim(name, " .")
	if name == "" {
		return "download"
	}
	return name
}
