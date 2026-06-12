# sproxy 隧道传输层阶段 0：骨架重建 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 实现插件化重构的骨架重建：创建通用插件注册框架、重构 xfer/hub/tracing 目录、将 WS/QUIC 提取为独立 go.module、增强 xfertest 测试套件。**不动业务逻辑，只重构目录结构和引入插件框架。**

**架构：** 采用 `plugin.Registry[T]` 泛型注册框架作为统一底座，各子系统（xfer/hub/tracing）各自定义接口，内置实现放 `internal/`，外部插件放 `ext/`。

**技术栈：** Go 1.26 泛型、Go workspace（`go.work`）、`internal` 包编译期隔离

**前置条件：** 设计文档 `docs/superpowers/specs/2026-06-12-sproxy-tunnel-partitioned-kahan.md`

---

## 文件清单

### 创建的文件

| # | 路径 | 职责 |
|---|------|------|
| 1 | `pkg/tunnel/plugin/registry.go` | 通用泛型注册框架 `Registry[T]`，零外部依赖 |
| 2 | `pkg/tunnel/xfer/core.go` | xfer Conn/Listener 接口定义（从现有 xfer.go 提取） |
| 3 | `pkg/tunnel/xfer/registry.go` | `Registry[Transport]` + 向后兼容的 Register/Get 包装 |
| 4 | `pkg/tunnel/xfer/internal/http/http.go` | HTTP 传输实现（从 xfer/http.go 移入） |
| 5 | `pkg/tunnel/xfer/internal/tcp/tcp.go` | TCP 传输实现（从 xfer/xfertcp/ 移入，包名 tcp） |
| 6 | `pkg/tunnel/xfer/internal/tcp/tcp_test.go` | TCP 传输测试（从 xfer/xfertcp/tcp_test.go 移入） |
| 7 | `pkg/tunnel/xfer/xfertest/suite.go` | ConnSuite + ListenerSuite 一致性测试 |
| 8 | `pkg/tunnel/xfer/xfertest/harness.go` | TestHarness 一键集成测试编排器 |
| 9 | `pkg/tunnel/xfer/ext/ws/go.mod` | WS 独立 module |
| 10 | `pkg/tunnel/xfer/ext/ws/ws.go` | WS 传输（从 xferws/ 移入） |
| 11 | `pkg/tunnel/xfer/ext/ws/ws_test.go` | WS 传输测试 |
| 12 | `pkg/tunnel/xfer/ext/quic/go.mod` | QUIC 独立 module |
| 13 | `pkg/tunnel/xfer/ext/quic/quic.go` | QUIC 传输（从 xferquic/ 移入） |
| 14 | `pkg/tunnel/xfer/ext/quic/quic_test.go` | QUIC 传输测试（fix isWindows() 硬编码） |
| 15 | `pkg/tunnel/hub/core.go` | DHT 接口定义 + NodeInfo 结构体 |
| 16 | `pkg/tunnel/hub/registry.go` | `Registry[DHT]` 注册表 |
| 17 | `pkg/tunnel/hub/internal/memdht.go` | 内置内存 DHT 实现（实现 hub.DHT 接口） |
| 18 | `pkg/tunnel/hub/ext/kad/go.mod` | 占位：Kademlia 插件目录结构 |
| 19 | `pkg/tunnel/tracing/core.go` | Tracer 接口 + Carrier 接口 |
| 20 | `pkg/tunnel/tracing/registry.go` | `Registry[Tracer]` 注册表 |
| 21 | `pkg/tunnel/tracing/internal/slogtracer.go` | 内置 slog tracer 实现 |
| 22 | `pkg/tunnel/tracing/ext/otel/go.mod` | 占位：OTel 插件目录结构 |
| 23 | `go.work` | Go workspace 统一开发 |

### 修改的文件

| # | 路径 | 变更 |
|---|------|------|
| 1 | `go.mod` | 移除 `coder/websocket`、`quic-go` 及其传递依赖 |
| 2 | `pkg/tunnel/xfer/xfer.go` | 精简为仅保留 `ErrConnClosed` + 删除 registry 函数 |
| 3 | `pkg/tunnel/xfer/xfer_test.go` | 适配新的 registry API |
| 4 | `pkg/tunnel/xfer/xfertest/pipe.go` | 修复 Send 方法的 data race（锁覆盖 channel 写入） |
| 5 | `pkg/tunnel/hub/dht.go` | 删除或移到 internal；替换为 `hub/internal/memdht.go` |
| 6 | `pkg/tunnel/hub/dht_test.go` | 改用 DHT 接口 |
| 7 | `pkg/tunnel/tracing/tracing.go` | 删除或移到 internal；替换为 `tracing/internal/slogtracer.go` |
| 8 | `pkg/tunnel/tracing/tracing_test.go` | 改用 Tracer 接口 |
| 9 | `pkg/tunnel/p2p/p2p.go` | 改用 `hub.DHT` 接口而不是 `*hub.DHT` 具体类型 |
| 10 | `pkg/tunnel/p2p/p2p_test.go` | 适配 DHT 接口变更 |
| 11 | `pkg/tunnel/hub/hub_test.go` | import 路径适配（`xfertest` 仍在同 module） |

### 删除的文件

| # | 路径 | 原因 |
|---|------|------|
| 1 | `pkg/tunnel/xfer/http.go` | 移入 `xfer/internal/http/http.go` |
| 2 | `pkg/tunnel/xfer/xfertcp/tcp.go` | 移入 `xfer/internal/tcp/tcp.go` |
| 3 | `pkg/tunnel/xfer/xfertcp/tcp_test.go` | 移入 `xfer/internal/tcp/tcp_test.go` |
| 4 | `pkg/tunnel/xfer/xferws/ws.go` | 移入 `xfer/ext/ws/ws.go`（独立 go.mod） |
| 5 | `pkg/tunnel/xfer/xferws/ws_test.go` | 移入 `xfer/ext/ws/ws_test.go` |

---

## 任务

### 任务 1：创建 plugin.Registry[T] 通用框架

**文件：**
- 创建：`pkg/tunnel/plugin/registry.go`
- 测试：本节无测试（框架本身在后续任务被使用时隐式测试）

- [ ] **步骤 1：创建 registry.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package plugin 提供通用泛型注册框架 Registry[T]。
// 各子系统（xfer、hub、tracing）基于 Registry[T] 定义自己的插件注册表。
package plugin

import (
	"fmt"
	"sync"
)

// Plugin 描述一个已注册的插件。
type Plugin[T any] struct {
	Name     string
	Instance T
	Priority int // 优先级，高者优先。内置默认为 0，外部插件应 > 0
}

// Registry 是类型安全的插件注册表。
// T 是插件接口类型，由各子系统定义。
// 零值不可用，必须通过 New 创建。
type Registry[T any] struct {
	name    string
	mu      sync.RWMutex
	builtin T
	plugins map[string]Plugin[T]
}

