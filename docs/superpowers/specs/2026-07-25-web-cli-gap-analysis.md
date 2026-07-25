# sproxy Web UI 与 CLI 差异分析及发展规划

> **面向 AI 代理的工作者：** 本文档为发展规划，非实现计划。后续实现请使用 `writing-plans` 技能创建具体任务级计划。

---

## 第一部分：总览

sproxy 提供两个用户接口通道：

| 通道 | 技术栈 | 用户群 | 定位 |
|------|--------|--------|------|
| **Web UI** | `web/static/` (原生 JS + CSS + HTML) | 浏览器用户 | 可视化文件管理 |
| **CLI (sclient)** | `cmd/sclient/` (Cobra) | 脚本/终端用户 | 自动化、批量操作 |

**核心原则**：两端策略可以不一致，最终效果一致即可。CLI 面向自动化（精度、可控性、脚本友好），Web UI 面向交互（直观、反馈、易用性）。下文分析差异时，允许各自保留最合适的实现策略。

---

## 第二部分：功能覆盖矩阵

### 2.1 文件操作 (Core File Operations)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 上传文件 | ✅ 分块上传（串行，自适应分块 4MB-64MB，3 次重试，续传） | ✅ 简单上传 + 分块上传（自动选择，并发 4，续传，checksum 校验） | **策略不同**：UI 串行，CLI 并发。UI 无简单上传路径。 | ⚪ 维持现状 |
| 下载文件 | ✅ 简单下载（Range header 流式，大文件流式 SHA-256 校验） | ✅ 简单下载 + 分块下载（并发，预分配，分块级校验） | **策略不同**：UI 流式，CLI 并发分块。UI 无分块下载。 | ⚪ 维持现状 |
| 删除文件 | ✅ 带 checksum 校验 | ✅ 带 checksum 校验，可选 `--check-local` | 基本一致 | ⚪ 维持现状 |
| 重命名/移动 | ✅ 带 checksum 校验 | ✅ 先 Stat 获取 checksum，再 Rename | 逻辑一致 | ⚪ 维持现状 |
| 列出文件 | ✅ 分页（每页 500），面包屑导航 | ✅ 支持 `--subdir`，受 `cd` 影响 | **UI 更友好**（分页 + 面包屑），CLI 功能完整 | ⚪ 维持现状 |
| 搜索文件 | ✅ 实时搜索 | ✅ 模糊匹配 | 基本一致 | ⚪ 维持现状 |
| 创建目录 | ✅ 内联输入 | ✅ `mkdir` 子命令 | 基本一致 | ⚪ 维持现状 |
| 删除目录 | ✅ Confirm 确认 | ✅ `--force` 跳过确认 | CLI 更灵活（可脚本化） | ⚪ 维持现状 |
| 文件统计 | ✅ 表格显示 size/checksum | ✅ `stat` 子命令 | 基本一致 | ⚪ 维持现状 |
| 文件预览 | ✅ 图片预览弹窗，文本前 100 行 | ❌ 无预览功能 | **CLI 缺失** | 🟡 低优先级 |
| 文件版本管理 | ✅ 查看/恢复/删除版本 | ✅ 查看/恢复/删除版本 | 基本一致 | ⚪ 维持现状 |

### 2.2 批量操作 (Batch Operations)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 批量删除 | ✅ `POST /api/batch/delete`（单次 API 调用） | ✅ 逐文件 `POST /delete`（继续处理模式） | **策略不同**：UI 一次 API 保证原子性，CLI 逐文件保证可追踪性 | ⚪ 维持现状 |
| 批量重命名 | ✅ `POST /api/batch/rename`（单次 API 调用） | ✅ 逐文件 `POST /rename`（继续处理模式） | **策略不同**：同上 | ⚪ 维持现状 |
| 下载归档 | ✅ 选择文件 → 打包 tar.gz 下载 | ✅ `archive` 子命令 | 基本一致 | ⚪ 维持现状 |
| 目录打包 | ✅ 目录行"打包下载"按钮 | ✅ `archive-dir` 子命令 | 基本一致 | ⚪ 维持现状 |

