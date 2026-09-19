// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// fakePath 是测试用 PathProvider：固定 Kind、固定延迟、固定 Enabled。
type fakePath struct {
	name     string
	kind     string
	delay    time.Duration
	priority int
	enabled  bool
	callCh   chan string // 记录 Dial 调用
}

func (f *fakePath) Name() string  { return f.name }
func (f *fakePath) Priority() int { return f.priority }
func (f *fakePath) Enabled(_ context.Context, _ *client.FileClient) bool {
	return f.enabled
}

func (f *fakePath) Dial(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	if f.callCh != nil {
		f.callCh <- f.name
	}
	time.Sleep(f.delay)
	return &Result{Conn: nil, Kind: f.kind, Latency: f.delay}, nil
}

// TestSmartPathRegistry_BuiltinProviders：注册表内置 direct + relay。
// 注：不断言 len==2（全局注册表可能被并行测试的临时注入项污染——R18 门禁下所有
// 测试 t.Parallel，Register/Delete 窗口会造成 flaky）。只断言 builtin 存在即可。
func TestSmartPathRegistry_BuiltinProviders(t *testing.T) {
	t.Parallel()
	names := SmartPathRegistry.Names()
	has := func(want string) bool {
		return slices.Contains(names, want)
	}
	if !has("direct") || !has("relay") {
		t.Fatalf("builtin providers missing direct/relay: %v", names)
	}
}

// TestSmartPathRegistry_RegisterNewPath：Register 新提供者可被 Names 发现（可扩展性）。
func TestSmartPathRegistry_RegisterNewPath(t *testing.T) {
	t.Parallel()
	p := &fakePath{name: "via-node", kind: "via-node", delay: time.Millisecond, enabled: true}
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "via-node", Instance: p, Priority: 10})
	t.Cleanup(func() { SmartPathRegistry.Delete("via-node") })

	if _, ok := SmartPathRegistry.Get("via-node"); !ok {
		t.Fatal("Register 后 Get(via-node) 应命中")
	}
}