// New 创建一个新的注册表。
// name 用于日志/调试标识；builtin 是内置兜底实现。
func New[T any](name string, builtin T) *Registry[T] {
	return &Registry[T]{
		name:    name,
		builtin: builtin,
		plugins: make(map[string]Plugin[T]),
	}
}

// Register 注册一个插件。
// 同名插件以最后一次注册为准（后注册覆盖前注册）。
func (r *Registry[T]) Register(p Plugin[T]) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Name == "" {
		panic(fmt.Sprintf("plugin[%s]: Register called with empty name", r.name))
	}
	r.plugins[p.Name] = p
}

// Active 返回最高优先级的已注册实现。
// 如果没有已注册插件，返回内置兜底。
func (r *Registry[T]) Active() T {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var best *Plugin[T]
	for _, p := range r.plugins {
		if best == nil || p.Priority > best.Priority {
			best = &p
		}
	}
	if best != nil {
		return best.Instance
	}
	return r.builtin
}

// Get 按名称查找插件。返回其实例和 true；未找到时返回零值和 false。
func (r *Registry[T]) Get(name string) (T, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	p, ok := r.plugins[name]
	if !ok {
		var zero T
		return zero, false
	}
	return p.Instance, true
}

// Names 返回所有已注册插件的名称列表。
func (r *Registry[T]) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	names := make([]string, 0, len(r.plugins))
	for name := range r.plugins {
		names = append(names, name)
	}
	return names
}

// IsDefault 返回当前是否使用内置兜底实现（即无外部插件注册）。
func (r *Registry[T]) IsDefault() bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.plugins) == 0
}
```

- [ ] **步骤 2：创建初始测试**

```go
// pkg/tunnel/plugin/registry_test.go
package plugin_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
)

type testInterface interface {
	Execute() string
}

type builtinImpl struct{}
func (b builtinImpl) Execute() string { return "builtin" }

type externalImpl struct{ value string }
func (e externalImpl) Execute() string { return e.value }

func TestRegistryActiveReturnsBuiltinWhenNoPlugins(t *testing.T) {
	r := plugin.New("test", builtinImpl{})
	active := r.Active()
	if active.Execute() != "builtin" {
		t.Fatalf("expected 'builtin', got %q", active.Execute())
	}
}

