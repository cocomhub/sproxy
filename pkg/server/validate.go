// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// ValidateFilePath 校验并规范化用户提供的文件路径（可能包含子目录）。
// 返回使用平台分隔符的清洗后相对路径，或描述性错误。
//
// 规则：
//   - 拒绝空字符串
//   - 拒绝空字节（\x00）
//   - 拒绝绝对路径（以 / 或 \ 开头）
//   - filepath.Clean 规范化
//   - 逐组件检查 ".."（路径穿越）
//   - Windows 上检查 <>:"|?* 非法字符
//   - 返回路径为 filepath.ToSlash 格式（使用 / 分隔符），适合作为 API 返回值
func ValidateFilePath(filename string) (string, error) {
	filename = strings.TrimSpace(filename)

	if filename == "" {
		return "", fmt.Errorf("文件名不能为空")
	}

	// 拒绝空字节
	if strings.ContainsRune(filename, 0) {
		return "", fmt.Errorf("文件名包含空字节")
	}

	// 拒绝绝对路径（以 / 或 \ 开头）
	if filename[0] == '/' || filename[0] == '\\' {
		return "", fmt.Errorf("文件名不能是绝对路径: %s", filename)
	}

	// 清理路径
	cleaned := filepath.Clean(filename)
	if cleaned == "." {
		return "", fmt.Errorf("无效的文件名: %s", filename)
	}

	// Clean 后再次检查绝对路径（Windows 上如 C:\ 会在 Clean 后才被 IsAbs 捕获）
	if filepath.IsAbs(cleaned) {
		return "", fmt.Errorf("文件名不能是绝对路径: %s", filename)
	}

	// 逐组件检查 ".."（路径穿越）
	parts := strings.Split(cleaned, string(filepath.Separator))
	if slices.Contains(parts, "..") {
		return "", fmt.Errorf("文件名不能包含路径穿越: %s", filename)
	}

	// Windows 非法字符检查（在 Clean 之后执行，使用 cleaned 路径）
	if runtime.GOOS == "windows" {
		const invalidChars = `<>:"|?*`
		for _, c := range cleaned {
			if strings.ContainsRune(invalidChars, c) {
				return "", fmt.Errorf("文件名包含非法字符 %q: %s", c, filename)
			}
		}
	}

	// 统一分隔符为 / 用于 API 序列化
	return filepath.ToSlash(cleaned), nil
}

// joinSafePath 在 baseDir 下安全拼接 userPath，确认结果不越界。
// userPath 必须已通过 ValidateFilePath 校验。返回安全绝对路径，失败时返回空字符串。
// 内部记录 warn 日志以便追踪非法访问尝试。
// 注意：使用 slog.Default() 而不是注入 logger，因为此函数是无状态工具函数，
// 即使 logger 未初始化也能输出日志（防御性编程设计）。
func joinSafePath(baseDir, userPath string) string {
	fullPath := filepath.Join(baseDir, userPath)
	absPath, err := filepath.Abs(fullPath)
	if err != nil {
		slog.Default().Warn("joinSafePath: Abs 解析失败", "full_path", fullPath, "error", err)
		return ""
	}
	absBase, err := filepath.Abs(baseDir)
	if err != nil {
		slog.Default().Warn("joinSafePath: baseDir Abs 解析失败", "base_dir", baseDir, "error", err)
		return ""
	}
	// 解析 absBase 的符号链接，获取规范路径（处理 Windows 短路径名如 RUNNER~1 → runneradmin）
	if resolvedBase, e := filepath.EvalSymlinks(absBase); e == nil {
		absBase = resolvedBase
	}
	// 解析 absPath 中所有已存在的目录部分的符号链接，获取规范路径
	// 注意：absBase 已被解析为规范路径（如 C:runneradmin...），
	// 而 absPath 仍是 Windows 短格式（C:RUNNER~1...），两者不可直接比较。
	// 需要逐级解析所有父目录组件，因为短路径名可能出现在任意层级。
	resolvedPath := absPath
	for p := resolvedPath; ; p = filepath.Dir(p) {
		if r, e := filepath.EvalSymlinks(p); e == nil {
			rel, _ := filepath.Rel(p, resolvedPath)
			resolvedPath = filepath.Join(r, rel)
			// 继续解析已解析路径的父目录（短路径名可能在更高层级）
			// 因为 EvalSymlinks 只解析当前路径的符号链接，不递归解析父目录
			if r == p || filepath.Dir(r) == filepath.Dir(p) {
				break
			}
			p = r // 继续从已解析的路径向上解析
		}
		if p == filepath.Dir(p) {
			break
		}
	}
	// 路径越界检查（absBase 已解析为规范路径，resolvedPath 也已解析）
	if !strings.HasPrefix(resolvedPath, absBase+string(filepath.Separator)) && resolvedPath != absBase {
		slog.Default().Warn("joinSafePath: 路径越界", "upload_dir", absBase, "resolved_path", absPath)
		return ""
	}
	// 递归检查父目录符号链接：如果 absPath 本身不存在，逐级向上检查父目录的符号链接是否指向 base 外
	resolved, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		dir := absPath
		for dir != absBase && dir != "." && dir != "/" {
			dir = filepath.Dir(dir)
			r, e := filepath.EvalSymlinks(dir)
			if e == nil {
				if !strings.HasPrefix(r, absBase+string(filepath.Separator)) && r != absBase {
					slog.Default().Warn("joinSafePath: 父目录符号链接指向外部路径", "upload_dir", absBase, "joined", absPath, "dir", dir, "real", r)
					return ""
				}
				break
			}
		}
		// 文件不存在时 EvalSymlinks 会失败，这是正常情况（例如上传前文件还不存在）
		// 此时不返回空，直接返回已校验过的 absPath
		return absPath
	}
	// 二次校验：符号链接指向的真实路径也必须在 base 内
	if !strings.HasPrefix(resolved, absBase+string(filepath.Separator)) && resolved != absBase {
		slog.Warn("joinSafePath: 符号链接指向外部路径", "upload_dir", absBase, "joined", absPath, "real", resolved)
		return ""
	}
	return absPath
}

// IsPathWithin 检查 child 路径是否在 parent 目录内（路径穿越防护）。
// 使用 filepath.Clean 标准化后通过前缀匹配判断，确保 parent 以分隔符结尾避免误判（如 /a/b 和 /a/bb）。
// 注意：
//   - 此函数仅验证路径包含关系，不保证文件实际存在。
//   - 不处理符号链接解析：如果 child 或 parent 包含符号链接，应在外层调用 filepath.EvalSymlinks 后再传入。
func IsPathWithin(child, parent string) bool {
	absChild, err := filepath.Abs(child)
	if err != nil {
		return false
	}
	absParent, err := filepath.Abs(parent)
	if err != nil {
		return false
	}
	cleanChild := filepath.Clean(absChild)
	cleanParent := filepath.Clean(absParent)
	prefix := cleanParent
	if !strings.HasSuffix(prefix, string(filepath.Separator)) {
		prefix += string(filepath.Separator)
	}
	return strings.HasPrefix(cleanChild, prefix)
}
