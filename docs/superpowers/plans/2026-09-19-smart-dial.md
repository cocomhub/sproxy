<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# SmartDial 多路径竞速择优（自动选最佳路由）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 mesh connect / socks 提供 `--smart` 自动选路：在全部可达路径（直连 / 中继 / 网关 / 经中间节点多跳）中按端到端建连耗时（RTT 近似）并行竞速择优，胜者缓存 TTL 内单路复用。

**架构：** 在 `pkg/tunnel/mesh` 新增 `smart.go`：路径提供者注册表（复用 `pkg/plugin.Registry[T]`）+ 竞速核心（胜者缓存 + 并行 Dial + 首胜关闭其余）。`relay start --dial-allow` 声明新 `CapabilityOutboundDial` 使中间节点可被发现。`mesh connect` / `socks` 加 `--smart` flag（默认关 = 现有行为零回归）。部署指南同步更新。

**技术栈：** Go 标准库（context/sync/time）+ 现有 `plugin.Registry[T]` + 现有 `mesh.Dial` 原语（DialWebRTC / RelayStream / GatewayConnect）。

**规格：** `docs/superpowers/specs/2026-09-19-smart-dial-design.md`（本计划的论证依据；执行者两份都读）

## 全局约束

- 纯标准库测试（无 testify/gomock/gomega），沿用 `t.Fatalf`/`t.Errorf`。
- 所有含 HTTP 服务的测试监听 127.0.0.1（httptest 默认 loopback），禁止 0.0.0.0/localhost。
- Windows 兼容：路径用 `filepath.Join`/`ToSlash`；测试不依赖 Unix-only 特性。
- SPDX 头：每个新建/修改 Go 文件带 `// Copyright 2026 The Cocomhub Authors. All rights reserved.` + `// SPDX-License-Identifier: Apache-2.0`。
- 所有新增顶层测试默认 `t.Parallel()`（R18 并发注册门禁，`internal/archcheck` 扫描；无法并发的显式 `// sproxy:serial:` 注释）。
- 竞速窗口默认 5s；测试用短窗口（如 200ms）避免慢测试。
- 候选路径总数 ≤ 4（P1/P2 必选 + 最多 3 个中间节点 P4..Pn，P3 仅 --gateway）。
- 提交信息用 Conventional Commits（`feat(mesh): ...`），不加署名行。
- 推送走 https/SSH；只 `git add` 本任务文件（禁 `-A`/`.`）。

---

## 文件结构

| 文件 | 职责 | 动作 |
|---|---|---|
| `pkg/tunnel/mesh/smart.go` | PathProvider 接口 + SmartPathRegistry + winnerCache + DialSmart 竞速核心 | 新建 |
| `pkg/tunnel/mesh/smart_test.go` | 8 个 TDD 用例（mock 路径拨号器 + 延迟注入） | 新建 |
| `pkg/tunnel/hub/router.go` | 新增 `CapabilityOutboundDial` 常量 | 修改（~51 行附近） |
| `cmd/sclient/relay.go` | `dialAllow` 时注册帧追加 `CapabilityOutboundDial` | 修改（~182 行） |
| `pkg/tunnel/mesh/mesh.go` | `Result` 扩展 `Latency time.Duration` 字段 | 修改（~52 行） |
| `cmd/sclient/mesh.go` | `--smart` flag + dial 组装 | 修改 |
| `cmd/sclient/socks.go` | `--smart` flag + dial 组装 | 修改 |
| `build/lab/DEPLOY-GUIDE.md` | §5.3/§7-6/§9 更新为自动选路 | 修改 |
| `docs/superpowers/specs/2026-09-19-smart-dial-design.md` | 设计文档（已 commit） | 已存在 |

---

### 任务 1：Result 扩展 Latency + CapabilityOutboundDial + relay 声明

**文件：**
- 修改：`pkg/tunnel/mesh/mesh.go:52`（Result 结构）
- 修改：`pkg/tunnel/hub/router.go:51`（capability 常量）
- 修改：`cmd/sclient/relay.go:182`（注册帧携带新 capability）

- [ ] **步骤 1：编写失败的测试**

`pkg/tunnel/mesh/mesh_test.go` 追加：

