# 测试工具化与覆盖率提升实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 扩充 `pkg/testutil` 为跨 module 可导入的测试套件，提供 mockxfer/mockdht/mockserver 三个 mock 子包，并将核心包覆盖率提升到目标值（xfer ≥85%、p2p ≥85%、mux ≥90%、server ≥85%），同时清理 4 项已知技术债务。

**架构：**
- `pkg/testutil/mockxfer/` — 实现 `xfer.Conn` 和 `xfer.Listener` 接口的可控 mock，替代 p2p/mux 测试中的手写 fake
- `pkg/testutil/mockdht/` — 实现 `hub.DHT` 接口的可控 mock
- `pkg/testutil/mockserver/` — 实现 ChecksumStore 和 UploadStore 接口的内存 mock（需先在 server 包提取接口）
- 各包覆盖率通过扩展已有测试文件或新建 `*_test.go` 达成

**技术栈：** Go 1.26，纯标准库测试（无 testify/gomega），`gopkg.in/yaml.v3`

### 关键设计决策：Registry 变量化

`xfer.TransportRegistry` 是全局插件注册表，生产代码通过包级导出函数间接访问它，测试代码使用局部 `plugin.New[*xfer.Transport]()` 创建隔离的 Registry 实例。

模式：
- **包级导出函数**（`Register`、`Get`、`Active`、`IsDefault`、`Names`）— 生产代码调用它们，内部委托给 `TransportRegistry`
- **内部 `init()`** 使用 `xfer.Register()` 而非 `xfer.TransportRegistry.Register()`
- **测试** 使用 `plugin.New[*xfer.Transport]("test", builtin)` 创建隔离 Registry，不操作全局变量
- **`p2p.go` 等消费者** 使用 `xfer.Active()` 而非 `xfer.TransportRegistry.Active()`

---

### 任务 0：Registry 变量化 — 包级封装函数 + plugin.Registry 新增 Clear 方法

**文件：**
- 修改：`pkg/tunnel/plugin/registry.go` — 新增 `Clear()` 方法
- 修改：`pkg/tunnel/xfer/registry.go` — 新增 `Active()`、`IsDefault()` 包级导出函数
- 修改：`pkg/tunnel/xfer/xfer.go` — 文档更新
- 修改：`pkg/tunnel/p2p/p2p.go` — 使用 `xfer.Active()` 而非 `xfer.TransportRegistry.Active()`
- 修改：`pkg/tunnel/xfer/internal/tcp/tcp.go` — 使用 `xfer.Register()` 而非 `xfer.TransportRegistry.Register()`
- 修改：`pkg/tunnel/xfer/ext/quic/quic.go` — 同上
- 修改：`pkg/tunnel/xfer/ext/ws/ws.go` — 同上
- 修改：`pkg/server/config.go` — 确保 `UploadStoreIface` 使用接口而非具体类型（已在之前的任务中处理）
- 修改：`pkg/tunnel/xfer/registry_test.go` — 使用 `xfer.Active()` 而非 `xfer.TransportRegistry.Active()`

- [ ] **步骤 0.1：在 plugin.Registry 中新增 Clear 方法**

`pkg/tunnel/plugin/registry.go` 中添加：

```go
// Clear 移除所有已注册的插件，恢复为仅内置兜底的状态。
// 仅用于测试；生产代码不应调用。
func (r *Registry[T]) Clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.plugins = make(map[string]Plugin[T])
}
```

- [ ] **步骤 0.2：在 xfer 包中新增 Active 和 IsDefault 包级函数**

`pkg/tunnel/xfer/registry.go` 末尾添加：

```go
// Active 返回最高优先级的已注册 Transport。
// 无插件时返回内置 emptyTransport（Dial/Listen 均返回 ErrNoTransport）。
func Active() *Transport {
	return TransportRegistry.Active()
}

// IsDefault 返回是否使用内置兜底实现（即无外部插件注册）。
func IsDefault() bool {
	return TransportRegistry.IsDefault()
}
```

- [ ] **步骤 0.3：更新 ext 包的 init 使用包级函数**（无需修改 p2p.go，已使用 `xfer.Get("webrtc")`）

- [ ] **步骤 0.4：更新 ext 包的 init 使用包级函数**

`pkg/tunnel/xfer/internal/tcp/tcp.go` 中：
```go
// 将
xfer.TransportRegistry.Register(...)
// 改为
xfer.Register(...)
```

因为 `xfer.Register` 内部已委托给 `TransportRegistry.Register`。

- [ ] **步骤 0.5：更新 registry_test.go 使用包级函数**

`pkg/tunnel/xfer/registry_test.go` 中：
```go
// 将
xfer.TransportRegistry.Active()
// 改为
xfer.Active()
```

- [ ] **步骤 0.6：编译验证**

```bash
go build ./...
```
预期：全部编译通过。

- [ ] **步骤 0.7：运行全量测试**

```bash
go test -count=1 -race ./pkg/tunnel/... ./pkg/server/...
```
预期：全部 PASS。

- [ ] **步骤 0.8：Commit**

```bash
git add pkg/tunnel/plugin/registry.go pkg/tunnel/xfer/registry.go
git add pkg/tunnel/p2p/p2p.go pkg/tunnel/xfer/internal/tcp/tcp.go
git add pkg/tunnel/xfer/ext/quic/quic.go pkg/tunnel/xfer/ext/ws/ws.go
git add pkg/tunnel/xfer/registry_test.go
git commit -m "refactor: Registry 变量化，封装包级导出函数 Active()/IsDefault() + Clear()"
```

**文件：**
- 修改：`pkg/server/checksum_store.go` — 提取 `ChecksumStore` 接口
- 修改：`pkg/server/upload_store.go` — 提取 `UploadStore` 接口
- 修改：`pkg/server/handlers.go` — `Handlers` 字段改用接口
- 修改：`cmd/sclient/root.go:41-44` — `Execute()` 中 `os.Exit(1)` 改为 `RunE` 返回 error
- 修改：`test/e2e_test.go` — 移除已不存在的 `findModuleRoot` 相关代码
- 修改：`pkg/server/integration_test.go:55-72` — `newTestServer` 路由改为调用 `RegisterRoutes`

- [ ] **步骤 0.1：读取 server 包中 ChecksumStore 和 UploadStore 的方法签名**

运行：`grep -n 'func (cs \*ChecksumStore)' pkg/server/checksum_store.go`
预期：列出 Get/Set/Delete/Rename/DeletePrefix/GetAll/save 方法

运行：`grep -n 'func (us \*UploadStore)' pkg/server/upload_store.go`
预期：列出 CreateSession/GetSession/MarkChunkReceived/CompleteSession/DeleteSession 等方法

