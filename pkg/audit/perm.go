// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"fmt"
	"os"
	"regexp"
)

// perm.go 保留**审计落盘的安全原语**（原 store.go 的实现细节，非 API）：
// scopeID 白名单、私有目录判定与自愈。审计是证据 ⇒ 写入目标可预测 ⇒ 必须防重定向/防读取。

// scopeIDPattern 是 scopeID 的合法形态：字母/数字开头，其后仅 [A-Za-z0-9._-]，长度 ≤128。
//
// 白名单校验是**路径安全第一道闸**：scopeID 直接参与段文件名拼接，含 "/"、".."、NUL 等
// 会逃逸段目录（写任意路径）。本包是通用能力，不能依赖「调用方一定传安全值」。
var scopeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// privatePermOK 判定权限位是否**私有**（不含 group/other 任何位）。跨平台纯函数。
func privatePermOK(perm os.FileMode) bool { return perm&0o077 == 0 }

// ensurePrivateDir 确保 dir 是**私有**（0700）且**非符号链接**的目录；否则 fail-closed。
//
// 覆盖两类实测风险：① 共享路径（如 /tmp）下被他人预置的同名目录 ⇒ 审计文件可被读取/
// 删除/篡改（审计完整性失效）；② 预置符号链接 ⇒ 写入被重定向到任意路径。
func ensurePrivateDir(dir string) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("audit: stat 段目录 %q: %w", dir, err)
		}
		if merr := os.MkdirAll(dir, 0o700); merr != nil {
			return fmt.Errorf("audit: 创建段目录 %q: %w", dir, merr)
		}
		fi, err = os.Lstat(dir)
		if err != nil {
			return fmt.Errorf("audit: stat 段目录 %q: %w", dir, err)
		}
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("audit: 段目录 %q 是符号链接，拒绝使用（防审计写入被重定向）", dir)
	}
	if !fi.IsDir() {
		return fmt.Errorf("audit: %q 不是目录", dir)
	}
	if privatePermOK(fi.Mode().Perm()) {
		return nil // 已私有：无需自愈（Unix 上真实可依；非 Unix 由 checkPrivatePerm 放行）
	}
	return checkPrivatePerm(dir, fi)
}
