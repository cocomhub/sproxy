# 链式工作流 API 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）或 superpowers:executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在 sproxy 的 pkg/client 中实现链式工作流 API，支持"云端下载 → 等待 → 打包 → 下载本地 → 清理远端"一键完成，所有场景保留原始文件 mtime，弱网指数退避重试，状态可持久化到 KVStore 支持重启恢复。

**架构：** 新增 `KVStore` 接口（通用键值存储，默认 JSON 文件实现）和 `ChainRunner` 接口（链式操作执行器），`ChainManager` 负责编排和持久化。`CloudDownloadChain` 实现云下载具体业务逻辑。服务端修复 `HTTPDownloader` 从 `Last-Modified` 提取 mtime，`CloudTask` 保存 mtime。

**技术栈：** Go 标准库（json、os、context、slog、sync），无新增外部依赖。复用 `pkg/plugin` 注册表模式。

**当前分支：** `feat/sdk-completeness-and-fixes`

---

## 文件结构

### 新增文件

| 文件 | 职责 |
|------|------|
| `pkg/client/store.go` | `KVStore` 接口定义、`StructCodec` 编解码、`KVStoreRegistry` 插件注册表 |
| `pkg/client/store_json.go` | `JSONKVStore` 默认实现：基于 JSON 文件的键值存储 |
| `pkg/client/store_test.go` | `JSONKVStore` 和 `StructCodec` 的测试 |
| `pkg/client/chain.go` | `ChainRunner` 接口、`ChainManager` 编排层、`ChainOption` 选项、`ChainResult` 结果 |
| `pkg/client/chain_cloud_download.go` | `CloudDownloadChain` 实现：提交 → 等待 → 打包 → 下载 → 清理 |
| `pkg/client/chain_test.go` | `ChainManager` 和 `CloudDownloadChain` 的测试 |

### 修改文件

| 文件 | 改动 |
|------|------|
| `pkg/client/client.go` | `FileClient` 新增 `chainManager` 字段、`WithKVStore`/`WithCacheDir` Option、`CloudDownloadChain`/`ResumeChain`/`ListChains`/`DeleteChain` 方法 |
| `pkg/client/chunked.go` | 分块下载增加指数退避重试 |
| `pkg/client/chunked_test.go` | 新增指数退避和重试测试 |
| `pkg/server/downloader/downloader.go` | `Result` 结构体增加 `ModTime time.Time` 字段 |
| `pkg/server/downloader/http_downloader.go` | 从 HTTP `Last-Modified` 响应头提取 mtime 并写入 `Result.ModTime` |
| `pkg/server/downloader/http_downloader_test.go` | 新增 mtime 保留测试 |
| `pkg/server/cloud_download.go` | `CloudTask` 增加 `FileMTime` 字段；下载完成后 `os.Chtimes` 恢复 mtime |
| `pkg/server/cloud_download_test.go` | 新增 mtime 保留测试和清理测试 |
| `pkg/server/download_handler.go` | 确保云文件路径正确设置 `X-File-MTime` 响应头 |
| `pkg/server/archive_test.go` | 新增 tar 归档 mtime 保留测试 |
| `pkg/server/cloud_archive_handler_test.go` | 新增云归档 mtime 保留测试 |
| `test/e2e_test.go` | 新增端到端链式操作测试 |

---

## 任务分解

### 任务 1：KVStore 接口 + StructCodec + JSONKVStore 默认实现

**文件：**
- 创建：`pkg/client/store.go`
- 创建：`pkg/client/store_json.go`
- 创建：`pkg/client/store_test.go`

- [ ] **步骤 1：编写 `store.go` 定义 KVStore 接口、StructCodec、KVStoreRegistry**

```go
// pkg/client/store.go
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cocomhub/sproxy/pkg/plugin"
)

// KVStore 通用键值存储接口。
// 所有方法接收 ctx context.Context 以支持 trace 传播。
type KVStore interface {
	// Save 保存 key 对应的数据（原子写入语义）
	Save(ctx context.Context, key string, value map[string]any) error
	// Load 加载 key 对应的数据
	Load(ctx context.Context, key string) (map[string]any, error)
	// List 列出指定前缀的所有 key
	List(ctx context.Context, prefix string) ([]string, error)
	// Delete 删除 key
	Delete(ctx context.Context, key string) error
	// Close 关闭存储，释放资源
	Close() error
}

// StructCodec 在 struct（带 json tag）和 map[string]any 之间转换。
// 使用 json.Marshal → json.Unmarshal 到 map，确保字段名与 json tag 一致。
type StructCodec struct{}

// ToMap 将结构体编码为 map[string]any。
func (StructCodec) ToMap(v any) (map[string]any, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("结构体序列化失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("JSON 解码为 map 失败: %w", err)
	}
	return m, nil
}

// FromMap 从 map[string]any 解码到结构体。
func (StructCodec) FromMap(m map[string]any, v any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("map 序列化失败: %w", err)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("JSON 解码到结构体失败: %w", err)
	}
	return nil
}

// KVStoreRegistry 是可插拔的 KVStore 注册表。
// 默认使用 JSONKVStore，可通过独立 go.mod 注册其他实现（如 bbolt）。
var KVStoreRegistry = plugin.New[KVStoreFactory]("kv_store", &jsonKVStoreFactory{})

// KVStoreFactory 是 KVStore 工厂接口。
type KVStoreFactory interface {
	// Name 返回存储类型名称，用于注册表查找。
	Name() string
	// Open 创建存储实例，cfg 为配置键值对。
	Open(ctx context.Context, cfg map[string]string) (KVStore, error)
}

// jsonKVStoreFactory 是 JSONKVStore 的工厂实现。
type jsonKVStoreFactory struct{}

func (f *jsonKVStoreFactory) Name() string { return "json" }

func (f *jsonKVStoreFactory) Open(ctx context.Context, cfg map[string]string) (KVStore, error) {
	dir := cfg["dir"]
	if dir == "" {
		cacheDir, err := os.UserCacheDir()
		if err != nil {
			return nil, fmt.Errorf("获取用户缓存目录失败: %w", err)
		}
		dir = filepath.Join(cacheDir, "sproxy", "kvstore")
	}
	return NewJSONKVStore(ctx, dir, slog.Default())
}

// MemoryKVStore 内存 KVStore 实现（用于测试）。
type MemoryKVStore struct {
	mu   sync.RWMutex
	data map[string]map[string]any
}

func NewMemoryKVStore() *MemoryKVStore {
	return &MemoryKVStore{data: make(map[string]map[string]any)}
}

func (s *MemoryKVStore) Save(_ context.Context, key string, value map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	// 深拷贝
	clone := make(map[string]any, len(value))
	for k, v := range value {
		clone[k] = v
	}
	s.data[key] = clone
	return nil
}

func (s *MemoryKVStore) Load(_ context.Context, key string) (map[string]any, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.data[key]
	if !ok {
		return nil, fmt.Errorf("key not found: %s", key)
	}
	clone := make(map[string]any, len(v))
	for k, v := range v {
		clone[k] = v
	}
	return clone, nil
}

func (s *MemoryKVStore) List(_ context.Context, prefix string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var keys []string
	for k := range s.data {
		if strings.HasPrefix(k, prefix) {
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (s *MemoryKVStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.data, key)
	return nil
}

func (s *MemoryKVStore) Close() error { return nil }
```

