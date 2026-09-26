// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package i18n 提供 sclient CLI 文本输出的多语言字典（roadmap 11.10-⑥）。
//
// 语言由环境变量探测：LC_ALL > LC_MESSAGES > LANG（空跳过）。
//   - zh*/C/POSIX/空 → zh-CN（默认，输出原中文文案）
//   - en* → en
//   - 其余合法语言代码（fr/ja/de/foo 等 ISO 639 形态）→ en（国际通用回退）
//   - 非法形态（数字开头/纯符号等）→ zh-CN（设计文档错误处理段：非法 locale 回退默认）
//
// 与设计文档「包级 once 缓存」的差异：探测结果不缓存，每次调用重新读取环境变量。
// 原因：测试通过 lookupEnv seam 与 t.Setenv 覆盖环境，包级 once 缓存会让同一测试
// 进程内的用例互相污染（package main 的 output 测试与 i18n 包测试同进程切换环境）；
// 进程内环境变量在 CLI 生命周期内不变，行为与「LC_ALL 变化需重启」等效。
package i18n

import (
	"fmt"
	"os"
	"strings"
)

// 语言代码常量。
const (
	// zhCN 是默认语言（原中文文案）。
	zhCN = "zh-CN"
	// en 是英文语言。
	en = "en"
)

// lookupEnv 是环境变量读取 seam；测试替换它来注入环境（不修改真实进程环境）。
var lookupEnv = os.Getenv

// dict 是语言 → 键 → 翻译 的字典。键为输出文案原文（中文），zh-CN 表值与键相同，
// en 表为对应英文翻译。两表键集合必须相等（i18n_test.go 的 TestDict_KeySetsEqual
// 门禁式校验，防漏译——变异验证：删 en 表任意键即红）。
var dict = map[string]map[string]string{
	"zh-CN": {
		"暂无分享链接":           "暂无分享链接",
		"活跃":               "活跃",
		"已过期":              "已过期",
		"分享链接: %s":         "分享链接: %s",
		"有效期至: %s":         "有效期至: %s",
		"最大下载次数: %d":       "最大下载次数: %d",
		"一次性: %v":          "一次性: %v",
		"已撤销分享: %s":        "已撤销分享: %s",
		"远程配置已更新: %s = %s": "远程配置已更新: %s = %s",
		"暂无云端下载任务":         "暂无云端下载任务",
		"任务ID":             "任务ID",
		"文件名":              "文件名",
		"状态":               "状态",
		"组ID":              "组ID",
		"已取消云端下载任务: %s":    "已取消云端下载任务: %s",
		"取消云端下载任务失败: %s (%s)":      "取消云端下载任务失败: %s (%s)",
		"文件 '%s' 没有历史版本":           "文件 '%s' 没有历史版本",
		"文件 '%s' 的版本历史:":           "文件 '%s' 的版本历史:",
		"size:     %d 字节":          "size:     %d 字节",
		"服务器统计（自启动以来）":             "服务器统计（自启动以来）",
		"磁盘使用:":                    "磁盘使用:",
		"  目录:     %s":             "  目录:     %s",
		"  文件数:   %d":              "  文件数:   %d",
		"  总大小:   %s":              "  总大小:   %s",
		"  磁盘分区: %s / %s (%.1f%%)": "  磁盘分区: %s / %s (%.1f%%)",
		"请求统计:":                    "请求统计:",
		"  总请求数: %d":               "  总请求数: %d",
		"  活跃连接: %d":               "  活跃连接: %d",
		"传输统计:":                    "传输统计:",
		"  上传文件:   %d":             "  上传文件:   %d",
		"  上传字节:   %s":             "  上传字节:   %s",
		"  下载文件:   %d":             "  下载文件:   %d",
		"  下载字节:   %s":             "  下载字节:   %s",
		"  删除文件:   %d":             "  删除文件:   %d",
		"存储限制: %s / %s (%.1f%%)":   "存储限制: %s / %s (%.1f%%)",
		"远程服务器配置:":                 "远程服务器配置:",
		"已设置":                      "已设置",
		"未设置":                      "未设置",
	},
	"en": {
		"暂无分享链接":           "No share links",
		"活跃":               "active",
		"已过期":              "expired",
		"分享链接: %s":         "Share link: %s",
		"有效期至: %s":         "Expires at: %s",
		"最大下载次数: %d":       "Max downloads: %d",
		"一次性: %v":          "One-time: %v",
		"已撤销分享: %s":        "Revoked share: %s",
		"远程配置已更新: %s = %s": "Remote config updated: %s = %s",
		"暂无云端下载任务":         "No cloud download tasks",
		"任务ID":             "TASK ID",
		"文件名":              "FILENAME",
		"状态":               "STATUS",
		"组ID":              "GROUP",
		"已取消云端下载任务: %s":    "Cloud download task cancelled: %s",
		"取消云端下载任务失败: %s (%s)":      "Failed to cancel cloud download task: %s (%s)",
		"文件 '%s' 没有历史版本":           "No version history for file '%s'",
		"文件 '%s' 的版本历史:":           "Version history for file '%s':",
		"size:     %d 字节":          "size:     %d bytes",
		"服务器统计（自启动以来）":             "Server statistics (since startup)",
		"磁盘使用:":                    "Disk usage:",
		"  目录:     %s":             "  Root:      %s",
		"  文件数:   %d":              "  Files:     %d",
		"  总大小:   %s":              "  Total:     %s",
		"  磁盘分区: %s / %s (%.1f%%)": "  Partition: %s / %s (%.1f%%)",
		"请求统计:":                    "Request stats:",
		"  总请求数: %d":               "  Requests:  %d",
		"  活跃连接: %d":               "  Active:    %d",
		"传输统计:":                    "Transfer stats:",
		"  上传文件:   %d":             "  Files uploaded:   %d",
		"  上传字节:   %s":             "  Bytes uploaded:   %s",
		"  下载文件:   %d":             "  Files downloaded: %d",
		"  下载字节:   %s":             "  Bytes downloaded: %s",
		"  删除文件:   %d":             "  Files deleted:    %d",
		"存储限制: %s / %s (%.1f%%)":   "Storage limit: %s / %s (%.1f%%)",
		"远程服务器配置:":                 "Remote server config:",
		"已设置":                      "Set",
		"未设置":                      "Not set",
	},
}