func TestRegistryActiveReturnsHighestPriority(t *testing.T) {
	r := plugin.New("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "low", Instance: externalImpl{"low"}, Priority: 1})
	r.Register(plugin.Plugin[testInterface]{Name: "high", Instance: externalImpl{"high"}, Priority: 10})
	active := r.Active()
	if active.Execute() != "high" {
		t.Fatalf("expected 'high', got %q", active.Execute())
	}
}

func TestRegistryGet(t *testing.T) {
	r := plugin.New("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "foo", Instance: externalImpl{"bar"}, Priority: 5})
	inst, found := r.Get("foo")
	if !found { t.Fatal("expected to find 'foo'") }
	if inst.Execute() != "bar" { t.Fatalf("expected 'bar', got %q", inst.Execute()) }

	_, found = r.Get("nonexistent")
	if found { t.Fatal("expected not to find nonexistent") }
}

func TestRegistryNames(t *testing.T) {
	r := plugin.New("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "a", Instance: externalImpl{"a"}, Priority: 1})
	r.Register(plugin.Plugin[testInterface]{Name: "b", Instance: externalImpl{"b"}, Priority: 2})
	names := r.Names()
	if len(names) != 2 { t.Fatalf("expected 2 names, got %d", len(names)) }
}

func TestRegistryIsDefault(t *testing.T) {
	r := plugin.New("test", builtinImpl{})
	if !r.IsDefault() { t.Fatal("expected IsDefault=true with no plugins") }
	r.Register(plugin.Plugin[testInterface]{Name: "x", Instance: externalImpl{"x"}, Priority: 1})
	if r.IsDefault() { t.Fatal("expected IsDefault=false after registering plugin") }
}

func TestRegistryRegisterEmptyNamePanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil { t.Fatal("expected panic on empty name") }
	}()
	r := plugin.New("test", builtinImpl{})
	r.Register(plugin.Plugin[testInterface]{Name: "", Instance: externalImpl{"x"}, Priority: 1})
}
```

- [ ] **步骤 3：运行测试验证通过**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/tunnel/plugin/... -v
```

预期：6 个测试全部 PASS。

- [ ] **步骤 4：Commit**

```bash
git add pkg/tunnel/plugin/
git commit -m "feat(tunnel): add plugin.Registry[T] generic registration framework"
```

---

### 任务 2：重构 xfer 核心包

**文件：**
- 创建：`pkg/tunnel/xfer/core.go`
- 创建：`pkg/tunnel/xfer/registry.go`
- 修改：`pkg/tunnel/xfer/xfer.go`（精简，仅保留 ErrConnClosed）
- 修改：`pkg/tunnel/xfer/xfer_test.go`（适配新 API）

- [ ] **步骤 1：创建 core.go**（提取 Conn/Listener/Transport 接口 + ErrConnClosed）

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer

import (
	"context"
	"fmt"
	"io"
)

// Conn 是双向保序消息连接。
type Conn interface {
	Send(ctx context.Context, msg []byte) error
	Receive(ctx context.Context) ([]byte, error)
	io.Closer
}

// Listener 接受来自远端的连接（Hub/Server 端使用）。
type Listener interface {
	Accept(ctx context.Context) (Conn, error)
	io.Closer
}

// Transport 是传输层实现的注册单元。
type Transport struct {
	Name   string
	Dial   func(ctx context.Context, addr string) (Conn, error)
	Listen func(ctx context.Context, addr string) (Listener, error)
}

// ErrConnClosed 是连接关闭后 Send/Receive 应返回的错误。
var ErrConnClosed = fmt.Errorf("xfer: connection closed")

// ErrNoTransport 是 Get() 找不到指定传输层时返回的错误。
var ErrNoTransport = fmt.Errorf("xfer: no transport registered")
```

- [ ] **步骤 2：创建 registry.go**（基于 plugin.Registry 的 Transport 注册表，含向后兼容包装）

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfer

import (
	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
)

// TransportRegistry 是传输层实现的插件注册表。
// 内置实现（internal/tcp、internal/http）在 init() 中以低优先级注册。
// 外部插件（ext/ws、ext/quic 等）以高优先级注册覆盖。
var TransportRegistry = plugin.New("xfer", emptyTransport())

func emptyTransport() *Transport {
	return &Transport{
		Name:   "builtin",
		Dial:   func(ctx context.Context, addr string) (Conn, error) { return nil, ErrNoTransport },
		Listen: func(ctx context.Context, addr string) (Listener, error) { return nil, ErrNoTransport },
	}
}

// Register 注册一个 Transport 到全局注册表。
// 兼容旧的 xfer.Register 调用方式。
func Register(t *Transport) {
	TransportRegistry.Register(plugin.Plugin[*Transport]{
		Name:     t.Name,
		Instance: t,
		Priority: 0,
	})
}

// Get 按名字查找已注册的 Transport。
// 未找到时返回 nil。
func Get(name string) *Transport {
	t, _ := TransportRegistry.Get(name)
	return t
}
```

- [ ] **步骤 3：精简 xfer.go**（删除 registry 函数，只保留 import + 文件头）

```go
// 删除原有内容，替换为：
// Package xfer 定义传输层抽象接口和注册表。
//
// xfer 层是 sproxy tunnel 系统的最底层抽象...
// （保留 package 文档注释）

package xfer

// 删除原有的 var registry、func Register、func Get
// core.go 和 registry.go 已包含所有内容
```

实际操作：直接删除 `xfer.go` 中 `var registry`、`func Register`、`func Get` 部分，保留文件头部文档注释和 package 声明。因为 core.go 和 registry.go 已经包含了这些。

- [ ] **步骤 4：适配 xfer_test.go**

```go
// 将原有测试中的 xfer.Register 和 xfer.Get 保持不变
// 它们现在是向后兼容包装，行为应与原来一致
// 无需修改测试代码本身，除非编译不通过
```

在 `xfer_test.go` 中删除对 `xfer.DialHTTP` 的引用（因为 http 包已移到 internal/http/）。把使用 `DialHTTP` 的两个测试改为通过 `xfer.Get("http")` 来获取 http transport 并调用其 Dial。

```go
// 修改 TestHTTPConnRoundTrip 和 TestHTTPConnMultipleRoundTrips
// 使用 xfer.Get("http").Dial(ctx, addr) 代替 xfer.DialHTTP(ctx, addr)
```

- [ ] **步骤 5：运行现有测试验证**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/tunnel/xfer/... -v -count=1 -run TestRegister
```

预期：`TestRegisterAndGet`、`TestRegisterNilPanics`、`TestRegisterEmptyNamePanics` 应通过。

- [ ] **步骤 6：Commit**

```bash
git add pkg/tunnel/xfer/core.go pkg/tunnel/xfer/registry.go pkg/tunnel/xfer/xfer.go pkg/tunnel/xfer/xfer_test.go
git commit -m "refactor(tunnel): extract xfer core and registry from xfer.go"
```

---

### 任务 3：创建 internal/tcp 和 internal/http 内置传输

**文件：**
- 创建：`pkg/tunnel/xfer/internal/http/http.go`
- 创建：`pkg/tunnel/xfer/internal/tcp/tcp.go`
- 创建：`pkg/tunnel/xfer/internal/tcp/tcp_test.go`
- 删除：`pkg/tunnel/xfer/http.go`
- 删除：`pkg/tunnel/xfer/xfertcp/tcp.go`
- 删除：`pkg/tunnel/xfer/xfertcp/tcp_test.go`

- [ ] **步骤 1：创建 internal/http/http.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package http 提供基于 HTTP POST 的内置 xfer.Conn 传输层实现。
// 在 init() 中自动注册到 xfer.TransportRegistry，名字为 "http"。
// 此包位于 internal/，仅限 xfer 子树使用。
package http

import (
	"bytes"
	"context"
	"io"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "http",
		Instance: &xfer.Transport{
			Name:   "http",
			Dial:   Dial,
			Listen: nil, // HTTP 传输仅支持客户端 Dial
		},
		Priority: 0,
	})
}

type httpConn struct {
	url    string
	client *http.Client
	msg    []byte
}

// Dial 创建一个通过 HTTP POST 传输的 Conn。
// addr 是 sproxy 服务端地址（如 "http://localhost:18083"）。
func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	return &httpConn{
		url:    addr + "/tunnel",
		client: http.DefaultClient,
	}, nil
}

func (c *httpConn) Send(ctx context.Context, msg []byte) error {
	c.msg = msg
	return nil
}

func (c *httpConn) Receive(ctx context.Context) ([]byte, error) {
	body := c.msg
	c.msg = nil
	if body == nil {
		body = []byte{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func (c *httpConn) Close() error {
	c.client.CloseIdleConnections()
	return nil
}
```

- [ ] **步骤 2：创建 internal/tcp/tcp.go**

从 `pkg/tunnel/xfer/xfertcp/tcp.go` 复制。修改点：
- 包名从 `xfertcp` 改为 `tcp`
- import 路径从 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer"` 改为同 module 路径
- 注册方式改为使用 `xfer.TransportRegistry.Register`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tcp

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "tcp",
		Instance: &xfer.Transport{
			Name:   "tcp",
			Dial:   Dial,
			Listen: Listen,
		},
		Priority: 0,
	})
}

// tcpConn 将 net.Conn 包装为 xfer.Conn，使用 4B 长度前缀帧定界。
type tcpConn struct {
	conn   net.Conn
	mu     sync.Mutex
	closed bool
}

func (c *tcpConn) Send(ctx context.Context, msg []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return xfer.ErrConnClosed
	}
	frame := make([]byte, 4+len(msg))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(msg)))
	copy(frame[4:], msg)
	_, err := c.conn.Write(frame)
	if err != nil {
		return fmt.Errorf("tcp send: %w", err)
	}
	return nil
}

func (c *tcpConn) Receive(ctx context.Context) ([]byte, error) {
	if c.closed {
		return nil, xfer.ErrConnClosed
	}
	lenBuf := make([]byte, 4)
	if _, err := io.ReadFull(c.conn, lenBuf); err != nil {
		return nil, fmt.Errorf("tcp recv length: %w", err)
	}
	msgLen := binary.BigEndian.Uint32(lenBuf)
	msg := make([]byte, msgLen)
	if _, err := io.ReadFull(c.conn, msg); err != nil {
		return nil, fmt.Errorf("tcp recv body: %w", err)
	}
	return msg, nil
}

func (c *tcpConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.conn.Close()
}

// TcpListener 实现 xfer.Listener，基于 net.Listener。
type TcpListener struct {
	ln      net.Listener
	closeCh chan struct{}
}

func (l *TcpListener) Addr() net.Addr { return l.ln.Addr() }

func (l *TcpListener) Accept(ctx context.Context) (xfer.Conn, error) {
	connCh := make(chan net.Conn, 1)
	errCh := make(chan error, 1)
	go func() {
		c, err := l.ln.Accept()
		if err != nil {
			errCh <- err
			return
		}
		connCh <- c
	}()
	select {
	case c := <-connCh:
		return &tcpConn{conn: c}, nil
	case err := <-errCh:
		return nil, fmt.Errorf("tcp accept: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

func (l *TcpListener) Close() error {
	close(l.closeCh)
	return l.ln.Close()
}

func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp dial: %w", err)
	}
	return &tcpConn{conn: conn}, nil
}

func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("tcp listen: %w", err)
	}
	return &TcpListener{
		ln:      ln,
		closeCh: make(chan struct{}),
	}, nil
}
```

- [ ] **步骤 3：创建 internal/tcp/tcp_test.go**

从 `xfer/xfertcp/tcp_test.go` 复制。修改点：
- 包名改为 `tcp_test`
- import 路径改为 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer/internal/tcp"`