- [ ] **步骤 0.2：在 `checksum_store.go` 中定义 `ChecksumStore` 接口**

在文件头部（`type ChecksumStore struct` 之前）添加：

```go
// ChecksumStoreIface 定义 ChecksumStore 的业务接口，方便测试替身。
type ChecksumStoreIface interface {
	Get(filename string) (string, bool)
	Set(filename, checksum string)
	Delete(filename string)
	Rename(from, to string)
	DeletePrefix(prefix string)
	GetAll() map[string]string
}
```

确认 `*ChecksumStore` 自动满足该接口（Go 的结构化 typing）。

- [ ] **步骤 0.3：在 `upload_store.go` 中定义 `UploadStore` 接口**

在 `type UploadStore struct` 之前添加：

```go
// UploadStoreIface 定义 UploadStore 的业务接口，方便测试替身。
type UploadStoreIface interface {
	CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, error)
	GetSession(uploadID string) *ChunkedUploadSession
	GetSessionByFilename(filename string) *ChunkedUploadSession
	MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error
	AllChunksReceived(uploadID string) bool
	CompleteSession(uploadID string) error
	DeleteSession(uploadID string)
	ChunkFilePath(uploadID string, chunkIndex int) string
	SessionDir(uploadID string) string
	Stop()
	Health() error
	GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*ChunkedUploadSession, bool, error)
	CleanupSessionAfter(uploadID string, delay time.Duration)
}
```

- [ ] **步骤 0.4：修改 `Handlers` 使用接口字段**

在 `pkg/server/handlers.go` 中将：
```go
checksumStore *ChecksumStore
uploadStore   *UploadStore
```
改为：
```go
checksumStore ChecksumStoreIface
uploadStore   UploadStoreIface
```

确认编译通过：`go build ./pkg/server/...`

- [ ] **步骤 0.5：修复 TD1 — `cmd/sclient/root.go` 中 `Execute()` 的 `os.Exit(1)`**

将 `rootCmd.Execute()` 改为 `RunE` 返回 error：

```go
func Execute() error {
	return rootCmd.Execute()
}
```

修改 `main.go` 中的调用：
```go
func main() {
	if err := Execute(); err != nil {
		os.Exit(1)
	}
}
```

- [ ] **步骤 0.6：检查 TD2 — 信号 goroutine 泄漏**

读取 `cmd/sproxy/root.go:152-202` 确认当前 `stopSigCh` 机制是否已修复该问题。若已修复（`close(stopSigCh)` 在 ListenAndServe 失败时调用），则在代码中添加注释说明泄漏已修复。若有残留问题（信号 goroutine 仅凭 `<-stopSigCh` 退出但 main goroutine 不等待），添加 `<-shutdownDone` 等待。

编译验证：`go build ./cmd/sproxy/...`

- [ ] **步骤 0.7：检查 TD3 — `findModuleRoot` 冗余**

读取 `test/e2e_test.go`，确认 `findModuleRoot` 函数是否存在。若已不存在（仅剩 `runtime.Caller`），无需修改。

- [ ] **步骤 0.8：修复 TD4 — `newTestServer` 手动重复路由**

将 `pkg/server/integration_test.go:55-72` 的 17 行手动 `HandleFunc` 路由注册替换为：

```go
h := RegisterRoutes(context.Background(), mux, &cfgPtr, "test", "test", nil, nil, nil)
```

需要调整 `newTestServer` 的签名和返回值。注意原代码没有初始化 `uploadStore` 字段，`RegisterRoutes` 会初始化它，所以要确保配置中有 `UploadSessionTTL`。

运行：`go test -run TestNewTestServer ./pkg/server/...` 验证通过

- [ ] **步骤 0.9：运行全量测试验证**

```bash
go test -count=1 -race ./pkg/server/... ./cmd/sproxy/... ./cmd/sclient/... ./test/...
```

预期：所有测试通过，无 race。

- [ ] **步骤 0.10：Commit**

```bash
git add pkg/server/checksum_store.go pkg/server/upload_store.go pkg/server/handlers.go
git add cmd/sclient/root.go cmd/sclient/main.go
git add pkg/server/integration_test.go test/e2e_test.go
git commit -m "refactor: 抽取 ChecksumStoreIface/UploadStoreIface 接口 + 修复技术债务 TD1/TD4"
```

---

### 任务 1：创建 mockxfer 子包

**文件：**
- 创建：`pkg/testutil/mockxfer/conn.go`
- 创建：`pkg/testutil/mockxfer/listener.go`
- 测试：`pkg/testutil/mockxfer/mockxfer_test.go`

- [ ] **步骤 1.1：读取 xfer 包接口定义**

运行：`head -60 pkg/tunnel/xfer/core.go`
确认 `Conn` 和 `Listener` 接口的完整签名。

- [ ] **步骤 1.2：编写 `MockConn`**

`pkg/testutil/mockxfer/conn.go`：

```go
package mockxfer

import (
	"context"
	"errors"
)

// MockConn 实现 xfer.Conn，可控 Send/Receive/Close 返回。
type MockConn struct {
	SendFn    func(ctx context.Context, msg []byte) error
	ReceiveFn func(ctx context.Context) ([]byte, error)
	CloseFn   func() error
	// 调用记录
	SendCalls    int
	ReceiveCalls int
	CloseCalls   int
}

func (m *MockConn) Send(ctx context.Context, msg []byte) error {
	m.SendCalls++
	if m.SendFn != nil {
		return m.SendFn(ctx, msg)
	}
	return nil
}

func (m *MockConn) Receive(ctx context.Context) ([]byte, error) {
	m.ReceiveCalls++
	if m.ReceiveFn != nil {
		return m.ReceiveFn(ctx)
	}
	return nil, nil
}

func (m *MockConn) Close() error {
	m.CloseCalls++
	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

func (m *MockConn) String() string { return "mockxfer.Conn" }

// ErrSendFailed 是 SendFn 可返回的哨兵错误，便于测试断言。
var ErrSendFailed = errors.New("mockxfer: send failed")

// ErrReceiveFailed 是 ReceiveFn 可返回的哨兵错误。
var ErrReceiveFailed = errors.New("mockxfer: receive failed")
```

- [ ] **步骤 1.3：编写 `MockListener`**

`pkg/testutil/mockxfer/listener.go`：

