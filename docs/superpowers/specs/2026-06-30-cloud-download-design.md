# 云端下载（Cloud Download）功能设计

> 状态：已确认 | 日期：2026-06-30 | 作者：suixibing

## 一、概述

### 1.1 背景与目标

sproxy 当前支持文件上传/下载/删除，但所有文件必须由客户端直接上传或下载。在实际使用场景中，用户希望先将文件从外部 URL 下载到 sproxy 服务端（"云端下载"），再从服务端下载到本地，实现类似"离线下载"的效果。

### 1.2 核心需求

1. **存储空间控制**：新增 `max_storage_bytes` 配置项，统一限制所有文件（包括用户上传、内部目录、云端下载）的总存储占用，超过上限直接拒绝
2. **云端下载**：sproxy 服务端从外部 URL 下载文件到云端存储，客户端再从云端下载到本地，校验 checksum 一致后删除云端副本
3. **客户端上传暂存**：客户端也可将文件上传到 sproxy 作为暂存，后续再从 sproxy 下载（复用现有 upload 能力）
4. **插件化下载器**：下载器支持通过接口扩展，配置驱动选择具体实现
5. **同步/异步模式切换**：小文件默认同步等待，大文件自动切异步模式，连接断开自动转为异步继续

### 1.3 分阶段实施

| 阶段 | 名称 | 内容 |
|---|---|---|
| 阶段零 | 插件框架提升 | 将 `pkg/tunnel/plugin/` 提升为 `pkg/plugin/`，供 server 和 tunnel 共享 |
| 阶段一 | 存储空间控制 | StorageManager 组件，配置上限，原子化空间预留/释放，分层统计 |
| 阶段二 | 云端下载服务端 | Downloader 接口 + Registry + HTTP 实现 + CloudDownloadManager + Handler |
| 阶段三 | sclient 客户端 | `cloud-download` 子命令，同步/异步切换，断线自动转异步，checksum 校验 |
| 阶段四 | Web UI | 云端下载管理页面，任务列表 + 进度条 + 操作按钮 |

---

## 二、阶段零：插件框架提升

### 2.1 目标

将 `pkg/tunnel/plugin/registry.go` 中的泛型 `Registry[T]` 提升到 `pkg/plugin/`，使其成为整个 sproxy 项目共享的通用插件框架，不再局限于 tunnel 包。

### 2.2 改动

```
# 现状
pkg/tunnel/plugin/registry.go       # Registry[T] 泛型注册表
pkg/tunnel/plugin/registry_test.go

# 目标
pkg/plugin/registry.go              # 提升到 pkg/plugin/
pkg/plugin/registry_test.go
pkg/tunnel/plugin/                  # 删除
```

### 2.3 影响范围

| 文件 | 当前 import | 改为 |
|---|---|---|
| `pkg/tunnel/xfer/registry.go` | `pkg/tunnel/plugin` | `pkg/plugin` |
| `pkg/tunnel/hub/registry.go` | `pkg/tunnel/plugin` | `pkg/plugin` |
| `pkg/tunnel/plugin/registry_test.go` | 自身包 | `pkg/plugin/registry_test.go` |

API 签名完全不变：`Registry[T]`、`Plugin[T]`、`New[T]()`、`Register()`、`Active()`、`Get()`、`Names()`、`IsDefault()`、`Clear()` 均保持不变。

### 2.4 测试要求

- 原 `registry_test.go` 测试用例完整迁移，确保覆盖率不降
- 新增测试：验证跨包引用正确（`pkg/tunnel/xfer` 和 `pkg/tunnel/hub` 使用新路径后 `Active()` 行为不变）

---

## 三、阶段一：存储空间控制

### 3.1 配置

```go
// pkg/server/config.go - Config 结构体新增
type Config struct {
    // ... 现有字段 ...
    MaxStorageBytes int64 `yaml:"max_storage_bytes"` // 存储上限（字节），0 = 不限制
}
```

- 默认值（`Default()`）：`0`（不限制）
- 环境变量：`SPROXY_MAX_STORAGE_BYTES`
- CLI flag：`--max-storage-bytes`
- SIGHUP 热重载：支持（运行时也可通过 API 调整）

### 3.2 StorageManager 组件

**新文件：`pkg/server/storage_manager.go`**