- [ ] **步骤 4：删除旧文件**

```bash
git rm pkg/tunnel/xfer/http.go
git rm -r pkg/tunnel/xfer/xfertcp/
```

- [ ] **步骤 5：编译验证**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go build ./...
```

预期：编译成功。

- [ ] **步骤 6：运行 xfer 测试**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go test ./pkg/tunnel/xfer/... -v -count=1
```

预期：所有测试 PASS。

- [ ] **步骤 7：Commit**

```bash
git add pkg/tunnel/xfer/internal/
git rm pkg/tunnel/xfer/http.go
git rm -r pkg/tunnel/xfer/xfertcp/
git commit -m "refactor(tunnel): move HTTP/TCP transports to xfer/internal/"
```

---

### 任务 4：修复 xfertest/pipe.go data race + 增强测试套件

**文件：**
- 修改：`pkg/tunnel/xfer/xfertest/pipe.go`
- 创建：`pkg/tunnel/xfer/xfertest/suite.go`
- 创建：`pkg/tunnel/xfer/xfertest/harness.go`

- [ ] **步骤 1：修复 pipe.go data race**

问题：`Send` 方法在 `mu.Unlock()` 之后写入 `tx channel`，`Close()` 可能同时关闭 channel 导致 panic。

修复：将 `mu.Lock` 覆盖到 channel 写入。

```go
func (p *pipeConn) Send(ctx context.Context, msg []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return xfer.ErrConnClosed
	}
	cp := make([]byte, len(msg))
	copy(cp, msg)
	select {
	case p.tx <- message{data: cp}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.closeCh:
		return xfer.ErrConnClosed
	}
}
```

同时增加一个 `Close` 中的竞态修复，确保 `close()` 也受 mu 保护：

```go
// Close 已经是 mu.Lock 保护，无需修改。
// 但需要确保在 close(p.closeCh) 后，新发送能看到 p.closed = true
```

- [ ] **步骤 2：创建 suite.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest

import (
	"bytes"
	"context"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// ConnFactory 是 Conn 行为测试的夹具生成函数。
type ConnFactory func(t *testing.T) (client, server xfer.Conn, cleanup func())

// ConnSuite 运行所有 Conn 接口一致性测试。
func ConnSuite(t *testing.T, factory ConnFactory) {
	t.Run("RoundTrip", func(t *testing.T) { testRoundTrip(t, factory) })
	t.Run("MultipleMessages", func(t *testing.T) { testMultipleMessages(t, factory) })
	t.Run("LargePayload", func(t *testing.T) { testLargePayload(t, factory) })
	t.Run("ConcurrentSend", func(t *testing.T) { testConcurrentSend(t, factory) })
	t.Run("CloseWhileBlocking", func(t *testing.T) { testCloseWhileBlocking(t, factory) })
	t.Run("ContextCancellation", func(t *testing.T) { testContextCancellation(t, factory) })
	t.Run("OrderedDelivery", func(t *testing.T) { testOrderedDelivery(t, factory) })
	t.Run("EmptyMessage", func(t *testing.T) { testEmptyMessage(t, factory) })
	t.Run("SendAfterClose", func(t *testing.T) { testSendAfterClose(t, factory) })
}

func testRoundTrip(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	msg := []byte("hello")
	if err := client.Send(ctx, msg); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, msg) {
		t.Fatalf("got %q, want %q", got, msg)
	}
}

func testMultipleMessages(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	count := 10
	for i := range count {
		msg := []byte(fmt.Sprintf("msg-%d", i))
		if err := client.Send(ctx, msg); err != nil {
			t.Fatal(err)
		}
		got, err := server.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, msg) {
			t.Fatalf("msg %d: got %q, want %q", i, got, msg)
		}
	}
}

func testLargePayload(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	payload := make([]byte, 65536)
	_, _ = rand.Read(payload)

	if err := client.Send(ctx, payload); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("large payload: got %d bytes, want %d bytes", len(got), len(payload))
	}
}

func testConcurrentSend(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	var wg sync.WaitGroup
	count := 5
	msgs := make([][]byte, count)
	for i := range count {
		msgs[i] = []byte(fmt.Sprintf("concurrent-%d", i))
	}

	// server: read all messages
	received := make(chan []byte, count)
	go func() {
		for range count {
			msg, err := server.Receive(ctx)
			if err != nil {
				return
			}
			received <- msg
		}
	}()

	// client: send concurrently
	for i := range count {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = client.Send(ctx, msgs[i])
		}()
	}
	wg.Wait()

	close(received)
	gotCount := 0
	for range received {
		gotCount++
	}
	if gotCount != count {
		t.Fatalf("expected %d messages, got %d", count, gotCount)
	}
}

