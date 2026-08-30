// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package client

import "context"

// MakeDir 在服务端创建指定子目录（含中间目录）。
//
// 命名对齐 pkg/sync.FS 接口的 MakeDir（LocalFS.MakeDir）；实现委托既有 Mkdir
// （POST /mkdir?dirname=）。供 pkg/sync.HTTPTransport 的 FS.MakeDir 使用。
func (c *FileClient) MakeDir(ctx context.Context, dirname string) error {
	return c.Mkdir(ctx, dirname)
}