- [ ] **步骤 2：编写 `store_json.go` 实现 JSONKVStore**

```go
// pkg/client/store_json.go
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// JSONKVStore 基于 JSON 文件的 KVStore 实现。
// 每个 key 对应一个 .json 文件，原子写入（tmp → rename）。
type JSONKVStore struct {
	dir    string
	logger *slog.Logger
}

// NewJSONKVStore 创建 JSONKVStore，确保缓存目录存在。
func NewJSONKVStore(ctx context.Context, dir string, logger *slog.Logger) (*JSONKVStore, error) {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("创建缓存目录失败: %w", err)
	}
	logger.DebugContext(ctx, "JSONKVStore 已创建", "dir", dir)
	return &JSONKVStore{dir: dir, logger: logger}, nil
}

// Save 保存 key 对应的数据，原子写入（写 .tmp 再 Rename）。
func (s *JSONKVStore) Save(ctx context.Context, key string, value map[string]any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("序列化失败: %w", err)
	}

	path := filepath.Join(s.dir, key+".json")
	tmpPath := path + ".tmp"

	if err := os.WriteFile(tmpPath, data, 0644); err != nil {
		return fmt.Errorf("写入临时文件失败: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		// 尝试清理临时文件
		os.Remove(tmpPath)
		return fmt.Errorf("重命名文件失败: %w", err)
	}
	return nil
}

// Load 加载 key 对应的数据。
func (s *JSONKVStore) Load(ctx context.Context, key string) (map[string]any, error) {
	path := filepath.Join(s.dir, key+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取缓存文件失败: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("解析缓存文件失败: %w", err)
	}
	return m, nil
}

// List 列出指定前缀的所有 key（不含 .json 后缀）。
func (s *JSONKVStore) List(ctx context.Context, prefix string) ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读取缓存目录失败: %w", err)
	}

	var keys []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".tmp.json") {
			continue
		}
		key := strings.TrimSuffix(name, ".json")
		if strings.HasPrefix(key, prefix) {
			keys = append(keys, key)
		}
	}
	return keys, nil
}

// Delete 删除 key 对应的文件。
func (s *JSONKVStore) Delete(ctx context.Context, key string) error {
	path := filepath.Join(s.dir, key+".json")
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("删除缓存文件失败: %w", err)
	}
	return nil
}

// Close 关闭存储（JSONKVStore 无需特殊清理）。
func (s *JSONKVStore) Close() error { return nil }
```

- [ ] **步骤 3：编写 `store_test.go`**

```go
// pkg/client/store_test.go
package client

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStructCodec_ToMap(t *testing.T) {
	t.Parallel()
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	codec := StructCodec{}
	m, err := codec.ToMap(testStruct{Name: "hello", Value: 42})
	if err != nil {
		t.Fatal(err)
	}
	if m["name"] != "hello" {
		t.Errorf("expected name=hello, got %v", m["name"])
	}
	if m["value"] != float64(42) {
		t.Errorf("expected value=42, got %v", m["value"])
	}
}

func TestStructCodec_FromMap(t *testing.T) {
	t.Parallel()
	type testStruct struct {
		Name  string `json:"name"`
		Value int    `json:"value"`
	}
	codec := StructCodec{}
	m := map[string]any{"name": "world", "value": float64(99)}
	var s testStruct
	if err := codec.FromMap(m, &s); err != nil {
		t.Fatal(err)
	}
	if s.Name != "world" {
		t.Errorf("expected name=world, got %s", s.Name)
	}
	if s.Value != 99 {
		t.Errorf("expected value=99, got %d", s.Value)
	}
}

func TestJSONKVStore_SaveLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	value := map[string]any{"key1": "val1", "num": float64(123)}
	if err := s.Save(ctx, "testkey", value); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(ctx, "testkey")
	if err != nil {
		t.Fatal(err)
	}
	if loaded["key1"] != "val1" {
		t.Errorf("expected val1, got %v", loaded["key1"])
	}
	if loaded["num"] != float64(123) {
		t.Errorf("expected 123, got %v", loaded["num"])
	}
}

func TestJSONKVStore_LoadNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	_, err = s.Load(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent key")
	}
}

func TestJSONKVStore_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Save(ctx, "chain:abc", map[string]any{"phase": "waiting"})
	s.Save(ctx, "chain:def", map[string]any{"phase": "completed"})
	s.Save(ctx, "other:xyz", map[string]any{"data": "test"})

	keys, err := s.List(ctx, "chain:")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d: %v", len(keys), keys)
	}
	hasABC := false
	hasDEF := false
	for _, k := range keys {
		if k == "chain:abc" {
			hasABC = true
		}
		if k == "chain:def" {
			hasDEF = true
		}
	}
	if !hasABC || !hasDEF {
		t.Errorf("missing keys, got %v", keys)
	}
}

func TestJSONKVStore_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	s.Save(ctx, "todelete", map[string]any{"data": "test"})
	if err := s.Delete(ctx, "todelete"); err != nil {
		t.Fatal(err)
	}
	_, err = s.Load(ctx, "todelete")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func TestJSONKVStore_DeleteNotFound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 删除不存在的 key 不应报错
	if err := s.Delete(ctx, "nonexistent"); err != nil {
		t.Fatal(err)
	}
}

func TestJSONKVStore_AtomicWrite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	s, err := NewJSONKVStore(ctx, dir, testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	// 写入后检查没有 .tmp 文件残留
	s.Save(ctx, "atomic", map[string]any{"data": "test"})
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("found tmp file: %s", e.Name())
		}
	}
}

func TestMemoryKVStore_SaveLoad(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemoryKVStore()
	defer s.Close()

	value := map[string]any{"key": "val"}
	if err := s.Save(ctx, "k1", value); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.Load(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}
	if loaded["key"] != "val" {
		t.Errorf("expected val, got %v", loaded["key"])
	}
}

func TestMemoryKVStore_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemoryKVStore()
	defer s.Close()

	s.Save(ctx, "chain:a", map[string]any{"data": "1"})
	s.Save(ctx, "chain:b", map[string]any{"data": "2"})
	s.Save(ctx, "other:c", map[string]any{"data": "3"})

	keys, err := s.List(ctx, "chain:")
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}
}

func TestMemoryKVStore_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := NewMemoryKVStore()
	defer s.Close()

	s.Save(ctx, "k1", map[string]any{"data": "1"})
	s.Delete(ctx, "k1")
	_, err := s.Load(ctx, "k1")
	if err == nil {
		t.Fatal("expected error after delete")
	}
}
```

