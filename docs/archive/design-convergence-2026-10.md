# 设计收敛：历史 plans/specs 的有效结论归档（2026-10 及更早）

> 来源：`docs/superpowers/plans/`、`docs/superpowers/specs/`（按父级 AGENTS 规则 3：
> 规划文档的**有价值内容提炼后即移除**）。此处**逐字保留**各源的「设计决策 / 结论 / 教训 /
> 风险 / 已知限制」小节，供后续维护者当**背景知识**复用；过程性内容（任务拆解、逐步操作、
> 验收清单）不保留（实现已完成，见 `docs/api.md`/`docs/architecture.md`/`docs/gate-*.md` 等权威文档）。


---

## 来源：`docs/superpowers/specs/2026-09-22-stealth-builder-design.md`

## 7. 已知限制与决策记录

1. **import path 不混淆**：`github.com/cocomhub/sproxy/pkg/...` 在符号表与
   panic 栈中仍可能出现（garble 默认对模块路径做 hash 化，但 module path 本身
   在 go.mod 内必须保留）。**判定：可接受**——混淆目标是不被 `strings` 一眼
   识别，不是防专业逆向；goroutine dump 里的包路径不构成品牌暴露。
2. **配置路径**：`~/.config/sproxy`（XDG）随品牌表替换为 `~/.config/prism`。
   **行为影响**：隐藏二进制与旧版数据/配置**不兼容**（不同目录）。
   用户已确认「隐藏场景接受独立配置目录」。
3. **TLS 证书**：自签证书的 CN/SAN 中可能含主机名，不涉及品牌；
   若用户在证书里写了 sproxy，需自行注意。
4. **日志与遥测**：日志前缀、tracer 名随品牌表替换；日志内容中的
   业务字段（如文件名）不含品牌，无需处理。
5. **单二进制双角色**：bridge 形态 = 隐藏配置（含 server_url、凭据、操作参数）
   加密后内嵌（见 §8），启动直接执行固定操作，不提供子命令。
6. **版本输出**：隐藏二进制不再提供真实版本号（避免版本指纹）。
   排障时用 `--build` 时间戳/哈希替代，或在构建报告里留档。
7. **网络协议指纹**（HTTP 头、路由路径、默认端口）不在本规格范围，
   由用户的协议伪装工程单独处理。

---

## 来源：`docs/superpowers/specs/2026-10-06-audit-webui-design.md`

## 3. 设计决策
### 决策一：卷类型分层 + schema 驱动建卷表单（`/api/backends` 扩展）

**卷类型 tag（4 类）**：
| tag | 含义 | 示例 type | 是否建卷 | 底层卷 |
|-----|------|-----------|----------|--------|
| `mt-local` | 系统本地卷（config `volume[]` 声明） | local | 只读展示 | — |
| `linked` | 网盘卷（需账号凭据） | baidupcs/pikpak/s3 | ✅ | — |
| `wrapper` | 封装卷（加密/冗余/出口/密钥） | secretdata/secrets/egress | ✅ | 可选择任意真实卷 |
| `secrets` | 密钥管理卷 | secrets | ✅ | 本地/网盘/加密盘 |

- **secrets 也是 wrapper**：底层本地盘、云盘、甚至**加密盘**（secretdata 之上再套一层）——`type=secrets` 复用 store 封装，target 可选任意真实卷。
- **嵌套允许 + 防成环**：底层卷可选任意已注册卷（含 wrapper），但创建时做**闭环检测**（防 `secrets→secretdata→secrets` 循环引用），fail-closed。

**`GET /api/backends` 扩展**（`pkg/server/backends_api.go`）：
```json
{"backends": [{
  "type": "secretdata",
  "category": "wrapper",
  "label": "数据加密封装卷",
  "fields": [
    {"key":"target","label":"底层卷","type":"volume-select","required":true,"allow_wrapper":true},
    {"key":"secret_url","label":"密钥引用","type":"text","required":true},
    {"key":"algorithm","label":"加密算法","type":"enum","options":["shardseal-high","shardseal-standard","shardseal-low","aes-256-gcm"]}
  ]
}]}
```
- **schema 来源**：后端实现可选 `registry.SchemaProvider` 接口（`Schema() FieldSchema`）；未实现则只给 `type`/`category`（表单只填 name）。
- **`NewBackend`** 用 schema 校验 `Extra`（封装卷必填底层卷 + 卷存在 + 防环）。
### 决策二：通用审计日志包 `pkg/audit`（核心）

**定位**：跨层级 span 式计量 + 独立文件每行落盘，**可复用**于任意场景（云下载/转存/加密/同步/上传/批处理）。

#### 4.1 核心类型 `pkg/audit/audit.go`

```go
type Type string
const (
    TypeAPI       Type = "api"
    TypeDownload  Type = "download"
    TypeTransfer  Type = "transfer"
    TypeEncrypt   Type = "encrypt"
    TypeResource  Type = "resource"  // CPU/内存采样行
    TypeSensitive Type = "sensitive" // 预迁移现有 server/audit 敏感操作
    TypeError     Type = "error"
)
type Level string
const (LevelDebug, LevelInfo, LevelWarn, LevelError)

// Dimension 用于 with 不同类型的审计维度（任务/系统/…）。
type Dimension string
const (DimTask Dimension="task", DimSystem Dimension="system")

// Row 一行审计日志（落盘一行 JSON）。
type Row struct {
    Type      Type   `json:"type"`
    Level     Level  `json:"level"`
    Dim       Dimension `json:"dim"`
    Step      string `json:"step"`
    Start     int64  `json:"start_ms"`  // 距作用域起点
    DurMS     int64  `json:"dur_ms"`
    Err       string `json:"err,omitempty"`
    BandwidthBps int64 `json:"bw_bps,omitempty"`
    Bytes     int64  `json:"bytes,omitempty"`
    Meta      any    `json:"meta,omitempty"`   // 灵活附加
    NodeID    string `json:"node,omitempty"`
}

type Logger struct { ... } // 作用域审计记录器
func NewScope(scopeID string, opts ScopeOpts) (*Logger, error)
func WithContext(ctx context.Context, l *Logger) context.Context
func From(ctx context.Context) *Logger          // 无则返回 nil（安全）
func (l *Logger) With(d Dimension, kv ...any) *Logger  // 复制出带维度的派生子（多维度观测）
func (l *Logger) Begin(step string, kv ...any) *Span
func (s *Span) End(kv ...any)
func (s *Span) Fail(err error)
func (l *Logger) Log(t Type, lvl Level, step string, kv ...any)
func (l *Logger) Err(err error, step string, kv ...any)
func (l *Logger) Save(ctx, key string, v any) error  // 跨函数计算值
func (l *Logger) Get(ctx, key string) (any, bool)
func (l *Logger) Flush() error
func (l *Logger) Close() error
```

#### 4.2 `Save/Get`（跨层级数据粘合）
Logger 承载一个 `map[string]any`（bound 到作用域），配合 `Span.End` 供上层累计（如 transfer 把字节写入，encrypt 读取对照明密文）。

