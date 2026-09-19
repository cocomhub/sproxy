// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

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
