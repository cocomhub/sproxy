// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

func TestPluginRegistry_RegisterBaiduPCS(t *testing.T) {
	t.Parallel()
	reg := plugin.New[StorageFactory]("baidupcs-test", DefaultFactory())
	reg.Register(plugin.Plugin[StorageFactory]{Name: "baidupcs", Instance: DefaultFactory(), Priority: 1})
	if reg.Active() == nil {
		t.Fatal("Active() 不应为 nil（注册后应返回实现）")
	}
	if got := reg.Names(); len(got) != 1 || got[0] != "baidupcs" {
		t.Fatalf("Names() = %v, want [baidupcs]", got)
	}
}

func TestPluginRegistry_DefaultFallback(t *testing.T) {
	t.Parallel()
	reg := plugin.New[StorageFactory]("baidupcs-test", DefaultFactory())
	if !reg.IsDefault() {
		t.Fatal("未注册时 IsDefault() 应为 true")
	}
	// 用默认工厂创建 Storage（fake adapter）。
	s, err := reg.Active().New(StorageConfig{
		Root:    "/baidu",
		Adapter: newFakeStorageAdapter(),
		Logger:  testLogger(),
	})
	if err != nil {
		t.Fatalf("默认工厂 New 失败: %v", err)
	}
	if s == nil {
		t.Fatal("Storage 不应为 nil")
	}
}
