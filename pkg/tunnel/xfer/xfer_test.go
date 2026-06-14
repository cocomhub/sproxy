// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func TestRegisterAndGet(t *testing.T) {
	// 验证空注册表 Get 返回 nil
	if got := xfer.Get("nonexistent"); got != nil {
		t.Fatal("expected nil for unknown transport")
	}

	// 注册一个测试 Transport
	t1 := &xfer.Transport{Name: "test", Dial: nil, Listen: nil}
	xfer.Register(t1)
	if got := xfer.Get("test"); got != t1 {
		t.Fatal("expected registered transport")
	}

	// 重复注册不会 panic（新 Registry 覆盖而非 panic）
	xfer.Register(t1)
}

func TestRegisterNilPanics(t *testing.T) {
	// 注册 nil Transport 因访问 nil 字段而 panic
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Transport")
		}
	}()
	xfer.Register(nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	xfer.Register(&xfer.Transport{Name: ""})
}
