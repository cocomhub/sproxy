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

// TestViaNodeExpand_GeneratesDualCandidates：Expand 对每个 X 生成**双候选**
// （via-relay:X + via-direct:X）——数据面经 hub 中继 / 数据面 webrtc 直连 X 平级竞速。
//
// 这是 via-direct-X 的核心回归钉：Expand 必须为每个 outbound-dial 中间节点 X 展开
// 两条路径（规格 §3.1），若退化为单候选（仅 via-relay）则 via-direct-X 不参与竞速。
func TestViaNodeExpand_GeneratesDualCandidates(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"id":"node-x1","capabilities":["outbound-dial"]},
			{"id":"node-x2","capabilities":["outbound-dial"]}
		]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "target", Addr: "t:1"})

	// 2 个 X × 2 候选 = 4（via-relay:x1 / via-direct:x1 / via-relay:x2 / via-direct:x2）。
	if len(cands) != 4 {
		t.Fatalf("Expand = %d 候选, want 4（2 X × 双候选）", len(cands))
	}
	// 每个 X 必须同时含 via-relay 与 via-direct 两条路径。
	want := map[string]bool{
		"via-relay:node-x1":  false,
		"via-direct:node-x1": false,
		"via-relay:node-x2":  false,
		"via-direct:node-x2": false,
	}
	for _, c := range cands {
		if _, ok := want[c.ID]; !ok {
			t.Fatalf("候选 ID %q 不在期望集合中", c.ID)
		}
		want[c.ID] = true
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("缺少候选 %q", id)
		}
	}
}

// TestViaNodeExpand_DirectCandidateDialNoSignaler：via-direct:X 候选在**无信令器**
// 时 Dial 返回错误（打洞到 X 需 hub 信令桥），via-relay:X 候选不受影响（RelayStream
// 无需信令器）。
//
// 这是 via-direct-X 的 fail-closed 回归钉：打洞能力缺失时 via-direct 候选必须
// 明确失败（由竞速聚合错误），而非静默假装成功或 panic。
func TestViaNodeExpand_DirectCandidateDialNoSignaler(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[{"id":"node-x1","capabilities":["outbound-dial"]}]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{}
	target := &client.MeshService{Node: "T", Addr: "t:1"}
	cands := p.Expand(context.Background(), svc, target)
	if len(cands) != 2 {
		t.Fatalf("Expand = %d 候选, want 2（双候选）", len(cands))
	}

	// 找到 via-direct:x1 候选，用 nil 信令器 Dial → 必须报错（无 hub 信令桥无法打洞）。
	var directCand *Candidate
	for i := range cands {
		if cands[i].ID == "via-direct:node-x1" {
			directCand = &cands[i]
			break
		}
	}
	if directCand == nil {
		t.Fatal("缺少 via-direct:x1 候选")
	}
	_, err := directCand.Dial(context.Background(), svc, nil, target, "local", DialOptions{})
	if err == nil {
		t.Fatal("via-direct:X 无信令器 Dial 应返回错误（需 hub 信令桥打洞）")
	}
	if got := err.Error(); !contains(got, "无可用信令器") {
		t.Fatalf("错误应含 '无可用信令器', got %q", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})())
}

// TestDialSmart_ViaDirectWinsWhenFastest：via-direct:X（快）vs via-relay:X（慢）
// → 数据面直连胜出（端到端 RTT 优先）。
//
// 这是 via-direct-X 的竞速核心回归钉：数据面直连 X 的 RTT 显著短于经 hub 中继时，
// 竞速必须选 via-direct（而非固守 via-relay 或首个注册）。
func TestDialSmart_ViaDirectWinsWhenFastest(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突（smartWithProviders 清/注册/恢复），不可并行
	viaDirect := &fakePath{name: "via-direct:x1", kind: "via-direct", delay: 5 * time.Millisecond, priority: 80, enabled: true}
	viaRelay := &fakePath{name: "via-relay:x1", kind: "via-node", delay: 200 * time.Millisecond, priority: 80, enabled: true}
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 500 * time.Millisecond, priority: 100, enabled: true}
	// 注册顺序 direct, via-relay, via-direct——via-direct 非首个，若实现退化为「取首个注册」
	// 或「优先级最高」则 direct 胜出，断言红（证明端到端 RTT 优先）。
	smartWithProviders(t, direct, viaRelay, viaDirect)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	if res.Kind != "via-direct" {
		t.Fatalf("Kind = %s, want via-direct（数据面直连 X 的 RTT 最短胜出）", res.Kind)
	}
}

// TestViaNodeExpand_TrustedNodesWhitelist：设置 trustedNodes 白名单后，Expand
// 只生成白名单内 X 的双候选（白名单外 X 被过滤）。
//
// 这是 --trust-x 中间节点白名单（T5）的回归钉：信任收敛——竞速只选白名单内的
// 中间节点 X（X 是经手中转、可能看到流量的节点，白名单 = 信任声明）。若实现
// 忽略白名单（X2 仍在候选）→ 断言红。
func TestViaNodeExpand_TrustedNodesWhitelist(t *testing.T) {
	// 局部实例免锁直调 Set/Expand（单线程安全）：全局实例（SmartPathRegistry 注册的
	// 指针单例）SetTrustedNodes 须持 smartRegistryMu（与注册表修改同锁，见 Set 文档）。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"id":"node-x1","capabilities":["outbound-dial"]},
			{"id":"node-x2","capabilities":["outbound-dial"]}
		]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	// 白名单只信 node-x1：x2 虽 outbound-dial 但不在白名单 → 不进候选。
	p := viaNodeProvider{}
	p.SetTrustedNodes([]string{"node-x1"})
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "target", Addr: "t:1"})

	if len(cands) != 2 {
		t.Fatalf("Expand(白名单=[node-x1]) = %d 候选, want 2（仅 node-x1 双候选）; got %v", len(cands), candIDs(cands))
	}
	want := map[string]bool{
		"via-relay:node-x1":  false,
		"via-direct:node-x1": false,
	}
	for _, c := range cands {
		if _, ok := want[c.ID]; !ok {
			t.Fatalf("白名单外候选 %q 不应出现（信任收敛 fail-closed）", c.ID)
		}
		want[c.ID] = true
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("缺少白名单内候选 %q", id)
		}
	}
}

