// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func TestEmptyTransport_DialReturnsError(t *testing.T) {
	// "builtin" 是 registry 的兜底，不注册在 plugins 中
	// 通过 Active() 获取
	ln, err := xfer.TransportRegistry.Active().Dial(context.Background(), "tcp://127.0.0.1:1")
	if err == nil {
		ln.Close()
		t.Fatal("expected error from empty transport Dial")
	}
}

func TestEmptyTransport_ListenReturnsError(t *testing.T) {
	ln, err := xfer.TransportRegistry.Active().Listen(context.Background(), "tcp://127.0.0.1:0")
	if err == nil {
		ln.Close()
		t.Fatal("expected error from empty transport Listen")
	}
}