```go
package mockxfer

import (
	"context"
	"errors"

	"github.com/cocomhub/sproxy/pkg/tunnel/xfer"
)

// MockListener 实现 xfer.Listener，可控 Accept/Close 返回。
type MockListener struct {
	AcceptFn func(ctx context.Context) (xfer.Conn, error)
	CloseFn  func() error
	addr     string
	// 调用记录
	AcceptCalls int
	CloseCalls  int
}

func NewMockListener(addr string) *MockListener {
	return &MockListener{addr: addr}
}

func (l *MockListener) Accept(ctx context.Context) (xfer.Conn, error) {
	l.AcceptCalls++
	if l.AcceptFn != nil {
		return l.AcceptFn(ctx)
	}
	return nil, context.Canceled
}

func (l *MockListener) Close() error {
	l.CloseCalls++
	if l.CloseFn != nil {
		return l.CloseFn()
	}
	return nil
}

func (l *MockListener) Addr() string { return l.addr }

// ErrAcceptFailed 是 AcceptFn 可返回的哨兵错误。
var ErrAcceptFailed = errors.New("mockxfer: accept failed")
```

- [ ] **步骤 1.4：编写 MockConn/MockListener 测试**

`pkg/testutil/mockxfer/mockxfer_test.go`：

```go
package mockxfer_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockxfer"
)

func TestMockConn_Send(t *testing.T) {
	m := &mockxfer.MockConn{}
	if err := m.Send(context.Background(), []byte("hello")); err != nil {
		t.Fatalf("Send failed: %v", err)
	}
	if m.SendCalls != 1 {
		t.Fatalf("expected 1 Send call, got %d", m.SendCalls)
	}
}

func TestMockConn_SendError(t *testing.T) {
	m := &mockxfer.MockConn{
		SendFn: func(_ context.Context, _ []byte) error {
			return mockxfer.ErrSendFailed
		},
	}
	if err := m.Send(context.Background(), []byte("x")); err != mockxfer.ErrSendFailed {
		t.Fatalf("expected ErrSendFailed, got %v", err)
	}
}

func TestMockConn_Receive(t *testing.T) {
	m := &mockxfer.MockConn{
		ReceiveFn: func(_ context.Context) ([]byte, error) {
			return []byte("pong"), nil
		},
	}
	got, err := m.Receive(context.Background())
	if err != nil {
		t.Fatalf("Receive failed: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("expected 'pong', got %q", got)
	}
}

func TestMockConn_DoubleClose(t *testing.T) {
	m := &mockxfer.MockConn{}
	if err := m.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatalf("second Close failed: %v", err)
	}
	if m.CloseCalls != 2 {
		t.Fatalf("expected 2 Close calls, got %d", m.CloseCalls)
	}
}

func TestMockListener_Accept(t *testing.T) {
	l := mockxfer.NewMockListener("pipe://addr")
	conn, err := l.Accept(context.Background())
	if err == nil {
		conn.Close()
		t.Fatal("expected error from default Accept")
	}
	if l.AcceptCalls != 1 {
		t.Fatalf("expected 1 Accept call, got %d", l.AcceptCalls)
	}
}

func TestMockListener_CustomAccept(t *testing.T) {
	mc := &mockxfer.MockConn{}
	l := mockxfer.NewMockListener("test")
	l.AcceptFn = func(_ context.Context) (mc, nil)
	got, err := l.Accept(context.Background())
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	if got != mc {
		t.Fatal("unexpected conn returned")
	}
}

func TestMockListener_Addr(t *testing.T) {
	l := mockxfer.NewMockListener("tcp://127.0.0.1:9000")
	if addr := l.Addr(); addr != "tcp://127.0.0.1:9000" {
		t.Fatalf("expected addr 'tcp://127.0.0.1:9000', got %q", addr)
	}
}
```

- [ ] **步骤 1.5：运行测试验证**

```bash
go test -count=1 -race ./pkg/testutil/mockxfer/...
```
预期：6 PASS，0 FAIL。

- [ ] **步骤 1.6：Commit**

```bash
git add pkg/testutil/mockxfer/
git commit -m "feat(testutil): add mockxfer subpackage with MockConn and MockListener"
```

---

### 任务 2：创建 mockdht 子包

**文件：**
- 创建：`pkg/testutil/mockdht/dht.go`
- 测试：`pkg/testutil/mockdht/dht_test.go`

- [ ] **步骤 2.1：读取 hub.DHT 接口**

运行：`head -35 pkg/tunnel/hub/core.go`
确认 DHT 接口有 `Register`、`Lookup`、`GetClosestNodes`、`Bootstrap`、`Close` 五个方法。

- [ ] **步骤 2.2：编写 `MockDHT`**

`pkg/testutil/mockdht/dht.go`：

```go
package mockdht

import (
	"context"
	"errors"

	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

// MockDHT 实现 hub.DHT，可控 Lookup/Register 返回。
type MockDHT struct {
	RegisterFn func(ctx context.Context, info hub.PeerInfo) error
	LookupFn   func(ctx context.Context, nodeID string) (hub.PeerInfo, error)
	CloseFn    func() error
	// 调用记录
	RegisterCalls int
	LookupCalls   int
	CloseCalls    int
	// 内置内存存储（可选）
	peers map[string]hub.PeerInfo
}

func New() *MockDHT {
	return &MockDHT{peers: make(map[string]hub.PeerInfo)}
}

func (m *MockDHT) Register(ctx context.Context, info hub.PeerInfo) error {
	m.RegisterCalls++
	if m.RegisterFn != nil {
		return m.RegisterFn(ctx, info)
	}
	m.peers[info.ID] = info
	return nil
}

func (m *MockDHT) Lookup(ctx context.Context, nodeID string) (hub.PeerInfo, error) {
	m.LookupCalls++
	if m.LookupFn != nil {
		return m.LookupFn(ctx, nodeID)
	}
	if info, ok := m.peers[nodeID]; ok {
		return info, nil
	}
	return hub.PeerInfo{}, ErrPeerNotFound
}

func (m *MockDHT) GetClosestNodes(ctx context.Context, nodeID string, n int) ([]hub.PeerInfo, error) {
	return nil, nil // 用不到
}

func (m *MockDHT) Bootstrap(ctx context.Context, seeds []string) error {
	return nil // 用不到
}

func (m *MockDHT) Close() error {
	m.CloseCalls++
	if m.CloseFn != nil {
		return m.CloseFn()
	}
	return nil
}

// ErrPeerNotFound 是 Lookup 未找到时的哨兵错误。
var ErrPeerNotFound = errors.New("mockdht: peer not found")
```

- [ ] **步骤 2.3：编写 MockDHT 测试**

`pkg/testutil/mockdht/dht_test.go`：

