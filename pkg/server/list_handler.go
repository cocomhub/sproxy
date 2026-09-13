// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
)

// list_handler.go 在只读面（列表 / 搜索）平铺进 `pkg/files` 后，只保留装配层仍需的
// 路由注册**薄适配**（路由 pattern 与处理器名逐字不变，实体在 pkg/files/read.go）。
//
// 历史：本文件曾另含 `verifyFileWithChecksum`（os.Open 版本的校验和比对）。它的消费者只有
// `pkg/server/checksum_test.go`，属**纯测试辅助**却挂在生产文件里，已随之迁入该测试文件。
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