```go
type StorageCategory int

const (
    CategoryUserFiles StorageCategory = iota  // 用户文件（非 .__ 前缀目录）
    CategoryChunked                             // .__chunked__/
    CategoryVersions                            // .__versions__/
    CategoryCloud                               // .__cloud__/
)

type StorageManager struct {
    uploadsDir    string
    maxBytes      atomic.Int64  // 配置上限，0=不限制

    // 分层统计
    userFilesSize  atomic.Int64
    chunkedSize    atomic.Int64
    versionsSize   atomic.Int64
    cloudSize      atomic.Int64

    totalUsage     atomic.Int64  // 缓存总和
    checksumStore  ChecksumStoreIface
    logger         *slog.Logger
    scanMu         sync.Mutex    // 防止并发全量扫描
}
```

**核心方法：**

| 方法 | 说明 |
|---|---|
| `NewStorageManager(dir string, maxBytes int64, cs ChecksumStoreIface, logger *slog.Logger) *StorageManager` | 构造函数，启动时自动调用 `ScanAndRecalculate()` |
| `TryReserve(size int64, cat StorageCategory) error` | 原子操作：检查 `totalUsage + size <= maxBytes`，通过则同时累加对应分类计数和 totalUsage，返回 `ErrStorageFull` 表示超出 |
| `Release(size int64, cat StorageCategory)` | 释放空间，减少对应分类计数和 totalUsage |
| `SetMaxBytes(n int64)` | 运行时动态调整上限 |
| `MaxBytes() int64` | 返回当前上限 |
| `Usage() int64` | 返回当前总使用量 |
| `UsageByCategory() map[StorageCategory]int64` | 返回分类统计 |
| `ScanAndRecalculate()` | 全量扫描 `uploads_dir` 重新统计，受 `scanMu` 保护 |

**启动扫描逻辑：**

1. `filepath.WalkDir` 遍历 `uploads_dir`
2. 只统计普通文件（`IsRegular()`），目录本身不计入
3. 按路径前缀分类：包含 `.__chunked__/` → CategoryChunked，包含 `.__versions__/` → CategoryVersions，包含 `.__cloud__/` → CategoryCloud，其余 → CategoryUserFiles
4. 跳过 `.checksums.json` 等元数据文件
5. 更新各分类计数器和 totalUsage

**定时校准：** 启动一个 goroutine，每 30 分钟执行一次 `ScanAndRecalculate()`，用 `scanMu` 防止并发。发现漂移时以扫描结果为准（`atomic.Store` 覆盖）。

**集成点：** 所有写入路径（upload、upload/complete、mkdir、archive 恢复、云端下载完成）在写入前调用 `TryReserve`，删除路径（delete、rmdir、batch/delete）在删除后调用 `Release`。

### 3.3 API 端点

**扩展 `GET /api/stats`** 响应，新增存储空间字段：

```json
{
  "max_storage_bytes": 10737418240,
  "storage_usage": 5242880000,
  "storage_user_files": 4000000000,
  "storage_chunked": 500000000,
  "storage_versions": 200000000,
  "storage_cloud": 542880000,
  "disk_total": 500000000000,
  "disk_free": 200000000000,
  "disk_used": 300000000000
}
```

**新增 `PUT /api/storage/config`**（Bearer auth 认证），支持运行时调整存储上限：

```json
// 请求体
{ "max_storage_bytes": 21474836480 }

// 响应
{ "success": true, "max_storage_bytes": 21474836480 }
```

### 3.4 哨兵错误

```go
// pkg/server/errors.go 新增
var ErrStorageFull = errors.New("storage quota exceeded")
```

Handler 返回 HTTP 507 Insufficient Storage + JSON 错误信息。

### 3.5 测试要求

- 表驱动测试：`TryReserve` 各种边界（0 大小、负值、恰好满、超限、0 上限不限制）
- 并发测试：多个 goroutine 同时 `TryReserve` + `Release`，验证 `-race` 通过
- 校准测试：`ScanAndRecalculate` 恢复计数器一致性
- 集成测试：启动带 `MaxStorageBytes` 的测试服务器，上传文件验证超限拒绝

---

## 四、阶段二：云端下载服务端

### 4.1 Downloader 接口

**新文件：`pkg/server/downloader/downloader.go`**