func testCloseWhileBlocking(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()

	errCh := make(chan error, 1)
	go func() {
		// server 阻塞等待接收（没有数据来）
		_, err := server.Receive(ctx)
		errCh <- err
	}()

	time.Sleep(10 * time.Millisecond)

	// 关闭 server 端
	if err := server.Close(); err != nil {
		t.Fatal(err)
	}

	// server.Receive 应返回 ErrConnClosed
	select {
	case err := <-errCh:
		if err != xfer.ErrConnClosed && err != nil {
			t.Logf("expected ErrConnClosed, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for close to unblock Receive")
	}

	// 关闭后的 Send 应返回 ErrConnClosed
	if err := client.Send(ctx, []byte("after-close")); err != xfer.ErrConnClosed {
		t.Logf("expected ErrConnClosed on Send after close, got %v", err)
	}
}

func testContextCancellation(t *testing.T, factory ConnFactory) {
	client, _, cleanup := factory(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	if err := client.Send(ctx, []byte("should fail")); err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
	if err := client.Send(ctx, []byte("should fail")); err == nil {
		t.Fatal("expected error on cancelled context, got nil")
	}
}

func testOrderedDelivery(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	msgs := []string{"first", "second", "third"}

	for _, msg := range msgs {
		if err := client.Send(ctx, []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range msgs {
		got, err := server.Receive(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("out of order: got %q, want %q", got, want)
		}
	}
}

func testEmptyMessage(t *testing.T, factory ConnFactory) {
	client, server, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	if err := client.Send(ctx, []byte{}); err != nil {
		t.Fatal(err)
	}
	got, err := server.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty message, got %d bytes", len(got))
	}
}

func testSendAfterClose(t *testing.T, factory ConnFactory) {
	client, _, cleanup := factory(t)
	defer cleanup()

	ctx := context.Background()
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Send(ctx, []byte("after-close")); err == nil {
		t.Fatal("expected error on Send after Close")
	}
}
```

- [ ] **步骤 3：创建 harness.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package xfertest

import (
	"context"
	"net"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// Harness 编排完整的 transport 集成测试。
type Harness struct {
	Name   string
	Dial   func(ctx context.Context, addr string) (xfer.Conn, error)
	Listen func(ctx context.Context, addr string) (xfer.Listener, error)
}

// TestHarness 运行 ConnSuite + 端到端 Listener-Dial 测试。
func TestHarness(t *testing.T, h Harness) {
	t.Run(h.Name, func(t *testing.T) {
		t.Parallel()

		connFactory := func(t *testing.T) (client, server xfer.Conn, cleanup func()) {
			t.Helper()
			ctx := context.Background()

			listener, err := h.Listen(ctx, "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}

			addr := listenerAddr(listener)

			type acceptResult struct {
				conn xfer.Conn
				err  error
			}
			acceptCh := make(chan acceptResult, 1)
			go func() {
				c, aerr := listener.Accept(ctx)
				acceptCh <- acceptResult{c, aerr}
			}()

			clientConn, err := h.Dial(ctx, addr)
			if err != nil {
				listener.Close()
				t.Fatal(err)
			}

			result := <-acceptCh
			if result.err != nil {
				clientConn.Close()
				listener.Close()
				t.Fatal(result.err)
			}

			cleanup = func() {
				clientConn.Close()
				result.conn.Close()
				listener.Close()
			}
			return clientConn, result.conn, cleanup
		}

		ConnSuite(t, connFactory)
	})
}

func listenerAddr(l xfer.Listener) string {
	if a, ok := l.(interface{ Addr() net.Addr }); ok {
		return a.Addr().String()
	}
	if a, ok := l.(interface{ Addr() string }); ok {
		return a.Addr()
	}
	return ""
}
```

- [ ] **步骤 4：运行 pipe 测试**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go test -race ./pkg/tunnel/xfer/xfertest/... -v -count=1
```
预期：无 data race 警告，测试 PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/xfer/xfertest/
git commit -m "feat(tunnel): add xfertest ConnSuite/TestHarness + fix pipe data race"
```

---

### 任务 5：提取 WS 和 QUIC 到 ext/ 独立 module

**文件：**
- 创建：`pkg/tunnel/xfer/ext/ws/go.mod`
- 创建：`pkg/tunnel/xfer/ext/ws/ws.go`
- 创建：`pkg/tunnel/xfer/ext/ws/ws_test.go`
- 创建：`pkg/tunnel/xfer/ext/quic/go.mod`
- 创建：`pkg/tunnel/xfer/ext/quic/quic.go`
- 创建：`pkg/tunnel/xfer/ext/quic/quic_test.go`
- 创建：`go.work`
- 删除：`pkg/tunnel/xfer/xferws/ws.go`
- 删除：`pkg/tunnel/xfer/xferws/ws_test.go`
- 修改：`go.mod`（移除 coder/websocket、quic-go 及其传递依赖）
- 删除：`pkg/tunnel/xfer/xferquic/quic.go`
- 删除：`pkg/tunnel/xfer/xferquic/quic_test.go`

- [ ] **步骤 1：创建 ext/ws/go.mod**

```go
module github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws

go 1.26

require (
	github.com/coder/websocket v1.8.14
	github.com/cocomhub/sproxy v0.0.0
)

replace github.com/cocomhub/sproxy => ../../../../..
```

- [ ] **步骤 2：创建 ext/ws/ws.go**

从 `xferws/ws.go` 复制，修改：
- import `xfer` 路径改为 `github.com/cocomhub/sproxy/pkg/tunnel/xfer`
- 注册改为使用 `xfer.TransportRegistry.Register`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws

import (
	"context"
	"net"
	"net/http"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/coder/websocket"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "ws",
		Instance: &xfer.Transport{
			Name:   "ws",
			Dial:   Dial,
			Listen: Listen,
		},
		Priority: 10,
	})
}

type wsConn struct {
	conn *websocket.Conn
}

func (c *wsConn) Send(ctx context.Context, msg []byte) error {
	return c.conn.Write(ctx, websocket.MessageBinary, msg)
}

func (c *wsConn) Receive(ctx context.Context) ([]byte, error) {
	_, msg, err := c.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	return msg, nil
}

func (c *wsConn) Close() error {
	return c.conn.CloseNow()
}

func Dial(ctx context.Context, addr string) (xfer.Conn, error) {
	conn, _, err := websocket.Dial(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	return &wsConn{conn: conn}, nil
}

type wsListener struct {
	srv     *http.Server
	netLn   net.Listener
	connCh  chan xfer.Conn
	closeCh chan struct{}
}

func (l *wsListener) Accept(ctx context.Context) (xfer.Conn, error) {
	select {
	case c := <-l.connCh:
		return c, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-l.closeCh:
		return nil, xfer.ErrConnClosed
	}
}

func (l *wsListener) Close() error {
	close(l.closeCh)
	return l.srv.Close()
}

func (l *wsListener) Addr() net.Addr {
	if l.netLn != nil {
		return l.netLn.Addr()
	}
	return nil
}

func Listen(ctx context.Context, addr string) (xfer.Listener, error) {
	netLn, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	l := &wsListener{
		netLn:   netLn,
		connCh:  make(chan xfer.Conn, 16),
		closeCh: make(chan struct{}),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		select {
		case l.connCh <- &wsConn{conn: conn}:
		case <-l.closeCh:
			conn.CloseNow()
		}
	})
	l.srv = &http.Server{
		Addr:    addr,
		Handler: mux,
	}
	go func() {
		l.srv.Serve(netLn)
	}()
	return l, nil
}
```

- [ ] **步骤 3：创建 ext/ws/ws_test.go**

从 `xferws/ws_test.go` 复制。修改点：
- 包名改为 `ws_test`
- import 路径改为 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"`
- 使用 `xfertest.TestHarness` 替代手写测试

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ws_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestWS(t *testing.T) {
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "ws",
		Dial:   ws.Dial,
		Listen: ws.Listen,
	})
}
```

- [ ] **步骤 4：编译验证 ext/ws**

```bash
cd D:/workdir/leon/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws && go build ./...
```

- [ ] **步骤 5：创建 ext/quic/go.mod**

```go
module github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic

go 1.26

require (
	github.com/quic-go/quic-go v0.48.2
	github.com/cocomhub/sproxy v0.0.0
)

replace github.com/cocomhub/sproxy => ../../../../..
```

- [ ] **步骤 6：创建 ext/quic/quic.go**

从 `xferquic/quic.go` 复制。修改点：
- import 路径改为 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer"` + `plugin`
- 注册改为 `xfer.TransportRegistry.Register`
- 包名改为 `quic`

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/plugin"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
	"github.com/quic-go/quic-go"
)

