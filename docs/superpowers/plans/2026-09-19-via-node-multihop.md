<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# via-node 多跳提供者（候选展开模型）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 将 SmartDial 的 `PathProvider` 重构为候选展开模型，新增 via-node 真实多跳提供者（经中间节点 X 中转，端到端 RTT 择优），并透出 hub 节点 Capabilities 支持 X 发现。

**架构：** `PathProvider` 接口从 `Dial` 改为 `Expand() []Candidate`——竞速核心只理解 `Candidate`（最小单位），via-node 展开为多个 X 候选（每个 = `RelayStream(X, T)`，X 出站拨 T，零新协议）。hub 侧 `NodeInfo.Capabilities` 持久化 + `ListHubNodes()` 透出，via-node 据此发现候选 X。

**技术栈：** Go 标准库 + 现有 `plugin.Registry[T]` + 现有 `RelayStream` 语义。

**规格：** `docs/superpowers/specs/2026-09-19-via-node-multihop-design.md`（本计划的论证依据；执行者两份都读）

## 全局约束

- 纯标准库测试（无 testify/gomock/gomega），沿用 `t.Fatalf`/`t.Errorf`。
- 所有含 HTTP 服务的测试监听 127.0.0.1（httptest 默认 loopback），禁止 0.0.0.0/localhost。
- Windows 兼容：路径用 `filepath.Join`/`ToSlash`；测试不依赖 Unix-only 特性。
- SPDX 头：每个新建/修改 Go 文件带 `// Copyright 2026 The Cocomhub Authors. All rights reserved.` + `// SPDX-License-Identifier: Apache-2.0`。
- 所有新增顶层测试默认 `t.Parallel()`（R18 并发注册门禁）；无法并发的显式 `// sproxy:serial:` 注释。
- **禁止 `time.Sleep(` 字面量**（R14 睡眠棘轮，只减不增）——等待用 `testutil.WaitFor` / 条件轮询。
- 候选数上限 `MaxCandidates` 默认从 4 升到 **5**（direct+relay+3X）。
- 只 `git add` 本任务文件（禁 `-A`/`.`/`-a`）。
- 提交信息用 Conventional Commits（`feat(mesh): ...`），不加署名行。
- 推送走 https/SSH；CI 全绿后才 push。

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `pkg/tunnel/mesh/smart.go` | PathProvider 接口重构（Dial→Expand）+ Candidate + 竞速核心展开 + 缓存候选 ID + MaxCandidates 默认 5 | 修改 |
| `pkg/tunnel/mesh/via_node.go` | 新建：viaNodeProvider（Expand 展开 X 候选） | 新建 |
| `pkg/tunnel/mesh/via_node_test.go` | 新建：Expand/竞速/缓存/过滤用例 | 新建 |
| `pkg/tunnel/mesh/mesh.go` | +KindViaNode 常量 | 修改 |
| `pkg/tunnel/mesh/smart_test.go` | fakePath 改造（Expand 返回单候选）+ 既有用例适配 | 修改 |
| `pkg/tunnel/hub/route_table.go` | NodeInfo.Capabilities 字段 | 修改 |
| `pkg/tunnel/hub/router.go` | registerNode 保存 reg.Capabilities | 修改 |
| `pkg/server/hub_handler.go` | nodeResp.Capabilities 透出 | 修改 |
| `pkg/client/hub.go` | HubNodeInfo.Capabilities 字段 | 修改 |
| `docs/superpowers/specs/2026-09-19-via-node-multihop-design.md` | 规格（已 commit） | 已存在 |
| `build/lab/DEPLOY-GUIDE.md` | §5.3/§7/§9 补 via-node 说明 | 修改（独立仓库） |

---

### 任务 1：hub 能力透出（NodeInfo.Capabilities → ListHubNodes）

**文件：**
- 修改：`pkg/tunnel/hub/route_table.go`（NodeInfo 结构）
- 修改：`pkg/tunnel/hub/router.go`（registerNode 保存 capabilities）
- 修改：`pkg/server/hub_handler.go`（nodeResp 透出）
- 修改：`pkg/client/hub.go`（HubNodeInfo.Capabilities）
- 测试：`pkg/tunnel/hub/router_test.go` + `pkg/server/hub_handler_test.go`（追加）

- [ ] **步骤 1：编写失败的测试**

`pkg/tunnel/hub/router_test.go` 追加（能力保存）：

