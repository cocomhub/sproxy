// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package audit

import (
	"fmt"
	"os"
)

// openAuditFile 打开审计文件（非 Unix 平台无 O_NOFOLLOW ⇒ 先 Lstat 拒符号链接）。
//
// Windows 上创建符号链接需特权（或开发者模式），风险面明显小于 Unix；此处仍做打开前
// Lstat 校验，保持与 Unix 分支**同语义**（同一份审计安全契约，不做平台差异化降级）。
func openAuditFile(path string) (*os.File, error) {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("audit: 目标 %q 是符号链接，拒绝写入（防审计重定向）", path)
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// checkPrivatePerm 在非 Unix 平台不校验权限位：Windows 的 `Mode().Perm()` 不反映 ACL
// （`os.MkdirAll(dir, 0o700)` 建出的目录也报 0777）⇒ 按位校验会把审计**整体误禁**。
// 访问控制由目录 ACL（继承父目录）负责；跨平台的符号链接校验仍在 ensurePrivateDir 生效。
func checkPrivatePerm(string, os.FileInfo) error { return nil }
