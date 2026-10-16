<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Web UI 全功能补齐 + 通用审计日志包设计规格

- 日期：2026-10-06
- 状态：草案（待用户审查）
- 范围：sproxy 主仓 `github.com/cocomhub/sproxy`
- 关联分支：当前开发在为 v0.23.0..master 期间交付的功能补齐 Web UI（secret/secretdata 加密卷、Range 关键帧播放、pikpak/hybrid、cloud transfer、egress）+ 新增通用审计日志包。

## 1. 背景与目标

v0.23.0..master 期间交付了大量**后端能力**（secret 加密卷、视频关键帧分块播放、HTTP Range 三态、pikpak/hybrid 多账号下载、cloud transfer 转存、egress 集群出口），但**缺少一层完善的 Web UI 交互**与**运行级可观测数据**。本规格：

1. 为全部上述功能补齐可交互、易用、安全的 Web UI；
2. 新增**通用审计日志包** `pkg/audit`，提供跨层级的 span 式阶段/耗时/计量埋点，每任务独立文件落盘，供后续优化与归档。

目标（用户明确，2026-10-06）：

- **覆盖 v0.23.0..master 全部新功能**（含 #738 #739），不只 secret/secretdata；
- **审计数据**：每任务使用下载器、下载方式、占用空间、流量、带宽、下载速度、转存速度、各阶段耗时、CPU/内存、数据类型、加密速度、是否加密、是否打包、加密耗时、加密前后文件大小等维度；且设计成**通用审计包**（可复用、跨层 span、独立文件、Save/Query、可归档）；
- **加密卷可见性**：对相关 owner 可见；加密的是**存到卷的数据**而非上传用户不可见；选择卷即可查看对应卷数据；看到的是**加密前文件信息**（明文文件名/大小/类型），而非加密后分片；
- **卷管理**：创建/删除多种类型外部卷（传账号等 extra）；创建 secretdata/secrets 封装卷时可**选择真实卷作底层卷**；细化卷类型 tag（系统本地卷 / 网盘卷 / 封装卷[加密/冗余] / secrets）；按卷类型设计对应特定选项。

## 2. 已核实的架构前提（落地依据）

### 2.1 运行期卷 API 已存在
- `POST /api/volumes/user`（`pkg/server/user_volume_api.go:39` `createUserVolumeHandler`）+ `DELETE /api/volumes/user?name=`（`:106`）——运行期创建/删除**外部类型**用户卷；`createUserVolumeRequest`（`:26-33`）含 `{Name, Type, Capacity, Extra: map[string]any}`。
- `GET /api/backends`（`pkg/server/backends_api.go:4`）返回 `registry.BackendTypes()`（纯字符串列表）。——做卷管理 UI 的动态 type 下拉**零硬编码**。
- **缺口**：`Extra` 裸 map；封装卷「选底层卷」的字段依赖前端按类型设计 + 后端补语义校验；`/api/backends` 目前无 per-type 字段 schema。

### 2.2 云任务对象（`pkg/cloud/manager.go`）
- `CloudTask`（`:40-90`）：`Status/TotalSize/Downloaded/Transfer/TransferURL/Save/CleanupStatus` 等**已有**；**缺**耗时/下载器/带宽/加密/明密文等计量字段。
- `TransferSpec`（`:724`）：`{Volume, Path, OwnerPrefix}`——转存目标卷（secretdata 自动加密）。
- `TaskParams`（`:737`）：`{Transfer, DownloadLocal, Save}`——三行为正交。

### 2.3 加密卷列表缺口
- `/api/files`（`pkg/files/read_ops.go:92-148`）只索引**本地卷租户根**；外部卷（secretdata/secrets）无 `*storage.Tenant`，列表不返回其文件。
- → 需 `/api/files?volume=<外部卷>` 透传到该卷 `ListDir`（逻辑目录视图，明文信息），隐藏底层分片与加密 meta。

### 2.4 播放 URL 形态（已就绪，无需额外参数）
- `/download?filename=<rel>&volume=<secretdata卷>` + 标准 `Range` 头 → `ServeContent`+`RangeSeeker`+`OpenRangeRead` 段级解密（#735）。
- 备选 `/download/chunk?...&offset&length`。

### 2.5 现有审计基建（并存 + 预留迁移）
- `pkg/server/audit.go`：`AuditEvent`（`json: action/actor/.../ts`）+ `RecordAudit`。
- `pkg/server/audit_store.go`：`AuditStore`（独立文件 + 旋转）。
- `pkg/server/audit_ring.go`：`AuditRing`（有界缓冲，实时 `/api/audit`）。
- `GET /api/audit`：查询。
- 语义为「谁在何时对谁做了什么」的**敏感操作审计**。