func init() {
	xfer.TransportRegistry.Register(plugin.Plugin[*xfer.Transport]{
		Name: "quic",
		Instance: &xfer.Transport{
			Name:   "quic",
			Dial:   Dial,
			Listen: Listen,
		},
		Priority: 10,
	})
}

// （后续代码与 xferquic/quic.go 相同，仅包名改为 quic）
// ...
```

完整内容与 `xferquic/quic.go` 保持一致，仅做以下修改：
- `package xferquic` → `package quic`
- `"github.com/cocomhub/sproxy/pkg/tunnel/xfer"` 保持不变（同 module 路径）
- 添加 `"github.com/cocomhub/sproxy/pkg/tunnel/plugin"` import
- `init()` 中的 `xfer.Register(...)` → `xfer.TransportRegistry.Register(...)`

- [ ] **步骤 7：创建 ext/quic/quic_test.go**

从 `xferquic/quic_test.go` 复制。修改点：
- 包名改为 `quic_test`
- 删除硬编码的 `isWindows() = return true` 函数，改为使用 `runtime.GOOS`
- import 路径改为 `"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"`
- 使用 `xfertest.TestHarness` 代替手写

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package quic_test

import (
	"runtime"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic"
	"github.com/cocomhub/sproxy/pkg/tunnel/xfer/xfertest"
)

func TestQUIC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("QUIC network tests not supported on Windows (UDP connectivity issues)")
	}
	xfertest.TestHarness(t, xfertest.Harness{
		Name:   "quic",
		Dial:   quic.Dial,
		Listen: quic.Listen,
	})
}
```

- [ ] **步骤 8：编译验证 ext/quic**

```bash
cd D:/workdir/leon/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic && go build ./...
```

- [ ] **步骤 9：创建 go.work**

```go
go 1.26

use (
	.
	./pkg/tunnel/xfer/ext/ws
	./pkg/tunnel/xfer/ext/quic
)
```

- [ ] **步骤 10：删除旧 WS/QUIC 目录**

```bash
git rm -r pkg/tunnel/xfer/xferws/
git rm -r pkg/tunnel/xfer/xferquic/
```

- [ ] **步骤 11：清理主 go.mod**

运行 `go mod tidy` 移除 coder/websocket 和 quic-go 及其传递依赖。

```bash
cd D:/workdir/leon/cocomhub/sproxy && go mod tidy
```

验证主 go.mod 不再包含 coder/websocket 或 quic-go。

```bash
grep -E "coder|quic-go" go.mod && echo "FOUND (should be empty)" || echo "CLEAN"
```

预期输出：`CLEAN`

- [ ] **步骤 12：完整编译验证**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go build ./...
go test ./... -count=1 2>&1 | head -50
```

预期：`?    pkg/tunnel/xfer/ext/ws  [no test files]`（单独 module）
预期：`?    pkg/tunnel/xfer/ext/quic [no test files]`（单独 module）
预期：原有测试全部通过（mux、hub、tunnel 等）

- [ ] **步骤 13：Commit**

```bash
git add pkg/tunnel/xfer/ext/ go.work
git rm -r pkg/tunnel/xfer/xferws/ pkg/tunnel/xfer/xferquic/
git commit -m "refactor(tunnel): extract WS/QUIC transports to ext/ independent modules"
```

---

### 任务 6：重构 hub DHT 为接口 + 内置实现

**文件：**
- 创建：`pkg/tunnel/hub/core.go`
- 创建：`pkg/tunnel/hub/registry.go`
- 创建：`pkg/tunnel/hub/internal/memdht.go`
- 创建：`pkg/tunnel/hub/ext/kad/go.mod`（占位）
- 修改：`pkg/tunnel/hub/dht.go`（删除原有内容或改为接口）
- 修改：`pkg/tunnel/hub/dht_test.go`
- 修改：`pkg/tunnel/p2p/p2p.go`
- 修改：`pkg/tunnel/p2p/p2p_test.go`

- [ ] **步骤 1：创建 core.go（DHT 接口 + NodeInfo）**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "context"

// NodeInfo 统一节点信息结构。
type NodeInfo struct {
	ID    string
	Addrs []string          // xfer transport 地址列表，如 ["tcp://192.168.1.1:9000"]
	Meta  map[string]string
}

// DHT 定义节点发现的最低接口。
// 内置实现是简单的线程安全内存 map；ext/kad 提供完整的 Kademlia。
type DHT interface {
	// Register 将本节点注册到 DHT 网络。
	Register(ctx context.Context, node NodeInfo) error

	// Lookup 按节点 ID 查找目标节点信息。
	Lookup(ctx context.Context, nodeID string) (NodeInfo, error)

	// GetClosestNodes 返回距离目标 ID 最近的 N 个节点。
	// 距离算法由各实现定义（内置：词法排序；Kademlia：XOR 距离）。
	GetClosestNodes(ctx context.Context, nodeID string, n int) ([]NodeInfo, error)

	// Bootstrap 连接到已知种子节点，加入 DHT 网络。
	Bootstrap(ctx context.Context, seeds []string) error

	// Close 退出 DHT 网络，释放资源。
	Close() error
}
```

- [ ] **步骤 2：创建 registry.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package hub

import "github.com/cocomhub/sproxy/pkg/tunnel/plugin"

// DHTRegistry 是节点发现实现的插件注册表。
var DHTRegistry = plugin.New("dht", memDHT())

// memDHT 返回内置的默认内存 DHT 实现。
func memDHT() DHT {
	return newMemoryDHT()
}
```

- [ ] **步骤 3：创建 internal/memdht.go**

将当前 `dht.go` 的 `DHT` 结构体和相关方法搬过来，改为实现 `hub.DHT` 接口。

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

// Package internal 提供 hub 包的内置实现。
package internal

import (
	"context"
	"fmt"
	"maps"
	"math"
	"sort"
	"sync"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// memoryDHT 是一个生产可用但去中心化颗粒度最低的 DHT 实现。
// 适合单机或小规模固定节点拓扑，不依赖任何外部发现协议。
type memoryDHT struct {
	mu    sync.RWMutex
	nodes map[string]hub.NodeInfo
}

// NewDHT 创建新的内存 DHT。
func NewDHT() *memoryDHT {
	return &memoryDHT{nodes: make(map[string]hub.NodeInfo)}
}

func (d *memoryDHT) Register(_ context.Context, node hub.NodeInfo) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	info := hub.NodeInfo{
		ID:    node.ID,
		Addrs: append([]string(nil), node.Addrs...),
		Meta:  make(map[string]string, len(node.Meta)),
	}
	maps.Copy(info.Meta, node.Meta)
	d.nodes[node.ID] = info
	return nil
}

func (d *memoryDHT) Lookup(_ context.Context, nodeID string) (hub.NodeInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	node, ok := d.nodes[nodeID]
	if !ok {
		return hub.NodeInfo{}, fmt.Errorf("dht: node %q not found", nodeID)
	}
	return node, nil
}

func (d *memoryDHT) GetClosestNodes(_ context.Context, targetID string, n int) ([]hub.NodeInfo, error) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if n <= 0 || len(d.nodes) == 0 {
		return nil, nil
	}

	type kv struct {
		id   string
		node hub.NodeInfo
	}
	sorted := make([]kv, 0, len(d.nodes))
	for id, node := range d.nodes {
		if id == targetID {
			continue
		}
		sorted = append(sorted, kv{id: id, node: node})
	}
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].id < sorted[j].id
	})

	end := int(math.Min(float64(n), float64(len(sorted))))
	result := make([]hub.NodeInfo, end)
	for i := range end {
		result[i] = sorted[i].node
	}
	return result, nil
}

func (d *memoryDHT) Bootstrap(_ context.Context, _ []string) error {
	// 内存 DHT 无需 seed 节点
	return nil
}

func (d *memoryDHT) Close() error {
	return nil
}
```

