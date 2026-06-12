# sproxy 隧道传输层插件化重构与发展路线

- **版本**: v1
- **日期**: 2026-06-12
- **状态**: 设计稿
- **关联文档**: [tunnel-bidirectional-relay-design](./2026-06-10-tunnel-bidirectional-relay-design.md),
  [sproxy-roadmap-design](./2026-06-11-sproxy-roadmap-design.md)

---

## 1. 动机

### 1.1 现状问题

sproxy 隧道传输层已完成分层架构（xfer → mux → tunnel → hub）和多种传输后端（TCP/HTTP/WS/QUIC/gRPC/WebRTC）的骨架实现，
但存在三个核心问题：

1. **依赖膨胀**：主 `go.mod` 当前依赖 `coder/websocket`、`quic-go` 等重型包，而很多用户只需要基础文件传输功能。
2. **实现成熟度不均**：WS 已达生产级，QUIC/WebRTC/gRPC/DHT/Tracing 处于骨架或 alpha 状态，混合在主代码中难以独立演进。
3. **缺乏一致性保障**：各 transport 实现各自测试，没有统一的行为契约保障，切换后端可能引入行为差异。

### 1.2 设计目标

1. **宿主-插件分离**：主项目保持轻量（仅 `stdlib + cobra + viper + xdg + yaml.v3`），高级功能以独立 Go module 的插件形式提供。
2. **类型安全的插件框架**：提供一个通用的泛型注册表，各子系统定义自己的插件契约。
3. **行为一致性**：通过 `xfertest` 提供标准测试套件，所有 transport 实现必须通过一致性测试。
4. **依赖隔离**：每个 `ext/` 插件拥有独立 `go.mod`，主项目不 import 插件包。

---

## 2. 目录架构

```
pkg/tunnel/
│
├── plugin/                        ★ 新增：通用泛型注册框架
│   └── registry.go                ← Registry[T]，零依赖
│
├── xfer/
│   ├── core.go                    ← Conn / Listener / Transport 接口
│   ├── registry.go                ← Registry[Transport]（基于 plugin/）
│   │
│   ├── internal/                  ★ 内置实现，仅 xfer/ 树内使用
│   │   ├── tcp/                   ★ 从 xfertcp/ 移入
│   │   │   ├── tcp.go
│   │   │   └── tcp_test.go
│   │   └── http/                  ★ 从 xfer/http.go 移入
│   │       ├── http.go
│   │       └── http_test.go
│   │
│   ├── xfertest/                  ★ 增强：行为一致性测试套件
│   │   ├── pipe.go                ← 修复已知 data race
│   │   ├── suite.go               ★ ConnSuite + ListenerSuite
│   │   └── harness.go             ★ TestHarness 编排器
│   │
│   └── ext/                       ★ 外部插件，各独立 go.mod
│       ├── ws/                    ★ 从主 go.mod 移出
│       ├── quic/                  ★ 从主 go.mod 移出
│       ├── webrtc/                ★ 从 xfer/webrtc/ 移入
│       └── grpc/                  ★ 新增
│
├── hub/
│   ├── core.go                    ★ DHT 接口定义 + NodeInfo 结构
│   ├── registry.go                ★ Registry[DHT]
│   ├── route_table.go             ← 已有
│   └── internal/
│       └── memdht.go              ★ 内置内存 map DHT
│   └── ext/
│       └── kad/                   ★ 新增：自建 Kademlia（独立 go.mod）
│
└── tracing/
    ├── core.go                    ★ Tracer 接口定义 + Carrier 接口
    ├── registry.go                ★ Registry[Tracer]
    └── internal/
        └── slogtracer.go          ★ 内置 slog tracer + traceparent 支持
    └── ext/
        └── otel/                  ★ 新增：全量 OTel（独立 go.mod）
```

**关键编译期约束：**

| 路径 | module | 内部包可导入 | 外部可见 |
|------|--------|-------------|---------|
| `pkg/tunnel/plugin/` | sproxy | 所有 sproxy 包 | ✅ |
| `pkg/tunnel/xfer/internal/` | sproxy | 仅 `xfer/` 树 | ❌ Go `internal` 规则拒绝 |
| `pkg/tunnel/xfer/ext/ws/` | 独立 module | 仅自身 | ✅（外部可 import） |
| `pkg/tunnel/hub/ext/kad/` | 独立 module | 仅自身 | ✅ |

