# sproxy Web UI 与 CLI 功能差异分析及发展规划

- **日期**: 2026-07-24
- **作者**: Claude Code
- **状态**: 设计稿

## 背景

sproxy 项目同时提供 Web UI（`web/static/` 单页应用）和 CLI（`sclient` 命令行工具）两种交互界面。经过功能审计发现，两者在功能覆盖上存在显著差异：

- 部分功能仅有 Web UI（分享管理、服务器监控）
- 部分功能仅有 CLI（Hub 中继、配置管理、密钥生成、诊断工具）
- 部分功能体验不一致（Web UI 的面包屑导航 vs CLI 的 cd/pwd）

本设计规划按模块逐期补齐差异，采用 **按模块双线并行** 策略，每期独立 PR。

## 范围

本设计覆盖以下四个模块，每期交付一个完整功能模块的 Web UI + CLI 同时增强：

| 期次 | 模块 | 后端 API 变更 | 预计影响文件数 |
|------|------|-------------|--------------|
| 第 1 期 | 分享管理 | +2 端点 | ~10 files |
| 第 2 期 | 监控与配置 | +2 端点 | ~12 files |
| 第 3 期 | Hub 中继管理 | 无需新增 | ~6 files |
| 第 4 期 | 体验与脚本化 | 无需新增 | ~15 files |

## 设计

### 第 1 期：分享管理模块

#### 现状

- 分享功能仅有 `POST /api/share`（创建）和 `GET /s/{token}`（访问）两个端点
- Web UI 通过 `shareFile()` 函数调用 `prompt` 链创建分享，但**无法查看/管理已有分享**
- CLI 完全无 share 命令
- 存储为纯内存 `ShareStore`，无持久化
- 数据模型 `ShareLink` 缺少 `CreatedAt` 字段

#### 变更

**后端新增 API：**

| 端点 | 方法 | 用途 | 认证 |
|------|------|------|------|
| `GET /api/shares` | GET | 列出所有活跃分享链接（含过期标记） | authMiddleware |
| `DELETE /api/shares/{token}` | DELETE | 撤销指定分享链接 | authMiddleware |

**数据模型变更（`pkg/server/share.go`）：**

```go
type ShareLink struct {
    Token        string    `json:"token"`
    Filename     string    `json:"filename"`
    AbsPath      string    `json:"-"`
    CreatedAt    time.Time `json:"created_at"`    // 新增
    ExpiresAt    time.Time `json:"expires_at"`
    MaxDownloads int       `json:"max_downloads"`
    Downloads    int       `json:"downloads"`
    OneTime      bool      `json:"one_time"`
    // Expired 由 List() 动态计算，不存储
}
```

**ShareStore 新增方法：**

```go
func (s *ShareStore) List() []*ShareLink    // 返回所有链接，过期/已消耗的标记 Expired=true
func (s *ShareStore) Revoke(token string) error  // 删除链接，不存在返回 error
```

**Web UI 变更（`web/static/app.js`）：**

分享弹窗从单输入升级为双标签页：

| 标签页 | 内容 |
|--------|------|
| **创建分享** | 现有逻辑：输入 TTL/次数/一次性 → 创建 → 复制链接 |
| **管理分享** | 表格展示所有分享：文件名、剩余时长、下载次数、状态、撤销按钮 |

**CLI 新增命令（`cmd/sclient/share.go`）：**

```
sclient share create <filename>          # 创建分享链接
  --ttl              string   24h        有效期
  --max-downloads    int      0          最大下载次数（0=不限）
  --one-time         bool     false      一次性分享

sclient share list                       # 列出所有分享

sclient share revoke <token>             # 撤销分享
```

**客户端 SDK 新增（`pkg/client/share.go`）：**

```go
func (c *FileClient) CreateShare(ctx context.Context, filename string, ttl time.Duration, maxDownloads int, oneTime bool) (*ShareLink, error)
func (c *FileClient) ListShares(ctx context.Context) ([]*ShareLink, error)
func (c *FileClient) RevokeShare(ctx context.Context, token string) error
```

**变更文件清单：**

| 操作 | 文件 | 说明 |
|------|------|------|
| 🛠 | `pkg/server/share.go` | 新增 `CreatedAt`、`List()`、`Revoke()`、两个 handler |
| 🛠 | `pkg/server/handlers.go` | 注册 `GET /api/shares`、`DELETE /api/shares/{token}` |
| ✅ | `pkg/server/share_test.go` | 新增 List/Revoke 测试用例 |
| ✅ | `pkg/client/share.go` | 客户端 SDK 方法 |
| ✅ | `pkg/client/share_test.go` | 客户端 SDK 测试 |
| ✅ | `cmd/sclient/share.go` | share 命令及三个子命令 |
| ✅ | `cmd/sclient/share_test.go` | CLI 命令测试 |
| 🛠 | `cmd/sclient/root.go` | 注册 shareCmd |
| 🛠 | `web/static/app.js` | 分享弹窗升级为双标签页 |

---

### 第 2 期：监控与配置模块

#### 现状

- Web UI 已有监控弹窗（`GET /api/stats`）和存储上限设置（`PUT /api/storage/config`）
- 但无法查看/修改其他运行时配置（日志级别、速率限制等）
- CLI 完全无 `stats` 和远程配置管理命令

#### 变更

**后端新增 API：**