```go
// TestRegisterNode_SavesCapabilities：注册帧带 outbound-dial 能力 → NodeInfo.Capabilities 保存。
func TestRegisterNode_SavesCapabilities(t *testing.T) {
	t.Parallel()
	rt := NewMeshRouteTable()
	s := &HubServer{rt: rt, logger: DiscardLogger()}
	a, _ := xfertest.Pipe()
	m := mux.New(a, mux.RoleDialer)
	t.Cleanup(func() { _ = m.Close() })

	info, err := s.registerNode(&RegisterFrame{
		NodeID:       "node-caps",
		AccessKey:    "ak-test",
		AccessKeyProof: "proof",
		TS:           time.Now().Unix(),
		Nonce:        "n1",
		Capabilities: []string{CapabilityPerNodeSecret, CapabilityOutboundDial},
	}, m)
	if err != nil {
		t.Fatalf("registerNode: %v", err)
	}
	if !slices.Contains(info.Capabilities, CapabilityOutboundDial) {
		t.Fatalf("Capabilities 未保存 outbound-dial: %v", info.Capabilities)
	}
}
```

`pkg/client/hub_test.go` 追加（HubNodeInfo 解析能力）：

```go
// TestHubNodeInfo_HasCapabilities：hub 节点列表响应含 capabilities → 解析正确。
func TestHubNodeInfo_HasCapabilities(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"id":"node-x","capabilities":["outbound-dial"]}]`))
	}))
	defer ts.Close()
	svc := client.NewFileClient(ts.URL)
	nodes, err := svc.ListHubNodes(context.Background())
	if err != nil {
		t.Fatalf("ListHubNodes: %v", err)
	}
	if len(nodes) != 1 || !slices.Contains(nodes[0].Capabilities, "outbound-dial") {
		t.Fatalf("HubNodeInfo.Capabilities 解析失败: %+v", nodes)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run "TestRegisterNode_SavesCapabilities|TestHubNodeInfo_HasCapabilities" ./pkg/tunnel/hub/ ./pkg/client/`
预期：FAIL，编译错误 `unknown field 'Capabilities' in struct literal` / `unknown field 'Capabilities' in nodeResp`

- [ ] **步骤 3：实现——能力保存 + 透出**

`pkg/tunnel/hub/route_table.go` NodeInfo 加字段：

```go
	// Capabilities 是节点注册时声明的能力标志（如 CapabilityOutboundDial）。
	// 供上层（server hub handler / client ListHubNodes）透出，支持 via-node 等发现。
	Capabilities []string
```

`pkg/tunnel/hub/router.go` registerNode 保存：

```go
	info.Capabilities = append([]string(nil), reg.Capabilities...)
```

`pkg/server/hub_handler.go` nodeResp 加字段 + 填充：

```go
		Capabilities []string `json:"capabilities,omitempty"`
	// ...
		resp = append(resp, nodeResp{
			ID:           string(n.ID),
			Addr:         n.Addr,
			VirtualIP:    vipStr,
			Connected:    n.Connected,
			Capabilities: n.Capabilities,
		})
```

`pkg/client/hub.go` HubNodeInfo 加字段：

```go
	// Capabilities 是节点声明的能力标志（如 "outbound-dial"：可作中转出口）。
	// via-node 多跳据此发现候选中间节点（fail-closed：无此标记不选）。
	Capabilities []string `json:"capabilities,omitempty"`
```

> 注意：`capabilities,omitempty` 使旧响应/无能力节点省略字段——向后兼容。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run "TestRegisterNode_SavesCapabilities|TestHubNodeInfo_HasCapabilities" ./pkg/tunnel/hub/ ./pkg/client/` + `go build ./...`
预期：PASS + 编译通过

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/hub/route_table.go pkg/tunnel/hub/router.go pkg/server/hub_handler.go pkg/client/hub.go <测试文件>
git commit -m "feat(hub): NodeInfo/HubNodeInfo 透出 Capabilities（via-node 发现前置）
```

---

### 任务 2：PathProvider 接口重构为候选展开模型

**文件：**
- 修改：`pkg/tunnel/mesh/smart.go`（接口 + direct/relay + DialSmart + 缓存）
- 修改：`pkg/tunnel/mesh/mesh.go`（+KindViaNode）
- 修改：`pkg/tunnel/mesh/smart_test.go`（fakePath 改造）

- [ ] **步骤 1：编写失败的测试（接口重构后编译失败即红灯）**

改造 `pkg/tunnel/mesh/smart_test.go` 的 fakePath（现实现 Enabled/Dial）为 Expand：

```go
// fakePath 是测试用 PathProvider：Expand 返回单候选（ID=name）。
type fakePath struct {
	name     string
	kind     string
	delay    time.Duration
	priority int
	enabled  bool
	callCh   chan string // 记录 Dial 调用
	conn     net.Conn    // 候选返回的真实连接（LoserConnClosed 用）
	closeCh  chan string // 连接关闭记录
}

func (f *fakePath) Name() string     { return f.name }
func (f *fakePath) Priority() int    { return f.priority }
func (f *fakePath) Enabled(_ context.Context, _ *client.FileClient) bool { return f.enabled }
func (f *fakePath) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	if !f.enabled {
		return nil
	}
	return []Candidate{{
		ID:       f.name,
		Priority: f.priority,
		Dial:     f.dial,
	}}
}

