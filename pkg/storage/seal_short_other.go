// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package storage

// seal_short_other.go —— 非 Windows 平台无 8.3 短名概念（等价形态由 Unicode NFC 归一覆盖，
// 见 sealCompareKey）；恒返回「不掌握」，使别名展开退化为 no-op。

func resolveShortPath(string) (string, bool) { return "", false }
