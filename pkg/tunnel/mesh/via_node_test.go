// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
)

// TestViaNodeExpand_ReturnsXCandidates：mock hub 返回 2 个 outbound-dial 中间节点 +
// 1 个 target（应排除）→ viaNodeProvider.Expand 展开 2 候选，ID 正确。
func TestViaNodeExpand_ReturnsXCandidates(t *testing.T) {
	t.Parallel()
	// mock hub：/api/hub/nodes 返回 2 个 outbound-dial 节点 + 1 个 target（应排除）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"id":"node-x1","capabilities":["outbound-dial"]},
			{"id":"node-x2","capabilities":["outbound-dial"]},
			{"id":"target","capabilities":["outbound-dial"]}
		]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "target", Addr: "t:1"})

	if len(cands) != 2 {
		t.Fatalf("Expand = %d 候选, want 2（node-x1/x2；target 排除）", len(cands))
	}
	if cands[0].ID != "via-node:node-x1" || cands[1].ID != "via-node:node-x2" {
		t.Fatalf("候选 ID = %s,%s, want via-node:node-x1,via-node:node-x2", cands[0].ID, cands[1].ID)
	}
}

// TestViaNodeExpand_FiltersNoCapability：无 outbound-dial 标记 → 不展开（fail-closed）。
func TestViaNodeExpand_FiltersNoCapability(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"id":"node-a"}]`)) // 无 capabilities
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "T", Addr: "t:1"})
	if len(cands) != 0 {
		t.Fatalf("无能力节点不应展开: %d 候选", len(cands))
	}
}

// TestViaNodeExpand_ListHubNodesErr：hub 发现失败（500）→ Expand 返回 nil（fail-closed，
// via-node 无候选，direct/relay 仍参与竞速）。
func TestViaNodeExpand_ListHubNodesErr(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "hub down", http.StatusInternalServerError)
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "T", Addr: "t:1"})
	if len(cands) != 0 {
		t.Fatalf("发现失败应返回 0 候选（fail-closed）: %d", len(cands))
	}
}

// TestViaNodeExpand_MaxViaNodesTruncated：超过 3 个候选中间节点 → 截断到 maxViaNodes（3）。
func TestViaNodeExpand_MaxViaNodesTruncated(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"id":"node-x1","capabilities":["outbound-dial"]},
			{"id":"node-x2","capabilities":["outbound-dial"]},
			{"id":"node-x3","capabilities":["outbound-dial"]},
			{"id":"node-x4","capabilities":["outbound-dial"]},
			{"id":"node-x5","capabilities":["outbound-dial"]}
		]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "T", Addr: "t:1"})
	if len(cands) != maxViaNodes {
		t.Fatalf("Expand = %d 候选, want maxViaNodes=%d（截断）", len(cands), maxViaNodes)
	}
}

// TestDialSmart_ViaNodeWinsWhenFastest：via-node:X(快) + direct(慢) + relay(中)
// → 端到端 RTT 最短路（via-node:X）胜出。**多 node 核心场景**：经中间节点 X 中转
// 的端到端延迟（20ms）优于直连（400ms）/中继（300ms）时，竞速必须选 via-node。
func TestDialSmart_ViaNodeWinsWhenFastest(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	via := &fakePath{name: "via-node:X1", kind: "via-node", delay: 20 * time.Millisecond, priority: 80, enabled: true}
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 400 * time.Millisecond, priority: 100, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 300 * time.Millisecond, priority: 50, enabled: true}
	// 注册顺序 direct, relay, via——via 非首个注册，若实现退化为「取首个注册」则 direct 胜出，
	// 断言红（消除「最快胜出 vs 取首个」盲区）；via 胜出证明端到端 RTT 优先于注册顺序与优先级。
	smartWithProviders(t, direct, relay, via)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	if res.Kind != "via-node" {
		t.Fatalf("Kind = %s, want via-node（多跳端到端 RTT 最短胜出）", res.Kind)
	}
}
