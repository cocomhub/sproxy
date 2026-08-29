// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package main

import "syscall"

// sdpWriteFlags Unix 上附加 O_NOFOLLOW，拒绝符号链接（防攻击者预置链接覆盖敏感文件）。
func sdpWriteFlags() int { return syscall.O_NOFOLLOW }