| 端点 | 方法 | 用途 | 认证 |
|------|------|------|------|
| `GET /api/config` | GET | 获取当前服务器配置（脱敏） | authMiddleware |
| `PUT /api/config` | PUT | 更新运行时配置项 | authMiddleware |

**可动态更新的配置项（软配置）：**

| 配置项 | 类型 | 说明 |
|--------|------|------|
| `log_level` | string | debug/info/warn/error |
| `log_format` | string | text/json |
| `auth_token` | string | 更新认证令牌 |
| `rate_limit.requests` | int | 速率限制请求数 |
| `rate_limit.window` | duration | 速率限制窗口 |
| `max_storage_bytes` | int64 | 存储上限（合并现有 `PUT /api/storage/config`） |

**Web UI 变更：**

监控弹窗升级为"监控 + 配置"面板，增加配置标签页，支持下拉选择/输入框就地修改。

**CLI 新增命令：**

```
sclient stats                          # 查看服务器统计信息
sclient config remote                  # 查看远程服务器配置
sclient config remote set <k> <v>      # 更新远程配置
```

**客户端 SDK 新增（`pkg/client/stats.go`、`pkg/client/config.go`）：**

```go
func (c *FileClient) GetStats(ctx context.Context) (*ServerStats, error)
func (c *FileClient) GetConfig(ctx context.Context) (*ServerConfig, error)
func (c *FileClient) UpdateConfig(ctx context.Context, key string, value interface{}) error
```

---

### 第 3 期：Hub 中继管理模块

#### 现状

- 服务端已注册 `GET /api/hub/nodes`、`DELETE /api/hub/nodes/{id}`、`GET /api/hub/stats`
- Web UI 完全无对应界面
- CLI 有 `relay` 命令（启动中继节点）和 `diag` 命令（诊断工具）

#### 变更

**Web UI 新增 Hub 管理面板：**

在监控弹窗中增加 Hub 标签页，包含：

- 节点列表表格：节点 ID、地址、传输协议、连接时间、状态、移除按钮
- Hub 统计：总节点数、活跃节点数、转发流量、活跃隧道数

**CLI 增强（`cmd/sclient/relay.go`）：**

```
sclient relay start                    # 启动中继节点（现有行为）
sclient relay status                   # 查看本地中继状态
sclient relay stop                     # 优雅停止中继节点
```

**无后端 API 变更**，复用现有三个 Hub 端点。

---

### 第 4 期：体验优化与脚本化增强

#### 现状

- Web UI 极简风格，无暗色模式、无键盘快捷键、无拖拽上传、无文件预览
- CLI 输出固定为文本格式，不适合脚本调用

#### 变更

**Web UI 体验优化：**

| 功能 | 实现方式 |
|------|---------|
| 暗色模式 | CSS 变量 + `prefers-color-scheme` + 手动切换按钮 |
| 键盘快捷键 | `u`上传、`r`刷新、`/`搜索、`Ctrl+A`全选、`Delete`删除、`Esc`关闭 |
| 移动端适配 | 响应式布局：窄屏卡片式列表、折叠工具栏 |
| 拖拽上传 | `dragenter`/`dragover`/`drop` 事件，多文件支持 |
| 文件预览 | 图片灯箱、文本前 100 行显示 |

**CLI 脚本化增强：**

所有命令增加 `--json` flag，输出结构化 JSON：

```bash
# 文本输出（默认，保持向后兼容）
sclient list
       report.pdf   1.2 MiB  a1b2c3d4...

# JSON 输出
sclient list --json
{"files":[{"name":"report.pdf","size":1258291,"checksum":"a1b2c3d4..."}],"total":1}
```

标准化退出码体系：

| 退出码 | 含义 |
|--------|------|
| 0 | 成功 |
| 1 | 一般错误 |
| 2 | 参数错误 |
| 3 | 认证失败 |
| 4 | 资源不存在 |
| 5 | 网络错误 |

管道友好输出：`--no-header` 跳过表头、`--name-only` 只输出文件名。

输出格式化重构：`cmd/sclient/output.go` 引入 `OutputFormatter` 接口，支持 `TextFormatter` 和 `JSONFormatter` 两种实现。

---

## 不变的原则

所有四期设计中，以下原则保持不变：

1. **纯内存存储** — ShareStore 不引入持久化，服务重启后分享链接丢失为可接受设计
2. **纯标准库测试** — 不使用 testify/gomock/gomega，延续 `t.Fatalf`/`t.Errorf` 模式
3. **127.0.0.1 回环绑定** — 所有含 HTTP 服务的测试必须监听 127.0.0.1
4. **Windows 兼容** — 路径分隔符使用 `filepath.Join`/`filepath.ToSlash`
5. **全局状态隔离** — 测试须用 `t.Cleanup` 恢复包级全局变量
6. **Viper 隔离** — 测试优先使用 `viper.New()` 创建独立实例
7. **每期独立 PR** — 每个模块的变更独立提 PR，互不依赖

## 各期依赖关系

```
第 1 期（分享管理）    ← 无前置依赖，可先做
第 2 期（监控配置）    ← 无前置依赖，可并行
第 3 期（Hub 管理）    ← 无前置依赖，可并行
第 4 期（体验优化）    ← 建议在前 3 期之后做，因为涉及全部命令的 --json 改造
```