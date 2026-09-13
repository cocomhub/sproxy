// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clientfactory

import (
	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
)

// mock.go 提供**仅供测试**的 Factory 实现：让 cmd/sclient 的命令测试能直接注入预配置的
// *client.FileClient，而不经过配置加载与真实网络。它不在任何生产路径上被引用。
//
// 为何放在同包的非 _test.go 文件里：Go 的 *_test.go 中的符号无法被**其他包**的测试导入，
// 而 cmd/sclient 的测试文件都以 clientfactory.NewMock 构造受测命令。

// mockFactory 是测试实现，直接返回预配置的 client。
type mockFactory struct {
	client *client.FileClient
	err    error
}

// NewMock 创建测试实现的 Factory。
func NewMock(c *client.FileClient, err error) Factory {
	return &mockFactory{client: c, err: err}
}

// NewClient 直接返回预配置的 client 与 error，不做任何配置加载。
func (f *mockFactory) NewClient(_ *cobra.Command) (*client.FileClient, error) {
	return f.client, f.err
}

// 编译期检查 mockFactory 实现 Factory 接口。
var _ Factory = (*mockFactory)(nil)
