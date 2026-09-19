// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// fakeExpandCountingPath 是测试用 PathProvider：记录 Expand 调用次数（断言缓存命中
// 不重新展开），Dial 恒成功返回 net.Pipe 的连接。
type fakeExpandCountingPath struct {
	name     string
	priority int
	mu       sync.Mutex
	expandN  int
}

func (p *fakeExpandCountingPath) Name() string  { return p.name }
func (p *fakeExpandCountingPath) Priority() int { return p.priority }
func (p *fakeExpandCountingPath) Enabled(context.Context, *client.FileClient) bool {
	return true
}

func (p *fakeExpandCountingPath) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.expandN
}

func (p *fakeExpandCountingPath) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	p.mu.Lock()
	p.expandN++
	p.mu.Unlock()
	_, dialConn := net.Pipe() // 恒返回可成功拨号路径（竞速中可能被 drainOutcomes 关闭）
	return []Candidate{{
		ID:       p.name,
		Priority: p.priority,
		Dial: func(context.Context, *client.FileClient, webrtc.Signaler,
			*client.MeshService, string, DialOptions) (*Result, error) {
			return &Result{Conn: dialConn, Kind: "mem", Latency: time.Millisecond}, nil
		},
	}}
}

// TestDialSmart_CacheHitSkipsExpand：缓存命中应**跳过 Expand**（候选索引：胜出时刻
// 快照直接复用，零 Expand、零 ListHubNodes 网络 I/O）。
//
// 这是审查 Minor-3 的回归钉：via-node 的 Expand 每次做 ListHubNodes HTTP 往返，
// 「TTL 内纯内存复用」的初衷对 via-node 必须成立——缓存命中不得重新展开。
func TestDialSmart_CacheHitSkipsExpand(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakeExpandCountingPath{name: "direct", priority: 100}
	relay := &fakeExpandCountingPath{name: "relay", priority: 50}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次竞速：两路都 Expand + Dial，胜者写缓存（快照）。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	// 首次：每提供者 Expand 恰 1 次（竞速收集阶段）。
	if got := direct.count(); got != 1 {
		t.Fatalf("direct Expand 次数 = %d, want 1", got)
	}

	// 缓存命中：只走胜者快照 Dial，**不得重新 Expand**（候选索引核心：gen 未变零展开）。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("cached dial: %v", err)
	}
	if got := direct.count(); got != 1 {
		t.Fatalf("缓存命中后 direct Expand 次数 = %d, want 1（不得重新展开）", got)
	}
	if got := relay.count(); got != 1 {
		t.Fatalf("缓存命中后 relay Expand 次数 = %d, want 1（不得重新展开）", got)
	}
}

// TestDialSmart_RegistryGenChangeForcesRerace：注册表代次（gen）变化 → 缓存快照失效
// → 强制重新竞速（快照可能过期：提供者注册/删除/替换，候选集合已变）。
//
// 场景：首次竞速 direct 胜出写缓存 → 注册表被改（Delete+Register relay，gen 递增）
// → 再次 DialSmart 必须重新竞速（两路都 Expand + Dial），而非复用旧快照。
func TestDialSmart_RegistryGenChangeForcesRerace(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	direct := &fakeExpandCountingPath{name: "direct", priority: 100}
	relay := &fakeExpandCountingPath{name: "relay", priority: 50}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次竞速：direct 胜出（候选 ID 写缓存）。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	if got := direct.count(); got != 1 {
		t.Fatalf("direct Expand 次数 = %d, want 1", got)
	}

	// 注册表代次递增：删除并重注册 relay（模拟运行期插件注册/替换）。
	smartRegistryMu.Lock()
	deleteProvider("relay")
	registerProvider(plugin.Plugin[PathProvider]{Name: "relay", Instance: relay, Priority: 50})
	smartRegistryMu.Unlock()

	// gen 变化 → 缓存快照失效 → 重新竞速：两路都重新 Expand + Dial。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("rerace dial: %v", err)
	}
	if got := direct.count(); got != 2 {
		t.Fatalf("gen 变化后 direct Expand 次数 = %d, want 2（必须重新展开）", got)
	}
	if got := relay.count(); got != 2 {
		t.Fatalf("gen 变化后 relay Expand 次数 = %d, want 2（必须重新展开）", got)
	}
}