### 2.3 文件分享 (Share Links)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 创建分享 | ✅ (TTL, 最大下载次数, 一次性) | ✅ `share create` (TTL, max-downloads, one-time) | 基本一致，CLI 支持 `--json` | ⚪ 维持现状 |
| 列出分享 | ✅ | ✅ `share list` | 基本一致，CLI 支持 `--json` | ⚪ 维持现状 |
| 撤销分享 | ✅ | ✅ `share revoke` | 基本一致 | ⚪ 维持现状 |
| 复制分享链接 | ✅ 自动复制到剪贴板 | ❌ 需手动拼接 | **CLI 缺失** | 🟢 低优先级 |

### 2.4 云端下载 (Cloud Download)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 创建单任务 | ✅ | ✅ | 基本一致 | ⚪ 维持现状 |
| 批量创建任务 | ✅ `POST /api/cloud/download/batch` | ✅ 逐 URL 创建（多个 URL 参数） | **策略不同**：UI 一次批量 API，CLI 逐 URL | ⚪ 维持现状 |
| 列出所有任务 | ✅ 3 秒轮询列表 | ❌ 只有单个任务轮询 | **CLI 缺失** | 🟢 高优先级 |
| 取消任务 | ✅ 按钮点击 | ❌ 无取消命令 | **CLI 缺失** | 🟢 高优先级 |
| 删除任务 | ✅ 按钮点击 | ✅ 下载后自动清理 | 基本一致 | ⚪ 维持现状 |
| 下载到本地 | ✅ 自动下载，校验，清理 | ✅ 自动下载，校验，清理 | 基本一致 | ⚪ 维持现状 |

### 2.5 监控与配置 (Monitoring & Config)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 服务器统计 | ✅ 统计面板（磁盘、请求、传输） | ❌ 无 `stats` 命令 | **CLI 缺失** | 🔴 高优先级 |
| 读取运行时配置 | ✅ 配置面板 | ❌ `config remote` 无读取 | **CLI 缺失** | 🔴 高优先级 |
| 更新运行时配置 | ✅ 快速编辑（日志级别、速率限制、存储上限） | ✅ `config remote set` (部分字段) | **CLI 功能不全**（缺少存储上限等） | 🟢 中优先级 |
| Hub 节点列表 | ✅ 面板显示 | ✅ `relay status` | 基本一致 | ⚪ 维持现状 |
| Hub 移除节点 | ✅ 按钮 | ❌ 无命令 | **CLI 缺失** | 🟢 低优先级 |
| Hub 统计 | ✅ 面板显示 | ❌ 无命令 | **CLI 缺失** | 🟢 低优先级 |

### 2.6 认证与传输 (Auth & Transport)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| Bearer Token | ✅ localStorage 持久化 | ✅ `--auth-token` / 配置文件 | 基本一致 | ⚪ 维持现状 |
| 加密隧道 | ✅ tunnel.js AES-256-GCM | ✅ `tunnel` 子命令 + `relay` 模式 | 基本一致 | ⚪ 维持现状 |
| 密钥生成 | ❌ 无界面 | ✅ `genkey` 子命令 | 合理（CLI 生成密钥更自然） | ⚪ 维持现状 |

### 2.7 输出格式 (Output Format)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 文本输出 | ✅ DOM 渲染 | ✅ 纯文本表格 | 各自最佳 | ⚪ 维持现状 |
| JSON 输出 | ❌ N/A | ✅ `--json`（仅 list/search/config remote/share/stats） | **CLI 部分命令缺失 JSON 输出** | 🟢 中优先级 |
| 进度条 | ✅ DOM 进度条 | ✅ stderr 进度条 | 各自最佳 | ⚪ 维持现状 |
| 错误提示 | ✅ Toast 通知 | ✅ stderr 错误输出 | 各自最佳 | ⚪ 维持现状 |

### 2.8 用户体验增强 (UX Enhancements)