#### 4.3 Sink —— 独立文件每行（天然任务隔离 + 旋转）
`pkg/audit/store.go`：
- 复用现有 `AuditStore` 落盘模式泛化：`NewScopeSink(dir, scopeID, dim)` → `<audit_root>/<scopeID>/<dim>.audit.log`。
- `Append(Row)` 写一行 JSON；行 sync、旋转、`Recent(filter)`。
- **每任务独立文件** ⇒ 天然隔离，多任务并行互不串行。

#### 4.4 ctx 跨层（span 式）
所有阶段埋点在任何深度经 `From(ctx)` 拿同一 Logger 记 span；任务隔离靠独立文件（scopeID 派生）。

#### 4.5 归档到卷（未来扩展，接口预留 + 配置门）
```go
type Archiver interface { Archive(ctx, scopeID, rows []Row) error }
// ScopeOpts.Archive *ArchiveHook；默认关。
```
- **默认不归档**；开启时需**选卷 + 路径**（config `audit.archive.enabled/volume/path`）。把 `<scope>.audit.log` 副本写入所选卷（如 secretdata://audit/）。

#### 4.6 与现有 `server/audit*.go` 关系：**并存 + 预留迁移**
- 保留现有 `AuditEvent`/`AuditStore`/`/api/audit`（敏感操作即时 ring），**零改动**。
- 新 `pkg/audit` 承载 span 计量 + 每任务文件；`TypeSensitive` 预留迁移桥，未来可合流。

---

## 来源：`docs/superpowers/plans/2026-10-06-audit-webui.md`（任务拆解已执行完毕；仅保留设计性约束与审查重点）

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

---

## 来源：`docs/superpowers/plans/2026-10-06-cloud-download-integrity.md`（任务拆解已执行完毕；仅保留设计性约束与审查重点）

## 全局约束

- 无外部依赖的校验器放 `pkg/integrity` 根包；唯一外部依赖（ffprobe）走 `pkg/integrity/ext/video/`（独立 go.mod）
- tar 校验器用标准库 `archive/tar` + `compressx`（不加依赖）
- 图片校验移植 cocom `pkg/imaging` 语义（`image.DecodeConfig`+`Decode`+Bounds 非空），不引入 cocom 依赖
- 校验点嵌入 `runRetryLoop`（runDownloadAttempt 成功分支），不单独循环
- 默认 `IntegrityMustPass=false` 放行 + 标记 `IntegrityStatus="damaged"`；`true` 阻断（failTask）
- 两次本地 checksum 一致仍异常 = 原始文件问题（不重下，按任务处置）
- 所有测试只绑 `127.0.0.1`；提交身份 `suixibing <suixibing@gmail.com>`，不加署名行
## 审查重点（Review Focus）

1. 未知类型文件（无校验器）→ 仅字节级校验，不误报 damaged（`Lookup` 返回 nil 视为通过）
2. 空文件/0 字节 → image/tar 校验器应判失败（Bounds 0 / tar 无条目），但字节级校验通过
3. 校验失败但重试耗尽 → 不无限重下（两次 checksum 一致即停，防死循环）
4. `IntegrityMustPass=true` 但校验器不存在（未知类型）→ 视为通过（无语义校验可执行），不误阻断
5. PikPak GCID 复算未命中（大文件分块自适应失败）→ 回落 ② 态（语义校验兜底），不误报权威

---

### 任务 1：`pkg/integrity` 注册表 + Checker 接口

**文件：**
- 创建：`pkg/integrity/registry.go`
- 创建：`pkg/integrity/errors.go`
- 测试：`pkg/integrity/registry_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
func TestRegisterAndLookup(t *testing.T) {
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
    c := integrity.Lookup("a.png")
    if c == nil { t.Fatal("png 应匹配 image/*") }
    if integrity.Lookup("x.xyz") != nil { t.Fatal("未知类型应 nil") }
}
func TestRegisterDuplicatePanics(t *testing.T) {
    defer func() { if recover() == nil { t.Fatal("重复注册应 panic") } }()
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
    integrity.Register("image/*", func() integrity.Checker { return fakeChecker{} })
}
func TestCheckerInterface(t *testing.T) {
    var _ integrity.Checker = fakeChecker{} // 编译期断言
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/...`
预期：FAIL（registry 未定义）

- [ ] **步骤 3：在 `pkg/integrity/registry.go` 实现注册表（shardseal RegisterAlgorithm 模式）**

```go
type Checker interface {
    Kind() string
    Matches(name string) bool
    Check(ctx context.Context, path string, size int64) (*Report, error)
}
type Report struct { OK bool; Reason string }
type CheckerFactory func() Checker
func Register(kind string, f CheckerFactory)   // 全局 map + 重复 panic
func Lookup(name string) Checker               // 按扩展名分发：注册 Kind 匹配扩展名族
```
扩展名匹配规则：注册 `image/*` 匹配 `.png/.jpg/.jpeg/.gif/.webp/.bmp`；`archive/tar` 匹配 `.tar/.tar.gz/.tgz/.tar.zst/.tar.br`；未知扩展 → nil。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/integrity/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/integrity/ && git commit -m "feat(integrity): Checker 注册表 + 按类型分发（shardseal 模式）"
```

---

### 任务 2：image/tar 校验器（标准库）

**文件：**
- 创建：`pkg/integrity/image.go`
- 创建：`pkg/integrity/tar.go`
- 测试：`pkg/integrity/image_test.go`、`pkg/integrity/tar_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// image_test.go
func TestCheckImage_ValidPNG(t *testing.T) {
    path := writePNG(t) // 1x1 有效 PNG（image/png 编码）
    rep, err := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if err != nil || !rep.OK { t.Fatal("有效 PNG 应 OK") }
}
func TestCheckImage_Corrupt(t *testing.T) {
    path := writeBytes(t, []byte("not-an-image"))
    rep, _ := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("损坏图片应失败") }
}
func TestCheckImage_ZeroBounds(t *testing.T) {
    // 0x0 PNG → 编码合法但 Bounds 空 → 应失败（Review Focus 2）
    path := writeZeroPNG(t)
    rep, _ := integrity.ImageChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("0x0 图片应失败（Bounds 空）") }
}

