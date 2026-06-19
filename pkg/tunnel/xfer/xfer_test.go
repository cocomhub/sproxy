// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestRegisterAndGet(t *testing.T) {
	// 使用独立 Registry 实例，不污染全局 TransportRegistry。
	// 否则注册的 transport 可能被 Active() 选中，导致依赖 builtin 的测试失败。
	reg := plugin.New[*xfer.Transport]("test", &xfer.Transport{
		Name:   "builtin",
		Dial:   func(_ context.Context, _ string) (xfer.Conn, error) { return nil, xfer.ErrNoTransport },
		Listen: func(_ context.Context, _ string) (xfer.Listener, error) { return nil, xfer.ErrNoTransport },
	})

	t.Run("empty registry returns builtin", func(t *testing.T) {
		got := reg.Active()
		if got == nil {
			t.Fatal("Active() returned nil")
		}
	})

	t.Run("Get returns nil for unknown", func(t *testing.T) {
		got, ok := reg.Get("nonexistent")
		if ok || got != nil {
			t.Fatal("expected nil, false for unknown transport")
		}
	})

	t1 := &xfer.Transport{
		Name:   "test",
		Dial:   func(_ context.Context, _ string) (xfer.Conn, error) { return nil, xfer.ErrNoTransport },
		Listen: func(_ context.Context, _ string) (xfer.Listener, error) { return nil, xfer.ErrNoTransport },
	}
	reg.Register(plugin.Plugin[*xfer.Transport]{
		Name:     t1.Name,
		Instance: t1,
		Priority: 0,
	})

	t.Run("Get returns registered transport", func(t *testing.T) {
		got, ok := reg.Get("test")
		if !ok || got == nil {
			t.Fatal("expected registered transport")
		}
		if got != t1 {
			t.Fatal("expected the same transport instance")
		}
	})

	t.Run("duplicate register overwrites", func(t *testing.T) {
		reg.Register(plugin.Plugin[*xfer.Transport]{
			Name:     t1.Name,
			Instance: t1,
			Priority: 0,
		})
		got, ok := reg.Get("test")
		if !ok || got == nil {
			t.Fatal("expected transport after re-register")
		}
	})
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

func TestConnSendAfterClose(t *testing.T) {
	client, server := xfertest.Pipe()
	defer client.Close()
	defer server.Close()

	client.Close()
	err := client.Send(context.Background(), []byte("after close"))
	if err == nil {
		t.Fatal("expected error sending after close")
	}
}

func TestConnReceiveAfterClose(t *testing.T) {
	client, server := xfertest.Pipe()
	defer client.Close()
	defer server.Close()

	client.Close()
	_, err := client.Receive(context.Background())
	if err == nil {
		t.Fatal("expected error receiving after close")
	}
}