---

## 3. 插件系统设计

### 3.1 通用注册表 `plugin.Registry[T]`

```go
package plugin

// Registry 是一个类型安全的插件注册表。
// T 是插件接口类型，由各子系统定义。
type Registry[T any] struct {
    name    string
    builtin T
    plugins map[string]Plugin[T]
}

// Plugin 描述一个已注册的外部插件
type Plugin[T any] struct {
    Name     string
    Instance T
    Priority int   // 优先级，高者优先。内置默认为 0，外部应 > 0
}

func New[T any](name string, builtin T) *Registry[T]
func (r *Registry[T]) Register(p Plugin[T])       // 由 init() 调用
func (r *Registry[T]) Active() T                  // 返回最高优先级实现
func (r *Registry[T]) Get(name string) (T, bool)  // 按名称获取
func (r *Registry[T]) Names() []string            // 列出已注册插件
func (r *Registry[T]) IsDefault() bool            // 是否使用内置实现
```

### 3.2 各子系统的注册表

| 子系统 | 接口 T | 内置实现 | ext 插件 |
|--------|--------|---------|---------|
| xfer | `Transport { Dial, Listen }` | `internal/tcp/`, `internal/http/` | `ws`, `quic`, `webrtc`, `grpc` |
| hub | `DHT` | `internal/memdht.go` | `kad` |
| tracing | `Tracer` | `internal/slogtracer.go` | `otel` |

### 3.3 插件使用范例

```go
// internal/tcp/tcp.go — 低优先级内置注册
func init() {
    xfer.TransportRegistry.Register(plugin.Plugin[xfer.Transport]{
        Name: "tcp",
        Instance: &TCPTransport{},
        Priority: 0,
    })
}

// ext/ws/ws.go — 高优先级外置注册（需用户显式 import）
func init() {
    xfer.TransportRegistry.Register(plugin.Plugin[xfer.Transport]{
        Name: "ws",
        Instance: &WSTransport{},
        Priority: 10,
    })
}

// 使用方（pkg/tunnel 或 cmd/sproxy）
t := xfer.TransportRegistry.Active()
conn, err := t.Dial(ctx, addr)
```

### 3.4 选择机制

`Active()` 在有多个同名插件时返回 `Priority` 最高者；默认 `IsDefault()` 检查当前 active 是否为内置实现。
调用链不直接依赖具体实现类型 —— 全部通过接口解耦。

---

## 4. xfertest 增强设计

### 4.1 ConnSuite — Conn 接口行为一致性套件

每个测试函数使用 `ConnFactory` 夹具，由 transport 实现者提供。

```go
type ConnFactory func(t *testing.T) (client, server xfer.Conn, cleanup func())

func ConnSuite(t *testing.T, factory ConnFactory) {
    t.Run("RoundTrip",          func(t *testing.T) { ... })
    t.Run("MultipleMessages",   func(t *testing.T) { ... })
    t.Run("LargePayload",       func(t *testing.T) { ... })    // 1MB
    t.Run("ConcurrentSend",     func(t *testing.T) { ... })
    t.Run("CloseWhileBlocking", func(t *testing.T) { ... })
    t.Run("ContextCancellation", func(t *testing.T) { ... })
    t.Run("OrderedDelivery",    func(t *testing.T) { ... })
    t.Run("EmptyMessage",       func(t *testing.T) { ... })
    t.Run("SendAfterClose",     func(t *testing.T) { ... })    // 必须返回 ErrClosed
}
```

### 4.2 ListenerSuite — Listener 接口行为套件

```go
type ListenerFactory func(t *testing.T) (addr string, listen func() xfer.Listener, cleanup func())

func ListenerSuite(t *testing.T, lf ListenerFactory, df ConnFactory) { ... }
```

### 4.3 TestHarness — 一键集成测试

```go
type Harness struct {
    Name   string
    Dial   func(ctx context.Context, addr string) (xfer.Conn, error)
    Listen func(ctx context.Context, addr string) (xfer.Listener, error)
}

func TestHarness(t *testing.T, h Harness) {
    t.Run(h.Name, func(t *testing.T) {
        ConnSuite(t, h.ConnFactory())
        ListenerSuite(t, h.ListenerFactory())
    })
}
```

