// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
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
//
// 注意：本函数仅做基础路径清洗，**不**做租户/功能桶隔离。用户文件路径到租户
// user/ 桶的映射与段名校验（含 .__ 内部前缀拒绝、Windows 保留设备名等）统一由
// pkg/storage 的 Tenant.UserRel / ValidSegmentName 承担——写入/读取侧 handler 在
// ValidateFilePath 后必须再经 UserRel 判定，避免触碰服务端内部桶。sync 等子包
// 仍引用本函数做基础清洗，故保留（指向 pkg/storage 的归一原语 NormalizeRemote）。
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

	// 注意：.__ 首段拦截**不**放在此处——ValidateFilePath 被 upload/sync 等写路径
	// 复用，若全局拒绝 .__ 首段会破坏含 .__ 前缀文件的同步推送。服务端内部目录访问
	// 防护收敛到 pkg/storage.Tenant.UserRel/FeatureRel 的段名校验（ValidSegmentName
	// 拒绝 .__ 前缀）与 hasServiceInternalPrefix（读取侧响应开始前拦截）。

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

// hasServiceInternalPrefix 判断 rel 路径中任意段是否携带服务端内部目录前缀标记。
// 对齐 pkg/storage.Tenant.UserRel 的判定语义：.__ 前缀任意深度拒绝（ValidSegmentName）；
// __ 前缀仅首段拒绝（isLegacyUnderscorePrefix）。供读取侧 handler（归档源等）在响应
// 开始前拦截——UserRel 虽已保证 user/ 桶内映射，但部分路径在流式输出后才解析，
// 需提前给出明确 400（归档源 validateArchiveFiles）。
func hasServiceInternalPrefix(rel string) bool {
	segs := strings.FieldsFunc(rel, func(r rune) bool { return r == '/' || r == '\\' })
	for i, seg := range segs {
		if strings.HasPrefix(seg, ".__") {
			return true
		}
		if i == 0 && strings.HasPrefix(seg, "__") {
			return true
		}
	}
	return false
}
