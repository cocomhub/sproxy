// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// cloudTaskResponse 表示云端下载任务响应，与 client.CloudTask 字段对齐。
// 被 cloud_list.go 通过 cloudTaskInfo 类型别名引用，output.go 也依赖此类型。
type cloudTaskResponse struct {
	ID         string `json:"id"`
	URL        string `json:"url"`
	Filename   string `json:"filename"`
	Status     string `json:"status"`
	TotalSize  int64  `json:"total_size"`
	Downloaded int64  `json:"downloaded"`
	Checksum   string `json:"checksum"`
	Error      string `json:"error"`
}