- [ ] **步骤 4：运行测试验证通过**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestStructCodec_|TestJSONKVStore_|TestMemoryKVStore_' ./pkg/client/... -v
```

- [ ] **步骤 5：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/client/store.go pkg/client/store_json.go pkg/client/store_test.go
git commit -m "feat: add KVStore interface and JSONKVStore implementation"
```

---

### 任务 2：ChainRunner 接口 + ChainManager + ChainOption

**文件：**
- 创建：`pkg/client/chain.go`
- 创建：`pkg/client/chain_test.go`

- [ ] **步骤 1：编写 `chain.go` 定义 ChainRunner 接口和 ChainManager**

```go
// pkg/client/chain.go
package client

import (
	"context"
	"fmt"
	"time"
)

// Phase 常量
const (
	PhaseSubmitting  = "submitting"
	PhaseWaiting     = "waiting"
	PhaseArchiving   = "archiving"
	PhaseDownloading = "downloading"
	PhaseCleaning    = "cleaning"
	PhaseCompleted   = "completed"
	PhaseFailed      = "failed"
)

// Status 常量
const (
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// ChainRunner 链式操作执行器接口。
// 每个链式操作实现此接口，ChainManager 负责编排、持久化和恢复。
type ChainRunner interface {
	// ID 返回链式操作唯一标识
	ID() string
	// Phase 返回当前阶段名称（用于进度展示和持久化）
	Phase() string
	// Status 返回当前状态（running / completed / failed）
	Status() string
	// Run 执行链式操作
	// reportFn 用于阶段变更通知（ctx 携带 trace 信息）
	Run(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error
	// State 返回当前状态（用于持久化，map[string]any 格式）
	State() map[string]any
	// Restore 从持久化状态恢复
	Restore(state map[string]any) error
}

// ChainResult 链式操作结果。
type ChainResult struct {
	ChainID     string           `json:"chain_id"`
	Phase       string           `json:"phase"`
	Status      string           `json:"status"`
	Error       string           `json:"error,omitempty"`
	Raw         ChainRunner      `json:"-"` // 原始 runner，供调用者获取业务字段
}

// chainOptions 链式操作选项。
type chainOptions struct {
	pollInterval time.Duration
	timeout      time.Duration
	keepFiles    bool
	progressFn   func(ctx context.Context, phase string, msg string, current, total int)
}

// ChainOption 链式操作选项函数。
type ChainOption func(*chainOptions)

// WithChainPollInterval 设置轮询间隔（默认 3s）。
func WithChainPollInterval(d time.Duration) ChainOption {
	return func(o *chainOptions) {
		if d > 0 {
			o.pollInterval = d
		}
	}
}

// WithChainTimeout 设置整体超时（默认 30m）。
func WithChainTimeout(d time.Duration) ChainOption {
	return func(o *chainOptions) {
		if d > 0 {
			o.timeout = d
		}
	}
}

// WithChainKeepFiles 保留远端文件（默认 false，即完成后自动删除）。
func WithChainKeepFiles() ChainOption {
	return func(o *chainOptions) {
		o.keepFiles = true
	}
}

// WithChainProgress 进度回调。
func WithChainProgress(fn func(ctx context.Context, phase string, msg string, current, total int)) ChainOption {
	return func(o *chainOptions) {
		o.progressFn = fn
	}
}

// defaultChainOptions 返回默认选项。
func defaultChainOptions() chainOptions {
	return chainOptions{
		pollInterval: 3 * time.Second,
		timeout:      30 * time.Minute,
		keepFiles:    false,
	}
}

// ChainManager 链式操作管理器，负责编排、持久化和恢复。
type ChainManager struct {
	store KVStore
	codec StructCodec
}

// NewChainManager 创建 ChainManager。
func NewChainManager(store KVStore) *ChainManager {
	return &ChainManager{store: store, codec: StructCodec{}}
}

// Run 执行链式操作（自动持久化，支持恢复）。
// 1. 持久化初始状态  2. 执行 (reportFn 触发阶段变更持久化)  3. 完成/失败后更新状态
func (m *ChainManager) Run(ctx context.Context, runner ChainRunner) error {
	// 持久化初始状态
	m.saveState(ctx, runner)

	// 执行
	reportFn := func(ctx context.Context, phase string, msg string, current, total int) {
		m.saveState(ctx, runner)
	}
	err := runner.Run(ctx, reportFn)

	// 完成/失败后更新状态
	m.saveState(ctx, runner)

	// 成功则清理缓存
	if err == nil {
		if delErr := m.store.Delete(ctx, "chain:"+runner.ID()); delErr != nil {
			// 非致命：仅记录日志
		}
	}
	return err
}

// Resume 从断点恢复链式操作。
func (m *ChainManager) Resume(ctx context.Context, chainID string) (ChainRunner, error) {
	state, err := m.store.Load(ctx, "chain:"+chainID)
	if err != nil {
		return nil, fmt.Errorf("加载链状态失败: %w", err)
	}
	runner, err := resolveRunner(ctx, state)
	if err != nil {
		return nil, fmt.Errorf("解析 runner 类型失败: %w", err)
	}
	if err := runner.Restore(state); err != nil {
		return nil, fmt.Errorf("恢复 runner 状态失败: %w", err)
	}
	return runner, nil
}

// List 列出所有活跃链式操作。
func (m *ChainManager) List(ctx context.Context) ([]ChainRunner, error) {
	keys, err := m.store.List(ctx, "chain:")
	if err != nil {
		return nil, err
	}
	var runners []ChainRunner
	for _, key := range keys {
		state, err := m.store.Load(ctx, key)
		if err != nil {
			continue
		}
		status, _ := state["status"].(string)
		if status == StatusCompleted || status == StatusFailed {
			continue
		}
		runner, err := resolveRunner(ctx, state)
		if err != nil {
			continue
		}
		if err := runner.Restore(state); err != nil {
			continue
		}
		runners = append(runners, runner)
	}
	return runners, nil
}

// Delete 删除链式操作缓存。
func (m *ChainManager) Delete(ctx context.Context, chainID string) error {
	return m.store.Delete(ctx, "chain:"+chainID)
}

// saveState 持久化 runner 状态。
func (m *ChainManager) saveState(ctx context.Context, runner ChainRunner) {
	state := runner.State()
	if err := m.store.Save(ctx, "chain:"+runner.ID(), state); err != nil {
		// 非致命：持久化失败不影响执行
	}
}

// runnerRegistry 是 ChainRunner 类型注册表（type → 工厂函数）。
var runnerRegistry = map[string]func() ChainRunner{}

// RegisterRunner 注册 ChainRunner 类型。
func RegisterRunner(typeName string, factory func() ChainRunner) {
	runnerRegistry[typeName] = factory
}

// resolveRunner 根据 state["type"] 创建对应的 Runner 实例。
func resolveRunner(ctx context.Context, state map[string]any) (ChainRunner, error) {
	typeName, ok := state["type"].(string)
	if !ok {
		return nil, fmt.Errorf("state 缺少 type 字段")
	}
	factory, ok := runnerRegistry[typeName]
	if !ok {
		return nil, fmt.Errorf("未知的 runner 类型: %s", typeName)
	}
	return factory(), nil
}
```