```go
package downloader

type ProgressFunc func(downloaded, total int64)

type Result struct {
    Size     int64  // 实际下载大小
    Checksum string // SHA-256 十六进制
}

type Downloader interface {
    // Download 从 source 下载到 destPath。
    // ctx 取消时尽早退出，保留已下载的部分。
    // onProgress 可为 nil（不关心进度）。
    Download(ctx context.Context, source string, destPath string, onProgress ProgressFunc) (*Result, error)

    // Supports 判断是否支持该 source。
    Supports(source string) bool

    // Name 返回下载器名称（如 "http"、"ftp"）。
    Name() string
}
```

### 4.2 下载器注册表

**新文件：`pkg/server/downloader/registry.go`**

```go
package downloader

import "github.com/cocomhub/sproxy/pkg/plugin"

var Registry = plugin.New[Downloader]("downloader", &HTTPDownloader{client: defaultHTTPClient})

// NewFromConfig 按配置名称创建下载器，未找到时回退到最高优先级注册实现。
func NewFromConfig(name string) Downloader {
    if name == "" {
        name = "http"
    }
    d, ok := Registry.Get(name)
    if !ok {
        // 回退到 Active()（最高优先级已注册实现）
        return Registry.Active()
    }
    return d
}
```

### 4.3 HTTP 下载器

**新文件：`pkg/server/downloader/http_downloader.go`**

```go
type HTTPDownloader struct {
    client *http.Client
}

func (d *HTTPDownloader) Supports(source string) bool {
    u, err := url.Parse(source)
    return err == nil && (u.Scheme == "http" || u.Scheme == "https")
}

func (d *HTTPDownloader) Name() string { return "http" }
```

**Download 实现要点：**

1. 创建 HTTP GET 请求（带 ctx）
2. 发送 HEAD 先获取 Content-Length（用于进度和 Reserve 预估）
3. 若目标文件已存在部分内容（`.tmp` 文件），发送 `Range: bytes=<existingSize>-` 续传
4. 流式写入 `destPath`（临时文件），每 1 MiB 调用 `onProgress`
5. 下载完成后计算 SHA-256（流式读取文件）
6. 返回 `Result{Size, Checksum}`

### 4.4 CloudDownloadManager

**新文件：`pkg/server/cloud_download.go`**

```go
type CloudTask struct {
    ID         string    `json:"id"`
    URL        string    `json:"url"`
    Method     string    `json:"method"`     // "url" | "upload"
    Filename   string    `json:"filename"`   // 云端存储文件名
    Status     string    `json:"status"`     // pending | downloading | completed | failed | cancelled
    TotalSize  int64     `json:"total_size"` // -1 表示未知
    Downloaded int64     `json:"downloaded"`
    Checksum   string    `json:"checksum"`
    Error      string    `json:"error"`
    CreatedAt  time.Time `json:"created_at"`
    UpdatedAt  time.Time `json:"updated_at"`
    ExpiresAt  time.Time `json:"expires_at"`
}

type CloudDownloadManager struct {
    tasks         map[string]*CloudTask
    mu            sync.RWMutex
    uploadsDir    string
    cloudDir      string                   // uploadsDir/.__cloud__/
    persistDir    string                   // uploadsDir/.__downloads__/
    storage       *StorageManager
    downloader    downloader.Downloader
    checksumStore ChecksumStoreIface
    logger        *slog.Logger
    semaphore     chan struct{}            // 并发控制
    syncThreshold int64                    // 同步模式阈值
    taskTTL       time.Duration
    failedTaskTTL time.Duration
}
```

**任务状态流转：**

```
pending → downloading → completed → (客户端删除后清理)
                │             │
                ├→ failed     └→ expired（超时自动清理）
                └→ cancelled
```

**并发控制：** 用带缓冲 channel 做信号量，容量为 `cloud_max_concurrent`（默认 3）。

**持久化：**

- 每个任务一个 JSON 文件：`.__downloads__/<task-id>.json`
- 状态变更触发持久化：`pending→downloading`、`downloading→completed`、`downloading→failed`、`→cancelled`
- 进度更新不持久化（仅内存），每 30s 批量持久化一次

**启动恢复：** 扫描 `.__downloads__/` 恢复所有任务，`pending`/`downloading` 重新入队。

**过期清理：** 后台 goroutine 每 5 分钟扫描一次，清理过期任务：
- 已完成任务：保留 `cloud_task_ttl`（默认 24h）
- 失败/取消任务：保留 `cloud_failed_task_ttl`（默认 1h）

### 4.5 路由

