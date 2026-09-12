// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"os"
)

// list_handler.go 在只读面（列表 / 搜索）平铺进 `pkg/files` 后，只保留装配层仍需的两样：
//
//   - 路由注册引用的**薄适配**（路由 pattern 与处理器名逐字不变，实体在 pkg/files/read.go）；
//   - `verifyFileWithChecksum`（os.Open 版本，`pkg/server/checksum_test.go` 直接测它）。
//     它不属只读面：`pkg/files` 内下载/stat 用的是 root 相对版本 `verifyFileWithChecksumRoot`，
//     两者是不同入口（一个收绝对路径、一个收 storage.Root + rel），随本文件保留于此。
//
// 列表 DTO 只有一份定义，在 `pkg/files/read.go`（`FileInfo` / `ListResponse`）——它们在
// pkg/server 已无非测试消费者，故不再保留同名副本（响应 JSON 形状由该处定义唯一决定）。

// listFiles 是 GET /api/files 的薄适配（实体：files.Service.ListFiles）。
func (h *Handlers) listFiles(w http.ResponseWriter, r *http.Request) {
	h.fileService().ListFiles(w, r)
}

// searchFiles 是 GET /api/files/search 的薄适配（实体：files.Service.SearchFiles）。
func (h *Handlers) searchFiles(w http.ResponseWriter, r *http.Request) {
	h.fileService().SearchFiles(w, r)
}

// verifyFileWithChecksum 验证文件 SHA-256 checksum 是否匹配。
func verifyFileWithChecksum(filePath, expectedChecksum string) bool {
	f, err := os.Open(filePath)
	if err != nil {
		return false
	}
	defer f.Close()
	return verifyChecksum(expectedChecksum, f)
}
