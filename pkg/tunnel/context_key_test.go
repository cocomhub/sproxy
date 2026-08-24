// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tunnel

import (
	"context"
	"testing"
)

func TestCtxKey(t *testing.T) {
	ctx := SetTunnelKey(context.Background(), []byte("k"))
	if got := GetTunnelKey(ctx); string(got) != "k" {
		t.Fatalf("GetTunnelKey = %q", got)
	}
	if GetTunnelKey(context.Background()) != nil {
		t.Fatal("背景 ctx 无密钥应为 nil")
	}
}