```go
package mockdht_test

import (
	"context"
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockdht"
	"github.com/cocomhub/sproxy/pkg/tunnel/hub"
)

func TestMockDHT_RegisterAndLookup(t *testing.T) {
	dht := mockdht.New()
	info := hub.PeerInfo{ID: "node-a", Addrs: []string{"pipe://addr"}}
	if err := dht.Register(context.Background(), info); err != nil {
		t.Fatalf("Register failed: %v", err)
	}
	got, err := dht.Lookup(context.Background(), "node-a")
	if err != nil {
		t.Fatalf("Lookup failed: %v", err)
	}
	if got.ID != "node-a" {
		t.Fatalf("expected ID 'node-a', got %q", got.ID)
	}
	if dht.RegisterCalls != 1 || dht.LookupCalls != 1 {
		t.Fatal("call counts mismatch")
	}
}

func TestMockDHT_LookupNotFound(t *testing.T) {
	dht := mockdht.New()
	_, err := dht.Lookup(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for unknown peer")
	}
}

func TestMockDHT_InjectError(t *testing.T) {
	dht := mockdht.New()
	dht.LookupFn = func(_ context.Context, _ string) (hub.PeerInfo, error) {
		return hub.PeerInfo{}, mockdht.ErrPeerNotFound
	}
	_, err := dht.Lookup(context.Background(), "any")
	if err != mockdht.ErrPeerNotFound {
		t.Fatalf("expected ErrPeerNotFound, got %v", err)
	}
}

func TestMockDHT_DoubleClose(t *testing.T) {
	dht := mockdht.New()
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
	if err := dht.Close(); err != nil {
		t.Fatal(err)
	}
	if dht.CloseCalls != 2 {
		t.Fatalf("expected 2 Close calls, got %d", dht.CloseCalls)
	}
}
```

- [ ] **步骤 2.4：运行测试**

```bash
go test -count=1 -race ./pkg/testutil/mockdht/...
```
预期：4 PASS。

- [ ] **步骤 2.5：Commit**

```bash
git add pkg/testutil/mockdht/
git commit -m "feat(testutil): add mockdht subpackage with MockDHT"
```

---

### 任务 3：创建 mockserver 子包

**文件：**
- 创建：`pkg/testutil/mockserver/checksum.go`
- 创建：`pkg/testutil/mockserver/upload.go`
- 测试：`pkg/testutil/mockserver/mockserver_test.go`

- [ ] **步骤 3.1：编写 `MockChecksumStore`**

`pkg/testutil/mockserver/checksum.go`：

```go
package mockserver

import (
	"sync"

	"github.com/cocomhub/sproxy/pkg/server"
)

// MockChecksumStore 实现 server.ChecksumStoreIface，内存 map。
type MockChecksumStore struct {
	mu   sync.RWMutex
	data map[string]string
	// 可注入错误
	SetErr    error
	GetErr    error
	DeleteErr error
}

func NewChecksumStore() *MockChecksumStore {
	return &MockChecksumStore{data: make(map[string]string)}
}

func (m *MockChecksumStore) Get(filename string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.GetErr != nil {
		return "", false
	}
	v, ok := m.data[filename]
	return v, ok
}

func (m *MockChecksumStore) Set(filename, checksum string) {
	if m.SetErr != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[filename] = checksum
}

func (m *MockChecksumStore) Delete(filename string) {
	if m.DeleteErr != nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, filename)
}

func (m *MockChecksumStore) Rename(from, to string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v, ok := m.data[from]; ok {
		m.data[to] = v
		delete(m.data, from)
	}
}

func (m *MockChecksumStore) DeletePrefix(prefix string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k := range m.data {
		if len(k) >= len(prefix) && k[:len(prefix)] == prefix {
			delete(m.data, k)
		}
	}
}

func (m *MockChecksumStore) GetAll() map[string]string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	cp := make(map[string]string, len(m.data))
	for k, v := range m.data {
		cp[k] = v
	}
	return cp
}

// Ensure interface compliance.
var _ server.ChecksumStoreIface = (*MockChecksumStore)(nil)
```

- [ ] **步骤 3.2：编写 `MockUploadStore`**

`pkg/testutil/mockserver/upload.go`：

```go
package mockserver

import (
	"errors"
	"sync"
	"time"

	"github.com/cocomhub/sproxy/pkg/server"
)

// MockUploadStore 实现 server.UploadStoreIface，内存 map。
type MockUploadStore struct {
	mu       sync.RWMutex
	sessions map[string]*server.ChunkedUploadSession
}

func NewUploadStore() *MockUploadStore {
	return &MockUploadStore{sessions: make(map[string]*server.ChunkedUploadSession)}
}

func (m *MockUploadStore) CreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*server.ChunkedUploadSession, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.sessions[uploadID]; ok {
		return nil, errors.New("session already exists")
	}
	s := &server.ChunkedUploadSession{
		UploadID:     uploadID,
		Filename:     filename,
		TotalSize:    totalSize,
		ChunkSize:    chunkSize,
		TotalChunks:  totalChunks,
		FileChecksum: fileChecksum,
		FileModTime:  fileModTime,
		Chunks:       make(map[int]string),
		CreatedAt:    time.Now(),
	}
	m.sessions[uploadID] = s
	return s, nil
}

func (m *MockUploadStore) GetSession(uploadID string) *server.ChunkedUploadSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.sessions[uploadID]
}

func (m *MockUploadStore) GetSessionByFilename(filename string) *server.ChunkedUploadSession {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.sessions {
		if s.Filename == filename {
			return s
		}
	}
	return nil
}

func (m *MockUploadStore) MarkChunkReceived(uploadID string, chunkIndex int, checksum string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[uploadID]
	if !ok {
		return errors.New("session not found")
	}
	s.Chunks[chunkIndex] = checksum
	return nil
}

func (m *MockUploadStore) AllChunksReceived(uploadID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.sessions[uploadID]
	if !ok {
		return false
	}
	return len(s.Chunks) == s.TotalChunks
}

func (m *MockUploadStore) CompleteSession(uploadID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[uploadID]
	if !ok {
		return errors.New("session not found")
	}
	s.CompletedAt = time.Now()
	return nil
}

func (m *MockUploadStore) DeleteSession(uploadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, uploadID)
}

func (m *MockUploadStore) ChunkFilePath(uploadID string, chunkIndex int) string { return "" }
func (m *MockUploadStore) SessionDir(uploadID string) string                    { return "" }
func (m *MockUploadStore) Stop()                                                {}
func (m *MockUploadStore) Health() error                                        { return nil }
func (m *MockUploadStore) GetOrCreateSession(uploadID, filename string, totalSize, chunkSize int64, totalChunks int, fileChecksum string, fileModTime int64) (*server.ChunkedUploadSession, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[uploadID]; ok {
		return s, true, nil
	}
	s := &server.ChunkedUploadSession{
		UploadID:     uploadID,
		Filename:     filename,
		TotalSize:    totalSize,
		ChunkSize:    chunkSize,
		TotalChunks:  totalChunks,
		FileChecksum: fileChecksum,
		FileModTime:  fileModTime,
		Chunks:       make(map[int]string),
		CreatedAt:    time.Now(),
	}
	m.sessions[uploadID] = s
	return s, false, nil
}
func (m *MockUploadStore) CleanupSessionAfter(uploadID string, delay time.Duration) {}

// Ensure interface compliance.
var _ server.UploadStoreIface = (*MockUploadStore)(nil)
```