func (f *fakePath) dial(ctx context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	if f.callCh != nil {
		f.callCh <- f.name
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if f.conn != nil {
		return &Result{Conn: f.conn, Kind: f.kind, Latency: f.delay}, nil
	}
	return &Result{Conn: nil, Kind: f.kind, Latency: f.delay}, nil
}
```

> 注：fakePath 保留 Enabled（PathProvider 若实现它则 DialSmart 跳过 disabled——见任务 3 的
> Enabled 保留决策）。Expand 内部检查 enabled 返回 nil 或单候选。

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 ./pkg/tunnel/mesh/`
预期：FAIL，编译错误 `fakePath does not implement PathProvider (missing Expand method)` / `Dial undefined`

- [ ] **步骤 3：实现——接口重构**

`pkg/tunnel/mesh/smart.go` PathProvider 重构：

```go
// PathProvider 是 SmartDial 的一条路径类型（P1 直连 / P2 中继 / P4 经中间节点多跳）。
// 通过 Expand 展开为竞速候选——一个类型可有多个候选（via-node 的每个中间节点 X）。
type PathProvider interface {
	Name() string
	Priority() int
	Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate
}

// Candidate 是竞速核心的最小单位：一条具体路径实例。
// ID 是候选唯一标识（缓存 key，如 "via-node:node-x" / "direct" / "relay"）。
type Candidate struct {
	ID       string
	Priority int // 继承提供者 Priority（排序/截断用），自包含
	Dial     func(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
}

// Enabled 是可选接口：提供者实现它可条件启用（如 --gateway 存在时 gateway 才启用）。
// 未实现 = 恒启用。
type EnabledProvider interface {
	Enabled(ctx context.Context, svc *client.FileClient) bool
}
```

> 设计说明：原 `Enabled()` 从 PathProvider 移除，改为可选接口 `EnabledProvider`——
> 因为 Expand 已是「条件展开」入口（via-node 无候选返回 nil），恒启用提供者无需
> Enabled；条件提供者（未来 gateway）实现 EnabledProvider 让 DialSmart 跳过。
> 版本未发布，直接重构（无兼容层）。

direct/relay 改造：

```go
func (directProvider) Name() string     { return "direct" }
func (directProvider) Priority() int    { return 100 }
func (directProvider) Expand(_ context.Context, _ *client.FileClient, _ *client.MeshService) []Candidate {
	return []Candidate{{
		ID:       "direct",
		Priority: 100,
		Dial:     directDial,
	}}
}

// directDial 是直连候选的拨号函数（原 directProvider.Dial 逻辑）。
func directDial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, _ string, opts DialOptions) (*Result, error) {
	start := time.Now()
	if !SignalerUsable(signaler) || target.Node == "" {
		return nil, fmt.Errorf("direct: 无可用信令器或目标节点为空")
	}
	conn, err := DialWebRTC(ctx, signaler, target, opts.ICE)
	if err != nil {
		return nil, fmt.Errorf("direct: %w", err)
	}
	return &Result{Conn: conn, Kind: KindWebRTC, Latency: time.Since(start)}, nil
}
```

relay 同理（relayDial 函数 + Expand 返回单候选）。

`pkg/tunnel/mesh/mesh.go` 加常量：

```go
	// KindViaNode 表示经中间节点 X 中转路径（X 出站拨号到目标）。
	KindViaNode = "via-node"
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run "TestSmartPathRegistry|TestDialSmart" ./pkg/tunnel/mesh/`
预期：PASS（fakePath 已改造，DialSmart 测试用 fake 候选）

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/mesh.go pkg/tunnel/mesh/smart_test.go
git commit -m "refactor(mesh): PathProvider 重构为候选展开模型（Expand() []Candidate）
```

---

### 任务 3：DialSmart 竞速核心展开 + 缓存候选 ID

**文件：**
- 修改：`pkg/tunnel/mesh/smart.go`（竞速核心）

- [ ] **步骤 1：编写失败的测试（缓存候选 ID 断言）**

`pkg/tunnel/mesh/smart_test.go` 追加：

```go
// TestDialSmart_CacheKeyUsesCandidateID：胜者候选 ID 写入缓存（via-node:X 可区分）。
func TestDialSmart_CacheKeyUsesCandidateID(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突，不可并行
	fast := &fakePath{name: "via-node:X1", kind: "via-node", delay: time.Millisecond, priority: 80, enabled: true}
	slow := &fakePath{name: "direct", kind: "webrtc", delay: 500 * time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, fast, slow)
	smartCacheClear()

	_, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	// 缓存应存候选 ID "via-node:X1"（而非提供者名）。
	smartCache.mu.Lock()
	entry, ok := smartCache.m["T"]
	smartCache.mu.Unlock()
	if !ok || entry.Provider != "via-node:X1" {
		t.Fatalf("缓存候选 ID = %q (ok=%v), want via-node:X1", entry.Provider, ok)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestDialSmart_CacheKeyUsesCandidateID ./pkg/tunnel/mesh/`
预期：FAIL（当前缓存存提供者名 "via-node" 而非候选 ID "via-node:X1"）

- [ ] **步骤 3：实现——竞速核心展开 + 缓存候选 ID + MaxCandidates 5**

`pkg/tunnel/mesh/smart.go` 竞速核心改造：

```go
	// 2. 收集候选：遍历提供者 → Expand 展开全部候选（direct=1, relay=1, via-node=N）。
	// smartRegistryMu 串行化：避免并行测试的 smartWithProviders 在遍历 Names 中途改注册表。
	smartRegistryMu.Lock()
	cands := make([]Candidate, 0, so.MaxCandidates)
	for _, name := range SmartPathRegistry.Names() {
		p, ok := SmartPathRegistry.Get(name)
		if !ok {
			continue
		}
		if ep, ok := p.(EnabledProvider); ok && !ep.Enabled(ctx, svc) {
			continue // 条件提供者未启用
		}
		cands = append(cands, p.Expand(ctx, svc, target)...)
	}
	smartRegistryMu.Unlock()
	// 按 Candidate.Priority 降序排序 + 截断 MaxCandidates。
	slices.SortFunc(cands, func(a, b Candidate) int { return b.Priority - a.Priority })
	if len(cands) > so.MaxCandidates {
		cands = cands[:so.MaxCandidates]
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("smart dial: 无可用的路径候选")
	}

	// 3. 并行竞速：每条候选 goroutine 独立 Dial，首胜者胜出，其余关闭。
	raceCtx, raceCancel := context.WithTimeout(ctx, so.RaceWindow)
	defer raceCancel()
	outCh := make(chan smartOutcome, len(cands))
	started := 0
	for _, c := range cands {
		c := c
		go func() {
			res, err := c.Dial(raceCtx, svc, signaler, target, localNode, opts)
			outCh <- smartOutcome{name: c.ID, res: res, err: err}
		}()
		started++
	}
	// ...（错误聚合 / 首胜 / drain 逻辑不变，smartOutcome.name 已是候选 ID）
```

缓存写候选 ID：

```go
		smartCacheSet(target.Node, o.name, o.res.Latency, so.CacheTTL) // o.name = 候选 ID
```

`smartOptionsOrDefault` 的 MaxCandidates 默认 4→5：

```go
	if so.MaxCandidates == 0 {
		so.MaxCandidates = 5
	}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run "TestDialSmart" ./pkg/tunnel/mesh/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/smart_test.go
git commit -m "feat(mesh): DialSmart 竞速核心展开候选 + 缓存候选 ID（MaxCandidates 默认 5）
```

---

### 任务 4：via-node 提供者实现

**文件：**
- 创建：`pkg/tunnel/mesh/via_node.go`
- 创建：`pkg/tunnel/mesh/via_node_test.go`

- [ ] **步骤 1：编写失败的测试**

`pkg/tunnel/mesh/via_node_test.go`：

```go
// mockNodeClient 是 ListHubNodes 可注入的 FileClient 替身（viaNodeProvider 测试用）。
type mockNodeClient struct {
	nodes []client.HubNodeInfo
	err   error
}

func (m *mockNodeClient) ListHubNodes(_ context.Context) ([]client.HubNodeInfo, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.nodes, nil
}

// 注：viaNodeProvider.Expand 需要 client.FileClient 的具体类型还是接口？——
// 见任务 4 步骤 3 的设计决策（svc 抽象）。

// TestViaNodeExpand_ReturnsXCandidates：2 个 outbound-dial 节点 → 2 候选。
func TestViaNodeExpand_ReturnsXCandidates(t *testing.T) {
	t.Parallel()
	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), &mockNodeClient{nodes: []client.HubNodeInfo{
		{ID: "node-x1", Capabilities: []string{"outbound-dial"}},
		{ID: "node-x2", Capabilities: []string{"outbound-dial"}},
		{ID: "target", Capabilities: []string{"outbound-dial"}}, // == target.Node 应排除
	}}, &client.MeshService{Node: "target", Addr: "t:1"})

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
	p := viaNodeProvider{}
	cands := p.Expand(context.Background(), &mockNodeClient{nodes: []client.HubNodeInfo{
		{ID: "node-a"}, // 无能力
	}}, &client.MeshService{Node: "T", Addr: "t:1"})
	if len(cands) != 0 {
		t.Fatalf("无能力节点不应展开: %d 候选", len(cands))
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run "TestViaNodeExpand" ./pkg/tunnel/mesh/`
预期：FAIL，编译错误 `undefined: viaNodeProvider`

- [ ] **步骤 3：实现——viaNodeProvider**

**设计决策（svc 抽象）**：`PathProvider.Expand(ctx, svc *client.FileClient, target)` 的
svc 是具体类型。测试 mock 需要接口——**方案**：via-node 不依赖 svc 具体类型，Expand
内通过 `svc.ListHubNodes(ctx)` 拉取（真实 FileClient 有该方法）；测试用真实
FileClient + httptest mock hub（ListHubNodes 走 HTTP 拉取），而非 mockNodeClient。

`pkg/tunnel/mesh/via_node.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// maxViaNodes 是 via-node 展开的中间节点候选上限（受全局 MaxCandidates 约束）。
const maxViaNodes = 3

// viaNodeProvider 实现 P4 多跳：经中间节点 X 中转（X 出站拨号到目标 T）。
// 数据面经 X：本地 → hub → X（RelayStream）+ X 出站拨 T（relay.Serve 出口语义）。
type viaNodeProvider struct{}

func (viaNodeProvider) Name() string  { return "via-node" }
func (viaNodeProvider) Priority() int { return 80 } // direct(100) > via-node(80) > relay(50)

// Expand 展开为每个候选中间节点 X 的候选（ListHubNodes ∩ outbound-dial）。
func (p viaNodeProvider) Expand(ctx context.Context, svc *client.FileClient, target *client.MeshService) []Candidate {
	if svc == nil || target == nil {
		return nil
	}
	nodes, err := svc.ListHubNodes(ctx)
	if err != nil {
		return nil // 发现失败：无 via-node 候选（direct/relay 仍参与竞速）
	}
	out := make([]Candidate, 0, maxViaNodes)
	for _, n := range nodes {
		if len(out) >= maxViaNodes {
			break
		}
		if n.ID == target.Node || !slices.Contains(n.Capabilities, hub.CapabilityOutboundDial) {
			continue
		}
		xID := n.ID
		out = append(out, Candidate{
			ID:       "via-node:" + xID,
			Priority: p.Priority(),
			Dial: func(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
				target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
				start := time.Now()
				conn, err := svc.RelayStream(ctx, xID, target.Addr)
				if err != nil {
					return nil, fmt.Errorf("via-node(%s): %w", xID, err)
				}
				return &Result{Conn: conn, Kind: KindViaNode, Latency: time.Since(start)}, nil
			},
		})
	}
	return out
}
```

- [ ] **步骤 4：运行测试验证通过（测试改用 httptest mock hub）**

修正测试用真实 FileClient + mock hub：

```go
func TestViaNodeExpand_ReturnsXCandidates(t *testing.T) {
	t.Parallel()
	// mock hub：/api/hub/nodes 返回 2 个 outbound-dial 节点 + 1 个 target（应排除）。
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	if len(cands) != 2 { ... }
	if cands[0].ID != "via-node:node-x1" || cands[1].ID != "via-node:node-x2" { ... }
}
```

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/via_node.go pkg/tunnel/mesh/via_node_test.go
git commit -m "feat(mesh): via-node 多跳提供者（经中间节点 X 中转，端到端 RTT 竞速）
```

---

### 任务 5：端到端竞速验证（经 X 胜出核心场景）

**文件：**
- 修改：`pkg/tunnel/mesh/via_node_test.go`（追加竞速用例）

- [ ] **步骤 1：编写失败的测试（核心场景：via-node:X 快 vs direct 慢 → 选 X）**

```go
// TestDialSmart_ViaNodeWinsWhenFastest：via-node:X(快) + direct(慢) + relay(中)
// → 端到端 RTT 最短路（via-node:X）胜出。**多 node 核心场景**。
func TestDialSmart_ViaNodeWinsWhenFastest(t *testing.T) {
	// sproxy:serial: SmartPathRegistry 全局注册表注入冲突，不可并行
	via := &fakePath{name: "via-node:X1", kind: "via-node", delay: 20 * time.Millisecond, priority: 80, enabled: true}
	direct := &fakePath{name: "direct", kind: "webrtc", delay: 400 * time.Millisecond, priority: 100, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 300 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, via, direct, relay)
	smartCacheClear()

	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "T", Addr: "t:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart: %v", err)
	}
	if res.Kind != "via-node" {
		t.Fatalf("Kind = %s, want via-node（多跳端到端 RTT 最短胜出）", res.Kind)
	}
}
```

- [ ] **步骤 2：运行测试验证失败 → 通过**

运行：`go test -count=1 -run TestDialSmart_ViaNodeWinsWhenFastest ./pkg/tunnel/mesh/`
预期：先 FAIL（fakePath 未注册 via-node 或竞速未展开）→ 任务 2/3 完成后 PASS

> 注：此用例依赖任务 2（fakePath Expand）+ 任务 3（竞速展开）——若按顺序执行，
> 到任务 5 时已绿。红灯验证在任务 2/3 阶段完成；本任务验证核心场景语义。

- [ ] **步骤 3：Commit**

```bash
git add pkg/tunnel/mesh/via_node_test.go
git commit -m "test(mesh): via-node 端到端竞速核心场景（多跳 RTT 最短路胜出）
```

---

### 任务 6：全量验证 + 收尾 + 部署指南

**文件：**
- 修改：`build/lab/DEPLOY-GUIDE.md`（§5.3/§7/§9 补 via-node）

- [ ] **步骤 1：全量验证**

运行：`go test -count=1 -race ./pkg/tunnel/mesh/... ./pkg/tunnel/hub/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 2：门禁**

运行：`make check-format` + `make lint` + `go test -count=1 -run "TestSerialRatchet|TestSleepRatchet" ./internal/archcheck/`
预期：OK / 0 issues / PASS

- [ ] **步骤 3：部署指南更新（build/lab 独立仓库）**

`build/lab/DEPLOY-GUIDE.md` §5.3 加：

```markdown
`--smart` 开启后自动包含**经中间节点多跳**候选：hub 上声明 `--dial-allow` 的在线
节点（outbound-dial 能力）自动成为中转候选，端到端 RTT 最短者胜出。用户无需指定
中间节点——系统从 hub 节点列表自动发现。
```

§9 局限更新：中间节点需 `relay start --dial-allow`（已声明 outbound-dial）；via-node
为经 hub 中继到 X 的多跳（via-direct-X 打洞直连为后续增强）。

在 `build/lab/deploy-ops` 子仓库 commit。

- [ ] **步骤 4：提交收尾**

```bash
git status --porcelain  # 只剩本任务文件
git log --oneline -8    # 提交序列确认
```

- [ ] **步骤 5：开 PR**

```bash
gh pr create --base master --title "feat(mesh): via-node 多跳提供者（候选展开模型）" --body "..."
```
