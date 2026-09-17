// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package baidupcs

import (
	"context"
	"io"
	"testing"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

// storageFactory 是 plugin 注册的 Storage 工厂接口（最小面）。
type storageFactory interface {
	New(cfg StorageConfig) (*Storage, error)
}

// testFactory 是测试用工厂。
type testFactory struct{}

func (testFactory) New(cfg StorageConfig) (*Storage, error) {
	return NewStorage(cfg)
}

func TestPluginRegistry_RegisterBaiduPCS(t *testing.T) {
	t.Parallel()
	reg := plugin.New[storageFactory]("baidupcs-test", testFactory{})
	reg.Register(plugin.Plugin[storageFactory]{Name: "baidupcs", Instance: testFactory{}, Priority: 1})
	if reg.Active() == nil {
		t.Fatal("Active() 不应为 nil（注册后应返回实现）")
	}
	if got := reg.Names(); len(got) != 1 || got[0] != "baidupcs" {
		t.Fatalf("Names() = %v, want [baidupcs]", got)
	}
}

func TestPluginRegistry_DefaultFallback(t *testing.T) {
	t.Parallel()
	reg := plugin.New[storageFactory]("baidupcs-test", testFactory{})
	if !reg.IsDefault() {
		t.Fatal("未注册时 IsDefault() 应为 true")
	}
	// 用默认工厂创建一个真实 Storage（fake adapter）。
	s, err := reg.Active().New(StorageConfig{Root: "/baidu", Adapter: newFakeStorageAdapter(), Logger: testLogger()})
	if err != nil {
		t.Fatalf("默认工厂 New 失败: %v", err)
	}
	if s == nil {
		t.Fatal("Storage 不应为 nil")
	}
}

var _ = context.Background
var _ = io.Discard