| 方法 | 路径 | Handler | 说明 |
|---|---|---|---|
| POST | `/api/cloud/download` | createCloudDownload | 创建云端下载任务 |
| GET | `/api/cloud/tasks` | listCloudTasks | 列出任务（支持 `?status=xxx` 过滤） |
| GET | `/api/cloud/tasks/{id}` | getCloudTask | 查询单个任务 |
| POST | `/api/cloud/tasks/{id}/cancel` | cancelCloudTask | 取消任务 |
| DELETE | `/api/cloud/tasks/{id}` | deleteCloudTask | 删除已完成/失败任务及文件 |

### 4.6 同步/异步切换逻辑

**创建任务时判断模式：**

```
POST /api/cloud/download  {url, filename?}
  │
  ├── 1. HEAD <url> 获取 Content-Length
  ├── 2. 解析或生成 filename
  ├── 3. TryReserve 预留空间（预估大小或默认值）
  ├── 4. 创建 CloudTask
  ├── 5. 判断模式：
  │     Content-Length < syncThreshold → 同步
  │     Content-Length >= syncThreshold → 异步
  │     Content-Length 未知 → 异步
  │
  ├── 同步模式：
  │     └── downloadSync(w, r, task)
  │           ├── goroutine 执行下载（监听 r.Context()）
  │           ├── select { done, ctx.Done() }
  │           ├── done → 写入 JSON 响应，返回文件信息
  │           └── ctx.Done() → 客户端断开
  │                 ├── 下载 goroutine 切换到 background context 继续
  │                 ├── task.Status = "downloading"（转为异步）
  │                 └── 不写入响应（连接已断）
  │
  └── 异步模式：
        └── go downloadAsync(task)
        └── 立即返回 {task_id, status: "pending"}
```

### 4.7 配置

```go
// pkg/server/config.go 新增字段
type Config struct {
    // ...
    MaxStorageBytes    int64  `yaml:"max_storage_bytes"`     // 存储上限，默认 0
    CloudSyncThreshold int64  `yaml:"cloud_sync_threshold"`  // 同步阈值，默认 20 MiB
    CloudDownloader    string `yaml:"cloud_downloader"`       // 下载器名称，默认 "http"
    CloudTaskTTL       string `yaml:"cloud_task_ttl"`         // 完成任务保留时间，默认 "24h"
    CloudFailedTaskTTL string `yaml:"cloud_failed_task_ttl"`  // 失败任务保留时间，默认 "1h"
    CloudMaxConcurrent int    `yaml:"cloud_max_concurrent"`    // 最大并发下载数，默认 3
}
```

### 4.8 存储目录布局

```
<uploads_dir>/
  .__cloud__/                    # 云端下载文件存储
    <task-id>/                   # 按任务 ID 隔离（支持同名文件）
      <filename>                 # 已完成下载的文件
  .__downloads__/                # 任务持久化
    <task-id>.json               # 单任务元数据
    <task-id>.tmp                # 下载中临时文件
```

### 4.9 测试要求

- **Downloader 接口测试**：`Supports` 各种 URL scheme，`Download` 正常下载、中断续传、ctx 取消、进度回调
- **Registry 测试**：注册/查找/优先级/回退到内置
- **CloudDownloadManager 测试**：创建任务、同步完成、异步完成、断线转异步、并发控制、任务持久化与恢复、过期清理
- **Handler 集成测试**：用 `httptest.Server` 模拟外部 URL，走完整请求-响应链路
- **StorageManager 集成测试**：TryReserve + 下载完成后的实际大小修正 + Release 清理
- 所有测试基于标准库（不使用 testify/gomega），监听 `127.0.0.1`

---

## 五、阶段三：sclient 客户端

### 5.1 新增命令

**新文件：`cmd/sclient/cloud_download.go`**

```
sclient cloud-download <url> [flags]
```

**Flags：**

| Flag | 默认值 | 说明 |
|---|---|---|
| `--output`, `-o` | URL 提取的文件名 | 本地输出文件名 |
| `--force-async` | false | 强制异步模式 |
| `--no-cleanup` | false | 下载后不删除云端副本 |
| `--poll-interval` | 2s | 异步模式轮询间隔 |

### 5.2 客户端流程