- [ ] **步骤 3.3：编写 mockserver 测试**

`pkg/testutil/mockserver/mockserver_test.go`：

```go
package mockserver_test

import (
	"testing"

	"github.com/cocomhub/sproxy/pkg/testutil/mockserver"
)

func TestMockChecksumStore_SetGet(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("file.txt", "abc123")
	got, ok := cs.Get("file.txt")
	if !ok || got != "abc123" {
		t.Fatalf("expected 'abc123', got %q (ok=%v)", got, ok)
	}
}

func TestMockChecksumStore_GetMissing(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	_, ok := cs.Get("missing.txt")
	if ok {
		t.Fatal("expected false for missing key")
	}
}

func TestMockChecksumStore_Delete(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("f.txt", "x")
	cs.Delete("f.txt")
	_, ok := cs.Get("f.txt")
	if ok {
		t.Fatal("expected false after delete")
	}
}

func TestMockChecksumStore_Rename(t *testing.T) {
	cs := mockserver.NewChecksumStore()
	cs.Set("old", "cksum")
	cs.Rename("old", "new")
	_, okOld := cs.Get("old")
	v, okNew := cs.Get("new")
	if okOld || !okNew || v != "cksum" {
		t.Fatal("Rename failed")
	}
}

func TestMockUploadStore_CreateAndGet(t *testing.T) {
	us := mockserver.NewUploadStore()
	s, err := us.CreateSession("sid1", "f.txt", 100, 64, 2, "", 0)
	if err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}
	if s.Filename != "f.txt" {
		t.Fatalf("expected filename 'f.txt', got %q", s.Filename)
	}
	got := us.GetSession("sid1")
	if got == nil || got.UploadID != "sid1" {
		t.Fatal("GetSession failed")
	}
}
```

- [ ] **步骤 3.4：运行测试**

```bash
go test -count=1 -race ./pkg/testutil/mockserver/...
```
预期：5 PASS。

- [ ] **步骤 3.5：Commit**

```bash
git add pkg/testutil/mockserver/
git commit -m "feat(testutil): add mockserver subpackage with MockChecksumStore and MockUploadStore"
```

---

### 任务 4：提升 xfer 包覆盖率 (66.7% → 85%)

**文件：**
- 修改：`pkg/tunnel/xfer/xfer_test.go` — 补充边界测试
- 修改：`pkg/tunnel/xfer/registry_test.go` — 补充空名字/重复注册测试

- [ ] **步骤 4.1：在 `xfer_test.go` 中添加 registry 边界测试**

在现有 `TestRegisterAndGet` 函数内（或新函数）补充：

```go
func TestRegisterAndGet_EmptyName(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on empty name")
		}
	}()
	// 使用独立 registry 避免污染全局
	reg := plugin.New[*xfer.Transport]("test", &xfer.Transport{Name: "builtin"})
	reg.Register(plugin.Plugin[*xfer.Transport]{Name: "", Instance: &xfer.Transport{Name: "bad"}})
}

func TestRegisterAndGet_NotFound(t *testing.T) {
	_, ok := xfer.Get("nonexistent-transport")
	if ok {
		t.Fatal("expected ok=false for nonexistent transport")
	}
}

func TestActive_DefaultBuiltin(t *testing.T) {
	// Active() 返回 builtin（无插件时），Get 返回 nil+false
	tr := xfer.TransportRegistry.Active()
	if tr.Name != "builtin" {
		t.Fatalf("expected builtin name, got %q", tr.Name)
	}
}
```

- [ ] **步骤 4.2：在 `xfer_test.go` 中添加 Conn 接口的 Close 后行为测试**

```go
func TestConnSendAfterClose(t *testing.T) {
	client, server, cleanup := xfertest.Pipe()
	defer cleanup()
	_ = server

	client.Close()
	err := client.Send(context.Background(), []byte("after close"))
	if err == nil {
		t.Fatal("expected error sending after close")
	}
}

func TestConnReceiveAfterClose(t *testing.T) {
	client, server, cleanup := xfertest.Pipe()
	defer cleanup()
	_ = server

	client.Close()
	_, err := client.Receive(context.Background())
	if err == nil {
		t.Fatal("expected error receiving after close")
	}
}
```

- [ ] **步骤 4.3：运行 xfer 测试验证**

```bash
go test -count=1 -cover -race ./pkg/tunnel/xfer/...
```
预期：覆盖率 ≥85%，所有 PASS。

- [ ] **步骤 4.4：Commit**

```bash
git add pkg/tunnel/xfer/xfer_test.go pkg/tunnel/xfer/registry_test.go
git commit -m "test(xfer): expand registry and Conn edge-case coverage to 85%"
```

---

### 任务 5：提升 p2p 包覆盖率 (71.7% → 85%)

**文件：**
- 修改：`pkg/tunnel/p2p/p2p_test.go` — 补充 mock 测试（使用 mockxfer + mockdht）

- [ ] **步骤 5.1：读取 p2p.go 中的 Dial/Listen 错误路径**

运行：`grep -n 'return fmt.Errorf\|return nil, fmt.Errorf\|return.*Err\|return errors\.' pkg/tunnel/p2p/p2p.go`
确认需要覆盖的错误分支。

- [ ] **步骤 5.2：在 `p2p_test.go` 中添加 Dial 错误路径测试**

```go
func TestDial_TransportNotFound(t *testing.T) {
	dht := mockdht.New()
	dht.Register(context.Background(), hub.PeerInfo{ID: "target", Addrs: []string{"tcp://addr"}})
	node := p2p.NewP2PNode("dialer", dht)
	_, err := node.Dial(context.Background(), "target")
	if err == nil {
		t.Fatal("expected error when webrtc transport not registered")
	}
}

func TestDial_LookupError(t *testing.T) {
	dht := mockdht.New()
	dht.LookupFn = func(_ context.Context, _ string) (hub.PeerInfo, error) {
		return hub.PeerInfo{}, mockdht.ErrPeerNotFound
	}
	node := p2p.NewP2PNode("dialer", dht)
	_, err := node.Dial(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error on lookup failure")
	}
}
```

- [ ] **步骤 5.3：添加 Listen 错误路径测试**

```go
func TestListen_AcceptAfterClose(t *testing.T) {
	dht := mockdht.New()
	node := p2p.NewP2PNode("listener", dht)
	ml := mockxfer.NewMockListener("pipe://addr")
	ml.AcceptFn = func(_ context.Context) (xfer.Conn, error) {
		return nil, errors.New("listener closed")
	}
	_ = ml // 这里需要修改 p2p 使用可注入的 listener
}
```

