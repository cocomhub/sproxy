// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package storage

import (
	"syscall"
	"unsafe"
)

// seal_short_windows.go —— Windows 8.3 短名解析（封印别名展开用；见 seal.go sealAliasesOf）。
//
// 只依赖 stdlib syscall（kernel32!GetShortPathNameW），不引入新模块依赖。实测（2026-10-12，
// Windows/NTFS 本机）：现代 Windows 默认不为长名分配 8.3 短名（`dir /x` 无短名列、`%~sI`
// 展开等于长名）⇒ 本函数在这些卷上返回 ok=false（零回归）；启用了 8.3 的卷才会给出别名。

var procGetShortPathNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetShortPathNameW")

// resolveShortPath 返回 abs 的 8.3 短路径；无短名（未启用/无该项）或调用失败返回 ok=false。
// 注意：返回的短路径可能与入参大小写不同，调用方按 sealCompareKey 比较，不应依赖字节相等。
func resolveShortPath(abs string) (string, bool) {
	p, err := syscall.UTF16PtrFromString(abs)
	if err != nil {
		return "", false
	}
	buf := make([]uint16, 512)
	for range 4 { // 必要时扩容重试（上限 4 次，防恶意超长路径死循环）
		n, _, _ := procGetShortPathNameW.Call(
			uintptr(unsafe.Pointer(p)),
			uintptr(unsafe.Pointer(&buf[0])),
			uintptr(len(buf)),
		)
		switch {
		case n == 0:
			return "", false // 路径不存在/无权限/未分配短名
		case int(n) < len(buf):
			return syscall.UTF16ToString(buf[:n]), true
		default:
			buf = make([]uint16, n+1) // 缓冲区不足（n = 所需长度，含 NUL）→ 扩容重试
		}
	}
	return "", false
}
