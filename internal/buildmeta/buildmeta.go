// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package buildmeta 内嵌构建时生成的 dirty_info.txt，供各 cmd 二进制共享构建元信息。
// 文件由 Makefile 在 prepare 阶段生成：git diff HEAD > internal/build/dirty_info.txt。
package buildmeta

import (
	"crypto/md5" //nolint:gosec // 构建指纹，非安全场景（G501）
	_ "embed"
	"fmt"
)

//go:embed build/dirty_info.txt
var dirtyInfo string

// DirtyInfo 返回未提交变更 diff；干净工作区为空串。
func DirtyInfo() string { return dirtyInfo }

// DirtyID 返回 dirty_info 的 10 位 md5 摘要；干净工作区返回 "clean"。
// 与 github.com/cocomhub/buildinfo.Info.DirtyID 同规则。
func DirtyID() string {
	if dirtyInfo == "" {
		return "clean"
	}
	return md5hex10(dirtyInfo)
}

// md5hex10 返回输入字符串 md5 摘要的十六进制前 10 位；空串返回 "clean"。
// 仅用于生成构建指纹（非加密用途）。
func md5hex10(s string) string {
	if s == "" {
		return "clean"
	}
	v := md5.Sum([]byte(s)) //nolint:gosec // 构建指纹，非安全场景（G401）
	return fmt.Sprintf("%x", v)[:10]
}