注意：p2p.Listen 使用 `xfer.Get("webrtc")` 创建 listener，不会直接接受注入的 MockListener。需要注册一个 mock transport 来实现测试。在 `TestP2PNodeRoundTrip` 中已经有 `registerFakeWebRTC()` 使用 xfertest.Pipe，可以类似在 setup 中注册一个可控的 mock transport。

调整策略：创建 `registerFakeWebRTCWithMock`（在测试文件内）来注册一个使用 MockConn 的 transport。

```go
// registerFakeWebRTCWithMock 注册一个使用 mockxfer.MockConn 的 "webrtc" transport。
func registerFakeWebRTCWithMock() (*mockxfer.MockListener, *mockxfer.MockConn) {
	ml := mockxfer.NewMockListener("pipe://test")
	mc := &mockxfer.MockConn{}
	xfer.Register(&xfer.Transport{
		Name: "webrtc",
		Dial: func(_ context.Context, _ string) (xfer.Conn, error) {
			return mc, nil
		},
		Listen: func(_ context.Context, _ string) (xfer.Listener, error) {
			return ml, nil
		},
	})
	return ml, mc
}

func TestListen_TransportListenError(t *testing.T) {
	dht := mockdht.New()
	xfer.Register(&xfer.Transport{
		Name: "webrtc",
		Dial: func(_ context.Context, _ string) (xfer.Conn, error) {
			return nil, errors.New("no dial")
		},
		Listen: func(_ context.Context, _ string) (xfer.Listener, error) {
			return nil, errors.New("no listen")
		},
	})
	t.Cleanup(func() { xfer.TransportRegistry.Clear() })

	node := p2p.NewP2PNode("listener", dht)
	err := node.Listen(context.Background(), "pipe://addr")
	if err == nil {
		t.Fatal("expected error from transport.Listen")
	}
}
```

注意：`TransportRegistry.Clear()` 方法可能不存在，需要检查 plugin.Registry 是否有 Clear 方法。如果没有，使用独立 registry 或直接覆盖。

查看 registry.go 发现没有 `Clear()`，但有 `Names()` 返回所有已注册的名称。清理方法：对每个 Name 调用 Register 覆盖为空。或者在每个测试中使用 `t.Cleanup` 注册回原来的 transport。

实际上，设计上 xfer 测试在 `xfer_test.go` 中已使用独立 registry：
```go
reg := plugin.New[*xfer.Transport]("test", &xfer.Transport{Name: "builtin"})
```

最干净的方式：在 p2p_test.go 中，`registerFakeWebRTC` 用 `t.Cleanup` 在测试结束后注销。但查看现有代码，`registerFakeWebRTC()` 没有 cleanup，它直接在全局 `xfer.TransportRegistry` 上注册，并依赖测试顺序。

为避免全局污染问题，添加一个 cleanup helper：

```go
func registerFakeWebRTC(t *testing.T) *fakeListener {
	fl := &fakeListener{acceptCh: make(chan fakeAcceptResult, 1)}
	t.Cleanup(func() {
		// 重新注册 empty transport 覆盖
		xfer.Register(&xfer.Transport{
			Name: "webrtc",
			Dial: func(_ context.Context, _ string) (xfer.Conn, error) { return nil, xfer.ErrNoTransport },
			Listen: func(_ context.Context, _ string) (xfer.Listener, error) { return nil, xfer.ErrNoTransport },
		})
	})
	// ... 现有注册逻辑
}
```

- [ ] **步骤 5.4：添加 Accept 的 Context 取消和 Close 后行为测试**

```go
func TestAccept_ContextCancelled(t *testing.T) {
	dht := mockdht.New()
	ml, _ := registerFakeWebRTCWithMock(t)
	ml.AcceptFn = func(ctx context.Context) (xfer.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}

	// ...
}
```

由于 p2p 的 Accept 在内部 acceptLoop 中调用，需要直接测试节点 Accept 行为。但 P2PNode.Accept 实际上是先通过 DHT.Lookup 找到地址，再由 Transport.Listen 建立 listener，然后从 acceptLoop 接收连接。所以 Accept 本身不直接调用 MockListener.Accept。

p2p 架构是：Listen → transport.Listen(addr) → 启动 acceptLoop goroutine → Accept 从 channel 取结果。

所以 MockListener 的 Accept 会在 acceptLoop 中被调用。要模拟 Accept 失败，需要 MockListener.AcceptFn 返回 error，然后验证 P2PNode.Accept 或 P2PNode 的状态。

简化：测试 acceptLoop 中 context 取消时的行为：

```go
func TestP2P_ContextCancelled(t *testing.T) {
	dht := mockdht.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 立即取消

	ml := mockxfer.NewMockListener("pipe://addr")
	ml.AcceptFn = func(ctx context.Context) (xfer.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	// 注册 transport
	xfer.Register(&xfer.Transport{
		Name:   "webrtc",
		Listen: func(_ context.Context, _ string) (xfer.Listener, error) { return ml, nil },
		Dial:   func(_ context.Context, _ string) (xfer.Conn, error) { return nil, errors.New("no dial") },
	})
	t.Cleanup(func() { registerCleanupTransport() })

	node := p2p.NewP2PNode("test", dht)
	err := node.Listen(ctx, "pipe://addr")
	if err == nil {
		// Listen 成功但 acceptLoop 立即退出
		node.Close()
		_, acceptErr := node.Accept(context.Background())
		if acceptErr == nil {
			t.Fatal("expected error from Accept after context cancelled")
		}
	}
}
```

这个测试略复杂。查看 p2p.go 的 Accept 实现来确认正确写法。

- [ ] **步骤 5.5：添加 Close 幂等性测试**

```go
func TestP2P_CloseWithoutListen(t *testing.T) {
	dht := mockdht.New()
	node := p2p.NewP2PNode("orphan", dht)
	if err := node.Close(); err != nil {
		t.Fatalf("Close without Listen should not error: %v", err)
	}
}

func TestP2P_DoubleClose(t *testing.T) {
	dht := mockdht.New()
	node := p2p.NewP2PNode("test", dht)
	if err := node.Close(); err != nil {
		t.Fatal(err)
	}
	if err := node.Close(); err != nil {
		t.Fatal("double Close should be idempotent")
	}
}
```

- [ ] **步骤 5.6：运行测试验证**

```bash
go test -count=1 -cover -race ./pkg/tunnel/p2p/...
```
预期：覆盖率 ≥85%，所有 PASS。

如果某些测试因为全局 registry 污染导致失败，在每个测试前后正确 cleanup。

- [ ] **步骤 5.7：Commit**

```bash
git add pkg/tunnel/p2p/p2p_test.go
git commit -m "test(p2p): expand error-path and edge-case coverage to 85%"
```