- [ ] **步骤 2：编写 `chain_test.go` 测试 ChainManager**

```go
// pkg/client/chain_test.go
package client

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// testChainRunner 用于测试的简单 ChainRunner 实现。
type testChainRunner struct {
	id       string
	phase    string
	status   string
	runFn    func(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error
	stateMap map[string]any
}

func (r *testChainRunner) ID() string                                          { return r.id }
func (r *testChainRunner) Phase() string                                       { return r.phase }
func (r *testChainRunner) Status() string                                      { return r.status }
func (r *testChainRunner) Run(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {
	if r.runFn != nil {
		return r.runFn(ctx, reportFn)
	}
	return nil
}
func (r *testChainRunner) State() map[string]any {
	state := map[string]any{
		"type":   "test_chain",
		"id":     r.id,
		"phase":  r.phase,
		"status": r.status,
	}
	for k, v := range r.stateMap {
		state[k] = v
	}
	return state
}
func (r *testChainRunner) Restore(state map[string]any) error {
	r.id, _ = state["id"].(string)
	r.phase, _ = state["phase"].(string)
	r.status, _ = state["status"].(string)
	return nil
}

func init() {
	RegisterRunner("test_chain", func() ChainRunner { return &testChainRunner{} })
}

func TestChainManager_Run_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	runner := &testChainRunner{
		id:     "test-run-1",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {
			reportFn(ctx, "phase1", "doing work", 1, 2)
			return nil
		},
	}

	if err := cm.Run(ctx, runner); err != nil {
		t.Fatal(err)
	}

	// 成功 runner 应自动清理缓存
	_, err := store.Load(ctx, "chain:test-run-1")
	if err == nil {
		t.Fatal("expected cache to be deleted after successful run")
	}
}

func TestChainManager_Run_Failure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	expectedErr := errors.New("something went wrong")
	runner := &testChainRunner{
		id:     "test-run-2",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {
			return expectedErr
		},
	}

	err := cm.Run(ctx, runner)
	if err == nil {
		t.Fatal("expected error")
	}

	// 失败 runner 应保留缓存
	state, err := store.Load(ctx, "chain:test-run-2")
	if err != nil {
		t.Fatal("expected cache to be preserved after failed run")
	}
	if status, _ := state["status"].(string); status != StatusFailed {
		t.Errorf("expected status=failed, got %s", status)
	}
}

func TestChainManager_Resume(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	// 先保存状态
	store.Save(ctx, "chain:test-resume", map[string]any{
		"type":   "test_chain",
		"id":     "test-resume",
		"phase":  "phase2",
		"status": StatusRunning,
	})

	runner, err := cm.Resume(ctx, "test-resume")
	if err != nil {
		t.Fatal(err)
	}
	if runner.Phase() != "phase2" {
		t.Errorf("expected phase2, got %s", runner.Phase())
	}
}

func TestChainManager_List(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	// 保存两个活跃 runner 和一个已完成 runner
	store.Save(ctx, "chain:active1", map[string]any{
		"type": "test_chain", "id": "active1", "phase": "phase1", "status": StatusRunning,
	})
	store.Save(ctx, "chain:active2", map[string]any{
		"type": "test_chain", "id": "active2", "phase": "phase2", "status": StatusRunning,
	})
	store.Save(ctx, "chain:done1", map[string]any{
		"type": "test_chain", "id": "done1", "phase": PhaseCompleted, "status": StatusCompleted,
	})

	runners, err := cm.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runners) != 2 {
		t.Fatalf("expected 2 active runners, got %d", len(runners))
	}
}

func TestChainManager_Delete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	store.Save(ctx, "chain:todelete", map[string]any{
		"type": "test_chain", "id": "todelete",
	})

	if err := cm.Delete(ctx, "todelete"); err != nil {
		t.Fatal(err)
	}

	_, err := store.Load(ctx, "chain:todelete")
	if err == nil {
		t.Fatal("expected cache to be deleted")
	}
}

func TestChainManager_ContextCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	store := NewMemoryKVStore()
	cm := NewChainManager(store)

	var ran atomic.Bool
	runner := &testChainRunner{
		id:     "test-cancel",
		phase:  "running",
		status: StatusRunning,
		runFn: func(ctx context.Context, reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {
			ran.Store(true)
			<-ctx.Done()
			return ctx.Err()
		},
	}

	cancel() // 立即取消
	err := cm.Run(ctx, runner)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !ran.Load() {
		t.Fatal("expected runner to be started")
	}
}
```

- [ ] **步骤 3：运行测试验证通过**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestChainManager_' ./pkg/client/... -v
```

- [ ] **步骤 4：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/client/chain.go pkg/client/chain_test.go
git commit -m "feat: add ChainRunner interface and ChainManager"
```

---

### 任务 3：CloudDownloadChain 实现

**文件：**
- 创建：`pkg/client/chain_cloud_download.go`

- [ ] **步骤 1：编写 `chain_cloud_download.go`**

```go
// pkg/client/chain_cloud_download.go
package client

import (
	"context"
	"fmt"
	"path/filepath"
	"time"
)

func init() {
	RegisterRunner("cloud_download", func() ChainRunner { return &CloudDownloadChain{} })
}

// CloudDownloadChain 云端下载链式操作，实现 ChainRunner 接口。
type CloudDownloadChain struct {
	// 标识
	chainID string `json:"-"`
	phase   string `json:"-"`
	status  string `json:"-"`

	// 业务字段（持久化）
	ChainID     string   `json:"chain_id"`
	Phase       string   `json:"phase"`
	Status      string   `json:"status"`
	URLs        []string `json:"urls"`
	TaskIDs     []string `json:"task_ids,omitempty"`
	ArchiveName string   `json:"archive_name"`
	LocalDir    string   `json:"local_dir"`
	LocalPath   string   `json:"local_path,omitempty"`
	KeepFiles   bool     `json:"keep_files"`
	Completed   int      `json:"completed"`
	Failed      int      `json:"failed"`
	Total       int      `json:"total"`
	Error       string   `json:"error,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`

	// 依赖（不持久化）
	client *FileClient `json:"-"`
	opts   chainOptions `json:"-"`
}

// NewCloudDownloadChain 创建 CloudDownloadChain。
func NewCloudDownloadChain(client *FileClient, urls []string, archiveName, localDir string, opts chainOptions) *CloudDownloadChain {
	now := time.Now()
	return &CloudDownloadChain{
		chainID:     fmt.Sprintf("chain-%d", now.UnixNano()),
		phase:       "",
		status:      StatusRunning,
		ChainID:     fmt.Sprintf("chain-%d", now.UnixNano()),
		Phase:       "",
		Status:      StatusRunning,
		URLs:        urls,
		ArchiveName: archiveName,
		LocalDir:    localDir,
		KeepFiles:   opts.keepFiles,
		Total:       len(urls),
		CreatedAt:   now,
		UpdatedAt:   now,
		client:      client,
		opts:        opts,
	}
}

