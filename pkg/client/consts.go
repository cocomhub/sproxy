// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

// API 路径与文件前缀常量（S1192：注册/拼接/解析共享同一常量，防拼写漂移）。
const (
	apiCredentialsBase = "/api/credentials/"
	apiCloudTasksBase  = "/api/cloud/tasks/"
	apiCloudGroupsBase = "/api/cloud/groups/"
	apiSyncTasksBase   = "/api/sync/tasks/"
	chainPrefix        = "chain-"
	tmpJSONExt         = ".tmp.json"
)
