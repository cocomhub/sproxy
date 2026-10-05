<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# v0.23.0..master 全功能 Web UI + 通用审计日志包 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 v0.23.0..master 期间交付的功能（secretdata/secrets 加密卷、Range 关键帧播放、pikpak/hybrid、cloud transfer、egress）补齐 Web UI（加密卷明文目录浏览、播放器、卷管理、三行为透传、审计展示），并新增通用审计日志包 `pkg/audit`（跨层 span 埋点、独立文件每行落盘、Save/Get、可归档）。

**架构：** 后端新增 `pkg/audit`（Logger 嵌入 ctx 跨层 span + FileSink 每任务独立文件 + Archiver 接口预留），云下载/转存/加密阶段埋点；`/api/backends` 扩展 per-type schema 驱动卷创建表单；`/api/files?volume=` 透传外部卷 ListDir（明文视图）；前端新增卷下拉切换、video-player、卷管理面板、cloud.js 三行为透传、审计面板。

**技术栈：** Go 标准库 + `log/slog`（后端）；原生 JS（`*-format.js` 纯函数 + `node --test` 单测）——无第三方断言库、无前端框架。

**规格：** `docs/superpowers/specs/2026-10-06-audit-webui-design.md`（本计划依据；执行者先读规格再读本计划）。

## 全局约束

- **安全红线**：双口令派生仅 CLI（浏览器无 scrypt）——Web 只做 random/import/export；`Row.Meta`/审计持久化不得含原始凭据/口令/secret 明文；审计文件权限 0600。
- **审计数据不泄卷**：无 ACL 用户列表不见 + 下载 404 fail-closed；`?volume=` 指定无权卷 → 404（不泄卷存在性）。
- **防成环**：封装卷创建时底层卷闭环检测，fail-closed。
- **三行为语义空洞**：`download_local=false` + 无 transfer + `save=false` → 服务端拒绝创建。
- **前端约定**：纯函数放 `*-format.js`（顶层 function + module.exports）；跨文件隐式全局顶部 `// global:` + typeof 守卫；新 JS 同时进 Makefile `web-test` 的 node --check 与 node --test 双清单；动态 HTML 一律 `var(--…)` 禁内联亮色 hex；暗色 token 三块（:root/@media/[data-theme]）同步。
- **Go 约定**：错误优先 `fmt.Errorf("...: %w", err)`；哨兵错误 `var ErrXxx = errors.New(...)`；新测试默认 `t.Parallel()`（无法并发者显式登记，见 sproxy CLAUDE.md R18）；禁止 `http.DefaultClient`/共享 Transport；127.0.0.1 回环绑定。
- **命名**：文件 `snake_case.go`；构造函数 `New`+类型名；类型全名与规格 §4.1 一致。

## 审查重点（Review Focus）

1. **底层卷成环**：`secretdata→secrets→secretdata` 循环。创建封装卷时目标底层卷链必须无环，成环 → 400 拒绝，不落盘。
2. **无权卷列表**：`GET /api/files?volume=<无权外部卷>` → 404，不泄卷存在性（与既有 `?volume=` ACL 行为一致）。
3. **审计不含明文 secret**：`Row.Meta` 与 `/api/audit` 响应不得出现 64hex secret / 口令 / 下载凭据；落盘 0600。
4. **播放 Range 越界**：播放器发超范围 Range → 206/416 正常处理（`ServeContent` 语义），不 panic、不整文件泄漏。
5. **三行为空洞**：任务创建 `{download_local:false, transfer:null, save:false}` → 400。
6. **审计归档默认关**：未配置 `audit.archive.*` 时绝不写归档文件；开启需选卷+路径。

---

### 任务 1：`pkg/audit` 核心 Logger（Type/Level/Dimension/Row/NewScope/Begin/End/Save/Get/ctx）

**文件：**
- 创建：`pkg/audit/audit.go`
- 创建：`pkg/audit/audit_test.go`

- [ ] **步骤 1：编写失败测试 `pkg/audit/audit_test.go`**

```go
func TestLogger_BeginEnd_Dur(t *testing.T) {
    t.Parallel()
    var got []Row
    sink := &memSink{append: func(r Row) { got = append(got, r) }}
    l, err := NewScope("task-1", ScopeOpts{Sink: sink, NodeID: "n1"})
    if err != nil { t.Fatal(err) }
    s := l.Begin("download")
    time.Sleep(2 * time.Millisecond)
    s.End(ByteKV(Bytes: 1024), BandwidthKV(BandwidthBps: 4096))
    if err := l.Flush(); err != nil { t.Fatal(err) }
    if len(got) != 1 { t.Fatalf("want 1 row, got %d", len(got)) }
    r := got[0]
    if r.Type != TypeDownload || r.Step != "download" || r.Dim != DimTask {
        t.Fatalf("bad row: %+v", r)
    }
    if r.DurMS < 1 { t.Fatalf("dur not recorded: %+v", r) }
    if r.Bytes != 1024 || r.BandwidthBps != 4096 { t.Fatalf("kv lost: %+v", r) }
}

func TestLogger_SaveGet_CrossFunc(t *testing.T) {
    t.Parallel()
    l := mustLogger(t)
    ctx := WithContext(context.Background(), l)
    l.Save(ctx, "plain_bytes", int64(1000))
    v, ok := l.Get(ctx, "plain_bytes")
    if !ok || v != int64(1000) { t.Fatalf("get failed: %v ok=%v", v, ok) }
}

func TestLogger_From_NoLogger_Nil(t *testing.T) {
    t.Parallel()
    if From(context.Background()) != nil { t.Fatal("From on empty ctx must be nil") }
}

func TestLogger_With_Dimension(t *testing.T) {
    t.Parallel()
    l := mustLogger(t)
    sys := l.With(DimSystem)
    if sys == nil || sys == l { t.Fatal("With must return distinct logger") }
    if sys.dim != DimSystem { t.Fatalf("dim not set: %v", sys.dim) }
}

func TestLogger_Err_LevelError(t *testing.T) {
    t.Parallel()
    var got []Row
    l := mustLoggerSink(t, &memSink{append: func(r Row) { got = append(got, r) }})
    l.Err(errors.New("boom"), "encrypt")
    if len(got) != 1 || got[0].Level != LevelError || got[0].Err != "boom" {
        t.Fatalf("bad error row: %+v", got)
    }
}
```