// ChainRunner 接口实现
func (c *CloudDownloadChain) ID() string    { return c.ChainID }
func (c *CloudDownloadChain) Phase() string { return c.Phase }
func (c *CloudDownloadChain) Status() string { return c.Status }

// State 返回持久化状态（含 type 字段用于恢复时判断）。
func (c *CloudDownloadChain) State() map[string]any {
	return map[string]any{
		"type":         "cloud_download",
		"chain_id":     c.ChainID,
		"phase":        c.Phase,
		"status":       c.Status,
		"urls":         c.URLs,
		"task_ids":     c.TaskIDs,
		"archive_name": c.ArchiveName,
		"local_dir":    c.LocalDir,
		"local_path":   c.LocalPath,
		"keep_files":   c.KeepFiles,
		"completed":    c.Completed,
		"failed":       c.Failed,
		"total":        c.Total,
		"error":        c.Error,
		"created_at":   c.CreatedAt,
		"updated_at":   c.UpdatedAt,
	}
}

// Restore 从持久化状态恢复。
func (c *CloudDownloadChain) Restore(state map[string]any) error {
	codec := StructCodec{}
	return codec.FromMap(state, c)
}

// setClient 设置 client 依赖（恢复时需要）。
func (c *CloudDownloadChain) setClient(client *FileClient) {
	c.client = client
}

// setOptions 设置选项。
func (c *CloudDownloadChain) setOptions(opts chainOptions) {
	c.opts = opts
}

// Run 执行链式操作（从 phase 断点继续，跳过已完成阶段）。
func (c *CloudDownloadChain) Run(ctx context.Context,
	reportFn func(ctx context.Context, phase string, msg string, current, total int)) error {

	var err error

	switch c.Phase {
	case "":
		fallthrough
	case PhaseSubmitting:
		reportFn(ctx, PhaseSubmitting, "提交云端下载任务", 0, len(c.URLs))
		if err = c.submitTasks(ctx); err != nil {
			c.fail(err)
			return err
		}
		c.Phase = PhaseWaiting
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseWaiting, "等待下载完成", c.Completed, c.Total)
		fallthrough

	case PhaseWaiting:
		if err = c.waitForTasks(ctx); err != nil {
			c.fail(err)
			return err
		}
		c.Phase = PhaseArchiving
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseArchiving, "打包归档", 0, 1)
		fallthrough

	case PhaseArchiving:
		if err = c.archiveTasks(ctx); err != nil {
			c.fail(err)
			return err
		}
		c.Phase = PhaseDownloading
		c.UpdatedAt = time.Now()
		reportFn(ctx, PhaseDownloading, "下载到本地", 0, 1)
		fallthrough

	case PhaseDownloading:
		if err = c.downloadToLocal(ctx); err != nil {
			c.fail(err)
			return err
		}
		if !c.KeepFiles {
			c.Phase = PhaseCleaning
			c.UpdatedAt = time.Now()
			reportFn(ctx, PhaseCleaning, "清理远端文件", 0, len(c.TaskIDs)+1)
			fallthrough

		case PhaseCleaning:
			if err = c.cleanupRemote(ctx); err != nil {
				// 清理失败不影响主流程成功
			}
		}
	}

	c.Phase = PhaseCompleted
	c.Status = StatusCompleted
	c.UpdatedAt = time.Now()
	return nil
}

// submitTasks 提交云端下载任务。
func (c *CloudDownloadChain) submitTasks(ctx context.Context) error {
	tasks, err := c.client.CloudDownloadBatch(ctx, c.URLs)
	if err != nil {
		return fmt.Errorf("批量提交云端下载失败: %w", err)
	}
	for _, t := range tasks {
		c.TaskIDs = append(c.TaskIDs, t.ID)
	}
	c.Total = len(c.TaskIDs)
	return nil
}

// waitForTasks 等待所有任务完成（含存储超限重试）。
func (c *CloudDownloadChain) waitForTasks(ctx context.Context) error {
	maxRetries := 3
	pollInterval := c.opts.pollInterval
	timeout := c.opts.timeout

	for attempt := 0; attempt <= maxRetries; attempt++ {
		results, err := c.pollAllTasks(ctx, pollInterval, timeout)
		if err != nil {
			return err
		}

		// 统计完成/失败
		c.Completed = 0
		c.Failed = 0
		var storageFullTasks []string
		for _, r := range results {
			switch r.Status {
			case "completed":
				c.Completed++
			case "failed":
				// 检查是否是存储超限
				if isStorageFullError(r.Error) {
					storageFullTasks = append(storageFullTasks, r.URL)
				} else {
					c.Failed++
				}
			}
		}

		// 全部完成
		if len(storageFullTasks) == 0 {
			return nil
		}

		// 存储超限重试
		if attempt < maxRetries {
			select {
			case <-time.After(30 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
			// 重新提交失败的任务
			for _, url := range storageFullTasks {
				task, err := c.client.CloudDownload(ctx, url)
				if err != nil {
					return fmt.Errorf("重试提交失败: %w", err)
				}
				c.TaskIDs = append(c.TaskIDs, task.ID)
			}
		}
	}

	return fmt.Errorf("存储空间不足，已重试 %d 次", maxRetries)
}

// pollAllTasks 轮询所有任务直到完成或超时。
func (c *CloudDownloadChain) pollAllTasks(ctx context.Context, pollInterval, timeout time.Duration) ([]*CloudTaskStatus, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		select {
		case <-timeoutCtx.Done():
			return nil, timeoutCtx.Err()
		case <-time.After(pollInterval):
			allDone := true
			var results []*CloudTaskStatus
			for _, taskID := range c.TaskIDs {
				status, err := c.client.GetCloudTask(ctx, taskID)
				if err != nil {
					return nil, fmt.Errorf("查询任务 %s 失败: %w", taskID, err)
				}
				results = append(results, status)
				if status.Status == "pending" || status.Status == "downloading" {
					allDone = false
				}
			}
			if allDone {
				return results, nil
			}
		}
	}
}

// archiveTasks 服务端打包归档。
func (c *CloudDownloadChain) archiveTasks(ctx context.Context) error {
	result, err := c.client.ArchiveCloudTasks(ctx, c.TaskIDs, c.ArchiveName)
	if err != nil {
		return fmt.Errorf("打包归档失败: %w", err)
	}
	c.LocalPath = filepath.Join(c.LocalDir, c.ArchiveName)
	if !result.Success {
		return fmt.Errorf("打包归档失败: %s", result.Message)
	}
	return nil
}

// downloadToLocal 下载归档到本地。
func (c *CloudDownloadChain) downloadToLocal(ctx context.Context) error {
	// 从归档结果中获取服务端文件路径
	// 需要先通过 Stat 获取文件信息
	archivePath := c.LocalPath // 服务端相对路径

	// 使用分块下载（断点续传 + 指数退避 + checksum 验证）
	if err := c.client.ChunkedDownload(ctx, archivePath, c.LocalPath); err != nil {
		return fmt.Errorf("下载归档文件失败: %w", err)
	}
	return nil
}

// cleanupRemote 清理远端文件。
func (c *CloudDownloadChain) cleanupRemote(ctx context.Context) error {
	// 删除所有云任务（触发 DeleteTask: 清理 __cloud__ 目录 + 释放存储）
	for _, taskID := range c.TaskIDs {
		if err := c.client.DeleteCloudTask(ctx, taskID); err != nil {
			return fmt.Errorf("清理云端任务 %s 失败: %w", taskID, err)
		}
	}
	return nil
}

// fail 标记链式操作为失败。
func (c *CloudDownloadChain) fail(err error) {
	c.Status = StatusFailed
	c.Phase = PhaseFailed
	c.Error = err.Error()
	c.UpdatedAt = time.Now()
}

// isStorageFullError 判断错误是否为存储空间不足。
func isStorageFullError(errMsg string) bool {
	return errMsg == "storage full" || errMsg == "507" || len(errMsg) > 0 &&
		(errMsg == "Insufficient Storage" || errMsg == "insufficient storage")
}

// CloudTaskStatus 云端下载任务状态。
type CloudTaskStatus struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Status   string `json:"status"`
	Filename string `json:"filename"`
	Size     int64  `json:"size"`
	Checksum string `json:"checksum"`
	Error    string `json:"error,omitempty"`
}
```

- [ ] **步骤 2：运行 build 验证编译通过**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./pkg/client/...
```

