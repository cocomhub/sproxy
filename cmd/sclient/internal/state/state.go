// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package state 管理 CLI 的漫游当前目录。
// 每个测试应使用独立的 State 实例，无需全局变量。
package state

import (
	"fmt"
	"path/filepath"
	"strings"
)

// State 管理 CLI 的漫游当前目录（**含所在卷**）。
type State struct {
	// CurrentDir 是当前目录（卷内相对路径，空 = 卷根）。
	CurrentDir string
	// CurrentVolume 是当前目录所在卷（空 = 本地卷 / auto）。
	// 由 `cd <卷名>:/<路径>` 设置；`cd /<路径>`（前导斜杠）显式回到**本地卷**。
	// 持久化在 XDG 缓存（与 CurrentDir 并列的独立文件：旧缓存没有该文件 = 本地卷，零兼容负担）。
	CurrentVolume string
}

// Location 返回当前工作目录的可读定位：`<卷名>:/<目录>`（本地卷省略卷名 ⇒ `/目录`）。
func (s *State) Location() string {
	if s == nil {
		return "/"
	}
	if s.CurrentVolume == "" {
		if s.CurrentDir == "" {
			return "/"
		}
		return "/" + s.CurrentDir
	}
	return s.CurrentVolume + ":/" + s.CurrentDir
}

// ResolveRemotePath 根据当前目录和用户传入的路径，返回完整的远端路径。
// 绝对路径（/ 开头）绕过 currentDir；相对路径拼接 currentDir。
// 包含父级引用（..）时返回错误。
func (s *State) ResolveRemotePath(userPath string) (string, error) {
	if userPath == "" {
		return s.CurrentDir, nil
	}

	// 在路径拼接和 Clean 之前检查父级引用，防止 ../ 被 Clean 解析掉
	if containsParentRef(userPath) {
		return "", fmt.Errorf("路径包含父级引用 '..'，禁止访问上层目录: %s", userPath)
	}

	var raw string
	switch {
	case strings.HasPrefix(userPath, "/"):
		raw = userPath[1:]
	case s.CurrentDir != "":
		raw = s.CurrentDir + "/" + userPath
	default:
		raw = userPath
	}

	// 拼接后再次检查（CurrentDir 本身可能含 ..）
	if containsParentRef(raw) {
		return "", fmt.Errorf("路径包含父级引用 '..'，禁止访问上层目录: %s", userPath)
	}

	cleaned := filepath.ToSlash(filepath.Clean(raw))
	if cleaned == "." {
		cleaned = ""
	}
	return cleaned, nil
}

// containsParentRef 检查路径是否包含父级引用（..）作为路径组件。
func containsParentRef(path string) bool {
	return path == ".." || strings.HasPrefix(path, "../") || strings.HasSuffix(path, "/..") || strings.Contains(path, "/../")
}

// ResolveRemotePathOrErr 是 ResolveRemotePath 的便捷封装，返回 error。
func (s *State) ResolveRemotePathOrErr(userPath string) (string, error) {
	cleaned, err := s.ResolveRemotePath(userPath)
	if err != nil {
		return "", fmt.Errorf("无效的路径: %w", err)
	}
	return cleaned, nil
}