| 功能 | Web UI | CLI | 策略差异 | 优先级 |
|------|--------|-----|---------|--------|
| 拖拽上传 | ✅ | ❌ N/A | 合理（CLI 无拖拽概念） | ⚪ 维持现状 |
| 键盘快捷键 | ✅ (u/r//Esc/Ctrl+A/Delete) | ❌ N/A | 合理（CLI 有 shell 补全） | ⚪ 维持现状 |
| 暗色模式 | ✅ CSS 变量 + 系统偏好检测 | ❌ N/A | 合理 | ⚪ 维持现状 |
| 面包屑导航 | ✅ | ✅ `cd`/`pwd` | 各自最佳 | ⚪ 维持现状 |
| 续传 | ✅ localStorage 持久化 | ✅ 自动检测 | 基本一致 | ⚪ 维持现状 |

---

## 第三部分：CLI 缺失功能详细规划（按优先级）

### 🔴 高优先级（严重影响使用）

#### 1. 添加 `stats` 子命令（服务器统计）

**背景**：Web UI 的监控面板展示了磁盘使用、请求统计、传输统计等关键信息，CLI 用户完全无法获取。

**API 端点**：`GET /api/stats` → 返回 `StatsResponse` JSON

**建议实现**：
- 新建 `cmd/sclient/stats.go`
- 调用 `GET /api/stats`，解析 `StatsResponse`
- 纯文本输出格式（参考 Web UI 统计面板的排版）
- 支持 `--json` 输出

**策略**：与 Web UI 使用相同 API，但输出格式不同（CLI 文本表格 vs UI 图表面板）。

#### 2. 添加 `config remote` 读取功能

**背景**：CLI 的 `config remote set` 可以修改远程配置，但缺少读取功能。`config remote` 当前只支持 set 子命令。

**API 端点**：`GET /api/config` → 返回运行时配置（不包含 secrets）

**建议实现**：
- 修改 `cmd/sclient/config.go` 中的 `config remote` 子命令
- 无参数时默认读取远程配置
- 支持 `--json` 输出

#### 3. 添加 `cloud cancel` 子命令

**背景**：Web UI 可以一键取消云端下载任务，CLI 用户无法取消。

**API 端点**：`POST /api/cloud/tasks/{id}/cancel`

**建议实现**：
- 增加 `cloud-download cancel <task-id>` 子命令
- 或增加 `cloud-download --cancel <task-id>` flag

#### 4. 添加 `cloud list` 子命令

**背景**：Web UI 以 3 秒轮询展示所有云端下载任务，CLI 仅能轮询单个任务。

**API 端点**：`GET /api/cloud/tasks`

**建议实现**：
- 增加 `cloud-download list` 子命令
- 支持 `--status` 过滤（等待中/下载中/已完成/失败）
- 支持 `--json` 输出

### 🟢 中优先级

#### 5. 部分命令补全 `--json` 输出

**当前已支持 JSON 的命令**：`list`、`search`、`config remote`、`share`、`stats`

**待补充的命令**：
- `cloud-download list`（新增后自然支持）
- `version list`（版本历史）
- `stat`（文件元信息）

#### 6. `config remote set` 补全配置字段

**当前支持**：`log_level`, `log_format`, `auth_token`, `rate_limit_requests`, `rate_limit_window`

**缺失字段**：`max_storage_bytes`

### 🟡 低优先级

#### 7. 文件预览（`preview` 子命令）

Web UI 支持图片预览和文本预览，CLI 可考虑：
- 图片预览：下载到临时目录后打开
- 文本预览：下载前 100 行到 stdout

#### 8. Hub 节点管理命令

添加 `hub remove-node <id>` 子命令。

#### 9. 分享链接复制到剪贴板

`share create` 输出分享链接时，提供可复制格式。

---

## 第四部分：Web UI 缺失功能详细规划（按优先级）

### 🟡 低优先级（影响较小）

#### 1. 简单上传路径

Web UI 当前只使用分块上传，小文件也走分块流程。可以增加一个快速上传路径：小文件直接用 `POST /upload` 上传，跳过分块初始化流程。

**策略**：CLI 已有此策略，Web UI 可以借鉴但不需要完全对齐。

#### 2. 分块下载

Web UI 当前使用简单下载（Range header 流式），大文件下载体验不如 CLI 的分块并行下载。可以考虑：
- 大文件自动切换到分块下载模式
- 并行分块 + 进度显示

**策略**：CLI 的分块下载策略更适合 CLI（文件直接保存到磁盘），Web UI 的流式下载更适合浏览器（直接展示/保存对话）。**建议维持现状**。

---

## 第五部分：策略差异分析（允许不一致）

以下差异是设计上的合理选择，**不建议强行统一**：

| 差异点 | Web UI 策略 | CLI 策略 | 理由 |
|--------|-------------|----------|------|
| 上传方式 | 全分块上传（串行） | 自动选择（简单/分块，并发 4） | UI 需要进度可视化 + 续传，串行更可控；CLI 追求吞吐量，并发更高效 |
| 下载方式 | 流式下载（Range header） | 简单 + 分块并行下载 | UI 浏览器原生支持流式保存；CLI 需要大文件并行加速 |
| 批量删除 | `POST /api/batch/delete` 单次 API | 逐文件 `POST /delete` | UI 一次操作直观；CLI 逐文件可追踪单个失败 |
| 批量重命名 | `POST /api/batch/rename` 单次 API | 逐文件 `POST /rename` | 同上 |
| 批量创建云端下载 | `POST /api/cloud/download/batch` | 逐 URL 创建 | UI 批量提交快；CLI 逐 URL 可控 |
| 任务轮询间隔 | 3s | 2s (可配置 `--poll-interval`) | UI 受浏览器限制，CLI 用户可调 |
| 预览 | 图片/文本弹窗 | 无 | 终端预览限制多，优先级低 |

---

## 第六部分：发展规划（Roadmap）

### 阶段一：CLI 高优先级补齐（建议 1-2 个开发周期）

```
1.  stats 子命令
    ├── 新建 cmd/sclient/stats.go
    ├── 调用 GET /api/stats
    ├── 纯文本输出（磁盘/请求/传输）
    └── 支持 --json

2.  config remote 读取
    ├── 修改 cmd/sclient/config.go
    ├── config remote 无参数时读取并显示
    └── 支持 --json

3.  cloud-download list/cancel
    ├── 新建 cmd/sclient/cloud_list.go
    ├── 新建 cmd/sclient/cloud_cancel.go
    ├── cloud list: 调用 GET /api/cloud/tasks
    └── cloud cancel: 调用 POST /api/cloud/tasks/{id}/cancel
```

### 阶段二：CLI 中优先级补齐（建议 1 个开发周期）

```
4.  补全 --json 输出
    ├── version list → 支持 --json
    └── stat → 支持 --json

5.  config remote set 补全字段
    └── 增加 max_storage_bytes 支持
```

### 阶段三：Web UI 低优先级增强（可选）

```
6.  Web UI 简单上传路径（小文件跳过分块初始化）
    └── 参考 CLI 策略，文件 < 4MB 时直接用 POST /upload
```

### 阶段四：CLI 低优先级

```
7.  preview 子命令
    ├── 图片：下载到临时目录，尝试打开
    └── 文本：下载前 N 行到 stdout

8.  hub remove-node <id> 子命令

9.  hub stats 子命令
```

---

## 第七部分：已对齐的功能（无需修改）

以下功能两端已对齐，**无需额外工作**：

| 功能 | 状态 |
|------|------|
| 上传（核心逻辑） | ✅ 各取所需策略 |
| 下载（核心逻辑） | ✅ 各取所需策略 |
| 删除 + checksum 校验 | ✅ 已对齐 |
| 重命名/移动 | ✅ 已对齐 |
| 列表 + 搜索 | ✅ 已对齐 |
| 目录 CRUD | ✅ 已对齐 |
| 文件版本管理 | ✅ 已对齐 |
| 分享功能 | ✅ 已对齐 |
| 云端下载（创建任务 + 下载到本地） | ✅ 已对齐 |
| 加密隧道 | ✅ 已对齐 |
| 认证 | ✅ 已对齐 |
| 续传 | ✅ 已对齐 |
| 归档打包 | ✅ 已对齐 |

---

## 第八部分：设计原则

1. **CLI 优先填充**：Web UI 已覆盖完整功能，CLI 缺失的监控/管理功能是主要短板
2. **策略差异保留**：两端策略不一致时，各自保留最适合的实现方式
3. **增量开发**：每个阶段独立可交付，不依赖前一阶段
4. **测试先行**：每个新子命令 / 新功能都先写测试再实现
5. **保持风格一致**：新 CLI 子命令遵循现有命令行风格（输出格式、错误处理、flag 命名）