- [ ] **步骤 3：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/client/chain_cloud_download.go
git commit -m "feat: add CloudDownloadChain implementation"
```

---

### 任务 4：FileClient 集成链式 API

**文件：**
- 修改：`pkg/client/client.go`

- [ ] **步骤 1：在 FileClient 中增加 chainManager 字段和新 Option**

```go
// 在 FileClient struct 中增加字段
type FileClient struct {
	// ... 现有字段
	chainManager *ChainManager // 链式操作管理器，nil=不启用
}

// WithKVStore 设置自定义 KVStore 实现。
// 启用链式操作持久化功能。
func WithKVStore(store KVStore) Option {
	return func(c *FileClient) {
		c.chainManager = NewChainManager(store)
	}
}

// WithCacheDir 使用默认 JSONKVStore 并指定缓存目录。
// 启用链式操作持久化功能。
func WithCacheDir(dir string) Option {
	return func(c *FileClient) {
		store, err := NewJSONKVStore(context.Background(), dir, c.logger)
		if err != nil {
			c.logger.Warn("创建缓存目录失败，使用内存存储", "dir", dir, "error", err)
			c.chainManager = NewChainManager(NewMemoryKVStore())
			return
		}
		c.chainManager = NewChainManager(store)
	}
}
```

- [ ] **步骤 2：添加 CloudDownloadChain 方法**

```go
// CloudDownloadChain 一键链式操作：提交任务 → 等待完成 → 打包 → 下载到本地 → 清理远端。
// 默认：成功后自动删除远端所有相关文件。
// 选项：WithChainKeepFiles() 保留远端文件。
func (c *FileClient) CloudDownloadChain(ctx context.Context,
	urls []string, archiveName, localDir string,
	opts ...ChainOption) (*ChainResult, error) {

	options := defaultChainOptions()
	for _, o := range opts {
		o(&options)
	}

	runner := NewCloudDownloadChain(c, urls, archiveName, localDir, options)

	if c.chainManager != nil {
		if err := c.chainManager.Run(ctx, runner); err != nil {
			return nil, err
		}
	} else {
		if err := runner.Run(ctx, func(ctx context.Context, phase string, msg string, current, total int) {
			c.logger.DebugContext(ctx, "链式操作进度", "phase", phase, "msg", msg, "current", current, "total", total)
		}); err != nil {
			return nil, err
		}
	}

	return &ChainResult{
		ChainID: runner.ChainID,
		Phase:   runner.Phase,
		Status:  runner.Status,
		Raw:     runner,
	}, nil
}

// ResumeChain 从缓存恢复链式操作。
func (c *FileClient) ResumeChain(ctx context.Context, chainID string) (*ChainResult, error) {
	if c.chainManager == nil {
		return nil, fmt.Errorf("链式操作未启用持久化，请使用 WithCacheDir 或 WithKVStore 创建客户端")
	}

	runner, err := c.chainManager.Resume(ctx, chainID)
	if err != nil {
		return nil, err
	}

	// 设置 client 依赖
	if cdc, ok := runner.(*CloudDownloadChain); ok {
		cdc.setClient(c)
	}

	if err := c.chainManager.Run(ctx, runner); err != nil {
		return nil, err
	}

	return &ChainResult{
		ChainID: runner.ID(),
		Phase:   runner.Phase(),
		Status:  runner.Status(),
		Raw:     runner,
	}, nil
}

// ListChains 列出所有活跃链式操作。
func (c *FileClient) ListChains(ctx context.Context) ([]*ChainState, error) {
	// 简化：返回 ChainRunner 的摘要信息
	if c.chainManager == nil {
		return nil, nil
	}
	runners, err := c.chainManager.List(ctx)
	if err != nil {
		return nil, err
	}
	var states []*ChainState
	for _, r := range runners {
		states = append(states, &ChainState{
			ChainID: r.ID(),
			Phase:   r.Phase(),
			Status:  r.Status(),
		})
	}
	return states, nil
}

// DeleteChain 删除链式操作缓存。
func (c *FileClient) DeleteChain(ctx context.Context, chainID string) error {
	if c.chainManager == nil {
		return nil
	}
	return c.chainManager.Delete(ctx, chainID)
}

// ChainState 链式操作摘要状态。
type ChainState struct {
	ChainID string `json:"chain_id"`
	Phase   string `json:"phase"`
	Status  string `json:"status"`
}
```

- [ ] **步骤 3：运行 build 验证编译通过**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./pkg/client/...
go build ./cmd/...
```

- [ ] **步骤 4：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/client/client.go
git commit -m "feat: integrate ChainManager into FileClient"
```

---

### 任务 5：mtime 修复 — 下载器端

**文件：**
- 修改：`pkg/server/downloader/downloader.go`
- 修改：`pkg/server/downloader/http_downloader.go`
- 修改：`pkg/server/downloader/http_downloader_test.go`

- [ ] **步骤 1：在 `downloader.go` 的 Result 中增加 ModTime 字段**

```go
// pkg/server/downloader/downloader.go

