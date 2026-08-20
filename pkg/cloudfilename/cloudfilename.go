// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package cloudfilename 提供云端下载文件名生成与清理规则。
// 服务端 (pkg/server) 与客户端 (cmd/sclient) 共用同一套规则，
// 保证 URL → 默认保存文件名的推导结果双端一致（wget 行为）。
package cloudfilename

import (
	"net/url"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// DefaultFromURL 遵循 wget 行为从 URL 推导默认文件名并做安全清理。
// 返回即为可安全落盘的文件名，调用方无需再调用 Safe。
// 内部先将不安全版通过 Safe 清理，保证"生成即安全"。
func DefaultFromURL(rawURL string) string {
	return Safe(defaultFromURLUnsafe(rawURL))
}

// defaultFromURLUnsafe 保留原始 wget 推导逻辑（仅供内部精确测试）：
//   - 路径末尾为 / 时使用 "index.html"（如 /xx/?a=v → index.html?a=v）
//   - 查询参数（? 后的 raw query）直接附加到文件名后
//   - 路径最后一段做百分号解码
//
// 无效 URL 或非绝对 URL（无 host）返回 "download"。
// 返回值未做路径穿越清理，调用方应自行调用 Safe。
func defaultFromURLUnsafe(rawURL string) string {
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

// maxNameBytes 是文件名最大 UTF-8 字节数。
// NTFS / ext4 限制文件名为 255 字节，留 1 字节余量确保落盘安全。
const maxNameBytes = 254

// winReservedBase 是 Windows 保留设备名（大小写不敏感，匹配首个 '.' 前的基名）。
// 这类名字在 Windows 上无法作为文件名创建（如 CON、COM1、LPT9 等）。
var winReservedBase = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// Safe 清理文件名中的路径分隔符，防止路径穿越。
//   - 替换 \ / ? : < > | " * 与 NUL 为下划线，去除首尾空白与点
//   - 抵御 Windows 保留设备名（基名匹配时加 "_" 前缀）
//   - 按 maxNameBytes 字节截断（优先保留扩展名，不劈开 UTF-8 字符）
//
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
	if reservedBase(name) {
		name = "_" + name
	}
	if name = truncateName(name, maxNameBytes); name == "" {
		return "download"
	}
	return name
}

// reservedBase 判断文件名基名（首个 '.' 前、转为大写）是否为 Windows 保留设备名。
// 注意：CON 后接其他字符（如 CONTEXT.txt）不匹配，只有完整基名 CON/CON.txt 命中。
func reservedBase(name string) bool {
	base := strings.ToUpper(name)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	_, ok := winReservedBase[base]
	return ok
}

// truncateName 将文件名按字节截断到 maxBytes 内，优先保留扩展名。
// 扩展名本身超长（>= maxBytes）时放弃保留，直接整体截断。
func truncateName(name string, maxBytes int) string {
	if len(name) <= maxBytes {
		return name
	}
	if ext := filepath.Ext(name); ext != "" && len(ext) < maxBytes {
		base := strings.TrimSuffix(name, ext)
		if base != "" {
			return truncateBytes(base, maxBytes-len(ext)) + ext
		}
	}
	return truncateBytes(name, maxBytes)
}

// truncateBytes 将字符串截断到最多 maxBytes 字节，不劈开 UTF-8 字符。
func truncateBytes(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	var b []byte
	bytes := 0
	for _, r := range s {
		l := utf8.RuneLen(r)
		if bytes+l > maxBytes {
			break
		}
		bytes += l
		b = utf8.AppendRune(b, r)
	}
	return string(b)
}