```go
func TestResult_HasLatencyField(t *testing.T) {
	t.Parallel()
	r := &Result{Conn: nil, Kind: KindRelay, Latency: 150 * time.Millisecond}
	if r.Latency != 150*time.Millisecond {
		t.Fatalf("Latency = %v, want 150ms", r.Latency)
	}
	if r.Conn != nil || r.Kind != KindRelay {
		t.Fatalf("Result 现有字段回归: %+v", r)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestResult_HasLatencyField ./pkg/tunnel/mesh/`
预期：FAIL，编译错误 `unknown field 'Latency' in struct literal`

- [ ] **步骤 3：实现——Result 扩展 + capability 常量 + relay 声明**

`pkg/tunnel/mesh/mesh.go` Result 结构追加字段：

```go
// Result 是一次 mesh 直连的结果：数据面连接 + 实际使用的路径。
type Result struct {
	Conn net.Conn
	Kind string
	// Latency 是建连耗时（端到端 RTT 近似：发起 → 拨号 ack 首字节可读；
	// 含打洞/中继/多跳各段网络往返）。SmartDial 竞速时填充；单路径 Dial 为 0。
	Latency time.Duration
}
```

`pkg/tunnel/hub/router.go` 常量区追加（~51 行 `CapabilityPerNodeSecret` 旁）：

```go
// CapabilityOutboundDial 表示节点可作为中转出口：收到拨号帧时允许向目标地址
// 发起出站 TCP 连接（relay start --dial-allow 时声明）。SmartDial 据此从
// ListHubNodes 中发现「可作多跳中间节点」的候选（fail-closed：无此标记不选）。
const CapabilityOutboundDial = "outbound-dial"
```

`cmd/sclient/relay.go:182` 注册帧携带新 capability（仅在 `dialAllow` 时）：

```go
	caps := []string{hub.CapabilityPerNodeSecret, hub.CapabilityVirtualIP}
	if dialAllow {
		caps = append(caps, hub.CapabilityOutboundDial)
	}
	if serr := conn.Send(ctx, hub.NewRegisterFrame(nodeID, accessKey, proof, ts, nonce, meta, caps...)); serr != nil {
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run TestResult_HasLatencyField ./pkg/tunnel/mesh/` + `go build ./pkg/tunnel/hub/ ./cmd/sclient/`
预期：PASS + 编译通过

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/mesh.go pkg/tunnel/hub/router.go cmd/sclient/relay.go pkg/tunnel/mesh/mesh_test.go
git commit -m "feat(mesh): Result 扩展 Latency + CapabilityOutboundDial 中间节点可发现性
```

---

### 任务 2：PathProvider 接口 + SmartPathRegistry（可扩展性核心）

**文件：**
- 创建：`pkg/tunnel/mesh/smart.go`（接口 + 注册表 + builtin 提供者）
- 测试：`pkg/tunnel/mesh/smart_test.go`

- [ ] **步骤 1：编写失败的测试**

`pkg/tunnel/mesh/smart_test.go`（先建接口/注册表测试）：

```go
package mesh