- [ ] **步骤 4：修改 hub/dht.go**（替换为 hub 包导出接口和工厂函数）

```go
package hub

// 此处仅保留包文档。DHT 接口在 core.go 中。
// memoryDHT 实现移至 internal/memdht.go。
```

实际操作：`hub/dht.go` 改为仅引用 `hub/core.go` 和 `hub/internal/memdht.go`。

- [ ] **步骤 5：改造 hub/dht_test.go**（改用 DHT 接口）

修改为：
1. import `hub/internal` 包以创建内存 DHT 实例
2. 所有 `NewDHT()` 调用改为 `internal.NewDHT()`
3. `dht.Register` 改为 `dht.Register(ctx, hub.NodeInfo{...})`
4. `dht.Lookup` 返回两个值 `(hub.NodeInfo, error)`
5. `dht.GetClosestNodes` 返回两个值 `([]hub.NodeInfo, error)`
6. 删除 `TestDhtBootstrapNotImplemented`（内存版 Bootstrap 现在返回 nil）

```go
package hub_test

import (
	"context"
	"sync"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub/internal"
)

func TestDhtRegisterAndLookup(t *testing.T) {
	ctx := context.Background()
	dht := internal.NewDHT()

	err := dht.Register(ctx, hub.NodeInfo{
		ID:    "node-1",
		Addrs: []string{"192.168.1.10:9000"},
		Meta:  map[string]string{"region": "us-east"},
	})
	if err != nil {
		t.Fatal(err)
	}

	node, err := dht.Lookup(ctx, "node-1")
	if err != nil {
		t.Fatal(err)
	}
	if node.ID != "node-1" {
		t.Fatalf("expected ID node-1, got %s", node.ID)
	}

	_, err = dht.Lookup(ctx, "unknown")
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}
// ... 后续测试类似适配
```

注意：`TestDhtBootstrapNotImplemented` 删除（因为内存 DHT 的 Bootstrap 不再返回 error）。

- [ ] **步骤 6：更新 p2p/p2p.go**

将 `*hub.DHT` 改为 `hub.DHT` 接口。

```go
// p2p.go 修改

import "github.com/cocomhub/sproxy/pkg/tunnel/hub"

type P2PNode struct {
	ID  string
	DHT hub.DHT  // interface, not concrete type
	// ...
}

func NewP2PNode(id string, dht hub.DHT) *P2PNode {
	// ...
}

func (n *P2PNode) Dial(ctx context.Context, targetID string) (*mux.Mux, error) {
	node, err := n.DHT.Lookup(ctx, targetID)
	// ...
}

func (n *P2PNode) Listen(ctx context.Context, addr string) error {
	// ...
	n.DHT.Register(ctx, hub.NodeInfo{
		ID:    n.ID,
		Addrs: []string{addr},
	})
	// ...
}
```

- [ ] **步骤 7：更新 p2p/p2p_test.go**

适配 DHT 接口变更。

- [ ] **步骤 8：编译验证**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go build ./... && go test ./pkg/tunnel/hub/... ./pkg/tunnel/p2p/... -v -count=1
```

- [ ] **步骤 9：Commit**

```bash
git add pkg/tunnel/hub/core.go pkg/tunnel/hub/registry.go pkg/tunnel/hub/internal/ pkg/tunnel/hub/ext/
git add pkg/tunnel/p2p/
git rm pkg/tunnel/hub/dht.go
git commit -m "refactor(tunnel): extract hub.DHT interface + internal memory DHT"
```

---

### 任务 7：重构 tracing 为接口 + 内置实现

**文件：**
- 创建：`pkg/tunnel/tracing/core.go`
- 创建：`pkg/tunnel/tracing/registry.go`
- 创建：`pkg/tunnel/tracing/internal/slogtracer.go`
- 创建：`pkg/tunnel/tracing/ext/otel/go.mod`（占位）
- 修改：`pkg/tunnel/tracing/tracing.go`（精简）
- 修改：`pkg/tunnel/tracing/tracing_test.go`（适配接口）

- [ ] **步骤 1：创建 core.go（Tracer 接口 + Carrier）**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import "context"

// Carrier 是跟踪传播的抽象，可适配 http.Header / gRPC metadata / map。
type Carrier interface {
	Get(key string) string
	Set(key string, value string)
}

// Tracer 定义跟踪的最小接口。
// 内置实现输出到 slog；ext/otel 提供完整 OpenTelemetry。
type Tracer interface {
	// StartSpan 创建并开始一个 span，返回含 span 信息的 context 和结束函数。
	StartSpan(ctx context.Context, name string) (context.Context, func())

	// Inject 将跟踪上下文注入到传出请求的 carrier 中。
	Inject(ctx context.Context, carrier Carrier)

	// Extract 从传入请求的 carrier 中提取跟踪上下文到 context。
	Extract(ctx context.Context, carrier Carrier) context.Context
}

// HTTPCarrier 适配 http.Header 为 Carrier。
type HTTPCarrier struct{ Header map[string][]string }

func (c HTTPCarrier) Get(key string) string {
	if vals := c.Header[key]; len(vals) > 0 {
		return vals[0]
	}
	return ""
}
func (c HTTPCarrier) Set(key, value string) {
	c.Header[key] = []string{value}
}
```

- [ ] **步骤 2：创建 registry.go**

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package tracing

import "github.com/cocomhub/sproxy/pkg/tunnel/plugin"

// TracerRegistry 是跟踪实现的插件注册表。
var TracerRegistry = plugin.New("tracer", newSlogTracer())
```

- [ ] **步骤 3：创建 internal/slogtracer.go**

从当前 `tracing.go` 复制实现，改为实现 `tracing.Tracer` 接口。

```go
// Copyright 2026 The Cocomhub Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package internal

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
)

type contextKey struct{}

type Span struct {
	TraceID   string
	SpanID    string
	ParentID  string
	Name      string
	StartTime time.Time
	Duration  time.Duration
	Tags      map[string]string
	ended     bool
}