### 4.4 ext/ 测试引用方式

```go
// ext/quic/quic_test.go
func TestQUIC(t *testing.T) {
    if runtime.GOOS == "windows" {
        t.Skip("QUIC UDP restricted on Windows")
    }
    xfertest.TestHarness(t, xfertest.Harness{
        Name:   "quic",
        Listen: quic.Listen,
        Dial:   quic.Dial,
    })
}
```

---

## 5. 子系统契约接口

### 5.1 DHT 接口（`hub/core.go`）

```go
// DHT 定义节点发现的最低接口。
type DHT interface {
    Register(ctx context.Context, node NodeInfo) error
    Lookup(ctx context.Context, nodeID string) (NodeInfo, error)
    GetClosestNodes(ctx context.Context, nodeID string, n int) ([]NodeInfo, error)
    Bootstrap(ctx context.Context, seeds []string) error
    Close() error
}

type NodeInfo struct {
    ID    string
    Addrs []string   // xfer 地址列表，如 ["tcp://192.168.1.1:9000"]
    Meta  map[string]string
}
```

内置实现 `memdht`：线程安全 map，节点登记但不主动发现，适合单机或固定拓扑。

### 5.2 Tracer 接口（`tracing/core.go`）

```go
type Tracer interface {
    StartSpan(ctx context.Context, name string) (context.Context, func())
    Inject(ctx context.Context, carrier Carrier)
    Extract(ctx context.Context, carrier Carrier) context.Context
}

type Carrier interface {  // 可适配 http.Header / gRPC metadata / map
    Get(key string) string
    Set(key, value string)
}
```

内置实现 `slogtracer`：零依赖，span 输出到 `log/slog`，支持 W3C traceparent 手动注入/提取。

### 5.3 xfer Conn / Listener / Transport 接口（已有，保持不变）

```go
type Conn interface {
    Send(ctx context.Context, msg []byte) error
    Receive(ctx context.Context) ([]byte, error)
    io.Closer
}

type Listener interface {
    Accept(ctx context.Context) (Conn, error)
    io.Closer
}

type Transport struct {
    Name   string
    Listen func(ctx context.Context, addr string) (Listener, error)
    Dial   func(ctx context.Context, addr string) (Conn, error)
}
```

---

## 6. 依赖关系图

```
plugin/     (stdlib only)
   │
   ├── xfer/core        (imports plugin/)
   │   ├── xfer/internal/tcp   ← 自动注册
   │   ├── xfer/internal/http  ← 自动注册
   │   ├── xfer/xfertest       (imports xfer/core)
   │   ├── xfer/ext/ws         (独立 go.mod)
   │   ├── xfer/ext/quic       (独立 go.mod)
   │   ├── xfer/ext/webrtc     (独立 go.mod)
   │   └── xfer/ext/grpc       (独立 go.mod)
   │
   ├── hub/             (imports plugin/)
   │   ├── hub/internal/memdht ← 自动注册
   │   └── hub/ext/kad         (独立 go.mod)
   │
   ├── tracing/         (imports plugin/)
   │   ├── tracing/internal/slogtracer ← 自动注册
   │   └── tracing/ext/otel     (独立 go.mod)
   │
   └── pkg/tunnel (上层) — 使用 Registry.Active()
```

**无循环依赖**。所有箭头同向：`plugin/` ← 子系统 ← ext 插件。

---

## 7. 实施优先级与时间线

### 阶段 0（必须前置）：骨架重建 — 3 天

> 不动业务逻辑，只重构目录结构和引入插件框架。

| 编号 | 任务 | 工作量 | 验证方式 |
|------|------|--------|---------|
| 0.1 | 创建 `plugin/registry.go` Registry[T] | 0.5 d | `go test ./pkg/tunnel/plugin/...` |
| 0.2 | xfer 目录重构：`core.go` + `registry.go`；`internal/tcp/` `internal/http/` | 1 d | 原有测试全绿 |
| 0.3 | WS/QUIC 提取为独立 go.module 到 `ext/` | 0.5 d | `cd ext/ws && go test ./...` |
| 0.4 | xfertest 增强：`suite.go` `harness.go`，修复 pipe data race | 0.5 d | `go test ./xfer/...` |
| 0.5 | hub/tracing 各自使用 Registry[T] | 0.5 d | 一致性测试通过 |
| | **小计** | **3 d** | |