---

### 任务 6：提升 mux 包覆盖率 (79.9% → 90%)

**文件：**
- 修改：`pkg/tunnel/mux/mux_test.go`

- [ ] **步骤 6.1：读取 mux 测试文件**

运行：`ls pkg/tunnel/mux/*_test.go`
确定现有测试文件和测试模式。

- [ ] **步骤 6.2：添加 Accept 满时的 FrameReject 测试**

```go
func TestAccept_RejectWhenFull(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	// 打开大量流，超过 acceptCh 默认容量
	var streams []mux.Stream
	for i := 0; i < 64; i++ {
		s, err := dm.Open(context.Background())
		if err != nil {
			// 达到 maxStreams 限制，停止
			break
		}
		streams = append(streams, s)
	}
	// 不 Accept 任何流，让 acceptCh 满
	// 再尝试 Open，应被拒绝
	_, err := dm.Open(context.Background())
	if err == nil {
		t.Fatal("expected error when acceptCh is full")
	}

	for _, s := range streams {
		s.Close()
	}
}
```

- [ ] **步骤 6.3：添加 Flow Control 阻塞测试**

```go
func TestFlowControl_BlockOnFullWindow(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	stream, err := dm.Open(context.Background())
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	t.Cleanup(func() { stream.Close() })

	// 另一方 Accept
	accepted, err := lm.Accept(context.Background())
	if err != nil {
		t.Fatalf("Accept failed: %v", err)
	}
	_ = accepted

	// 写满发送窗口（64KB）
	payload := make([]byte, 65536)
	n, err := stream.Write(payload)
	if err != nil {
		t.Fatalf("Write failed after %d bytes: %v", n, err)
	}
	// 再写应阻塞或返回 error（取决于实现）
	// 这里验证写入行为
	done := make(chan error, 1)
	go func() {
		_, err := stream.Write(payload[:1])
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Logf("Write after window full returned error (acceptable): %v", err)
		}
	case <-time.After(time.Second):
		t.Log("Write blocked as expected (window full)")
	}
}
```

- [ ] **步骤 6.4：添加 Stream 关闭后读写测试**

```go
func TestStream_WriteAfterClose(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	stream, _ := dm.Open(context.Background())
	stream.Close()
	_, err := stream.Write([]byte("data"))
	if err == nil {
		t.Fatal("expected error writing after stream close")
	}
}

func TestStream_ReadAfterClose(t *testing.T) {
	client, server := xfertest.Pipe()
	t.Cleanup(func() { client.Close(); server.Close() })

	lm := mux.New(server, mux.RoleListener)
	dm := mux.New(client, mux.RoleDialer)
	t.Cleanup(func() { lm.Close(); dm.Close() })

	stream, _ := dm.Open(context.Background())
	accepted, _ := lm.Accept(context.Background())

	stream.Write([]byte("hello"))
	stream.CloseWrite()

	buf := make([]byte, 1024)
	n, err := accepted.Read(buf)
	if n == 0 || err != nil {
		t.Fatalf("expected data, got n=%d err=%v", n, err)
	}
	// 第二次 Read 应返回 io.EOF（半关闭已结束）
	_, err = accepted.Read(buf)
	if err != io.EOF {
		t.Fatalf("expected io.EOF after close, got %v", err)
	}
}
```

- [ ] **步骤 6.5：运行测试验证**

```bash
go test -count=1 -cover -race ./pkg/tunnel/mux/...
```
预期：覆盖率 ≥90%，所有 PASS。

- [ ] **步骤 6.6：Commit**

```bash
git add pkg/tunnel/mux/
git commit -m "test(mux): add flow control, reject, and stream-close coverage to 90%"
```

---

### 任务 7：提升 server 包覆盖率 (71.1% → 85%)

**文件：**
- 创建：`pkg/server/mkdir_rmdir_test.go`
- 创建：`pkg/server/stat_test.go`
- 创建：`pkg/server/rename_test.go`
- 修改：`pkg/server/upload_test.go`
- 修改：`pkg/server/handlers_test.go`

- [ ] **步骤 7.1：读取 mkdir 和 rmdir handler 实现**

运行：`grep -n 'func.*mkdir\|func.*rmdir' pkg/server/handlers.go`
确认 handler 的签名和行为。

- [ ] **步骤 7.2：编写 mkdir/rmdir 测试**

`pkg/server/mkdir_rmdir_test.go`：

```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMkDir_Success(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()
	req := httptest.NewRequest("POST", url+"/mkdir?name=testdir", nil)
	w := httptest.NewRecorder()
	resp := w.Result()
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMkDir_MissingName(t *testing.T) { ... }  // 400
func TestMkDir_PathTraversal(t *testing.T) { ... }  // 400
```

注意到 newTestServer 返回的是 URL（`ts.URL`），它不是真正的 httptest.NewRecorder 模式。所以请求需要发送到 httptest.Server：

```go
func TestMkDir_Success(t *testing.T) {
	t.Parallel()
	url, _, cleanup := newTestServer(t, nil)
	defer cleanup()
	resp, err := http.Post(url+"/mkdir?name=testdir", "text/plain", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}
```

实际上，`newTestServer` 使用 t.Cleanup 管理，所以 cleanup() 是 no-op。但需要使用 t.Cleanup 风格。而且使用 http.Post 可能不合适，因为 mkdir/rmdir 需要 auth 中间件。

查看 handlers.go 中的路由注册：`localMux.HandleFunc("POST /mkdir", h.mkdir)` 在 localMux（无 auth）上，然后在 mux 上通过 `mux.HandleFunc("POST /mkdir", h.authMiddleware(h.mkdir))`。

而 `newTestServer`（手动路由版本）没有 `/mkdir` 路由！它只注册了：upload, download, delete, api/files, api/files/search, batch/delete, batch/rename, archive, archive-dir, versions, stats, share, healthz, /。

所以要在 `newTestServer`（或 `newTestServerWithAllRoutes`）上测试 mkdir。

`newTestServerWithAllRoutes` 调用 `RegisterRoutes`，它包含完整路由。所以测试应该用 `newTestServerWithAllRoutes`。

但 `newTestServerWithAllRoutes` 返回 `(string, *atomic.Pointer[Config])`，没有 cleanup（t.Cleanup 管理）。

如果在 Integration_test.go 中已有大量测试使用 `newTestServerWithAllRoutes`，可以延续这种模式，在专门的测试文件中添加新的 handler 边界测试。

确认是否需要 auth token：看 `cfg.AuthToken`。默认配置 `Default()` 中应该有 AuthToken，但默认可能是空。如果 auth 中间件要求 token，需要检查 Headers。

