// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package clientfactory 提供客户端创建工厂的接口和实现。
// 生产实现通过配置加载创建 client.Service，测试实现直接返回 mock。
package clientfactory

import (
	"fmt"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Factory 抽象客户端创建，生产/测试可替换。
type Factory interface {
	// NewClient 从 cobra 命令和配置创建 client.Service。
	NewClient(cmd *cobra.Command) (client.Service, error)
}

// factory 是生产实现，封装配置加载 + flag 覆盖 + 客户端构造。
type factory struct {
	cfgFile     string
	cfgProvider cfgBinder
}

// cfgBinder 抽象 flag 绑定能力，避免直接依赖 *pflag.Flag 类型。
type cfgBinder interface {
	BindPFlag(key string, flag *pflag.Flag)
}

// New 创建生产实现的 Factory。
func New(cfgFile string, cfgProvider cfgBinder) Factory {
	return &factory{
		cfgFile:     cfgFile,
		cfgProvider: cfgProvider,
	}
}

// NewClient 当前为占位实现，后续 PR 中完善。
func (f *factory) NewClient(cmd *cobra.Command) (client.Service, error) {
	return nil, fmt.Errorf("生产 Factory 尚未实现，请使用 mockFactory 进行测试")
}

// mockFactory 是测试实现，直接返回预配置的 client。
type mockFactory struct {
	svc client.Service
	err error
}

// NewMock 创建测试实现的 Factory。
func NewMock(svc client.Service, err error) Factory {
	return &mockFactory{svc: svc, err: err}
}

func (f *mockFactory) NewClient(cmd *cobra.Command) (client.Service, error) {
	return f.svc, f.err
}

// 编译期检查 mockFactory 实现 Factory 接口
var _ Factory = (*mockFactory)(nil)

// 编译期检查 factory 实现 Factory 接口
var _ Factory = (*factory)(nil)