### 2.6 Web UI 架构约定（`web/static/`）
- 单入口 `index.html`，脚本固定顺序引入（跨文件依赖契约）。
- 纯函数渲染 → `app-render.js` 或 `*-format.js`（顶层 function + module.exports，供 `node --test`）。
- 跨文件隐式全局必须顶部 `// global:` + typeof 守卫（CLAUDE.md 固化 + login.test.js 元测试兜底）。
- Makefile `web-test` 双清单（node --check + node --test）；新增 JS 必须同时进清单。
- 暗色 token 三块（:root / @media dark / [data-theme]）同步；动态 HTML 必须 `var(--…)` 禁内联亮色 hex；Lighthouse dark snapshot 对比度 score=1。
- 文件行卷徽标 `app-render.js buildFileRowHtml:122`（`.vol-badge`）；需按 category 细化。

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

### 5. 集成点（哪些地方接通用审计包）

| 场景 | Logger Use | 观测项 |
|------|-----------|--------|
| 云下载（`pkg/cloud/manager`） | 每 task 一个 scope | 下载器、分片宽、各阶段耗、带宽、字节、是否加密/打包 |
| 转存（`pkg/cloud/transfer`） | 同 task scope 派生 | 转存耗时、字节、加密耗时、明密文对比 |
| 加密（`secretdata.WriteFile`/`openRangeRead`） | 同 scope span | 加密耗时、算法、明文/密文字节 |
| 同步（`sync`） | 每传输 | 传输字节、耗时 |
| 系统资源 | DimSystem | CPU/内存采样行、节点 |
| 敏感操作 | TypeSensitive | 迁移桥 |

### 6. 前端交互

#### 6.1 加密卷列表（页面顶部下拉切换，Q1-A）
- 文件页顶部「卷」下拉：聚合 `GET /api/volumes`（含 owner 可见 secretdata/egress）→ 选中切到该卷明文目录视图（面包屑同普通列表）。
- `/api/files?volume=<外部卷>` 后端透传外部卷 `List`（明文信息）。

#### 6.2 播放器
- 视频行「▶ 播放」→ 弹窗 `<video src="/download?filename=&volume=">`（标准 Range）。
- `app-render` 渲染 meta。
`新 JS`：`web/static/video-player.js`（纯函数+模块）+ Makefile web-test 双清单。

#### 6.3 卷类型 tag 视觉
- `.vol-badge` 按 `category` 加次级类（`.vol-badge-wrapper` 锁/盾图标=加密），暗色 token 补 `--badge-wrapper-*`。

#### 6.4 卷管理面板（stats-modal 新 tab 或独立）
- 类型下拉 = `GET /api/backends` 动态；字段 = schema 渲染（封装卷含有底层卷下拉，可选 wrapper + 防环提示）。
- `POST /api/volumes/user` 提交；删除 `DELETE /api/volumes/user?name=`（有任务 409 → UI 提示）。

#### 6.5 cloud 转存 + 三行为透传
- `web/static/sclient/api/cloud.js` `createDownload/createBatch/createGroup` 透传 `transfer{volume,path}`、`save`、`download_local`。
- 目标卷选择器 + 转存路径输入。

#### 6.6 审计展示
- 任务详情行点开「审计」→ 渲染 `pkg/audit.Row` 列表（type/level/step/dur）+ 阶段耗时条 + 加密前后对比 + 资源采样。

### 7. API 变更清单（后端）

| 端点 | 现状 | 改动 |
|------|------|------|
| `GET /api/backends` | `[]string` | 返回 `{type,category,label,fields}` 数组 |
| `POST /api/volumes/user` | Extra 裸 map | 用 schema 校验 + 底层卷存在/防环 |
| `GET /api/files?volume=` | 本地卷 | 透传外部卷 List（明文视图） |
| `GET /api/audit` | 敏感操作 | 并存预留 + 任务审计查询纳入（scopeID 过滤维度） |
| 新增 | — | 任务详情含 `audit`（`pkg/audit` 行） |

### 8. 测试

**后端**：`pkg/audit` 单元（Logger/Begin/End/Flush/维度/多任务隔离/防环）；`/api/backends` schema；`createUserVolumeHandler` 底层卷校验；`/api/files?volume` 外部卷透传；云任务审计采集（各下载器/加密/转存）。
**前端**：`node --test`（审计/卷表单/播放 URL 纯函数）+ `web/e2e`（建卷→切加密卷→播放→转存全链路真浏览器）。
**全量**：`make check-ci`；`go test ./pkg/audit/... ./pkg/cloud/... ./pkg/server/...`。

### 9. 边界与安全

1. **审计不泄密**：`Row.Meta` 不写原始标准/凭据；持久化 0600；`/api/audit` owner 过滤。
2. **防成环**：封装卷创建 fail-closed。
3. **加密卷列表**：只显示明文 meta，不暴露分片/底层卷结构密钥（需要访问控制）。
4. **双口令只 CLI**：Web 只 random/import/export，不做 passphrase 派生（浏览器无 scrypt）。

### 10. 规模 / 分期

- 阶段一（后端审计包 + 埋点）：`pkg/audit` + 云/转存/加密埋点 + `/api/backends` schema + 建卷校验 + /api/files 透传。
- 阶段二（前端）：卷下拉/管理面板/audit 展示/cloud transfer 透传/video-player。
- 阶段三（集成验证 + e2e）。