（`memSink`/`mustLogger`/`mustLoggerSink`/`ByteKV`/`BandwidthKV` 为测试辅助，定义在同文件底部，实现 `Sink` 接口。）

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：编译失败，`undefined: NewScope / TypeDownload / ...`（包不存在）。

- [ ] **步骤 3：实现 `pkg/audit/audit.go`**

核心签名（函数体由实现者写，规格 §4.1 已定类型）：

```go
type Type string
const (
    TypeAPI       Type = "api"
    TypeDownload  Type = "download"
    TypeTransfer  Type = "transfer"
    TypeEncrypt   Type = "encrypt"
    TypeResource  Type = "resource"
    TypeSensitive Type = "sensitive"
    TypeError     Type = "error"
)
type Level string
const (LevelDebug Level = "debug"; LevelInfo Level = "info"; LevelWarn Level = "warn"; LevelError Level = "error")
type Dimension string
const (DimTask Dimension = "task"; DimSystem Dimension = "system")

type Row struct {
    Type         Type      `json:"type"`
    Level        Level     `json:"level"`
    Dim          Dimension `json:"dim"`
    Step         string    `json:"step"`
    StartMS      int64     `json:"start_ms"`
    DurMS        int64     `json:"dur_ms"`
    Err          string    `json:"err,omitempty"`
    BandwidthBps int64     `json:"bw_bps,omitempty"`
    Bytes        int64     `json:"bytes,omitempty"`
    Meta         any       `json:"meta,omitempty"`
    NodeID       string    `json:"node,omitempty"`
}

type Sink interface {
    Append(r Row) error
    Recent(f Filter) []Row
    Close() error
}

type Filter struct {
    Type  Type
    Level Level
    Dim   Dimension
    Step  string
}

type ScopeOpts struct {
    Sink    Sink
    Archive Archiver // 默认 nil = 不归档
    Start   time.Time
    NodeID  string
}

type Logger struct { /* scopeID, sink, start, dim, kvStore (map[string]any), mu */ }

func NewScope(scopeID string, opts ScopeOpts) (*Logger, error)
func (l *Logger) With(d Dimension, kv ...any) *Logger  // 复制派生（同 sink/scope，换 dim）
func (l *Logger) Begin(step string, kv ...any) *Span
func (s *Span) End(kv ...any)
func (s *Span) Fail(err error)
func (l *Logger) Log(t Type, lvl Level, step string, kv ...any)
func (l *Logger) Err(err error, step string, kv ...any)
func (l *Logger) Save(ctx context.Context, key string, v any) error
func (l *Logger) Get(ctx context.Context, key string) (any, bool)
func (l *Logger) Flush() error
func (l *Logger) Close() error
func WithContext(ctx context.Context, l *Logger) context.Context
func From(ctx context.Context) *Logger  // 无则 nil；nil Logger 方法必须安全 no-op
```

要点：`Begin` 记录 `StartMS`（距作用域起点）；`Span.End` 填 `DurMS` 并 `Append`；`Save/Get` 存 `map[string]any`（幂等）；`With` 返回同 scope 新 Logger 但 `dim` 已换；**nil Logger 上所有方法必须 no-op 不 panic**（`From` 返回 nil 时链式调用安全）。kv 解析：`kv ...any` 按 `key,value` 对解析，`ByteKV`/`BandwidthKV` 辅助返回 `(string,any)` 对；无法成对时忽略该 kv。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：PASS（含 `-race` 默认开启）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/audit/audit.go pkg/audit/audit_test.go
git commit -m "feat(audit): 通用审计日志包核心——scope Logger + span 步骤计时 + Save/Get + 多维度 With"
```

---

### 任务 2：`pkg/audit` FileSink（每任务独立文件 + 旋转 + 查询）与 Archiver 接口

**文件：**
- 创建：`pkg/audit/store.go`
- 创建：`pkg/audit/store_test.go`

- [ ] **步骤 1：编写失败测试 `pkg/audit/store_test.go`**

```go
func TestFileSink_AppendRecent(t *testing.T) {
    t.Parallel()
    dir := t.TempDir()
    s, err := NewScopeSink(dir, "task-1")
    if err != nil { t.Fatal(err) }
    t.Cleanup(func() { _ = s.Close() })
    if err := s.Append(Row{Type: TypeDownload, Step: "download", DurMS: 5}); err != nil { t.Fatal(err) }
    if err := s.Append(Row{Type: TypeError, Level: LevelError, Step: "encrypt", Err: "x"}); err != nil { t.Fatal(err) }
    rows := s.Recent(Filter{})
    if len(rows) != 2 { t.Fatalf("want 2, got %d: %+v", len(rows), rows) }
    if f := s.Recent(Filter{Type: TypeError}); len(f) != 1 { t.Fatalf("filter type: %+v", f) }
    // 落盘格式：独立文件每行 JSON
    raw, err := os.ReadFile(filepath.Join(dir, "task-1.audit.log"))
    if err != nil { t.Fatal(err) }
    if got := strings.Count(string(raw), "\n"); got != 2 { t.Fatalf("want 2 lines, got %d", got) }
}