// Result 下载结果。
type Result struct {
	Size     int64     // 下载文件大小（字节）
	Checksum string    // SHA-256 hex
	ModTime  time.Time // 原始文件修改时间（从 HTTP Last-Modified 提取）
}
```

- [ ] **步骤 2：在 `http_downloader.go` 中从 Last-Modified 提取 mtime**

```go
// 在 func (d *HTTPDownloader) Download 中，读取 resp.Body 之前：
var modTime time.Time
if lm := resp.Header.Get("Last-Modified"); lm != "" {
	if t, err := time.Parse(time.RFC1123, lm); err == nil {
		modTime = t
	} else if t, err := time.Parse(time.RFC1123Z, lm); err == nil {
		modTime = t
	}
}

// 在返回 Result 时：
return &Result{Size: downloaded, Checksum: checksum, ModTime: modTime}, nil
```

- [ ] **步骤 3：编写 http_downloader_test.go 的 mtime 测试**

```go
// pkg/server/downloader/http_downloader_test.go 新增

func TestHTTPDownloader_PreservesMTime(t *testing.T) {
	t.Parallel()

	// 创建测试服务器，返回 Last-Modified 头
	expectedTime := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", expectedTime.Format(time.RFC1123))
		w.Write([]byte("test content"))
	}))
	defer ts.Close()

	dl := &HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "output.txt")
	result, err := dl.Download(context.Background(), ts.URL, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ModTime.Equal(expectedTime) {
		t.Errorf("expected ModTime %v, got %v", expectedTime, result.ModTime)
	}

	// 验证文件 mtime 也被恢复
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(expectedTime) {
		t.Errorf("expected file mtime %v, got %v", expectedTime, info.ModTime())
	}
}

func TestHTTPDownloader_NoLastModified(t *testing.T) {
	t.Parallel()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("no last-modified header"))
	}))
	defer ts.Close()

	dl := &HTTPDownloader{}
	dest := filepath.Join(t.TempDir(), "output.txt")
	result, err := dl.Download(context.Background(), ts.URL, dest, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ModTime.IsZero() {
		t.Errorf("expected zero ModTime, got %v", result.ModTime)
	}
}
```

- [ ] **步骤 4：运行测试验证通过**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestHTTPDownloader_PreservesMTime|TestHTTPDownloader_NoLastModified' ./pkg/server/downloader/... -v
```

- [ ] **步骤 5：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/server/downloader/downloader.go pkg/server/downloader/http_downloader.go pkg/server/downloader/http_downloader_test.go
git commit -m "fix: extract ModTime from HTTP Last-Modified header in downloader"
```

---

### 任务 6：mtime 修复 — 服务端 CloudTask

**文件：**
- 修改：`pkg/server/cloud_download.go`
- 修改：`pkg/server/download_handler.go`

- [ ] **步骤 1：在 cloud_download.go 中增加 FileMTime 字段**

```go
// 在 CloudTask struct 中增加字段
type CloudTask struct {
	// ... 现有字段
	FileMTime int64 `json:"file_mtime,omitempty"` // 原始文件修改时间（UnixNano），从 URL 的 Last-Modified 提取
}

// 在 executeDownload 中，下载完成后：
if result.ModTime != (time.Time{}) {
	modTime := result.ModTime
	if err := os.Chtimes(destPath, modTime, modTime); err != nil {
		m.logger.Warn("设置文件修改时间失败", "task_id", task.ID, "error", err)
	}
	m.mu.Lock()
	task.FileMTime = result.ModTime.UnixNano()
	m.mu.Unlock()
}
```

- [ ] **步骤 2：在 download_handler.go 中确保云文件路径返回 X-File-MTime**

```go
// 在 downloadHandler 中，通过 os.Stat 获取文件信息：
// 关键：云文件路径（__cloud__/ 和 __cloud_archives__/）下的文件
// 如果在系统中设置了 mtime，os.Stat 会返回正确的值（由 os.Chtimes 设置）
// download_handler.go 已通过 info.ModTime().UnixNano() 设置 X-File-MTime
// 该逻辑已存在，无需修改——但需要确保 task 下载完成后调用 os.Chtimes
```

- [ ] **步骤 3：运行测试验证**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestCloudDownload' ./pkg/server/... -v
```

- [ ] **步骤 4：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/server/cloud_download.go
git commit -m "fix: preserve original file mtime in CloudTask from downloader"
```

---

### 任务 7：mtime 保留测试

**文件：**
- 修改：`pkg/server/archive_test.go`
- 修改：`pkg/server/cloud_archive_handler_test.go`
- 修改：`pkg/server/cloud_download_test.go`

- [ ] **步骤 1：archive_test.go 新增 mtime 保留测试**

```go
// pkg/server/archive_test.go 新增

