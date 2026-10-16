// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package audit

import (
	"fmt"
	"os"
	"syscall"
)

// openAuditFile 以 O_NOFOLLOW 打开审计文件：目标是符号链接时**原子失败**（无 TOCTOU 窗口）。
//
// 为什么必须防符号链接：审计文件的写入目标是**可预测**的（<dir>/<scopeID>.audit.log），
// 同机攻击者若能预置同名软链，就能把审计写入重定向到任意可写文件（伪造/破坏证据）。
func openAuditFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
}

// checkPrivatePerm 确保目录私有（不含 group/other 位）。
//
// Unix 权限位真实可依 ⇒ 必须强制；但**既有目录可能本就 0755**（如用户缓存目录被其它工具
// 预建、测试用 t.TempDir()）⇒ 先尝试**自愈**（chmod 0700），仍不可私有才 fail-closed。
// 只读拒绝会令审计在常见环境下整体不可用，而「审计目录最终必须私有」才是安全目标。
func checkPrivatePerm(dir string, fi os.FileInfo) error {
	if privatePermOK(fi.Mode().Perm()) {
		return nil
	}
	if cerr := os.Chmod(dir, 0o700); cerr != nil {
		return fmt.Errorf("audit: NewScopeSink: 目录 %q 权限 %04o 非私有且无法收紧: %w",
			dir, fi.Mode().Perm(), cerr)
	}
	after, serr := os.Lstat(dir)
	if serr != nil {
		return fmt.Errorf("audit: NewScopeSink: stat %q: %w", dir, serr)
	}
	if !privatePermOK(after.Mode().Perm()) {
		return fmt.Errorf("audit: NewScopeSink: 目录 %q 权限 %04o 非私有（要求 0700：否则同机他人可读/改审计）",
			dir, after.Mode().Perm())
	}
	return nil
}