**阶段 0 输出**：主 `go.mod` 仅 `cobra + viper + xdg + yaml.v3`；`go test ./...` 全绿。

### 阶段 1（高价值低投入）：已有模块完善 — 5 天

| 编号 | 任务 | 工作量 | 验证方式 |
|------|------|--------|---------|
| 1.1 | 修复 QUIC `isWindows()` 硬编码为 SKIP | 0.5 d | Linux CI QUIC 测试运行 |
| 1.2 | Tracing 接入 tunnel Do/Serve + mux 调用链 | 1.5 d | 端到端测试包含 trace id |
| 1.3 | mux 流控：FrameWindowUpdate + 原子窗口 | 2 d | 背压测试 + 内存限制验证 |
| 1.4 | WebRTC 注册到 xfer + 网络信令 | 1 d | `xfertest.Harness` 通过 |

### 阶段 2（核心 P2P）：新插件实现 — 7~10 天

| 编号 | 任务 | 工作量 | 说明 |
|------|------|--------|---------|
| 2.1 | gRPC transport 插件 `ext/grpc/` | 2 d | protobuf 定义 + `xfertest.Harness` |
| 2.2 | DHT 生产级 `hub/ext/kad/`（自建轻量 Kademlia） | 5 d | 160 桶 XOR 路由 + UDP 传输 + 4 RPC |
| 2.3 | P2P 节点生命周期管理（重连/保活/会话恢复） | 1 d | DHT + xfer + mux 装配 |

### 阶段 3（高阶运维）：可选增强

| 编号 | 任务 | 工作量 | 说明 |
|------|------|--------|---------|
| 3.1 | OTel 全量插件 `tracing/ext/otel/` | 2~3 d | 替换内置 slog tracer |
| 3.2 | CI 跨平台集成测试（Docker Compose 多节点） | 1 d | 实机联网验证 |
| 3.3 | mTLS 可插拔化 `xfer/ext/tls/` | 0.5 d | |

---

## 8. DHT 方案选择

推荐方案 A：**自建轻量 Kademlia**（`hub/ext/kad/`）：

| 维度 | 方案 A：自建 Kademlia | 方案 B：libp2p Kademlia |
|------|---------------------|------------------------|
| 依赖 | 仅 `golang.org/x/crypto` | `go-libp2p-kad-dht` + 数十传递依赖 |
| 二进制影响 | ~100KB | ~10MB |
| 实现量 | ~1000 行 | ~100 行胶水 |
| 节点规模 | <500 节点 | 数千节点 |
| 控制力 | 完全控制 | 依赖 libp2p 生态 |
| 后续扩展 | 可封装 libp2p 为另一插件 | 不可反向兼容 |

选择方案 A 的原因：sproxy 定位为轻量文件代理 + 隧道工具，不需要大规模 P2P 网络。
未来如有大规模需求，方案 B 可作为 `hub/ext/kad-libp2p/` 独立插件补充。

---

## 9. 关键风险与缓解

| 风险 | 可能性 | 缓解方案 |
|------|--------|---------|
| ext/ 独立 go.mod 导致 `replace` 指引复杂 | 中 | 使用 Go workspace（`go.work`）统一开发体验 |
| internal 包重构后回归 | 低 | `xfertest.ConnSuite` 保证一致性 |
| WebRTC 跨 NAT 穿透失败 | 高 | 阶段 2 中保留内存信令作为 fallback |
| 插件优先级冲突 | 低 | `Registry.Active()` 仅有明确优先级规则，日志输出优先级决定 |

---

## 10. 验证策略

1. **阶段 0 验收**：`go vet ./...` + `go test -race ./...` 全绿
2. **阶段 1 验收**：端到端隧道吞吐量测试、CI QUIC 运行、日志含 trace id
3. **阶段 2 验收**：`xfertest.TestHarness` 覆盖 ext/ 所有 transport、DHT Kademlia 3 节点集成测试
4. **阶段 3 验收**：跨平台 Docker Compose 多容器拓扑测试