func TestArchive_PreservesMTime(t *testing.T) {
	t.Parallel()
	url, _ := newTestServerWithAllRoutes(t, nil)

	// 上传文件
	body := []byte("hello world")
	uploadFile(t, url, "test.txt", body, map[string]string{
		"X-File-Checksum": sha256hex(body),
	})

	// 获取上传后的文件 mtime
	info, _ := os.Stat(filepath.Join(cfgPtr.Load().UploadsDir, "test.txt"))
	originalMTime := info.ModTime()

	// 归档
	resp, err := http.Post(url+"/api/archive", "application/json",
		strings.NewReader(`{"files":["test.txt"]}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	// 解包检查 tar header 中的 ModTime
	gr, _ := gzip.NewReader(resp.Body)
	defer gr.Close()
	tr := tar.NewReader(gr)
	header, err := tr.Next()
	if err != nil {
		t.Fatal(err)
	}

	// tar header 的 ModTime 是 Unix 秒精度，允许 1 秒误差
	diff := header.ModTime.Sub(originalMTime)
	if diff < -time.Second || diff > time.Second {
		t.Errorf("tar header ModTime %v differs from original %v (diff: %v)",
			header.ModTime, originalMTime, diff)
	}
}
```

- [ ] **步骤 2：cloud_download_test.go 新增 mtime 和清理测试**

```go
// pkg/server/cloud_download_test.go 新增

func TestCloudDownloadManager_DeleteTaskCleansAndReleases(t *testing.T) {
	t.Parallel()
	// 创建 Manager
	uploadsDir := t.TempDir()
	mgr := newTestManager(t, uploadsDir)
	defer mgr.Stop()

	// 创建任务并模拟完成
	task := &CloudTask{
		ID:        "cloud-test-1",
		URL:       "https://example.com/file.txt",
		Status:    "completed",
		Filename:  "file.txt",
		TotalSize: 1000,
		Checksum:  "abc123",
	}
	mgr.tasks[task.ID] = task

	// 创建模拟文件
	taskDir := filepath.Join(mgr.cloudDir, task.ID)
	os.MkdirAll(taskDir, 0755)
	os.WriteFile(filepath.Join(taskDir, task.Filename), []byte("test"), 0644)

	// 预留存储空间
	mgr.storage.TryReserve(1000, CategoryCloud)

	// 删除任务
	if err := mgr.DeleteTask(task.ID); err != nil {
		t.Fatal(err)
	}

	// 验证文件被删除
	if _, err := os.Stat(taskDir); !os.IsNotExist(err) {
		t.Error("task dir should be deleted")
	}

	// 验证存储空间被释放
	stats := mgr.storage.Stats()
	if stats.CloudSize != 0 {
		t.Errorf("expected cloud size 0, got %d", stats.CloudSize)
	}
}
```

- [ ] **步骤 3：运行测试**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestArchive_PreservesMTime|TestCloudDownloadManager_DeleteTaskCleans' ./pkg/server/... -v
```

- [ ] **步骤 4：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/server/archive_test.go pkg/server/cloud_download_test.go
git commit -m "test: add mtime preservation and cleanup tests"
```

---

### 任务 8：分块下载指数退避重试

**文件：**
- 修改：`pkg/client/chunked.go`
- 修改：`pkg/client/chunked_test.go`

- [ ] **步骤 1：在 chunked.go 中增加指数退避**

```go
// pkg/client/chunked.go

// downloadOneChunk 下载单个分块，支持指数退避重试。
func downloadOneChunk(ctx context.Context, urlStr string, offset, length int64,
	httpClient *http.Client, logger *slog.Logger) ([]byte, bool) {
	
	maxRetries := 3
	baseDelay := 500 * time.Millisecond

	for attempt := 0; attempt < maxRetries; attempt++ {
		data, ok := tryDownloadChunk(ctx, urlStr, offset, length, httpClient, logger)
		if ok {
			return data, true
		}

		if attempt < maxRetries-1 {
			delay := baseDelay * (1 << attempt) // 500ms, 1s, 2s
			select {
			case <-time.After(delay):
			case <-ctx.Done():
				return nil, false
			}
		}
	}
	return nil, false
}

// 现有 tryDownloadChunk 函数签名调整：
// func tryDownloadChunk(ctx context.Context, urlStr string, offset, length int64,
//     httpClient *http.Client, logger *slog.Logger) ([]byte, bool)
```

- [ ] **步骤 2：在 chunked_test.go 中新增测试**

```go
// pkg/client/chunked_test.go 新增

func TestDownloadChunk_ExponentialBackoff(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logger := testLogger()

	// 创建测试服务器，前两次返回 503，第三次成功
	attempt := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		if attempt <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("X-Chunk-Checksum", sha256hex([]byte("test data")))
		w.Write([]byte("test data"))
	}))
	defer ts.Close()

	// 使用测试用的 HTTP 客户端（短超时）
	hc := &http.Client{Timeout: 5 * time.Second}
	data, ok := tryDownloadChunk(ctx, ts.URL, 0, 9, hc, logger)
	if !ok {
		t.Fatal("expected success after retries")
	}
	if string(data) != "test data" {
		t.Errorf("expected 'test data', got %s", string(data))
	}
	if attempt != 3 {
		t.Errorf("expected 3 attempts, got %d", attempt)
	}
}

func TestDownloadChunk_RetryThenSuccess(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	logger := testLogger()

	attempt := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt++
		w.Header().Set("X-Chunk-Checksum", sha256hex([]byte("data")))
		w.Write([]byte("data"))
	}))
	defer ts.Close()

	hc := &http.Client{Timeout: 5 * time.Second}
	data, ok := tryDownloadChunk(ctx, ts.URL, 0, 4, hc, logger)
	if !ok {
		t.Fatal("expected success")
	}
	if string(data) != "data" {
		t.Errorf("expected 'data', got %s", string(data))
	}
	if attempt != 1 {
		t.Errorf("expected 1 attempt, got %d", attempt)
	}
}
```

- [ ] **步骤 3：运行测试**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestDownloadChunk_' ./pkg/client/... -v
```

- [ ] **步骤 4：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add pkg/client/chunked.go pkg/client/chunked_test.go
git commit -m "feat: add exponential backoff retry for chunked download"
```

---

### 任务 9：端到端测试

**文件：**
- 修改：`test/e2e_test.go`

- [ ] **步骤 1：编写端到端链式操作测试**

```go
// test/e2e_test.go 新增

func TestE2E_CloudDownloadChain(t *testing.T) {
	t.Parallel()
	baseURL, cleanup := startSPROXY(t)
	defer cleanup()

	client := NewFileClient(baseURL, WithCacheDir(t.TempDir()))

	// 创建一个测试 HTTP 服务器提供下载文件
	fileContent := []byte("test file content for cloud download")
	fileChecksum := sha256hex(fileContent)
	fileTs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Last-Modified", time.Date(2024, 6, 15, 12, 0, 0, 0, time.UTC).Format(time.RFC1123))
		w.Write(fileContent)
	}))
	defer fileTs.Close()

	// 执行链式操作
	ctx := context.Background()
	result, err := client.CloudDownloadChain(ctx,
		[]string{fileTs.URL},
		"test-archive.tar.gz",
		t.TempDir(),
		WithChainPollInterval(1*time.Second),
		WithChainTimeout(2*time.Minute),
	)
	if err != nil {
		t.Fatal(err)
	}

	if result.Status != StatusCompleted {
		t.Errorf("expected completed, got %s", result.Status)
	}

	// 验证本地文件存在
	cdc := result.Raw.(*CloudDownloadChain)
	if cdc.LocalPath != "" {
		if _, err := os.Stat(cdc.LocalPath); os.IsNotExist(err) {
			t.Errorf("local file not found: %s", cdc.LocalPath)
		}
	}
}
```

- [ ] **步骤 2：运行 E2E 测试**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -count=1 -run 'TestE2E_CloudDownloadChain' ./test/... -v -timeout=5m
```

- [ ] **步骤 3：Commit**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git add test/e2e_test.go
git commit -m "test: add E2E test for cloud download chain workflow"
```

---

### 任务 10：完整检查

- [ ] **步骤 1：运行全部测试**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go test -race -count=1 -timeout=10m ./pkg/client/... ./pkg/server/... ./test/...
```

- [ ] **步骤 2：运行 lint**

```bash
cd D:\workdir\leon\cocomhub\sproxy
golangci-lint run ./pkg/client/... ./pkg/server/... ./pkg/server/downloader/...
```

- [ ] **步骤 3：运行 build**

```bash
cd D:\workdir\leon\cocomhub\sproxy
go build ./...
make build
```

- [ ] **步骤 4：查看当前分支状态**

```bash
cd D:\workdir\leon\cocomhub\sproxy
git log --oneline -10
git status
```

- [ ] **步骤 5：请求代码审查**

调用 `requesting-code-review` 技能进行代码审查，修复缺陷后创建 PR。

---

## 执行计划

**计划已完成并保存到 `docs/superpowers/plans/2026-07-27-chain-workflow.md`。两种执行方式：**

**1. 子代理驱动（推荐）** - 每个任务调度一个新的子代理，任务间进行审查，快速迭代

**2. 内联执行** - 在当前会话中使用 executing-plans 执行任务，批量执行并设有检查点

**选哪种方式？**