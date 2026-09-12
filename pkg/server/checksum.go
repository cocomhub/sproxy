// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"io"
	"os"

	"github.com/cocomhub/sproxy/pkg/checksum"
	"github.com/cocomhub/sproxy/pkg/storage"
)

// Checksum 计算 src 的 SHA-256 十六进制摘要。
// 注意：调用方负责关闭 src 如果它实现了 io.Closer（如 os.File）。
// 返回的 hex 字符串均为小写字符。
//
// **委托单一事实源**：算法实现在 L0 顶层包 checksum.Reader，本函数只保留装配层的历史
// 导出名（pkg/files 的 checksumReader 委托到同一函数）。抽取期此处与
// `pkg/files.checksumReader` 各持一份逐字相同的实现（两侧无法互相 import，不能收敛）；
// 重新内联会被 `helper_impl_drift_test.go` 的源码级断言判红。
func Checksum(src io.Reader) (string, error) {
	return checksum.Reader(src)
}

// FileChecksum 计算文件的 SHA-256 十六进制摘要。
// 调用方需确保 filename 已通过 ValidateFilePath 校验，防止路径穿越。
func FileChecksum(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Checksum(f)
}

// FileChecksumRoot 计算 storage.Root 内相对路径文件的 SHA-256 十六进制摘要。
// 全程 root 内打开，防符号链接逃逸（多租户布局迁移后的写端/冲突端校验用）。
func FileChecksumRoot(root *storage.Root, rel string) (string, error) {
	f, err := root.Open(rel)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return Checksum(f)
}

// verifyChecksum 计算 reader 的实际 SHA-256 摘要并与 expected 比较。
// expected 为空时跳过校验，返回 true。
// 注意：此函数会完全消耗 reader，调用方需确保 reader 可重复读取或已备份。
func verifyChecksum(expected string, reader io.Reader) bool {
	if expected == "" {
		return true
	}
	actual, err := Checksum(reader)
	if err != nil {
		return false
	}
	return actual == expected
}