// tar_test.go
func TestCheckTar_ValidGz(t *testing.T) {
    path := writeTarGz(t, map[string]string{"a.txt": "x"})
    rep, err := integrity.TarChecker{}.Check(context.Background(), path, 0)
    if err != nil || !rep.OK { t.Fatal("合法 tar.gz 应 OK") }
}
func TestCheckTar_Corrupt(t *testing.T) {
    path := writeBytes(t, []byte("not-a-tar"))
    rep, _ := integrity.TarChecker{}.Check(context.Background(), path, 0)
    if rep.OK { t.Fatal("损坏 tar 应失败") }
}
func TestCheckTar_Empty(t *testing.T) {
    // 无条目 tar → 应失败（Review Focus 2）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/...`
预期：FAIL（Checker 未实现）

- [ ] **步骤 3：实现 `pkg/integrity/image.go`（移植 cocom VerifyImage 语义）**

```go
type ImageChecker struct{}
func (ImageChecker) Kind() string { return "image/*" }
func (ImageChecker) Matches(name string) bool { /* 扩展名族 */ }
func (ImageChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
    f, err := os.Open(path); if err != nil { return nil, err }
    defer f.Close()
    img, _, err := image.Decode(f) // 空白导入 image/gif, jpeg, png
    if err != nil { return &Report{OK: false, Reason: err.Error()}, nil }
    b := img.Bounds()
    if b.Dx() <= 0 || b.Dy() <= 0 { return &Report{OK: false, Reason: "invalid image bounds"}, nil }
    return &Report{OK: true}, nil
}
```

- [ ] **步骤 4：实现 `pkg/integrity/tar.go`**

```go
type TarChecker struct{}
func (TarChecker) Kind() string { return "archive/tar" }
func (TarChecker) Matches(name string) bool { /* .tar/.tar.gz/.tgz/.tar.zst/.tar.br */ }
func (TarChecker) Check(ctx context.Context, path string, size int64) (*Report, error) {
    f, err := os.Open(path); if err != nil { return nil, err }
    defer f.Close()
    var r io.Reader = f
    if gz, gzErr := gzip.NewReader(f); gzErr == nil { r = gz; defer gz.Close() }
    tr := tar.NewReader(r)
    count := 0
    for {
        _, err := tr.Next()
        if err == io.EOF { break }
        if err != nil { return &Report{OK: false, Reason: err.Error()}, nil }
        count++
    }
    if count == 0 { return &Report{OK: false, Reason: "empty tar"}, nil }
    return &Report{OK: true}, nil
}
```

- [ ] **步骤 5：运行测试验证通过**

运行：`go test ./pkg/integrity/...`
预期：PASS

- [ ] **步骤 6：Commit**

```bash
git add pkg/integrity/ && git commit -m "feat(integrity): image/tar 标准库校验器（移植 cocom VerifyImage 语义）"
```

---

### 任务 3：`pkg/downloader` IntegrityMode + Result 扩展

**文件：**
- 修改：`pkg/downloader/downloader.go`（Result 加 Integrity/AuthorityHash 字段 + IntegrityMode 常量 + IntegrityProvider 接口）
- 修改：`pkg/downloader/http_downloader.go`（HTTP 下载器声明 IntegrityMode）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（PikPak 声明 ModeLocalOnly + AuthorityHash）
- 测试：`pkg/downloader/downloader_test.go`、`pkg/volume/ext/pikpak/downloader_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// downloader_test.go
func TestIntegrityModeConstants(t *testing.T) {
    if downloader.ModeLocalOnly == downloader.ModeAuthority { t.Fatal("模式须互斥") }
}
func TestResultIntegrityField(t *testing.T) {
    r := downloader.Result{Integrity: downloader.ModeLocalOnly}
    if r.Integrity != downloader.ModeLocalOnly { t.Fatal("Result 应含 Integrity 字段") }
}
func TestHTTPDownloaderMode(t *testing.T) {
    d := downloader.NewHTTPDownloader(downloader.Config{}) // 现构造签名
    if ip, ok := d.(downloader.IntegrityProvider); ok {
        if ip.IntegrityMode() != downloader.ModeSelfVerified {
            t.Fatalf("HTTP 应 self_verified，got %v", ip.IntegrityMode())
        }
    } else { t.Fatal("HTTP 应实现 IntegrityProvider") }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/downloader/...`
预期：FAIL（IntegrityMode 未定义）

- [ ] **步骤 3：修改 `pkg/downloader/downloader.go`**

```go
type IntegrityMode int
const (
    ModeUnknown      IntegrityMode = iota
    ModeLocalOnly                  // 仅本地 checksum+size（② 态）
    ModeSelfVerified               // 下载器自算 checksum（② 态）
    ModeAuthority                  // 权威匹配：本地 == 服务端带外值（① 态）
)
// Result 扩展
type Result struct {
    Size     int64
    Checksum string
    ModTime  time.Time
    ETag     string
    Integrity    IntegrityMode
    AuthorityHash string // 服务端带外权威 hash（如 pikpak GCID；可空）
}
type IntegrityProvider interface { IntegrityMode() IntegrityMode }
```

- [ ] **步骤 4：`http_downloader.go` 加 `func (d *HTTPDownloader) IntegrityMode() IntegrityMode { return ModeSelfVerified }` + `var _ IntegrityProvider = (*HTTPDownloader)(nil)`**

- [ ] **步骤 5：`pikpak/downloader.go` 加 `func (d *PikpakDownloader) IntegrityMode() IntegrityMode { return ModeLocalOnly }` + `var _ IntegrityProvider = (*PikpakDownloader)(nil)`；`finalizeDownload` 的 Result 填 `Integrity: ModeLocalOnly`**

- [ ] **步骤 6：运行测试验证通过**

运行：`go test ./pkg/downloader/... && go test ./pkg/volume/ext/pikpak/...`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add pkg/downloader/ pkg/volume/ext/pikpak/ && git commit -m "feat(downloader): IntegrityMode 声明 + Result 扩展（HTTP self_verified / PikPak local_only）"
```

---

### 任务 4：`pkg/cloud` 校验点嵌入（完整性判定 + 语义校验 + 重下 + 状态）

**文件：**
- 修改：`pkg/cloud/manager.go`（CloudTask 加 IntegrityStatus/IntegrityMustPass + TaskParams 加 IntegrityMustPass）
- 修改：`pkg/cloud/manager_task.go`（runRetryLoop 成功分支加完整性判定；CreateTask 参数透传；状态写入）
- 测试：`pkg/cloud/integrity_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
func TestDownloadIntegrity_DamagedAllowsContinue(t *testing.T) {
    // 语义校验失败（损坏文件）→ 默认放行 + IntegrityStatus="damaged" + 任务 completed
    mgr, srv := newIntegrityTestMgr(t, corruptPayload) // 下载损坏 png（bytes 非图片）
    task, _ := mgr.SubmitAndStart("url", srv.URL, "bad.png", size, t.Context(), "", TaskParams{Save: true, IntegrityMustPass: false})
    // 轮询到 completed；task.IntegrityStatus == "damaged"
}
func TestDownloadIntegrity_ForceBlocks(t *testing.T) {
    // IntegrityMustPass=true + 语义校验失败 → 任务 failed（原因含 integrity）
}
func TestDownloadIntegrity_AuthoritySkipsSemantic(t *testing.T) {
    // 下载器 ModeAuthority + AuthorityHash 匹配 → 跳过语义校验 → verified
}
func TestDownloadIntegrity_TwiceConsistentStillDamaged(t *testing.T) {
    // 重下两次本地 checksum 一致仍异常 → 不无限重下 → damaged 放行（Review Focus 3）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/cloud/ -run TestDownloadIntegrity`
预期：FAIL

- [ ] **步骤 3：`manager.go` CloudTask 加字段**

```go
IntegrityStatus string `json:"integrity_status,omitempty"` // "" | verified | damaged
IntegrityMustPass  bool   `json:"integrity_must_pass,omitempty"`
```
TaskParams 加 `IntegrityMustPass bool`；CreateTask 透传。

- [ ] **步骤 4：`manager_task.go` 加完整性判定 helper + 嵌入点**

```go
// 嵌入：runRetryLoop 的 `if downloadErr == nil` 分支（recordTaskETag 后）
//   → m.checkDownloadIntegrity(ctx, task, destPath, result)
//   → 返回 errIntegrityFail（非「两次一致」）→ 继续 retry 循环（shouldRetryDownload 认可）
//   → 返回 errIntegrityPermanent（两次 checksum 一致仍异常）→ 按任务处置：
//       默认 → IntegrityStatus="damaged" + 放行（downloadErr=nil 继续完成）
//       IntegrityMustPass → failTask（错误含 integrity）

func (m *CloudDownloadManager) checkDownloadIntegrity(ctx context.Context, task *CloudTask, destPath string, result *downloader.Result) error {
    // ① 权威匹配（ModeAuthority + AuthorityHash 已由下载器确认）→ verified，跳过语义
    // ② 本地自洽 → integrity.Lookup(filepath.Ext(destPath)) → nil（未知类型）视为通过
    //    校验失败 → 语义异常：累计「本地 checksum 一致仍异常」次数
    //    2 次 → 按任务处置；否则 → errIntegrityFail（重下）
}
```
- 重下判定：校验失败且非永久 → `downloadErr = errIntegrityFail`（重试，复用退避）；`shouldRetryDownload` 加 `errIntegrityFail` 认可
- 两次一致 → 永久：`checkDownloadIntegrity` 用 task 本地 checksum 累计（跨 attempt 保留）

- [ ] **步骤 5：`integrity_test.go` 补辅助（newIntegrityTestMgr：注入完整性校验器 lookup + 真实下载器）**

```go
// 测试装配：mgr.integrityLookup = func(name) integrity.Checker（测试可控，注入 fake checker）
```

- [ ] **步骤 6：运行测试验证通过**

运行：`go test ./pkg/cloud/... -run TestDownloadIntegrity && go test ./pkg/cloud/...`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add pkg/cloud/ && git commit -m "feat(cloud): 下载完整性判定嵌入 runRetryLoop——checksum 三态 + 语义校验 + 重下 + damaged/force"
```

---

### 任务 5：客户端 flag + API 暴露

**文件：**
- 修改：`cmd/sclient/cloud_download.go`（--integrity-must-pass flag + 透传）
- 修改：`pkg/client/cloud.go`（CloudDownloadOption WithCloudDownloadIntegrityMustPass + body 字段）
- 修改：`pkg/server/cloud_download_handler.go`（请求解析 IntegrityMustPass + TaskParams）
- 测试：`pkg/client/chain_cloud_download_test.go`、`cmd/sclient/cloud_download_test.go`

- [ ] **步骤 1：编写失败的测试**

```go
// client_test.go
func TestCloudDownloadIntegrityMustPassOption(t *testing.T) {
    opts := []CloudDownloadOption{WithCloudDownloadIntegrityMustPass(true)}
    cfg := &cloudDownloadOptions{}
    for _, o := range opts { o(cfg) }
    if !cfg.forceIntegrity { t.Fatal("force 选项应生效") }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/client/ -run TestCloudDownloadIntegrityMustPass`
预期：FAIL

- [ ] **步骤 3：实现**

- `pkg/client/cloud.go`：`WithCloudDownloadIntegrityMustPass(v bool)` → cfg.forceIntegrity；batch/single body 加 `integrity_must_pass`
- `cmd/sclient/cloud_download.go`：`--integrity-must-pass` flag（默认 false）+ `WithChainIntegrityMustPass` + submit 透传（对齐既有三参链式）
- `pkg/server/cloud_download_handler.go`：请求 struct 加 `IntegrityMustPass bool` → TaskParams.IntegrityMustPass

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/client/... ./cmd/sclient/... ./pkg/server/...`
预期：PASS

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/ cmd/sclient/ pkg/server/ && git commit -m "feat(client): --integrity-must-pass flag 透传服务端（默认 false 零回归）"
```

---

### 任务 6：PikPak GCID 权威复算 + ext/video 插件装配

**文件：**
- 创建：`pkg/integrity/gcid.go`（GCID 复算：sha1(concat(sha1(分块))) + 候选分块）
- 创建：`pkg/integrity/gcid_test.go`
- 创建：`pkg/integrity/ext/video/go.mod` + `video.go`（ffprobe 校验器，独立 module）
- 修改：`cmd/sproxy/`（装配 ext/video 空白导入）
- 修改：`pkg/volume/ext/pikpak/downloader.go`（Result.AuthorityHash = FileMeta.Hash + Integrity=ModeAuthority 当 GCID 复算命中）

- [ ] **步骤 1：编写失败的测试（gcid_test.go）**

```go
func TestGCIDSmallFile_256KB(t *testing.T) {
    data := make([]byte, 262144) // 256KB 整数倍
    want := gcidCompute(data, 262144)
    got, ok := integrity.RecomputeGCID(data, []int64{262144, 524288, 4194304})
    if !ok || got != want { t.Fatal("256KB 分块应命中候选") }
}
func TestGCIDLargeFile_Fallback(t *testing.T) {
    // 大文件（非候选整数倍）→ 全部未命中 → ok=false（回落 ② 态，Review Focus 5）
    data := make([]byte, 5*1024*1024+1)
    if _, ok := integrity.RecomputeGCID(data, []int64{262144, 524288, 4194304}); ok {
        t.Fatal("非候选分块不应命中")
    }
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：FAIL（RecomputeGCID 未定义）

- [ ] **步骤 3：实现 `pkg/integrity/gcid.go`**

```go
// ComputeGCID 计算 GCID：sha1(concat(sha1(每个 blockSize 分块)))（PikPak 官方 hash 算法）
func ComputeGCID(data []byte, blockSize int64) string
// RecomputeGCID 尝试候选分块集合命中（固定算法，返回命中值 + ok）
func RecomputeGCID(data []byte, candidates []int64) (string, bool)
```
候选：256KB / 512KB / 1MB / 2MB / 4MB。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test ./pkg/integrity/ -run TestGCID`
预期：PASS

- [ ] **步骤 5：ext/video 插件（独立 go.mod module github.com/cocomhub/sproxy/pkg/integrity/ext/video）**

```go
// video.go：VideoChecker{} 复用 pkg/media/ffprobe
func (VideoChecker) Kind() string { return "video/mp4" }
func (VideoChecker) Matches(name string) bool { /* .mp4/.mkv/.webm/.mov/.ts */ }
func (VideoChecker) Check(ctx context.Context, path string, size int64) (*integrity.Report, error)
// 判定：ffprobe 可解析容器 + 有视频流 → OK；否则失败
// go.mod：require github.com/cocomhub/sproxy（根 module）+ replace ./（仓库内 module 模式，
// 对齐 pkg/volume/ext/baidupcs 既有独立 module 模式）
```

- [ ] **步骤 6：cmd/sproxy 装配**

```go
import _ "github.com/cocomhub/sproxy/pkg/integrity/ext/video" // 注册 video 校验器
```

- [ ] **步骤 7：PikPak GCID 接线**

`downloader.go`：下载完成后读文件 → `RecomputeGCID(file, candidates)` 命中官方 `FileMeta.Hash` → `result.Integrity=ModeAuthority + AuthorityHash=hash`；未命中 → `ModeLocalOnly`。

- [ ] **步骤 8：运行测试验证通过**

运行：`go test ./pkg/integrity/... ./pkg/volume/ext/pikpak/... ./cmd/sproxy/...`
预期：PASS

- [ ] **步骤 9：Commit**

```bash
git add pkg/integrity/ cmd/sproxy/ pkg/volume/ext/pikpak/ && git commit -m "feat(integrity): GCID 权威复算 + ext/video ffprobe 插件装配"
```

---

### 任务 7：WebUI 展示 IntegrityStatus

**文件：**
- 修改：`web/static/app-render.js`（云任务行展示 integrity 状态）
- 修改：`web/static/transfer-render.test.js`（node 单测）
- 修改：`web/e2e/ui_e2e_test.go`（Playwright e2e）

- [ ] **步骤 1：编写失败的 node 测试（transfer-render.test.js）**

```js
test('buildTransferRowHtml 云任务 damaged 展示完整性标记', () => {
  const html = r.buildTransferRowHtml({
    id: 'cloud-task-I', kind: 'cloud_task', filename: 'i.png', status: 'completed',
    meta: { raw: { integrity_status: 'damaged' } },
  });
  assert.ok(html.includes('完整性异常'), 'damaged 应展示标记');
});
```

- [ ] **步骤 2：运行 node 测试验证失败**

运行：`cd web/static && node --test transfer-render.test.js`
预期：FAIL（无标记）

- [ ] **步骤 3：`app-render.js` 云任务行加 integrity 展示**

`buildTransferRowHtml` 行徽章区（badge 旁）：`IntegrityStatus=="damaged"` → `完整性异常` 标记（黄色，`escHtml` 转义）；`"verified"` → 无额外标记（正常）。

- [ ] **步骤 4：运行 node 测试验证通过**

运行：`cd web/static && node --test transfer-render.test.js`
预期：PASS

- [ ] **步骤 5：Playwright e2e（ui_e2e_test.go 注入 damaged 云任务 → 断言标记可见）**

```go
// 复用 W2 的注入模式（window.transferStore.addOrUpdate damaged 云任务）
// 断言 #transfer-body InnerText 含「完整性异常」
```

- [ ] **步骤 6：运行 e2e 验证**

运行：`cd web/e2e && go test -run TestCloudDownloadIntegrityBadge`
预期：PASS

- [ ] **步骤 7：Commit**

```bash
git add web/static/ web/e2e/ && git commit -m "feat(web): 云任务 IntegrityStatus 展示（damaged 标记）"
```

---

### 任务 8：全量验证 + 收尾

- [ ] **步骤 1：全量测试**

运行：`go test ./pkg/... ./cmd/... ./internal/archcheck/... && golangci-lint run ./pkg/... ./cmd/... && cd web/static && node --test && cd web/e2e && go build ./...`
预期：全绿 + lint 0

- [ ] **步骤 2：覆盖全链路（下载→校验→damaged/force）回归**

运行：`go test ./pkg/cloud/... ./pkg/integrity/... -count=1`
预期：PASS

- [ ] **步骤 3：Commit 收尾（如有未提交）**

```bash
git add -A && git commit -m "test(integrity): 全量回归绿"
```

---

## 来源：`docs/superpowers/plans/2026-10-28-unified-volume-model.md`（任务拆解已执行完毕；仅保留设计性约束与审查重点）

## 全局约束

- 主仓只读：所有改动在独立 worktree（`sproxy-volunify`）进行，分支基于 origin/master，禁止直接改 `/d/workdir/leon/cocomhub/sproxy`。
- 中文回复；文件 UTF-8 无 BOM；SPDX 头（`Copyright 2026 The Cocomhub Authors. All rights reserved.` / `SPDX-License-Identifier: Apache-2.0`）。
- TDD：先写红测试再实现；测试纯标准库；绑定 `127.0.0.1`。
- 测试注入一律结构体构造注入（禁全局可变 seam）——沿用 AGENTS.md 7b。
- 不把 `storage.Root` 的 32 个方法塞进 `StorageVolume`（写内核保持 Root）。默认卷 `globalRoot`/`tenantFor` 语义不动。
- `volume://`（卷内定位序列化，已有）与 `local://`（协议寻址，U4 新增）并存，文档分工。
- 本地卷 trusted 装饰启用（meta sidecar + 校验）；老文件无 sidecar → 读侧跳过校验（fail-open，不破坏）。
- secrets 视图不入通用表（R26 白名单维持）；通用 `VolumeView` 拒绝 `secrets` use。
- 测试默认 `t.Parallel()`（除明确串行需求，见 AGENTS.md R18）。
## 审查重点（Review Focus）

按「最可能伤到使用者」排序——规格是愿景文档，下面每行是规格没提、但必须钉住的行为：

1. **`VolumeView(name, "user")` 的键空间**：本地卷落 `<owner>/user/<rel>`（ResolveLocation），外部卷同。若误拼成 `user/<rel>`（无 owner 前缀）→ 多租户文件交错。→ U2 加断言测试：`VolumeView` 后 `ResolveLocation(...).FSPath()` 前缀含 `NormalizeOwner(owner)`。
2. **转存到本地卷后 `/download` 必须命中**：产物落点与读路径键空间必须一致（否则「转存成功但下载 404」）。→ U3 e2e 断言：`target_volume=本地卷` 创建任务完成后 `GET /download?filename=<rel>` 200 且内容一致。
3. **`local://` URL 的路径穿越**：`ResolveURL("local://卷/../x")` 必须被 `ResolveUserLocation` 拒（fail-closed）。→ U4 加 URL 解析测试。
4. **本地卷装配接线后容量记账不丢**：`assembleLocalVolume` 改走 `localBackend` 后，用户写本地卷仍需走卷级记账（不可因「本地卷无 CapacityFS」漏记账 → 无限额）。→ U1 断言装配后 `Set.StorageVolume(name)` 实现 `capacity.CapacityFS` 语义（或等价配额权威在 FS 层）。
5. **U2 迁移后 `externalFSFor`/`externalVolumeTransferFS` 行为不变**：转存/读路径的装饰链（Guard→Wrap→capacity→staging）不能因换实现丢失。→ U2 对比测试：改前改后同一转存产物 meta sidecar 均存在、读回校验一致。

---

### 任务 1：Set 统一存储卷集合（U1）

**文件：**
- 修改：`pkg/volume/registry/set.go`（新增 `storageVolumes` map + `StorageVolume(name)` 访问器 + `NewSet` 收参）
- 修改：`pkg/volume/registry/set_test.go`（新测试）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 空串归一已有，无需动——确认）
- 修改：`pkg/server/volumes.go`（`assembleLocalVolume` 改走 `NewLocalBackend`，`volumeAccum` 加 `storageVolumes`）
- 修改：`pkg/server/volume_local_backend.go`（`localBackend` 补容量记账能力——见步骤 4）
- 测试：`pkg/volume/registry/storage_volume_unify_test.go`（新）

- [ ] **步骤 1：编写失败的测试（Set.StorageVolume 对本地卷返回 non-nil）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_StorageVolume_LocalReturnsBackend(t *testing.T) {
	t.Parallel()
	// 构造 Set：本地卷（TypeLocal、RootDir 指向 t.TempDir）
	lb := &fakeLocal{name: "v1"}  // 实现 StorageVolume 的最小 fake（Name/Kind/FS/Close）
	set := NewSet([]volume.Volume{{Name: "v1", Type: volume.TypeLocal}},
		map[string]*storage.Root{}, map[string]ExternalBackend{}, map[string]*quota.Pool{}, "v1")
	set.RegisterStorageVolume(lb) // 新方法：装配层把统一卷登记进 Set
	sv, ok := set.StorageVolume("v1")
	if !ok || sv == nil {
		t.Fatal("本地卷应经 StorageVolume 访问器取到")
	}
	if sv.Name() != "v1" || sv.Kind() != volume.KindStorage {
		t.Fatalf("Name/Kind 错误: %v %v", sv.Name(), sv.Kind())
	}
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume_LocalReturnsBackend`
预期：FAIL，`RegisterStorageVolume` 未定义

- [ ] **步骤 3：实现 `Set.RegisterStorageVolume` + `Set.StorageVolume`**

在 `pkg/volume/registry/set.go`：

```go
// RegisterStorageVolume 登记统一存储卷身份（装配期调用；本地卷由装配层经 NewLocalBackend
// 构造后登记，外部卷在 NewSet/AddExternalVolume 时同步登记）。
func (vs *Set) RegisterStorageVolume(sv StorageVolume) error

// StorageVolume 返回指定卷名的统一存储卷身份（本地/外部同形）。
func (vs *Set) StorageVolume(name string) (StorageVolume, bool)
```

`NewSet` 内部：`storageVolumes` map 初始化；为每个 `external` 条目登记（`storageVolumeOf` 或 `capacity.Backend` 已实现 `StorageVolume`）；`AddExternalVolume`/`AttachExternal` 同步登记；`RemoveExternalVolume` 同步清理。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 ./pkg/volume/registry/ -run TestSet_StorageVolume`
预期：PASS

- [ ] **步骤 5：`assembleLocalVolume` 改走 `NewLocalBackend`（装配接线）**

修改 `pkg/server/volumes.go`：
- `assembleLocalVolume`：把裸 `OpenLocalVolumeRoot` 改为 `NewLocalBackend(vc, rootDir, log)`（`NewLocalBackend` 已复用 `OpenLocalVolumeRoot`，含 at-rest 加密——零逻辑重复）。
- `volumeAccum` 加 `storageVolumes map[string]registry.StorageVolume`；`assembleLocalVolume` 把 `localBackend` 登记进去。
- `NewSet(...)` 调用处传新参数或在 `NewSet` 后调 `RegisterStorageVolume`。
- `roots` map **保留**（默认卷 tenant/globalRoot 仍走它——`localBackend.Root()` 暴露的 root 复用，`acc.roots[vc.Name] = lb.Root()`）。
- 确认 `localBackend` 已实现 `volume.Footprinter`/`PhysBound`（已有）→ 本地卷容量记账走 FS 层时能力不缺。

- [ ] **步骤 6：写 U1 验收测试（本地卷装配后容量记账不丢 + StorageVolume 可达）**

在 `pkg/server/volumes_unify_test.go`（新）：

```go
func TestAssembleVolumes_LocalBackendStorageVolume(t *testing.T) {
	// 单默认卷装配 → set.StorageVolume("default") 返回 non-nil、Kind=KindStorage、
	// 经 FS 层容量记账存在（capacity 语义）
}
```

- [ ] **步骤 7：运行 U1 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/ -run 'TestSet_StorageVolume|TestAssembleVolumes_LocalBackend'`
预期：PASS

- [ ] **步骤 8：Commit**

```bash
git add pkg/volume/registry/set.go pkg/volume/registry/storage_volume_unify_test.go pkg/server/volumes.go pkg/server/volumes_unify_test.go pkg/server/volume_local_backend.go
git commit -m "feat(volume): Set 统一存储卷集合——本地卷装配接线 StorageVolume（U1）"
```

---

### 任务 2：按用途统一访问器 VolumeView（U2）

**文件：**
- 创建：`pkg/volume/registry/views.go`（`use` 常量 + `VolumeView` 方法）
- 修改：`pkg/server/volumes.go`（`externalVolumeTransferFS`/`externalFSFor` → 委托 `VolumeView`）
- 修改：`pkg/server/files_service.go`（`ExternalVolume`/`externalSinkFor` 消费 `VolumeView`）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/volume/registry/set.go`（`VolumeView` 需要 trusted/staging 装饰——见步骤 3 的注入设计）

- [ ] **步骤 1：定义 `use` 常量 + `VolumeView` 签名（类型化防拼写错）**

在 `pkg/volume/registry/views.go`：

```go
// ViewUse 是卷用途视图的枚举（按用途封装——同一卷的不同用途看到不同装饰链/键空间）。
type ViewUse string

const (
	ViewUser ViewUse = "user"   // 用户文件（转存目标、上传 Sink、下载）——Guard→Wrap→capacity→staging
	ViewMeta ViewUse = "meta"   // 控制面（sidecar/checksum/台账）——Guard，不经容量/staging
	// secrets 不入通用表（安全：R26 白名单维持），装配期单独构造
)
```

- [ ] **步骤 2：`Set.VolumeView(name ViewUse) (FS, bool)` 基础实现（本地/外部同 shape）**

本地卷：经 `StorageVolume(name).FS()`（`localBackend.FS()` = `NewRootFS(root)`）→ 按 use 包装饰。
外部卷：`StorageVolume(name).FS()`（`be.FS()`）→ 同 use 包装饰 → `trusted.Guard(Wrap(fs))`（与现 `externalVolumeTransferFS` 同一 recipe）。
`ViewMeta`：`trusted.Guard(fs)`（不经 Wrap/capacity）——**meta 桶是本地卷概念**：外部卷经 `StorageVolume(name).FS()` 的 Guard 视图返回（无 meta 桶语义，消费方按需跳过 meta 相关调用）；**不返回 (nil,false)**（目录浏览/控制面仍可读）。

- [ ] **步骤 3：装饰链装配期注入（装配层套 trusted/staging，VolumeView 只取）**

关键：trusted Wrap 需要 `trusted.Options`（ChunkSize/Logger，来自 Handlers 配置），staging 是 per-owner（`ApplyStagingQuota(fs, trackerFor(owner))`）→ **不能在 registry 层硬编码**（registry 不 import server，也拿不到 Handlers）。

设计（装配期注入）：`Set` 新增可配置字段（构造注入，同 `NewSet` 参数）：

```go
// VolumeViewDecorator 由装配层提供：对裸卷 FS 按 use 套装饰链（返回用户可见视图）。
// 装配层（pkg/server）注入 trusted.Guard(Wrap(fs)) + capacity + staging 的**工厂**。
type VolumeViewDecorator func(name string, use ViewUse, raw syncpkg.FS) syncpkg.FS
```

`NewSet` 加参 `decorator VolumeViewDecorator`（nil = 裸返回，测试友好）。装配层传：

```go
func(h *Handlers) volumeDecorator(name string, use ViewUse, raw syncpkg.FS) syncpkg.FS {
	switch use {
	case ViewMeta:
		return trusted.Guard(raw)
	default: // ViewUser
		return h.stagingQuotaFS(owner?"", trusted.Guard(trusted.Wrap(raw, h.trustedWrapOpts())))
	}
}
```

**staging owner 问题**：`VolumeView(name, use)` 无 owner——staging 是 per-owner。两种：
(a) `VolumeView(name, owner, use)` 三参（owner 空 = 不经 staging）——U3 转存目标需要 owner；
(b) `VolumeView(name, use)` 返回**未套 staging** 的视图，staging 由消费方（已有 `h.stagingQuotaFS`）再套。
→ **选 (b)**：容量/可信/封印在装配期套好（根治 R27），staging 是 per-owner 瞬态，保持消费方 `h.stagingQuotaFS`（既有模式）。文档%2二架构图已明示 staging(owner) 在取值时。

- [ ] **步骤 4：写 U2 测试（VolumeView 本地/外部同 shape + meta 隔离 + 装饰链存在）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_VolumeView_UserViewHasDecorations(t *testing.T) {
	// Set 注入 decorator（断言被调到且返回 ≠ raw）
	// VolumeView("v1", ViewUser) 返回的 FS 与 raw 不同（装饰链存在）
	// ViewMeta 返回 Guard(fs)（≠ raw 但无 Wrap）
}
func TestSet_VolumeView_RejectsUnknownUse(t *testing.T) {
	// ViewUse("secrets") / ViewUse("bogus") → (nil, false)
}
```

- [ ] **步骤 5：`externalVolumeTransferFS`/`externalFSFor` 委托 `VolumeView`**

`pkg/server/volumes.go`：
- `externalVolumeTransferFS(volName)` → `h.volSet.VolumeView(volName, registry.ViewUser)`（套 staging 由调用方或内部补）。
- `externalFSFor(volName)` 同。
- 迁移后原函数保留作为薄包装（或删，若零调用点）。**验证**：`files_phys.go`/`du.go`/`files_service.go` 的既有调用不变。

- [ ] **步骤 6：跑 U2 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/server/`
预期：全绿（含既有外部卷转存/读路径测试——行为不变）

- [ ] **步骤 7：Commit**

```bash
git add pkg/volume/registry/views.go pkg/volume/registry/set.go pkg/server/volumes.go pkg/server/files_service.go pkg/volume/registry/storage_volume_unify_test.go
git commit -m "feat(volume): 按用途统一访问器 VolumeView（U2）——user/meta 视图 + 装配期装饰链注入"
```

---

### 任务 3：转存支持本地卷（U3）

**文件：**
- 修改：`pkg/server/cloud_download_handler.go`（`externalVolumeTransferFS` 调用点 → `VolumeView`；`cloudArchiveGroupToVolume` 目标解析）
- 修改：`pkg/cloud/manager_task.go`（`checkTransferVolumePreflight` 放宽：有 FS 即可，不要求 scheme 非空）
- 修改：`pkg/cloud/transfer.go`（`transferFS` 兜底：scheme 空时用 `"local"`）
- 修改：`pkg/server/routes_setup.go`（`TransferFSFor` 注入点）
- 修改：`web/static/app-render.js`（`isTransferTargetVolume` 放行 `local`）
- 修改：`web/static/app-render.test.js`（断言更新）
- 修改：`pkg/server/cloud_download_handler_test.go`（新测试）
- 新增：`test/e2e_volume_transfer_test.go`（e2e）

- [ ] **步骤 1：写红测试（转存到本地卷 preflight 通过 + 产物可下载）**

在 `pkg/server/cloud_download_handler_test.go`：

```go
func TestCloudDownload_TransferToLocalVolume(t *testing.T) {
	// 装配默认卷（本地）+ 构造 CloudDownload 任务 target_volume=默认卷名
	// 完成转存 → 断言产物在 <storage_root>/<owner>/user/<rel> 存在
	// → GET /download?filename=<rel> 200 内容一致
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（preflight 400「target volume not assembled」——当前 `volSet.External` 对本地卷 nil）

- [ ] **步骤 3：`checkTransferVolumePreflight` 放宽**

`pkg/cloud/manager_task.go`：`transferFS(volume)` 不再只认 scheme 非空——FS 非 nil 即可；scheme 空时按 `local` 处理（URL 生成 `local://<卷>/<rel>`，无需注册——见 U4）。

- [ ] **步骤 4：`externalVolumeTransferFS` → `VolumeView`（本地卷写面）**

`pkg/server/cloud_download_handler.go`：`cloudArchiveGroupToVolume`/`archiveGroupToVolume` 的目标 FS 解析改走 `h.volSet.VolumeView(t.TargetVolume, registry.ViewUser)`。本地卷返回 Guard(Wrap(NewRootFS(root)))（trusted 对本地启用——用户裁定 4）。`target_volume` ACL 校验（`volumeAllowedFor`）不变。

- [ ] **步骤 5：前端放行 local**

`web/static/app-render.js` `isTransferTargetVolume`：`t !== 'local'` 移除（保留 `t !== 'secrets'`）。
`web/static/app-render.test.js`：`isTransferTargetVolume({type:'local'})` 断言改为 `true`。

- [ ] **步骤 6：web 单测**

运行：`node --test web/static/app-render.test.js`
预期：PASS

- [ ] **步骤 7：e2e（转存到本地卷产物可下载）**

新增 `test/e2e_volume_transfer_test.go`：启动真实二进制，`POST /api/cloud/download`（target_volume=默认卷），轮询任务完成，`GET /download?filename=<rel>` 内容一致。

- [ ] **步骤 8：跑 U3 全量验证**

运行：`go test -count=1 ./pkg/server/ ./pkg/cloud/` + `make web-test`
预期：全绿；`make test-e2e`（或该 e2e 包）通过

- [ ] **步骤 9：Commit**

```bash
git add pkg/server/cloud_download_handler.go pkg/cloud/manager_task.go pkg/cloud/transfer.go pkg/server/routes_setup.go web/static/app-render.js web/static/app-render.test.js pkg/server/cloud_download_handler_test.go test/e2e_volume_transfer_test.go
git commit -m "feat(volume): 转存支持本地卷（U3）——VolumeView 目标解析 + preflight 放宽 + 前端放行 local"
```

---

### 任务 4：local:// scheme（U4）

**文件：**
- 修改：`pkg/server/volume_local_backend.go`（`RegisterLocalBackend` 加 `"local"` protocol）
- 修改：`pkg/volume/registry/backend.go`（`NewBackend` 分派已有；`ResolveURL` 对 local 卷处理）
- 修改：`pkg/volume/registry/set.go`（`ResolveURL` 增加 local:// 分支）
- 修改：`pkg/volume/registry/storage_volume_unify_test.go`（新测试）
- 修改：`pkg/server/cloud_download_handler.go`（转存 URL 生成用 `local://`）

- [ ] **步骤 1：写红测试（local:// URL 解析返回本地卷内容）**

在 `pkg/volume/registry/storage_volume_unify_test.go`：

```go
func TestSet_ResolveURL_LocalScheme(t *testing.T) {
	// 本地卷注册 "local" scheme → ResolveURL("local://v1/user/a.txt") 返回内容
	// 路径穿越："local://v1/../x" → 错误（fail-closed）
}
```

- [ ] **步骤 2：运行测试验证失败**

预期：FAIL（`无后端声明协议 "local"`）

- [ ] **步骤 3：`RegisterLocalBackend` 声明 protocol**

`pkg/server/volume_local_backend.go`：`registry.RegisterBackend(volume.TypeLocal, factory, "local")`。
（注：`RegisterBackend` 已允许 `TypeLocal` 注册——见 `backend.go` doc「local 允许注册」；protocol "local" 不与既有冲突——`schemeBackends` 无 "local"。）

- [ ] **步骤 4：`Set.ResolveURL` 支持 local://**

`registry.Set.ResolveURL`：scheme=="local" → 按 authority（卷名）经 `StorageVolume(name).FS()` 读；路径经 `volume.ResolveUserLocation`（权限门 + 路径安全）换算 FS 键（`<owner>/user/<rel>` 或匿名）。与既有 `URLResolver` 断言分支并列（local 卷无 `URLResolver` 接口——本地卷不经 `OpenURL`，直接 FS）。

- [ ] **步骤 5：转存 URL 生成用 local://（scheme 空归一）**

 `pkg/cloud/transfer.go`：`transferFS` 返回 scheme 空时归一为 `"local"`（与 U3 步骤 3 一致）；`targetURL("local", vol, rel)` 生成 URL。

- [ ] **步骤 6：跑 U4 全量验证**

运行：`go test -count=1 ./pkg/volume/registry/ ./pkg/cloud/ ./pkg/server/`
预期：全绿（含既有 URL 解析测试——行为不变，新增 local:// 分支）

- [ ] **步骤 7：Commit**

```bash
git add pkg/server/volume_local_backend.go pkg/volume/registry/set.go pkg/volume/registry/backend.go pkg/cloud/transfer.go pkg/volume/registry/storage_volume_unify_test.go pkg/server/cloud_download_handler.go
git commit -m "feat(volume): local:// scheme 声明与解析（U4）——转存 URL 一致化 + ResolveURL 读本地卷"
```

---

### 任务 5：R28/R27 门禁预算收缩（U5）

**文件：**
- 修改：`internal/archcheck/set_root_calls.tsv`（预算缩减）
- 修改：`internal/archcheck/set_root_accessor_gate_test.go`（若口径需更新）
- 修改：`pkg/server/handlers.go`（`volSet.Root` 存在性探测 → `HasLocalRoot`/`VolumeView`）
- 修改：`pkg/server/nested_enclosure.go`（`Root` 探测 → `HasLocalRoot`）
- 修改：`pkg/server/volume_retention.go`（`Root` → `HasLocalRoot`）

- [ ] **步骤 1：盘点可迁移调用点**

`grep -rn "volSet.Root(\|set.Root(" pkg/server/ pkg/volume/ pkg/files/ cmd/ --include="*.go" | grep -v _test`
→ 标识「存在性探测」（`if rt := h.volSet.Root(v.Name); rt != nil`）类 → 改 `HasLocalRoot`；「真需 root 方法」（`Root.Abs/Stat/SealAt/ListOwners/OpenFileEncrypted`）→ 保留。

- [ ] **步骤 2：迁移存在性探测类（每处一个 test 钉住行为）**

`pkg/server/handlers.go:688`、`pkg/server/nested_enclosure.go:383`、`pkg/server/volumes.go:847/986`、`pkg/server/volume_retention.go:106`：
`volSet.Root(x) != nil` → `volSet.HasLocalRoot(x)`。

- [ ] **步骤 3：更新 R28 预算表**

`internal/archcheck/set_root_calls.tsv`：扣除迁移行（或减 max_calls），同步注释。

- [ ] **步骤 4：跑 U5 验证**

运行：`make archcheck` + `go test -count=1 ./pkg/server/ ./pkg/volume/ ./pkg/files/`
预期：archcheck 全绿（预算收缩后不红）；功能测试全绿

- [ ] **步骤 5：Commit**

```bash
git add internal/archcheck/set_root_calls.tsv pkg/server/handlers.go pkg/server/nested_enclosure.go pkg/server/volume_retention.go pkg/server/volumes.go
git commit -m "refactor(volume): R28 门禁预算收缩——存在性探测改 HasLocalRoot（U5）"
```

---

### 任务 6：权威文档收敛 + 归档

**文件：**
- 修改：`docs/architecture.md`（统一卷模型章节）
- 修改：`docs/api.md`（转存 target_volume 支持本地卷）
- 修改：`docs/cli.md`（若 CLI 有转存相关说明）
- 修改：`docs/config.md`（若配置有变化）
- 新增：`docs/archive/unified-volume-model.md`（精华归档）

- [ ] **步骤 1：更新 `docs/architecture.md`（统一卷模型）**

新增章节：`StorageVolume` 唯一身份 + `Set.VolumeView(name, use)` 唯一入口 + 按用途视图表 + local:// scheme 分工。

- [ ] **步骤 2：更新 `docs/api.md`（转存 target_volume 支持本地卷）**

`target_volume` 描述：从「外部卷」改为「任意存储卷（含本地卷）」；URL 形态 `local://<卷>/<rel>`。

- [ ] **步骤 3：更新 `docs/cli.md` / `docs/config.md`（如有变化）**

`cloud-download`/`archive` 相关 CLI flag 说明；若配置无变化则跳过。

- [ ] **步骤 4：文档漂移检查**

运行：`make check-ci`（R9/R15 门禁）
预期：全绿

- [ ] **步骤 5：归档精华（`docs/archive/unified-volume-model.md`）**

按功能维度沉淀：统一模型的价值（新后端只实现 5 方法）、按用途封装的坑（secrets 不入通用表 / staging per-owner 在取值时 / Root 32 方法不塞接口）、迁移经验。

- [ ] **步骤 6：Commit**

```bash
git add docs/architecture.md docs/api.md docs/cli.md docs/config.md docs/archive/unified-volume-model.md
git commit -m "docs(volume): 统一卷模型权威文档收敛 + 归档（U6）"
```

---