import (
	"context"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/client"
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

func (f *fakePath) Name() string { return f.name }
func (f *fakePath) Priority() int { return f.priority }
func (f *fakePath) Enabled(_ context.Context, _ *client.FileClient) bool { return f.enabled }

func (f *fakePath) Dial(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	if f.callCh != nil {
		f.callCh <- f.name
	}
	time.Sleep(f.delay)
	return &Result{Conn: nil, Kind: f.kind, Latency: f.delay}, nil
}

// TestSmartPathRegistry_BuiltinProviders：注册表内置 direct + relay。
func TestSmartPathRegistry_BuiltinProviders(t *testing.T) {
	t.Parallel()
	names := SmartPathRegistry.Names()
	if len(names) != 2 {
		t.Fatalf("builtin providers = %v, want [direct relay]", names)
	}
	has := func(want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
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
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run "TestSmartPathRegistry" ./pkg/tunnel/mesh/`
预期：FAIL，编译错误 `undefined: SmartPathRegistry` / `undefined: PathProvider`

- [ ] **步骤 3：实现——smart.go 接口 + 注册表 + builtin 提供者**

创建 `pkg/tunnel/mesh/smart.go`：

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package mesh

import (
	"context"

	"github.com/cocomhub/sproxy/pkg/client"
	"github.com/cocomhub/sproxy/pkg/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/webrtc"
)

// PathProvider 是 SmartDial 的一条候选路径实现（P1 直连 / P2 中继 / P3 网关 /
// P4..Pn 经中间节点多跳，各实现一个）。未来新增路径只需实现本接口并 Register。
type PathProvider interface {
	// Name 是路径唯一名（"direct" / "relay" / "gateway" / "via-node"）。
	Name() string
	// Dial 建立数据面连接并写好拨号帧（复用 mesh.Dial 的现有原语）。
	Dial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
		target *client.MeshService, localNode string, opts DialOptions) (*Result, error)
	// Priority 竞速排序依据：高者优先（同优先级按注册顺序）。
	Priority() int
	// Enabled 条件启用：false 的提供者不参与竞速（如 P3 需 --gateway、P4 需候选中间节点）。
	Enabled(ctx context.Context, svc *client.FileClient) bool
}

// directProvider 实现 P1 直连（webrtc 打洞，不回落）。
type directProvider struct{}

func (directProvider) Name() string              { return "direct" }
func (directProvider) Priority() int             { return 100 }
func (directProvider) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (directProvider) Dial(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
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

// relayProvider 实现 P2 中继（hub 中继流）。
type relayProvider struct{}

func (relayProvider) Name() string              { return "relay" }
func (relayProvider) Priority() int             { return 50 }
func (relayProvider) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (relayProvider) Dial(ctx context.Context, svc *client.FileClient, _ webrtc.Signaler,
	target *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	start := time.Now()
	conn, err := svc.RelayStream(ctx, target.Node, target.Addr)
	if err != nil {
		return nil, fmt.Errorf("relay: %w", err)
	}
	return &Result{Conn: conn, Kind: KindRelay, Latency: time.Since(start)}, nil
}

// SmartPathRegistry 是 SmartDial 的路径提供者注册表。
// builtin: direct(P1) + relay(P2)；外部插件可 Register 覆盖/新增（gateway(P3)、via-node(P4)）。
var SmartPathRegistry = plugin.New[PathProvider]("smart-path", builtinProviders())

// builtinProviders 返回内置兜底（direct + relay），对应现有 mesh.Dial 固定顺序。
func builtinProviders() PathProvider { return directProvider{} }
```

> 注：`plugin.New[T]` 的 builtin 参数是**单个**兜底。内置两个提供者需先 Register 进注册表：

```go
func init() {
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "direct", Instance: directProvider{}, Priority: 100})
	SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "relay", Instance: relayProvider{}, Priority: 50})
}
```

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run "TestSmartPathRegistry" ./pkg/tunnel/mesh/`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/smart_test.go
git commit -m "feat(mesh): SmartDial 路径提供者注册表（可插拔，plugin.Registry 同构）
```

---

### 任务 3：竞速核心 DialSmart（胜者缓存 + 并行竞速 + 首胜关闭其余）

**文件：**
- 修改：`pkg/tunnel/mesh/smart.go`（竞速核心）
- 测试：`pkg/tunnel/mesh/smart_test.go`（用例 1-7）

- [ ] **步骤 1：编写失败的测试（用例 1-3：竞速择优）**

`pkg/tunnel/mesh/smart_test.go` 追加（先清注册表注入 fake 提供者）：

```go
// 注入测试提供者集合：清注册表 + 注册 fake（T.Cleanup 恢复 builtin）。
func smartWithProviders(t *testing.T, ps ...*fakePath) {
	t.Helper()
	for _, n := range SmartPathRegistry.Names() {
		SmartPathRegistry.Delete(n)
	}
	for _, p := range ps {
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: p.name, Instance: p, Priority: p.priority})
	}
	t.Cleanup(func() {
		for _, n := range SmartPathRegistry.Names() {
			SmartPathRegistry.Delete(n)
		}
		// 恢复 builtin
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "direct", Instance: directProvider{}, Priority: 100})
		SmartPathRegistry.Register(plugin.Plugin[PathProvider]{Name: "relay", Instance: relayProvider{}, Priority: 50})
	})
}

// 用例1：直连慢/中继快 → 选中继
func TestDialSmart_PicksFastestPath(t *testing.T) {
	t.Parallel()
	slow := &fakePath{name: "direct", kind: "webrtc", delay: 500 * time.Millisecond, priority: 100, enabled: true}
	fast := &fakePath{name: "relay", kind: "relay", delay: 50 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, slow, fast)

	smartCacheClear() // 清缓存避免跨用例污染
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "relay" {
		t.Fatalf("Kind = %s, want relay（快者胜出）", res.Kind)
	}
}

// 用例2：直连快 → 选直连
func TestDialSmart_PicksDirectWhenFast(t *testing.T) {
	t.Parallel()
	fast := &fakePath{name: "direct", kind: "webrtc", delay: 10 * time.Millisecond, priority: 100, enabled: true}
	slow := &fakePath{name: "relay", kind: "relay", delay: 300 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, fast, slow)
	smartCacheClear()
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "local", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "webrtc" {
		t.Fatalf("Kind = %s, want webrtc", res.Kind)
	}
}

// 用例3：多跳 home→office(快)→B vs home→B(慢) → 选多跳（端到端 RTT 最短路）
func TestDialSmart_PicksMultihopWhenFastest(t *testing.T) {
	t.Parallel()
	slowDirect := &fakePath{name: "direct", kind: "webrtc", delay: 400 * time.Millisecond, priority: 100, enabled: true}
	fastViaNode := &fakePath{name: "via-node", kind: "via-node", delay: 40 * time.Millisecond, priority: 10, enabled: true}
	relay := &fakePath{name: "relay", kind: "relay", delay: 500 * time.Millisecond, priority: 50, enabled: true}
	smartWithProviders(t, slowDirect, fastViaNode, relay)
	smartCacheClear()
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "B", Addr: "b:1"}, "home", DialOptions{})
	if err != nil {
		t.Fatalf("DialSmart err: %v", err)
	}
	if res.Kind != "via-node" {
		t.Fatalf("Kind = %s, want via-node（多跳端到端 RTT 最短胜出）", res.Kind)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run "TestDialSmart" ./pkg/tunnel/mesh/`
预期：FAIL，编译错误 `undefined: DialSmart` / `undefined: smartCacheClear`

- [ ] **步骤 3：实现——竞速核心**

`pkg/tunnel/mesh/smart.go` 追加：

```go
// winnerCacheEntry 是胜者缓存条目（key = 目标 node）。
type winnerCacheEntry struct {
	Kind     string
	Latency  time.Duration
	ExpireAt time.Time
}

var smartCache = struct {
	mu sync.Mutex
	m  map[string]winnerCacheEntry
}{m: make(map[string]winnerCacheEntry)}

// smartCacheTTL 是胜者缓存有效期（抖动链路自适应：过期自动重新竞速）。
const smartCacheTTL = 30 * time.Second

// smartRaceWindow 是竞速窗口：超过此时长仍未胜出的候选不再等待。
const smartRaceWindow = 5 * time.Second

// smartCacheClear 仅测试用：清空胜者缓存。
func smartCacheClear() {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m = make(map[string]winnerCacheEntry)
}

// DialSmart 是 SmartDial 入口：缓存命中走缓存路径（单路），miss/过期并行竞速。
func DialSmart(ctx context.Context, svc *client.FileClient, signaler webrtc.Signaler,
	target *client.MeshService, localNode string, opts DialOptions) (*Result, error) {
	if target == nil || target.Node == "" {
		return nil, fmt.Errorf("smart dial: 目标节点为空")
	}
	// 1. 缓存命中 → 单路走缓存路径。
	if cached, ok := smartCacheGet(target.Node); ok {
		if p, ok := SmartPathRegistry.Get(cached.Kind); ok && p.Enabled(ctx, svc) {
			return p.Dial(ctx, svc, signaler, target, localNode, opts)
		}
		smartCacheDelete(target.Node) // 缓存路径失效 → 删缓存重新竞速
	}

	// 2. 收集候选：注册表中 Enabled 的提供者，按 Priority 降序，总候选 ≤ 4。
	type cand struct {
		p PathProvider
	}
	cands := make([]cand, 0, 4)
	for _, name := range SmartPathRegistry.Names() {
		if len(cands) >= 4 {
			break
		}
		p, ok := SmartPathRegistry.Get(name)
		if !ok || !p.Enabled(ctx, svc) {
			continue
		}
		cands = append(cands, cand{p: p})
	}
	if len(cands) == 0 {
		return nil, fmt.Errorf("smart dial: 无可用的路径提供者")
	}

	// 3. 并行竞速：每条候选 goroutine 独立 Dial，首胜者胜出，其余关闭。
	raceCtx, raceCancel := context.WithTimeout(ctx, smartRaceWindow)
	defer raceCancel()
	type outcome struct {
		name string
		res  *Result
		err  error
	}
	outCh := make(chan outcome, len(cands))
	started := 0
	for _, c := range cands {
		p := c.p
		go func() {
			res, err := p.Dial(raceCtx, svc, signaler, target, localNode, opts)
			outCh <- outcome{name: p.Name(), res: res, err: err}
		}()
		started++
	}

	var firstErr error
	for range started {
		o := <-outCh
		if o.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", o.name, o.err)
			}
			continue
		}
		// 首胜者：关闭其余（raceCancel 触发其余 goroutine 的 ctx 取消 + defer 关闭），
		// 写缓存，返回。
		raceCancel()
		go drainOutcomes(outCh, started-1) // 收走其余结果，避免泄漏（goroutine 已由 raceCancel 结束）
		smartCacheSet(target.Node, o.res.Kind, o.res.Latency)
		return o.res, nil
	}
	return nil, fmt.Errorf("smart dial 全部候选失败: %w", firstErr)
}

func smartCacheGet(node string) (winnerCacheEntry, bool) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	e, ok := smartCache.m[node]
	if !ok {
		return winnerCacheEntry{}, false
	}
	if time.Now().After(e.ExpireAt) {
		delete(smartCache.m, node)
		return winnerCacheEntry{}, false
	}
	return e, true
}

func smartCacheSet(node, kind string, latency time.Duration) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	smartCache.m[node] = winnerCacheEntry{Kind: kind, Latency: latency, ExpireAt: time.Now().Add(smartCacheTTL)}
}

func smartCacheDelete(node string) {
	smartCache.mu.Lock()
	defer smartCache.mu.Unlock()
	delete(smartCache.m, node)
}

// drainOutcomes 收走剩余竞速结果（goroutine 已因 raceCancel 结束，仅防 channel 泄漏）。
func drainOutcomes(ch chan outcome, n int) {
	for range n {
		<-ch
	}
}
```

> 注：fake 提供者的 `Conn` 为 nil（无真实连接），竞速胜出后不 close——真实提供者的
> Dial 在 ctx 取消时内部 defer close（复用现有 DialWebRTC/RelayStream 的 ctx 语义）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run "TestDialSmart|TestSmartPathRegistry" ./pkg/tunnel/mesh/`
预期：PASS

- [ ] **步骤 5：编写缓存用例（4-5）+ 全失败用例（6）+ 并发安全用例（7）**

`pkg/tunnel/mesh/smart_test.go` 追加：

```go
// 用例4：缓存命中走缓存路径（单路，不竞速）
func TestDialSmart_CacheHitUsesCachedPath(t *testing.T) {
	t.Parallel()
	direct := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true, callCh: make(chan string, 8)}
	relay := &fakePath{name: "relay", kind: "relay", delay: time.Millisecond, priority: 50, enabled: true, callCh: make(chan string, 8)}
	smartWithProviders(t, direct, relay)
	smartCacheClear()

	// 首次：竞速（两路都拨号）。
	if _, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{}); err != nil {
		t.Fatalf("first dial: %v", err)
	}
	// 断言首次竞速两路都被调用。
	calls1 := drainCalls(direct.callCh) + drainCalls(relay.callCh)
	if calls1 < 2 {
		t.Fatalf("首次应竞速两路，实际 %d 次调用", calls1)
	}
	// 缓存命中：只走缓存路径（单路）。
	res, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err != nil {
		t.Fatalf("cached dial: %v", err)
	}
	if res.Kind != "webrtc" {
		t.Fatalf("cached Kind = %s, want webrtc（首次胜者）", res.Kind)
	}
	// 断言缓存命中时只有缓存路径被调用（另一路未拨号）。
	if got := drainCalls(direct.callCh) + drainCalls(relay.callCh); got != 0 {
		t.Fatalf("缓存命中应单路（0 新调用），实际 %d", got)
	}
}

// 用例6：全失败 → 聚合错误上下文
func TestDialSmart_AllFail(t *testing.T) {
	t.Parallel()
	failDirect := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	failRelay := &fakePath{name: "relay", kind: "relay", delay: time.Millisecond, priority: 50, enabled: true}
	// 复写 fake 的 Dial 返回错误：用 failPath 变体
	smartWithProviders(t, failDirect, failRelay)
	// 注：这里需 fakePath 支持 err 字段（见下 drainCalls/step 5 补充）
	// 简化：直接用现有 fakePath 但 enabled 为 true、Dial 返回 nil 错误无法模拟失败——
	// 改用 failingPath 变体（Dial 恒返回 error）。
}

// failingPath 恒失败提供者（测试全失败聚合错误）。
type failingPath struct{ name string }

func (f *failingPath) Name() string              { return f.name }
func (f *failingPath) Priority() int             { return 100 }
func (f *failingPath) Enabled(_ context.Context, _ *client.FileClient) bool { return true }
func (f *failingPath) Dial(_ context.Context, _ *client.FileClient, _ webrtc.Signaler,
	_ *client.MeshService, _ string, _ DialOptions) (*Result, error) {
	return nil, fmt.Errorf("boom-%s", f.name)
}

func TestDialSmart_AllFail(t *testing.T) {
	t.Parallel()
	smartWithProviders(t, &failingPath{name: "direct"}, &failingPath{name: "relay"})
	smartCacheClear()
	_, err := DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
	if err == nil {
		t.Fatal("全失败应返回错误")
	}
	if !strings.Contains(err.Error(), "direct") || !strings.Contains(err.Error(), "relay") {
		t.Fatalf("错误应含各候选上下文: %v", err)
	}
}

// 用例7：-race 并发安全（并发 DialSmart 读写缓存）
func TestDialSmart_ConcurrentCacheRace(t *testing.T) {
	t.Parallel()
	p := &fakePath{name: "direct", kind: "webrtc", delay: time.Millisecond, priority: 100, enabled: true}
	smartWithProviders(t, p)
	smartCacheClear()
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = DialSmart(context.Background(), nil, nil, &client.MeshService{Node: "n", Addr: "a:1"}, "l", DialOptions{})
		}()
	}
	wg.Wait()
}
```

- [ ] **步骤 6：运行全部用例验证通过**

运行：`go test -count=1 -race -run "TestDialSmart|TestSmartPathRegistry" ./pkg/tunnel/mesh/`
预期：PASS（-race 无竞态）

- [ ] **步骤 7：Commit**

```bash
git add pkg/tunnel/mesh/smart.go pkg/tunnel/mesh/smart_test.go
git commit -m "feat(mesh): DialSmart 竞速择优——胜者缓存 + 并行竞速 + 首胜关闭其余
```

---

### 任务 4：--smart 开关接入 mesh connect + socks

**文件：**
- 修改：`cmd/sclient/mesh.go`（--smart flag + dial 组装）
- 修改：`cmd/sclient/socks.go`（--smart flag + dial 组装）

- [ ] **步骤 1：编写失败的测试**

`cmd/sclient/mesh_test.go` 追加（flag 存在性）：

```go
func TestMeshConnect_HasSmartFlag(t *testing.T) {
	t.Parallel()
	cmd := newCmdMeshConnect()
	flag := cmd.Flags().Lookup("smart")
	if flag == nil {
		t.Fatal("mesh connect 缺 --smart flag")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--smart 默认值 = %s, want false", flag.DefValue)
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -run TestMeshConnect_HasSmartFlag ./cmd/sclient/`
预期：FAIL，`flag.Lookup("smart")` 返回 nil

- [ ] **步骤 3：实现——mesh connect 加 --smart**

`cmd/sclient/mesh.go` 的 `newCmdMeshConnect` 中：

```go
	// dial 组装：--smart 开启走 DialSmart（多路径竞速择优），默认走现有 Dial。
	smart, _ := cmd.Flags().GetBool("smart")
	dial := meshDialFunc(mesh.Dial)
	if smart {
		dial = meshDialFunc(mesh.DialSmart)
	}
	if gatewayAddr != "" {
		dial = meshGatewayDial(gatewayAddr, svc.AccessKeySecret(), ios)
	}
	if isVIP {
		dial = meshVIPDial(vipTable, vipSubnet, dial, ios)
	}
```

flag 注册（在 `newCmdMeshConnect` 的 flags 区追加）：

```go
	cmd.Flags().Bool("smart", false, "自动选最佳路由：并行竞速直连/中继/经中间节点多跳，按端到端建连耗时择优（胜者缓存 TTL 30s 内单路复用）")
```

`socks.go` 同样：dial 组装处加 `smart` 分支 + flag 注册。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -run TestMeshConnect_HasSmartFlag ./cmd/sclient/` + `go build ./cmd/sclient/`
预期：PASS + 编译通过

- [ ] **步骤 5：Commit**

```bash
git add cmd/sclient/mesh.go cmd/sclient/socks.go cmd/sclient/mesh_test.go
git commit -m "feat(cli): mesh connect/socks 加 --smart 自动选路开关（默认关零回归）
```

---

### 任务 5：部署指南更新

**文件：**
- 修改：`build/lab/DEPLOY-GUIDE.md`（§5.3/§7-6/§9）

- [ ] **步骤 1：§5.3 多链路手动切换 → 自动选路**

把「多链路手动切换」段改为：

```markdown
**多链路自动择优**（v2.0+ 的 `--smart`）：

| 出口 | 命令 |
|---|---|
| 自动选最佳 | `sclient socks -l 127.0.0.1:1080 --exit node-b --smart ...` |
| 自动选最佳（mesh 转发） | `sclient mesh connect fileapi -l 127.0.0.1:19900 --smart ...` |

`--smart` 开启后系统并行竞速直连/中继/经中间节点多跳路径，按端到端建连耗时
（RTT 近似）自动选最快者；胜者缓存 TTL 30s 内单路复用，链路变化自动重新竞速。
无需手动切换。
```

- [ ] **步骤 2：§7-6 链路择优 → 自动**

```markdown
6. **链路择优（自动）**：`--smart` 开启后系统自动按端到端 RTT 择优，无需手动
   实测固定；观察 `mesh connect --smart` 日志确认选中路径即可。
```

- [ ] **步骤 3：§9 局限更新**

```markdown
- ~~没有多路径实时择优~~：SmartDial（`--smart`）提供**建立时多路径竞速择优**——
  并行竞速直连/中继/经中间节点多跳，按端到端 RTT 自动选最快，胜者缓存 TTL 内
  单路复用、过期自动重新竞速。仍有的局限：**活跃连接不迁移**（链路劣化需断线
  重连才换路）；中间节点需 `relay start --dial-allow` 声明 `outbound-dial`
  能力才被选为多跳候选。
```

- [ ] **步骤 4：验证**

运行：`grep -n "smart" build/lab/DEPLOY-GUIDE.md` 确认 §5.3/§7-6/§9 都含 `--smart`

- [ ] **步骤 5：Commit**

```bash
git add build/lab/DEPLOY-GUIDE.md
git commit -m "docs(deploy): DEPLOY-GUIDE 更新为 --smart 自动选路（替代手动切换）
```

> 注：`build/lab/` 是独立本地 git 仓库（不推 GitHub）——commit 在 `build/lab` 子仓库内执行。

---

### 任务 6：全量验证 + 收尾

- [ ] **步骤 1：包级验证**

运行：`go test -count=1 -race ./pkg/tunnel/mesh/... ./pkg/tunnel/hub/... ./cmd/sclient/...`
预期：PASS

- [ ] **步骤 2：格式检查**

运行：`make check-format`
预期：OK（go fix/gofmt/addlicense 无残留）

- [ ] **步骤 3：lint**

运行：`make lint`
预期：0 issues

- [ ] **步骤 4：R18 门禁**

运行：`go test -count=1 -run TestSerialRatchet ./internal/archcheck/`
预期：PASS（新增测试均 t.Parallel）

- [ ] **步骤 5：提交收尾**

```bash
git status --porcelain  # 确认只剩本任务文件
git log --oneline -6    # 确认提交序列
```

- [ ] **步骤 6：开 PR**

```bash
gh pr create --base master --title "feat(mesh): SmartDial 多路径竞速择优（自动选最佳路由）" --body "..."
```
