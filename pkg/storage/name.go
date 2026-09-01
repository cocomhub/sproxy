// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package storage 提供多租户存储布局领域：Root（os.Root 封装 + LAYOUT_VERSION）、
// Tenant（租户目录布局 + UserRel/FeatureRel 路径判定）、段名校验单一权威。
// 不引入 pkg/quota（配额由 pkg/server 按 tenant.ID 关联）；meta 桶经 pkg/store 接入。
package storage

import "strings"

// windowsReservedBaseNames 是 Windows 保留设备名集合（基名判定：取首个 . 前的大写形式）。
// 仅 COM1-COM9 / LPT1-LPT9 保留，COM10/LPT10 合法。
var windowsReservedBaseNames = map[string]struct{}{
	"CON": {}, "NUL": {}, "PRN": {}, "AUX": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {},
	"COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {},
	"LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
}

// ValidSegmentName 校验 name 是否可作为单个路径段（租户名、upload_id、文件名段共用）。
// 拒绝：空、. / ..、含 / 或 \、Windows 非法字符 <>:"|?*、以 .__ 开头（魔法前缀禁止）、
// Windows 保留设备名（基名判定：CON/NUL/PRN/AUX/COM1-9/LPT1-9，含 CON.txt 形式）、
// 尾点/尾空格（Windows 文件系统会剥除导致目录合并）、长度 > 255。
func ValidSegmentName(name string) bool {
	if name == "" {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	if strings.ContainsAny(name, "/\\") {
		return false
	}
	if strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	if strings.HasPrefix(name, ".__") {
		return false
	}
	if strings.HasSuffix(name, ".") || strings.HasSuffix(name, " ") {
		return false
	}
	if len(name) > 255 {
		return false
	}
	// 保留设备名基名判定：取首个 . 前的大写形式。
	base := name
	if before, _, ok := strings.Cut(name, "."); ok {
		base = before
	}
	if _, ok := windowsReservedBaseNames[strings.ToUpper(base)]; ok {
		return false
	}
	return true
}