```
sclient cloud-download <url>
  │
  ├── 1. POST /api/cloud/download {url}
  │       │
  │       ├── 同步完成 → 返回 {filename, checksum, size}
  │       │     └── 跳到步骤 3
  │       │
  │       └── 异步模式 → 返回 {task_id, status: "pending"}
  │             └── 跳到步骤 2
  │
  ├── 2. 轮询 GET /api/cloud/tasks/{id}（每 2s）
  │       │
  │       ├── downloading → 打印进度条
  │       ├── completed  → 跳到步骤 3
  │       └── failed     → 打印错误退出
  │
  ├── 3. 从云端下载到本地
  │     GET /download?filename=.__cloud__/<task-id>/<filename>
  │     → 保存到当前目录（受 cd 命令影响）
  │
  ├── 4. 校验 checksum
  │     本地 SHA-256 == 服务端返回的 checksum？
  │     ├── 一致 → 步骤 5
  │     └── 不一致 → 报错，保留云端文件，退出
  │
  └── 5. 清理云端
        POST /delete?filename=.__cloud__/<task-id>/<filename>  (X-File-Checksum)
        DELETE /api/cloud/tasks/{id}
        → 完成，打印 "Downloaded <filename> (<size>)"
```

### 5.3 同步→异步断线处理

```
同步请求中...
  │
  ├── 正常收到响应 → 同步完成
  │
  └── 超时 / 连接错误（unexpected EOF / context deadline exceeded）
         │
         ├── 1. 尝试从响应体解析 task_id
         │     → 成功 → 进入轮询模式
         │
         └── 2. 无法获取 task_id
               → GET /api/cloud/tasks（按 URL 匹配）
               → 找到对应任务 → 进入轮询模式
```

### 5.4 测试要求

- 表驱动测试：flag 解析、URL 校验、文件名提取
- Mock server 测试：同步完成、异步完成、断线转异步、checksum 校验失败、清理失败
- 全局状态隔离：`t.Cleanup` 恢复 `currentDir`、`cfgFile` 等包级变量
- 配置隔离：通过 `--config` 指向临时配置文件

---

## 六、阶段四：Web UI

### 6.1 新增页面

在现有 Web UI 中新增「云端下载」标签页。

**UI 组件：**

- **输入区**：URL 输入框 + 「开始下载」按钮
- **任务列表**：表格展示所有任务（ID、URL、文件名、状态、进度、大小、创建时间）
- **进度条**：`downloading` 状态显示下载进度
- **操作按钮**：
  - 已完成 →「下载到本地」
  - 失败 →「重试」
  - 进行中 →「取消」
  - 已完成/失败 →「删除」
- **存储空间指示器**：顶部显示 `当前使用 / 上限` 进度条

### 6.2 测试要求

- 复用现有 Web UI E2E 测试框架（`web/e2e/`）
- 测试场景：创建任务、查看列表、取消任务、下载到本地、清理

---

## 七、通用技术约束

### 7.1 依赖策略

- **严格标准库**：不引入任何第三方依赖（除现有 `gopkg.in/yaml.v3`、`cobra`、`viper`、`xdg`）
- 文件操作使用 `os`、`io`、`path/filepath`
- JSON 序列化使用 `encoding/json`
- HTTP 使用 `net/http`
- 并发控制使用 `sync`、`sync/atomic`

### 7.2 测试规范（遵循项目 CLAUDE.md）

- 纯标准库测试，不使用 testify/gomega
- HTTP 测试监听 `127.0.0.1`
- Windows 兼容
- 全局状态隔离（`t.Cleanup`）
- Viper 隔离（`viper.New()`）
- TDD 原则：先写测试，再写实现

### 7.3 编码规范

- 日志统一 `log/slog`
- 错误包装 `fmt.Errorf("...: %w", err)`
- 文件 UTF-8 without BOM
- SPDX 许可证头

---

## 八、里程碑与交付物

| 阶段 | 交付物 | 验收标准 |
|---|---|---|
| 阶段零 | `pkg/plugin/` 迁移 | 现有 xfer/hub 测试全部通过，import 路径更新 |
| 阶段一 | StorageManager | 单元测试 + 并发测试 + 集成测试通过，覆盖率 ≥ 70% |
| 阶段二 | 云端下载服务端 | 所有 handler 测试通过，下载器可插拔，同步/异步切换正确 |
| 阶段三 | sclient 客户端 | 命令测试通过，同步/异步/断线恢复场景覆盖 |
| 阶段四 | Web UI | E2E 测试通过，UI 交互正确 |