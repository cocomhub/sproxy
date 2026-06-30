// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func TestEmptyTransport_DialReturnsError(t *testing.T) {
	// "builtin" 是 registry 的兜底，不注册在 plugins 中
	// 通过 Active() 获取
	ln, err := xfer.Active().Dial(t.Context(), "tcp://127.0.0.1:1")
	if err == nil {
		ln.Close()
		t.Fatal("expected error from empty transport Dial")
	}
}

func TestEmptyTransport_ListenReturnsError(t *testing.T) {
	ln, err := xfer.Active().Listen(t.Context(), "tcp://127.0.0.1:0")
	if err == nil {
		ln.Close()
		t.Fatal("expected error from empty transport Listen")
	}
}

func TestRegister_EmptyName(t *testing.T) {
	// 必须 panic——使用独立 registry
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	reg := plugin.New[*xfer.Transport]("test", &xfer.Transport{Name: "builtin"})
	reg.Register(plugin.Plugin[*xfer.Transport]{Name: "", Instance: &xfer.Transport{Name: "bad"}})
}

func TestGet_NotFound(t *testing.T) {
	tr := xfer.Get("nonexistent-transport")
	if tr != nil {
		t.Fatal("expected nil for nonexistent transport")
	}
}

func TestActive_DefaultBuiltin(t *testing.T) {
	// Active() 返回 emptyTransport 而不是 nil
	tr := xfer.Active()
	if tr.Name != "builtin" {
		t.Fatalf("expected builtin name, got %q", tr.Name)
	}
}