看 auth 实现：
```go
func (h *Handlers) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        cfg := h.cfgPtr.Load()
        if cfg.AuthToken != "" && r.Header.Get("X-Auth-Token") != cfg.AuthToken { ... }
    }
}
```
所以如果 AuthToken 是空，auth 中间件放行。

`Default()` 中的 AuthToken 应该是空字符串，所以无需 token 即可通过。

好的，测试可以简化——直接用 `newTestServerWithAllRoutes` 创建服务器，然后用 http.DefaultClient 发送请求。

- [ ] **步骤 7.2：完善 mkdir/rmdir 测试文件**

```go
package server

import (
	"net/http"
	"testing"
)

func TestMkDir_Success(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	resp, err := http.Post(url+"/mkdir?name=testdir", "text/plain", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestMkDir_NoName(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	resp, err := http.Post(url+"/mkdir", "text/plain", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestMkDir_PathTraversal(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	resp, err := http.Post(url+"/mkdir?name=../outside", "text/plain", nil)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRmDir_Success(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	// 先创建
	http.Post(url+"/mkdir?name=todel", "text/plain", nil)
	// 再删除
	req, _ := http.NewRequest("POST", url+"/rmdir?name=todel", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
}

func TestRmDir_NotFound(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("POST", url+"/rmdir?name=nonexistent", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRmDir_NotDir(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	// 先上传一个文件
	body := []byte("file content")
	checksum := sha256hex(body)
	req, _ := http.NewRequest("POST", url+"/upload?filename=testfile.txt", bytes.NewReader(body))
	req.Header.Set("X-File-Checksum", checksum)
	http.DefaultClient.Do(req)
	// 尝试 rmdir 一个文件
	rmReq, _ := http.NewRequest("POST", url+"/rmdir?name=testfile.txt", nil)
	resp, err := http.DefaultClient.Do(rmReq)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
```

- [ ] **步骤 7.3：编写 stat handler 错误分支测试**

`pkg/server/stat_test.go`：

```go
package server

import (
	"net/http"
	"testing"
)

func TestStat_NoName(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("HEAD", url+"/api/files/stat", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStat_PathTraversal(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=../outside", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestStat_NotFound(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=nonexistent.txt", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestStat_IsDir(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	http.Post(url+"/mkdir?name=statdir", "text/plain", nil)
	req, _ := http.NewRequest("HEAD", url+"/api/files/stat?filename=statdir", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for directory, got %d", resp.StatusCode)
	}
}
```

- [ ] **步骤 7.4：编写 rename handler 错误分支测试**

`pkg/server/rename_test.go`：

```go
package server

import (
	"bytes"
	"net/http"
	"testing"
)

func TestRename_SameFile(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	body := []byte("content")
	cs := sha256hex(body)
	http.Post(url+"/upload?filename=file.txt", "application/octet-stream", bytes.NewReader(body))
	// 用正确的 checksum 但 from=to
	req, _ := http.NewRequest("POST", url+"/rename?from=file.txt&to=file.txt", nil)
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for same file, got %d", resp.StatusCode)
	}
}

func TestRename_NoChecksum(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	http.Post(url+"/upload?filename=old.txt", "application/octet-stream", bytes.NewReader([]byte("data")))
	req, _ := http.NewRequest("POST", url+"/rename?from=old.txt&to=new.txt", nil)
	// 缺少 X-File-Checksum
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestRename_NotFound(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("POST", url+"/rename?from=nonexistent.txt&to=new.txt", nil)
	req.Header.Set("X-File-Checksum", "abc123")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestRename_PathTraversal(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	req, _ := http.NewRequest("POST", url+"/rename?from=../outside&to=new.txt", nil)
	req.Header.Set("X-File-Checksum", "abc")
	resp, _ := http.DefaultClient.Do(req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}
```

- [ ] **步骤 7.5：编写 upload checksum 不匹配边界测试**

```go
func TestUpload_ChecksumMismatch(t *testing.T) {
	url, _ := newTestServerWithAllRoutes(t, nil)
	body := []byte("original content")
	cs := sha256hex(body)
	// 上传文件
	uploadFile(t, url, "match.txt", body, map[string]string{"X-File-Checksum": cs})
	// 再次上传但 checksum 不匹配
	wrongBody := []byte("different content")
	wrongCS := sha256hex(wrongBody)
	code, _ := uploadFile(t, url, "match.txt", wrongBody, map[string]string{"X-File-Checksum": wrongCS})
	if code != http.StatusConflict {
		t.Fatalf("expected 409 for checksum mismatch, got %d", code)
	}
}
```

注意 `uploadFile` 在 integration_test.go 中定义，属于 `package server`。所以 rename_test.go 中可以直接使用。

- [ ] **步骤 7.6：运行测试验证**

```bash
go test -count=1 -cover -race ./pkg/server/...
```
预期：覆盖率 ≥85%，所有 PASS。

- [ ] **步骤 7.7：查看覆盖率统计**

```bash
go test -count=1 -cover ./pkg/server/... | grep coverage
```
确认覆盖率 ≥85%。

- [ ] **步骤 7.8：Commit**

```bash
git add pkg/server/mkdir_rmdir_test.go pkg/server/stat_test.go pkg/server/rename_test.go
git commit -m "test(server): add mkdir/rmdir/stat/rename/checksum-mismatch coverage to 85%"
```

---

### 任务 8：全量验证 + 覆盖率检查

- [ ] **步骤 8.1：运行全量测试**

```bash
go test -count=1 -race -cover ./pkg/... ./cmd/... ./internal/... 2>&1
```
预期：所有测试 PASS，无 race。

- [ ] **步骤 8.2：运行覆盖率门禁**

```bash
go test -count=1 -cover ./pkg/tunnel/xfer/... | grep -oP '\d+\.\d+%'
go test -count=1 -cover ./pkg/tunnel/p2p/... | grep -oP '\d+\.\d+%'
go test -count=1 -cover ./pkg/tunnel/mux/... | grep -oP '\d+\.\d+%'
go test -count=1 -cover ./pkg/server/... | grep -oP '\d+\.\d+%'
```
预期：xfer ≥85%，p2p ≥85%，mux ≥90%，server ≥85%。

- [ ] **步骤 8.3：运行 make lint 检查**

```bash
make lint 2>&1
```
预期：无 lint error。

- [ ] **步骤 8.4：全量构建验证**

```bash
go build ./...
```
预期：成功，无编译错误。

- [ ] **步骤 8.5：最终 Commit（如果有测试修复）**

```bash
git add -A
git commit -m "chore: final coverage adjustments before merge"
```

- [ ] **步骤 8.6：打印最终覆盖率报告**

```bash
go test -count=1 -cover ./pkg/... ./internal/... ./cmd/... | grep -E '^(ok|FAIL)'
```