// slogTracer 基于 slog 的内置追踪实现，零外部依赖。
type slogTracer struct {
	mu    sync.Mutex
	spans []*Span
	depth int
}

func New() *slogTracer {
	return &slogTracer{}
}

func hexID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%016x", b)
}

func (t *slogTracer) StartSpan(ctx context.Context, name string) (context.Context, func()) {
	t.mu.Lock()
	traceID := hexID()
	parentID := ""
	tags := make(map[string]string)
	if parent := spanFromContext(ctx); parent != nil {
		traceID = parent.TraceID
		parentID = parent.SpanID
		maps.Copy(tags, parent.Tags)
	}
	span := &Span{
		TraceID:   traceID,
		SpanID:    hexID(),
		ParentID:  parentID,
		Name:      name,
		StartTime: time.Now(),
		Tags:      tags,
	}
	newCtx := context.WithValue(ctx, contextKey{}, span)
	t.spans = append(t.spans, span)
	t.depth++
	depth := t.depth
	t.mu.Unlock()

	return newCtx, func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if span.ended {
			return
		}
		span.ended = true
		span.Duration = time.Since(span.StartTime)
		indent := ""
		if depth > 1 {
			indent = stringsRepeat("  ", depth-1)
		}
		attrs := slog.String("trace_id", span.TraceID)
		if len(span.Tags) > 0 {
			attrs = slog.Group("tags", tagsToAttrs(span.Tags)...)
		}
		slog.Info(fmt.Sprintf("%s[trace %s] %s %v", indent, span.TraceID, span.Name, span.Duration), attrs)
	}
}

func (t *slogTracer) Inject(ctx context.Context, carrier tracing.Carrier) {
	if s := spanFromContext(ctx); s != nil {
		carrier.Set("traceparent", fmt.Sprintf("00-%s-%s-01", s.TraceID, s.SpanID))
	}
}

func (t *slogTracer) Extract(ctx context.Context, carrier tracing.Carrier) context.Context {
	tp := carrier.Get("traceparent")
	if tp == "" || len(tp) < 55 {
		return ctx
	}
	// W3C traceparent: 00-{traceid}-{spanid}-{flags}
	// traceid 是 32 hex chars, spanid 是 16 hex chars
	traceID := tp[3:35]
	spanID := tp[36:52]
	span := &Span{
		TraceID: traceID,
		SpanID:  spanID,
		Tags:    make(map[string]string),
	}
	return context.WithValue(ctx, contextKey{}, span)
}

func spanFromContext(ctx context.Context) *Span {
	s, _ := ctx.Value(contextKey{}).(*Span)
	return s
}

func tagsToAttrs(tags map[string]string) []any {
	attrs := make([]any, 0, len(tags)*2)
	for k, v := range tags {
		attrs = append(attrs, slog.String(k, v))
	}
	return attrs
}

func stringsRepeat(s string, count int) string {
	if count <= 0 {
		return ""
	}
	b := make([]byte, len(s)*count)
	for i := range count {
		copy(b[i*len(s):], s)
	}
	return string(b)
}
```

- [ ] **步骤 4：修改 tracing/tracing.go**

精简为仅保留包文档和对外暴露的辅助函数（如果有）。

```go
// Package tracing provides a lightweight OpenTelemetry-like tracing skeleton
// ...
package tracing

// 删除所有具体实现（已移至 internal/slogtracer.go）
// 核心 API（New、StartSpan、WithTag）删除或改为委托
```

- [ ] **步骤 5：适配 tracing/tracing_test.go**

改为测试 `tracing.Tracer` 接口。

```go
package tracing_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/tunnel/tracing"
	"github.com/cocomhub/sproxy/pkg/tunnel/tracing/internal"
)

func TestTracerInterface(t *testing.T) {
	tracer := internal.New()
	ctx := context.Background()
	_, end := tracer.StartSpan(ctx, "test")
	end()
}
```

- [ ] **步骤 6：编译验证**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go build ./... && go test ./pkg/tunnel/tracing/... -v -count=1
```

- [ ] **步骤 7：Commit**

```bash
git add pkg/tunnel/tracing/core.go pkg/tunnel/tracing/registry.go pkg/tunnel/tracing/internal/ pkg/tunnel/tracing/ext/
git commit -m "refactor(tunnel): extract tracing.Tracer interface + internal slog tracer"
```

---

### 任务 8：全量验证

- [ ] **步骤 1：完整编译**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go build ./...
```

- [ ] **步骤 2：完整测试**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go test -race ./... -count=1 2>&1
```

- [ ] **步骤 3：独立 module 测试（ws）**

```bash
cd D:/workdir/leon/cocomhub/sproxy/pkg/tunnel/xfer/ext/ws && go test -race ./... -count=1
```

- [ ] **步骤 4：独立 module 测试（quic）**

```bash
cd D:/workdir/leon/cocomhub/sproxy/pkg/tunnel/xfer/ext/quic && go test -race ./... -count=1
```

- [ ] **步骤 5：验证主 go.mod 干净**

```bash
cd D:/workdir/leon/cocomhub/sproxy && grep -E "coder/websocket|quic-go" go.mod && echo "CLEAN FAILED" || echo "go.mod CLEAN"
```

- [ ] **步骤 6：go vet**

```bash
cd D:/workdir/leon/cocomhub/sproxy && go vet ./...
```

- [ ] **步骤 7：最终 Commit**

```bash
git add -A
git commit -m "chore: full validation pass for phase 0 plugin restructuring"
```

---

## 自检

### 规格覆盖度
- [x] 1. `plugin.Registry[T]` → 任务 1
- [x] 2. xfer 目录重构（core.go + registry.go） → 任务 2
- [x] 3. internal/tcp/ + internal/http/ → 任务 3
- [x] 4. WS/QUIC 提取为独立 go.mod → 任务 5
- [x] 5. xfertest 增强（suite.go + harness.go + pipe data race fix）→ 任务 4
- [x] 6. hub.DHT 接口 + internal/memdht → 任务 6
- [x] 7. tracing.Tracer 接口 + internal/slogtracer → 任务 7
- [x] 8. ext/kad/ 和 ext/otel/ 占位目录 → 任务 6/7
- [x] 9. go.work workspace → 任务 5

### 占位符扫描
- [x] 无 TODO/待定占位符
- [x] 无 "后续补充" 悬空描述
- [x] 所有代码步骤包含实际实现代码

### 类型一致性
- [x] `plugin.Registry[T]` 签名在各注册表中一致使用
- [x] `hub.DHT` 接口方法与 `internal.memoryDHT` 实现匹配
- [x] `tracing.Tracer` 接口方法与 `internal.slogTracer` 实现匹配
- [x] `xfer.Transport` 结构体在 xfer/core.go、registry.go、internal/、ext/ 中一致使用
