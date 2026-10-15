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

// checkPrivatePerm 校验目录权限不含 group/other 位（Unix 权限真实可依，必须强制）。
// 非私有目录（如 0777）意味着同机其他用户可读/删/改审计文件 ⇒ fail-closed。
func checkPrivatePerm(dir string, fi os.FileInfo) error {
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return fmt.Errorf("audit: NewScopeSink: 目录 %q 权限 %04o 非私有（要求 0700：否则同机他人可读/改审计）", dir, perm)
	}
	return nil
}