// T 返回键对应的翻译；键不存在时返回原键（fail-open，绝不返回空串——空串会破坏
// 表格布局）。CLI 无 slog 依赖，缺失键不输出日志，直接回退原文。
func T(key string) string {
	if tbl := table(); tbl != nil {
		if v, ok := tbl[key]; ok {
			return v
		}
	}
	return key
}

// F 返回翻译并用 fmt 格式化占位符（%s/%d/%v 等，与原 fmt.Fprintf 行为一致）；
// 键不存在时以原键为格式串。参数数量错误仍由 fmt 自身处理（与现状一致）。
func F(key string, args ...any) string {
	return fmt.Sprintf(T(key), args...)
}

// Locale 按 LC_ALL > LC_MESSAGES > LANG 顺序探测语言（空值跳过），返回归一化的
// 语言代码（"zh-CN" 或 "en"）。
func Locale() string {
	lang := strings.ToLower(strings.TrimSpace(firstNonEmpty(
		lookupEnv("LC_ALL"),
		lookupEnv("LC_MESSAGES"),
		lookupEnv("LANG"),
	)))
	switch {
	case lang == "" || lang == "c" || lang == "posix" ||
		strings.HasPrefix(lang, "c.") || strings.HasPrefix(lang, "posix."):
		return zhCN
	case strings.HasPrefix(lang, "zh"):
		return zhCN
	case strings.HasPrefix(lang, "en"):
		return en
	case validLocale(lang):
		// 其余合法语言代码（fr/ja/de 等）：没有翻译表，回退英文（国际通用）。
		return en
	default:
		// 非法 locale（数字开头/纯符号等）：回退默认 zh-CN。
		return zhCN
	}
}

// firstNonEmpty 返回第一个非空字符串。
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// validLocale 判断语言代码形态是否合法：语言段必须恰好 2 个小写字母
// （ISO 639-1，POSIX locale 规范形态），后跟可选区域/编码段
// （`_CN`/`-US`/`.UTF-8`，段内字母数字）。
//
// 3 字母语言段（ISO 639-2/3 扩展，如 foo/abc）判为非法：设计文档错误处理段
// 明确以 `LC_ALL=foo` 为例要求回退默认 zh-CN；真实环境变量标准形态是 2 字母。
func validLocale(s string) bool {
	i := 0
	for i < len(s) && s[i] >= 'a' && s[i] <= 'z' {
		i++
	}
	if i != 2 {
		return false
	}
	for i < len(s) {
		c := s[i]
		if c != '_' && c != '-' && c != '.' {
			return false
		}
		i++
		start := i
		for i < len(s) && isAlnum(s[i]) {
			i++
		}
		if i == start {
			return false
		}
	}
	return true
}

// isAlnum 判断字节是否为 ASCII 字母或数字。
func isAlnum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

// table 返回当前语言对应的字典表（Locale 已归一，dict 必有对应表）。
func table() map[string]string {
	return dict[Locale()]
}