// TestViaNodeExpand_TrustedNodesEmptyAllTrusted：trustedNodes 为空 = 全部可信
// （兼容现状：未配置 --trust-x 时行为不变）。
func TestViaNodeExpand_TrustedNodesEmptyAllTrusted(t *testing.T) {
	// 局部实例免锁直调（单线程安全）；全局实例 SetTrustedNodes 须持 smartRegistryMu。
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/hub/nodes" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`[
			{"id":"node-x1","capabilities":["outbound-dial"]},
			{"id":"node-x2","capabilities":["outbound-dial"]}
		]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)

	p := viaNodeProvider{} // 未设置 trustedNodes（空）
	cands := p.Expand(context.Background(), svc, &client.MeshService{Node: "target", Addr: "t:1"})

	// 空白名单 = 全部可信：2 个 X × 双候选 = 4。
	if len(cands) != 4 {
		t.Fatalf("Expand(空白名单) = %d 候选, want 4（全部 X 可信）", len(cands))
	}
}

// candIDs 提取候选 ID 列表（断言消息用）。
func candIDs(cands []Candidate) []string {
	out := make([]string, 0, len(cands))
	for _, c := range cands {
		out = append(out, c.ID)
	}
	return out
}

// TestViaNodeProvider_SetTrustedNodesGenInvalidation：白名单**实际变化**时递增
// smartRegistryGen（缓存快照一致性闸门——信任控制不能旁路缓存 fail-open），
// 幂等 Set（同值）**不递增**（缓存保持命中，via-node 缓存机制不报废）。
//
// 这是 R1 P1 的回归钉：若 SetTrustedNodes 不递增 gen，白名单收窄后同 target +
// TTL 内缓存命中会复用旧快照（可能含已不信任的 X 胜者，信任控制缓存旁路 fail-open）。
// 需持 smartRegistryMu（与注册表修改同锁，防并行竞速竞态）。
func TestViaNodeProvider_SetTrustedNodesGenInvalidation(t *testing.T) {
	smartRegistryMu.Lock()
	defer smartRegistryMu.Unlock()

	p := &viaNodeProvider{}
	// 基线 gen（幂等 Set 前后应一致）。
	base := smartRegistryGen

	// 幂等 Set（空 → 空）：不递增。
	p.SetTrustedNodes(nil)
	if smartRegistryGen != base {
		t.Fatalf("幂等 Set(nil→nil) 后 gen = %d, want %d（不应递增，缓存保持命中）", smartRegistryGen, base)
	}

	// 实际变化（空 → [x1]）：递增。
	p.SetTrustedNodes([]string{"node-x1"})
	if smartRegistryGen != base+1 {
		t.Fatalf("Set([x1]) 后 gen = %d, want %d（白名单变化须使缓存失效）", smartRegistryGen, base+1)
	}

	// 幂等 Set（同值 [x1] → [x1]）：不递增。
	p.SetTrustedNodes([]string{"node-x1"})
	if smartRegistryGen != base+1 {
		t.Fatalf("幂等 Set([x1]→[x1]) 后 gen = %d, want %d（不应递增）", smartRegistryGen, base+1)
	}

	// 实际变化（[x1] → [x1,x2]）：递增。
	p.SetTrustedNodes([]string{"node-x1", "node-x2"})
	if smartRegistryGen != base+2 {
		t.Fatalf("Set([x1,x2]) 后 gen = %d, want %d（白名单变化须使缓存失效）", smartRegistryGen, base+2)
	}
}

// TestViaNodeProvider_TrustedNodesCacheMiss：白名单变化（gen 递增）→ 同 target
// 的胜者缓存条目被判定过期（miss）——信任控制不旁路缓存。
//
// 结构：手动写缓存条目（携带 Set 前的 gen）→ SetTrustedNodes 变化（gen 递增）→
// smartCacheGet 返回 miss（gen 不一致）；幂等 Set（gen 不变）→ 缓存仍命中。
// 这是 R1 P1 的端到端回归钉：白名单收窄必须使旧快照失效，而非 TTL 内复用。
func TestViaNodeProvider_TrustedNodesCacheMiss(t *testing.T) {
	smartRegistryMu.Lock()
	defer smartRegistryMu.Unlock()
	smartCacheClear()

	p := &viaNodeProvider{}
	// 1. 幂等 Set 后写缓存（gen = base）。
	base := smartRegistryGen
	p.SetTrustedNodes(nil)
	if smartRegistryGen != base {
		t.Fatalf("幂等 Set 不应递增 gen: got %d want %d", smartRegistryGen, base)
	}
	smartCacheSet("target", "via-relay:node-x1", &Candidate{ID: "via-relay:node-x1"}, 10*time.Millisecond, time.Minute)
	if _, ok := smartCacheGet("target"); !ok {
		t.Fatal("幂等 Set 后缓存应命中（gen 未变）")
	}

	// 2. 白名单变化 → gen 递增 → 缓存 miss（旧快照失效，信任控制生效）。
	p.SetTrustedNodes([]string{"node-x1"})
	if _, ok := smartCacheGet("target"); ok {
		t.Fatal("白名单变化后缓存应 miss（gen 递增使旧快照失效）")
	}
}