func TestFileSink_MultiTaskIsolation(t *testing.T) {
    t.Parallel()
    dir := t.TempDir()
    a, _ := NewScopeSink(dir, "task-a")
    b, _ := NewScopeSink(dir, "task-b")
    t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
    _ = a.Append(Row{Type: TypeDownload})
    _ = b.Append(Row{Type: TypeTransfer})
    fa := a.Recent(Filter{})
    fb := b.Recent(Filter{})
    if len(fa) != 1 || fa[0].Type != TypeDownload { t.Fatalf("task-a polluted: %+v", fa) }
    if len(fb) != 1 || fb[0].Type != TypeTransfer { t.Fatalf("task-b polluted: %+v", fb) }
}

func TestScopeSink_FilePerm0600(t *testing.T) {
    if runtime.GOOS == "windows" { t.Skip("0600 仅 Unix") }
    dir := t.TempDir()
    s, _ := NewScopeSink(dir, "task-1")
    t.Cleanup(func() { _ = s.Close() })
    _ = s.Append(Row{Type: TypeDownload})
    fi, err := os.Stat(filepath.Join(dir, "task-1.audit.log"))
    if err != nil { t.Fatal(err) }
    if fi.Mode().Perm() != 0o600 { t.Fatalf("perm want 0600, got %v", fi.Mode().Perm()) }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：编译失败，`undefined: NewScopeSink`。

- [ ] **步骤 3：实现 `pkg/audit/store.go`**

```go
// NewScopeSink 创建每任务独立文件审计 sink：<dir>/<scopeID>.audit.log，每行一行 JSON Row。
func NewScopeSink(dir, scopeID string) (*FileSink, error)
type FileSink struct{ /* file *os.File, mu, filter support */ }
func (s *FileSink) Append(r Row) error      // 一行 JSON + \n，行尾 sync
func (s *FileSink) Recent(f Filter) []Row    // 读回文件解析 + 过滤
func (s *FileSink) Close() error
func (s *FileSink) Path() string

// Archiver 把作用域审计行归档到某卷（未来扩展；默认 nil = 不归档）。
type Archiver interface {
    Archive(ctx context.Context, scopeID string, rows []Row) error
}
```

要点：文件 `os.CreateTemp`-free 直接 `os.OpenFile(dir/scopeID+".audit.log", O_CREATE|O_WRONLY|O_APPEND, 0600)`；`Append` 行内 JSON 用 `encoding/json.Marshal`；`Recent` 逐行 `json.Unmarshal` 到 `Row` 再按 `Filter` 精确相等过滤；`Close` 幂等。目录不存在时 `os.MkdirAll`（0700）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/audit/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/audit/store.go pkg/audit/store_test.go
git commit -m "feat(audit): FileSink 每任务独立文件每行 JSON 落盘 + 旋转/查询 + Archiver 归档接口"
```

---

### 任务 3：云下载/转存/加密阶段埋点（接入 pkg/audit）

**文件：**
- 修改：`pkg/cloud/manager.go`（`CloudTask` 增 `Audit []audit.Row` json 字段）
- 修改：`pkg/cloud/manager_task.go`（下载阶段 Begin/End）
- 修改：`pkg/cloud/transfer.go`（转存阶段）
- 修改：`pkg/volume/secretdata/secretdata.go`（`WriteFile` 加密耗时 span）
- 修改：`pkg/server/cloud_download_handler.go`（任务详情响应含 audit）
- 测试：`pkg/cloud/audit_integration_test.go`（新建）

- [ ] **步骤 1：编写失败测试 `pkg/cloud/audit_integration_test.go`**

```go
func TestCloudTask_Audit_Collected(t *testing.T) {
    t.Parallel()
    // 用既有 test infra（pkg/cloud 内已有 manager_test 的测试注册/下载器桩）构造一次
    // 下载 + 转存到本地卷，任务完成后断言：
    //   task.Audit 非空；含 Type=download 的行（DurMS>=0）；含 Type=transfer 的行；
    //   加密卷转存含 Type=encrypt 行（Encrypted=true，CipherBytes>0）。
    // 注：若加密卷 e2e 依赖较重，本测试至少覆盖 download + transfer 两行；encrypt 行
    // 由任务 4 或独立 secretdata 单测覆盖。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：FAIL（`task.Audit` 不存在或为空）。

- [ ] **步骤 3：实现埋点**

- `CloudTask` 增 `Audit []audit.Row json:"audit,omitempty"`（`manager.go:40-90` 结构内）。
- `manager_task.go` 下载循环：`audit.From(ctx)` 拿 Logger（nil 安全）→ `Begin("download")`→下载→`End(ByteKV(Bytes: n), BandwidthKV(...))`；任务级 `NewScope(task.ID, ScopeOpts{NodeID})`，经 `WithContext` 注入 task ctx；完成/失败时 `Flush` 且把 `rows := sink.Recent(Filter{})` 复制进 `task.Audit`（持久化随任务）。
- `transfer.go` `transferOnce`：`Begin("transfer")`→`End(ByteKV(...))`；若目标为 secretdata（FS 断言 `TypeEncrypt` 路径），`Begin("encrypt")` 在 `WriteFile` 外包计时 + 明文/密文对比字节。
- `cloud_download_handler.go` 任务详情响应已含 `CloudTask` JSON → 自动带 `audit` 字段（无需改响应结构，验证即可）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：PASS。

- [ ] **步骤 5：回归全部 cloud 测试**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/cloud/`
预期：PASS（既有任务无回归）。

- [ ] **步骤 6：Commit**

```bash
git add pkg/cloud/manager.go pkg/cloud/manager_task.go pkg/cloud/transfer.go pkg/volume/secretdata/secretdata.go pkg/server/cloud_download_handler.go pkg/cloud/audit_integration_test.go
git commit -m "feat(audit): 云下载/转存/加密阶段 span 埋点 + CloudTask 持久化审计行"
```

---

### 任务 4：secretdata 加密耗时 span + 明密文对比（补全任务 3 的 encrypt 覆盖）

**文件：**
- 修改：`pkg/volume/secretdata/secretdata.go`（`WriteFile` 外包 audit span；`OpenRangeRead` 内 `decryptRangeBlocklet` 计时）
- 修改：`pkg/cloud/audit_integration_test.go`（加密卷转存断言补全）

- [ ] **步骤 1：编写失败测试（扩展 `TestCloudTask_Audit_Collected` 加密断言）**

在 `TestCloudTask_Audit_Collected` 中增：`transfer` 到 secretdata 卷 → 断言存在 `TypeEncrypt` 行且 `Bytes>0`（明文体积）、`Meta` 含 `cipher_bytes`。

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/`
预期：FAIL（无 encrypt 行）。

- [ ] **步骤 3：实现**

`secretdata.go` 的 `WriteFile`（`:555`）外包：`From(ctx).Begin("encrypt")` → 写后 `End(ByteKV(Bytes: size), KV("cipher_bytes", 已写密文字节))`；`decryptRangeBlocklet`（`:539`）同 span。加密算法/档位放入 `Meta`（`algorithm`）。nil Logger 安全。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run TestCloudTask_Audit_Collected ./pkg/cloud/ && go test -count=1 ./pkg/volume/secretdata/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/secretdata/secretdata.go pkg/cloud/audit_integration_test.go
git commit -m "feat(audit): secretdata 加密耗时 span + 明密文字节对比埋点"
```

---

### 任务 5：`registry.SchemaProvider` + `GET /api/backends` 扩展 per-type schema

**文件：**
- 修改：`pkg/volume/registry/backend.go`（新增接口 + `BackendSchemas()`）
- 修改：`pkg/server/backends_api.go`（响应结构 + 组装）
- 测试：`pkg/server/backends_api_test.go`（新建）；`pkg/volume/registry/backend_test.go`（增）

- [ ] **步骤 1：编写失败测试**

```go
// pkg/server/backends_api_test.go
func TestBackendsAPI_SchemaDriven(t *testing.T) {
    t.Parallel()
    // 注册一个实现了 SchemaProvider 的假后端（type "audit-test-schema"，见 backend_test.go 的 helper 或
    // 本文件内注册），GET /api/backends 断言响应含该 type 的 category/fields 数组。
    // 未实现 SchemaProvider 的 type 只返回 {type, category}（fields 缺省 []）。
}
// pkg/volume/registry/backend_test.go 增:
func TestBackendSchemas_Provider_Only(t *testing.T) {
    t.Parallel()
    // 注册实现 SchemaProvider 的假后端；BackendSchemas() 返回含其 fields；未实现者 fields 为空。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/server/ -run TestBackendsAPI_SchemaDriven ./pkg/volume/registry/ -run TestBackendSchemas_Provider_Only`
预期：FAIL（`BackendSchemas` undefined / 响应无 category）。

- [ ] **步骤 3：实现**

`pkg/volume/registry/backend.go` 增：

```go
// FieldSchema 声明后端创建表单的一个字段（前端 schema 驱动渲染）。
type FieldSchema struct {
    Key          string   `json:"key"`
    Label        string   `json:"label"`
    Type         string   `json:"type"`      // text|volume-select|enum|bool|number
    Required     bool     `json:"required"`
    Options      []string `json:"options,omitempty"`
    AllowWrapper bool     `json:"allow_wrapper,omitempty"` // volume-select 是否可选封装卷底层
}
// SchemaProvider 是 ExternalBackend 的可选扩展：声明本后端的创建表单 schema。
type SchemaProvider interface { Schema() []FieldSchema }

// BackendSchemas 返回已注册后端的 type→(category, fields) 映射；category 由协议推导
// （"secrets"/"secretdata"/"egress" → "wrapper"；其余外部 → "linked"；本地 → "mt-local"）。
func BackendSchemas() []BackendSchemaInfo
type BackendSchemaInfo struct {
    Type     string        `json:"type"`
    Category string        `json:"category"`
    Label    string        `json:"label,omitempty"`
    Fields   []FieldSchema `json:"fields"`
}
```

`pkg/server/backends_api.go` 的 `backendsHandler` 改返回 `BackendSchemas()`（替代 `[]string`）；`Label` 留空缺省（前端按 type 显示）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 ./pkg/server/ -run TestBackendsAPI_SchemaDriven && go test -count=1 ./pkg/volume/registry/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/registry/backend.go pkg/server/backends_api.go pkg/server/backends_api_test.go pkg/volume/registry/backend_test.go
git commit -m "feat(volumes): GET /api/backends 扩展 per-type schema（SchemaProvider 驱动建卷表单）"
```

---

### 任务 6：`POST /api/volumes/user` 封装卷底层卷校验（防成环）

**文件：**
- 修改：`pkg/server/user_volume_api.go`（`createUserVolumeHandler` 加 schema 校验 + 防环）
- 修改：`pkg/server/user_volume_api_test.go`（增用例）

- [ ] **步骤 1：编写失败测试**

```go
func TestCreateUserVolume_Wrapper_RequiresTarget(t *testing.T) {
    t.Parallel()
    // POST /api/volumes/user {type:"secretdata", name:"v1", extra:{}} → 400（缺底层卷）
    // POST /api/volumes/user {type:"secretdata", name:"v2", extra:{target:"nonexistent"}} → 400（底层卷不存在）
}
func TestCreateUserVolume_Wrapper_CycleRejected(t *testing.T) {
    t.Parallel()
    // 场景：v1=secretdata 已存在（底层 local）；再创建 secrets{target:v1} 合法；
    // 再创建 secretdata{target:v1-secrets 循环引用}（如 secrets.target==secretdata 且 secretdata.target==secrets）
    // → 400 防环，不落盘。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestCreateUserVolume' ./pkg/server/`
预期：FAIL（缺 target 仍成功 / 环仍成功）。

- [ ] **步骤 3：实现**

`createUserVolumeHandler` 在 `registry.NewBackend` 前增加：
1. `typ` 的 schema 中 `volume-select` 必填字段逐一校验 `Extra[key]` 非空；
2. `AllowWrapper=false` 的 volume-select 目标必须是非 wrapper 卷；`AllowWrapper=true` 允许 wrapper；
3. **防环**：沿 `target` 链向上遍历（`volSet.External(name)` 的 `Extra["target"]` 递归），若回落到自身 → 400 `errVolumeCycle`；
4. 复用既有 `UserVolume` store（Create 重名 409 / Delete 引用 409 保留）。

新增哨兵错误：`var errVolumeCycle = errors.New("volume: 底层卷成环引用")`（`user_volume_api.go`）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestCreateUserVolume' ./pkg/server/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/user_volume_api.go pkg/server/user_volume_api_test.go
git commit -m "feat(volumes): 封装卷建卷校验——schema 必填字段 + 底层卷存在性 + 成环 fail-closed"
```

---

### 任务 7：`/api/files?volume=` 外部卷 ListDir 透传（加密卷明文目录浏览）

**文件：**
- 修改：`pkg/files/read_ops.go`（`List` 增外部卷分支）
- 修改：`pkg/server/files_api_test.go` 或 `pkg/files/read_test.go`（增用例）
- 测试：`pkg/files/external_list_test.go`（新建）

- [ ] **步骤 1：编写失败测试**

```go
func TestList_ExternalVolume_PlainMeta(t *testing.T) {
    t.Parallel()
    // 构造含外部卷（fake backend，实现 ListDir 返回明文 Entry：dir/ + movie.mp4 2048B）的 handler；
    // GET /api/files?volume=<ext卷>&subdir= 断言响应含 movie.mp4（明文大小），且不含分片名/加密 meta。
}
func TestList_ExternalVolume_Unauthorized_404(t *testing.T) {
    t.Parallel()
    // 无 ACL 的 owner 请求 ?volume=<他人外部卷> → 404（不泄卷存在性，与既有 ?volume= 行为一致）。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestList_External' ./pkg/files/`
预期：FAIL（外部卷返回空）。

- [ ] **步骤 3：实现**

`read_ops.go` 的 `List`（`:92-148`）：当 `opts.Volume != ""` 且该卷是 `registry.Set.External(name)`（外部后端）时，改走 `be.FS().ListDir(ctx, subdir)` → 把 `[]syncpkg.Entry` 转 `[]FileInfo`（`IsDir`、`Name`、`Size`、无 checksum/无 volume 字段）返回；本地卷路径零改动。`listFallback` 对未知/无权卷保持 404（不泄存在性）。文件模型 `FileInfo`（`read.go:39-49`）复用。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && go test -count=1 -run 'TestList_External' ./pkg/files/ && go test -count=1 ./pkg/server/ -run 'TestFiles|TestList'`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/files/read_ops.go pkg/files/external_list_test.go
git commit -m "feat(files): /api/files?volume= 透传外部卷 ListDir——加密卷明文目录浏览 + 无权 404"
```

---

### 任务 8：前端 cloud.js 三行为透传 + 云下载目标卷选择器

**文件：**
- 修改：`web/static/sclient/api/cloud.js`（`createDownload/createBatch/createGroup` 透传）
- 修改：`web/static/app.js`（`doSubmitCloudTasks` 传 transfer/save/download_local）
- 修改：`web/static/app-render.js`（云下载表单含目标卷选择器/路径/三行为复选框）
- 修改：`web/static/app-render.test.js`（纯函数断言）
- 修改：`Makefile`（`web-test` 已覆盖 app-render，无需新文件；cloud.js 已在清单）

- [ ] **步骤 1：编写失败测试（`app-render.test.js` 增用例）**

```js
test('cloudDownloadFormHtml includes transfer volume selector + three behaviors', () => {
  const html = appRender.cloudDownloadFormHtml({ volumes: ['local', 'vault'], current: 'vault' });
  assert.match(html, /name="transfer-volume"/);
  assert.match(html, /name="save"/);
  assert.match(html, /name="download-local"/);
  assert.match(html, /vault/);
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js`
预期：FAIL（`cloudDownloadFormHtml` undefined）。

- [ ] **步骤 3：实现**

- `cloud.js`：`createDownload(url, filename, opts)`、`createBatch(urls, opts)`、`createGroup(name, urls, opts)` 三方法把 `opts.transfer`（`{volume,path}`）、`opts.save`、`opts.downloadLocal` 并入请求体（对齐后端 `TransferSpec`/`TaskParams`，三行为正交、缺省 undefined 不发送 → 零回归）。
- `app-render.js`：新增纯函数 `cloudDownloadFormHtml({volumes, current})`——目标卷下拉（`name="transfer-volume"`，选项 `local` 等 + 空=不转存）+ 目标路径输入 + 「保留服务端副本(save)」「下载到本地(download_local)」复选框。
- `app.js` `doSubmitCloudTasks`（`:2474`）：读表单字段 → 构造 `{transfer:{volume,path}, save, download_local}` 传 `sc.cloud.createDownload/createBatch`；`doChainDownloadCloud`/`doChainDownloadCloudGroup` 同步透传。
- 空白页/未选卷：transfer 字段为 `undefined`（后端空语义 = 不转存，零回归）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js && node --check web/static/sclient/api/cloud.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/sclient/api/cloud.js web/static/app.js web/static/app-render.js web/static/app-render.test.js
git commit -m "feat(webui): 云端下载三行为透传 + 目标卷/路径选择器（transfer/save/download_local）"
```

---

### 任务 9：文件页加密卷下拉切换 + 卷徽标分类

**文件：**
- 修改：`web/static/index.html`（文件页工具栏加「卷」下拉）
- 修改：`web/static/app.js`（卷下拉填充/切换 → refreshList 带 volume）
- 修改：`web/static/app-render.js`（`buildFileRowHtml` vol-badge 加 category 类）
- 修改：`web/static/style.css`（`--badge-wrapper-*` token 三块同步）
- 修改：`web/static/app-render.test.js`

- [ ] **步骤 1：编写失败测试（`app-render.test.js`）**

```js
test('volBadge includes category class for wrapper volumes', () => {
  const html = appRender.buildFileRowHtml({ name: 'a.mp4', is_dir: false, size: 10, volume: 'vault', volume_category: 'wrapper' });
  assert.match(html, /vol-badge-wrapper/);
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js`
预期：FAIL（`volume_category` 未消费）。

- [ ] **步骤 3：实现**

- `GET /api/files?volume=` 响应 `FileInfo` 增 `VolumeCategory string json:"volume_category,omitempty"`（`read.go:39-49`，任务 7 已透传外部卷，此字段补分类）。
- `app-render.js buildFileRowHtml`：`fi.volume_category === 'wrapper'` → badge 加 `vol-badge-wrapper` 类 + 🔒图标。
- `index.html`：文件页工具栏加 `<select id="volume-filter">`（首项「全部卷」）。
- `app.js`：`initVolumeFilter` 填充（`GET /api/volumes` 全部可见卷含加密卷）→ change 调 `refreshList({volume})`；`refreshList` 带 volume 参数时 `sc.files.list(subdir, {volume})` 已支持。
- `style.css`：三块补 `--badge-wrapper-bg/--badge-wrapper-fg`（暗色校验对比度，规则：白字 ≥4.5:1）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/app-render.test.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/index.html web/static/app.js web/static/app-render.js web/static/style.css web/static/app-render.test.js pkg/files/read.go
git commit -m "feat(webui): 文件页加密卷下拉切换 + 卷徽标分类（wrapper 🔒）"
```

---

### 任务 10：视频播放器组件（video-player.js + 弹窗）

**文件：**
- 创建：`web/static/video-player.js`
- 创建：`web/static/video-player.test.js`
- 修改：`web/static/index.html`（script 引入 + 播放器弹窗容器）
- 修改：`web/static/app-render.js`（视频行「▶ 播放」按钮）
- 修改：`web/static/style.css`（弹窗样式）
- 修改：`Makefile`（web-test 双清单加 video-player）

- [ ] **步骤 1：编写失败测试 `video-player.test.js`**

```js
test('videoPlayerUrl builds /download with volume+filename', () => {
  const u = videoPlayerUrl({ filename: 'dir/movie.mp4', volume: 'vault', base: '/download' });
  assert.equal(u, '/download?filename=dir%2Fmovie.mp4&volume=vault');
});
test('videoPlayerUrl omits volume when empty', () => {
  const u = videoPlayerUrl({ filename: 'a.mp4', volume: '', base: '/download' });
  assert.equal(u, '/download?filename=a.mp4');
});
test('videoPlayerModalHtml includes video src + standard Range player', () => {
  const html = videoPlayerModalHtml({ filename: 'dir/movie.mp4', volume: 'vault' });
  assert.match(html, /<video/);
  assert.match(html, /src=/);
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/video-player.test.js`
预期：FAIL（`videoPlayerUrl` undefined）。

- [ ] **步骤 3：实现**

- `video-player.js`：纯函数 + 顶层挂浏览器全局（非 UMD，module.exports 供测试）：
  - `videoPlayerUrl({filename, volume, base})` → `/download?filename=<encodeURIComponent(slash→%2F)>[&volume=]`（`filename` 用 `encodeURIComponent`，斜杠转 `%2F` 保持路径结构——对齐服务端 `ValidateFilePath` 读法）。
  - `videoPlayerModalHtml({filename, volume})` → 弹窗：`<video controls autoplay src="...">` + 文件名标题 + 关闭按钮（`data-close`）。
- `app-render.js`：视频行（`is_dir=false` 且扩展名 ∈ video 集合）加 `<button data-video-play>`；委托在 `app.js` `initDynamicEventDelegation` 挂 `data-video-play` → `videoPlayerModalHtml` + 弹窗 DOM。
- `index.html`：`<script src="/ui/video-player.js">` 置于 app.js 之前（依赖序：video-player 先加载）；`#video-player-modal` 容器。
- `style.css`：`.video-modal` 样式 + 三块 token（用现有 `--modal-*` 变量，不新增色）。
- `Makefile` web-test 清单加 `video-player.js`（node --check）+ `video-player.test.js`（node --test）。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/video-player.test.js && node --check web/static/video-player.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/video-player.js web/static/video-player.test.js web/static/index.html web/static/app-render.js web/static/app.js web/static/style.css Makefile
git commit -m "feat(webui): 视频播放器组件——标准 Range 播放 + 加密卷视频行入口"
```

---

### 任务 11：卷管理面板（schema 动态表单 + 创建/删除）

**文件：**
- 创建：`web/static/vol-manage-format.js`
- 创建：`web/static/vol-manage-format.test.js`
- 修改：`web/static/index.html`（stats-modal 增「卷管理」tab + script）
- 修改：`web/static/app.js`（switchStatsTab 挂卷管理 tab、加载/提交/删除）
- 修改：`web/static/app-render.js`（`volumesTableHtml` 加「管理」入口）
- 修改：`Makefile`（web-test 双清单加 vol-manage-format）

- [ ] **步骤 1：编写失败测试 `vol-manage-format.test.js`**

```js
test('volManageFormHtml renders schema fields dynamically', () => {
  const html = volManageFormHtml({ type: 'secretdata', category: 'wrapper', fields: [
    { key: 'target', label: '底层卷', type: 'volume-select', required: true },
    { key: 'algorithm', label: '算法', type: 'enum', options: ['shardseal-high','shardseal-standard'] },
  ]});
  assert.match(html, /name="target"/);
  assert.match(html, /shardseal-high/);
  assert.match(html, /required/);
});
test('volManageListHtml includes type+category+delete button', () => {
  const html = volManageListHtml({ volumes: [{ name: 'vault', type: 'secretdata', category: 'wrapper', capacity: 0 }] });
  assert.match(html, /vault/);
  assert.match(html, /wrapper/);
  assert.match(html, /data-delete-volume/);
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/vol-manage-format.test.js`
预期：FAIL（undefined）。

- [ ] **步骤 3：实现**

- `vol-manage-format.js`：
  - `volManageFormHtml({type, category, fields})` → 按 `fields` 渲染（`volume-select` → 底层卷下拉（值来自 `volManageFormHtml` 第二参数 `volumes`，`allow_wrapper` 决定是否含 wrapper 卷）；`enum` → `<select>`；`text`/`bool`/`number` → 对应 input）；含 `data-type` 隐藏字段。
  - `volManageListHtml({volumes})` → 现有用户卷表 + type/category 列 + 「删除」按钮（`data-delete-volume`）。
- `app.js`：stats-modal 加「卷管理」tab → `showVolManage`：`GET /api/backends`（类型 schema）+ `GET /api/volumes/user`（我的卷）→ 渲染表单+列表；创建提交 `POST /api/volumes/user`（extra 由表单字段组装）；删除 `DELETE /api/volumes/user?name=`（409 → toast 提示「有任务引用」）。
- `index.html`：stats-modal tab 增「卷管理」；script 引入 vol-manage-format.js。
- `Makefile`：web-test 双清单加 `vol-manage-format.js`/`vol-manage-format.test.js`。
- `style.css`：复用现有表单/弹窗样式，不新增 token。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/vol-manage-format.test.js && node --check web/static/vol-manage-format.js && node --check web/static/app.js`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/vol-manage-format.js web/static/vol-manage-format.test.js web/static/index.html web/static/app.js web/static/app-render.js Makefile
git commit -m "feat(webui): 卷管理面板——/api/backends schema 驱动建卷表单 + 删除（409 引用提示）"
```

---

### 任务 12：审计展示面板（任务详情审计 + 卷加密信息）

**文件：**
- 创建：`web/static/audit-rows-format.js`
- 创建：`web/static/audit-rows-format.test.js`
- 修改：`web/static/app-render.js`（任务行点开「审计」入口 + 卷内加密信息展示）
- 修改：`web/static/app.js`（任务详情弹窗接 audit 渲染）
- 修改：`Makefile`（web-test 双清单）

- [ ] **步骤 1：编写失败测试 `audit-rows-format.test.js`**

```js
test('auditRowsHtml renders type/step/dur + encrypt before-after', () => {
  const html = auditRowsHtml({ rows: [
    { type: 'download', step: 'download', dur_ms: 120, bytes: 2048, bw_bps: 4096 },
    { type: 'encrypt', step: 'encrypt', dur_ms: 30, bytes: 1024, meta: { cipher_bytes: 2048, algorithm: 'shardseal-high' } },
  ]});
  assert.match(html, /download/);
  assert.match(html, /120/);
  assert.match(html, /shardseal-high/);
  assert.match(html, /1024/);
  assert.match(html, /2048/);
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/audit-rows-format.test.js`
预期：FAIL（undefined）。

- [ ] **步骤 3：实现**

- `audit-rows-format.js`：`auditRowsHtml({rows})` → 表格：type/level/step/dur_ms/bytes/bw + 加密行并排明文 vs 密文（`bytes` vs `meta.cipher_bytes`）+ 算法。
- `app-render.js` `_cloudTaskActions`（`:565-591`）加「审计」按钮（`data-audit`，有 audit 行才显示）；`app.js` 委托挂 → 弹窗 `auditRowsHtml`。
- 卷内加密信息：任务 7 的 `FileInfo` 若带 `volume_category=wrapper`，详情行展示「加密卷」徽标（复用任务 9 badge）。
- `index.html`/`Makefile`：新 JS 双清单。

- [ ] **步骤 4：运行测试验证通过**

运行：`cd D:\workdir\leon\cocomhub\sproxy && node --test web/static/audit-rows-format.test.js && node --check web/static/audit-rows-format.js && node --check web/static/app.js && node --check web/static/app-render.js`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/audit-rows-format.js web/static/audit-rows-format.test.js web/static/app-render.js web/static/app.js Makefile
git commit -m "feat(webui): 任务审计面板——阶段耗时/带宽/加密明密文对比展示"
```

---

### 任务 13：端到端集成验证（建卷→切加密卷→播放→转存→审计）

**文件：**
- 创建：`web/e2e/audit_webui_e2e_test.go`（或扩展既有 `web/e2e/volumes_e2e_test.go`）
- 创建：`test/e2e_cli_audit_test.go`（或并入既有 CLI 套件）

- [ ] **步骤 1：编写 e2e 测试**

```go
// web/e2e：启动真实服务（含 secretdata 卷 + 本地卷），浏览器驱动：
//   1) UI 建 secretdata 封装卷（选本地底层卷 + secret_url）；
//   2) 文件页切到该加密卷 → 目录列表显示明文文件名；
//   3) 上传一个视频 → 行内「▶ 播放」→ 弹窗 `<video src="/download?...&volume=vault">`
//      → 断言 Range 请求返回 206（fetched via headless driver 或 rawHTTP Range 请求）；
//   4) 云下载 + 转存到该卷 → 任务详情含 audit 行（download+encrypt）。
// 断言铁律：每条正例必须落到真实副作用（磁盘文件/API 响应内容），不得只断退出码。
```

- [ ] **步骤 2：运行 e2e 验证**

运行：`cd D:\workdir\leon\cocomhub\sproxy && make test-e2e`
预期：PASS（含既有全部 e2e）。

- [ ] **步骤 3：Commit**

```bash
git add web/e2e/audit_webui_e2e_test.go test/e2e_cli_audit_test.go
git commit -m "test(e2e): 审计+加密卷 WebUI 全链路——建卷→切换→播放 Range→转存→审计"
```

---

### 任务 14：全量门禁 + 文档

**文件：**
- 修改：`config.example.yaml`（`audit` 配置段 + wrapper 卷建卷示例注释）
- 修改：`docs/config.md`（`audit.*` 字段文档）
- 修改：`docs/roadmap.md`（V7 待办勾除：外部卷播放器/文件浏览已交付）

- [ ] **步骤 1：补配置文档**

- `config.example.yaml` 加 `audit:` 段（`enabled`, `dir`, `archive: {enabled: false, volume: "", path: ""}`）——**默认不归档**；开启需选卷+路径（规格 §4.5）。
- `docs/config.md` 同步字段。
- `docs/roadmap.md` V7 勾除已交付项。

- [ ] **步骤 2：全量门禁**

运行：
```bash
cd D:\workdir\leon\cocomhub\sproxy && make check-ci
```
预期：EXIT=0（fmt-all → lint-all → check-ci 全绿；lint 0 issues）。

- [ ] **步骤 3：Commit**

```bash
git add config.example.yaml docs/config.md docs/roadmap.md
git commit -m "docs(config): audit 归档默认关闭配置 + roadmap V7 勾除"
```

---

## 执行交接

**任务依赖**：1→2→3→4；5→6（同卷管理域）；7→9（列表透传→UI）；8 独立；10 依赖 7/9；11 依赖 5/6；12 依赖 3/4；13 依赖 8-12；14→15 收尾。前端任务（8-12）与后端任务（1-7）并行度低，建议顺序推进避免界面错位。
**系统资源采样**（DimSystem）：任务 2 的 `TypeResource` + `DimSystem`（任务 1 已定义）在本期经任务 3 的云下载 scope 挂一个轻量周期采样（PeakCPU/PeakMem 写 `TypeResource` 行，`Row.Meta` 携带），供单任务资源维度展示；同步/敏感操作埋点为规格 §5 预留场景，本期不实做（YAGNI），接口（`TypeSensitive`）已随任务 1 定义。

**审查重点映射**：成环 → 任务 6；无权卷列表 → 任务 7；审计不泄密 → 任务 1/2/12；Range 越界 → 任务 10；三行为空洞 → 任务 8；归档默认关 → 任务 14。均已入各任务测试。
