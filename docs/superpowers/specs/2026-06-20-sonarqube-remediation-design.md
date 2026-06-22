# SonarQube 全量修复设计方案

## 背景

sproxy 项目在 SonarCloud 上有 293 个未关闭问题（质量门禁失败）。本次修复覆盖所有 Go 生产/测试代码、CI 配置、shell 脚本、CSS 和 Web UI，排除遗留参考文件 `fileclient.sh`。

## 修复范围总览

| 类别 | 数量 | 归属 PR |
|------|------|---------|
| Go 生产代码 — 常量提取 S1192 | 15 | PR #1 |
| Go 生产代码 — 小修 (godre/S100/S1135) | 9 | PR #1 |
| 配置 (SonarQube/CI) | 4 | PR #1 |
| Shell/PowerShell 脚本 | 9 | PR #1 |
| Go 生产代码 — 参数过多 S107 | 5 | PR #2 |
| Go 生产代码 — 认知复杂度 S3776 | 28 | PR #2 |
| Go 测试代码 — 路径遍历 S2083 | 8 | PR #2 |
| Go 测试代码 — 小修 | 8 | PR #2 |
| Web UI — JS/CSS 重构 | 106+6 | PR #3 |

## PR #1：基础修复 + 配置变更

> 机械性修复，无逻辑变化。预计提交 3 个。

### Commit 1.1：配置 + CI + Shell

**`sonar-project.properties`** — 追加 `fileclient.sh` 到 exclusions：
```properties
sonar.exclusions=test/**,tools/**,build/**,xfer/grpc/**,xfer/webrtc/**,fileclient.sh
```

**`.github/workflows/ci.yml`** — 3 处 `githubactions:S7637`：
- actions/checkout@v4 → commit SHA（从 v4.2.2 标签解析）
- actions/setup-go@v5 → commit SHA
- codecov/codecov-action@v5 → commit SHA

**`scripts/check-test-files.sh`** — 10 处 `shelldre:S7688` `[` → `[[`

**`scripts/install-make.ps1`** — 1 处 `powershelldre:S8659` `Invoke-Expression` → 安全解析替换

### Commit 1.2：go:S1192 重复字符串提取

策略：复用 `pkg/server/errors.go` 已有常量集群，新增按语义分组的常量。

| 文件 | 字面量 | 替换为 | 优化内容？ |
|------|--------|--------|-----------|
| `pkg/server/handlers.go` | `"无效的文件名"` | `errMsgInvalidFilename`（已有） | 否 |
| `pkg/server/handlers.go` | `"hub not enabled"` | `errMsgHubNotEnabled = "hub 未启用"` | **是**，改为中文 |
| `pkg/server/handlers.go` | `"创建目标父目录失败"` | `errMsgCreateParentDirFailed = "目标路径父目录创建失败"` | **是**，语序更清晰 |
| `pkg/server/handlers.go` | `"X-Request-ID"` | 新增 `headerRequestID` | 否 |
| `pkg/server/handlers.go` | `"X-File-MTime"` | 新增 `headerFileMTime` | 否 |
| `pkg/server/version.go` | `"版本管理未启用"` | `errMsgVersioningDisabled` | 否 |
| `pkg/server/chunked_download.go` | `"文件读取失败"` | `errMsgFileReadFailed`（区别于 `errMsgOpenFileFailed`） | 否 |
| `pkg/server/chunked_upload.go` | `"文件已存在, size: %d"` | `errFmtFileExists = "文件已存在，大小: %d"` | **是**，中文标点 |
| `pkg/server/chunked_upload.go` | `"upload_id 不存在或已过期"` | `errMsgUploadIDNotFound` | 否 |
| `cmd/sclient/cd.go` | `"无效的路径: %w"` | `errFmtInvalidPath`（`cmd/sclient/` 本地常量） | 否 |
| `pkg/client/client.go` | `"X-File-MTime"` | `headerFileMTime` | 只在 `pkg/client/` 内 |
| `pkg/tunnel/tunnel.go` | `"Content-Type"` | `headerContentType`（已有）| 否 |
| `pkg/client/chunked.go` | `"Content-Type"` | `headerContentType`（已有）| 否 |
| `pkg/client/client.go` | `"X-File-Checksum"` | `headerFileChecksum`（已有）| 否 |

常量放置策略：
- `pkg/server/errors.go` — server 包内跨文件共享的常量（已有集群）
- `cmd/sclient/errors.go` — **新建**，放 sclient CLI 的常量（`errFmtInitClient`、`errFmtInvalidPath`、`errFmtMkdirFailed`）
- `pkg/client/const.go` — **新建**或复用 `client.go`，放 HTTP 头常量
- `pkg/tunnel` 各文件的常量就近放

### Commit 1.3：godre + 小修

| 规则 | 文件 | 修改方式 |
|------|------|---------|
| `godre:S8193` x6 | `handlers.go`、`upload_store.go` 等 | 去掉多余临时变量，直接使用表达式 |
| `godre:S8209` x2 | `cmd/sclient/tunnel.go`、`chunked_upload.go` | 合并同类型参数 |
| `godre:S8184` x2 | `cmd/sclient/diag.go`、`relay.go` | 空白导入加注释 |
| `godre:S8188` x1 | `pkg/tunnel/mux/mux.go:694` | defer cancel() |
| `godre:S8242` x1 | `pkg/tunnel/mux/mux.go:290` | 移除 context 字段，改为方法参数 |
| `go:S1135` x1 | `pkg/tunnel/xfer/ext/quic/quic.go:25` | 处理 TODO（确认+注释或解决） |
| `gosecurity:S5144` x1 | `pkg/server/relay.go:83` | URL 拼接做输入校验/白名单 |
| `go:S100` x2 | `pkg/server/metrics_test_extra.go` | 重命名函数匹配 `TestXxx` 模式 |

---

## PR #2：Go 代码结构改进

> 有逻辑变更，需要 review。预计提交 3 个。

### Commit 2.1：go:S107 减少函数参数

| 文件 | 函数 | 参数数 | 策略 |
|------|------|--------|------|
| `pkg/client/chunked.go:112` | `chunkedUploadSegment` | 9 | 提取 `segmentOpts` struct |
| `pkg/server/handlers.go:76` | `handleCRUD` | 8 | 提取 `fileOpCtx` struct |
| `pkg/server/handlers.go:337` | `handleRename` | 8 | 同上 |
| `cmd/sclient/tunnel.go:79` | `tunnelRequest` | 8 | 提取 `tunnelReqOpts` struct |
| `pkg/client/chunked.go:522` | `retryChunkUpload` | 10 | 提取 `chunkUploadOpts` struct |

模式：
```go
// before
func handleCRUD(w http.ResponseWriter, r *http.Request, h *Handlers, name string, checksum string, ...)

// after
type fileOpCtx struct {
    w        http.ResponseWriter
    r        *http.Request
    h        *Handlers
    name     string
    checksum string
    // ...
}
func handleCRUD(ctx *fileOpCtx)
```

### Commit 2.2：go:S3776 认知复杂度（Top 8 高复杂度）

按复杂度排序修复：

| 复杂度 | 文件:函数 | 策略 |
|--------|----------|------|
| 45 | `pkg/client/chunked.go:handleChunkLogic` | 提取 checksum 验证、重试逻辑、session 存储为独立函数 |
| 34 | `pkg/server/chunked_download.go:handleChunkDownload` | 提取流式校验 + 文件 seek/read 重试路径 |
| 30 | `pkg/server/auth.go:requireAuthIfNeeded` | 扁平化嵌套，early return |
| 28 | `pkg/tunnel/tunnel_mux.go:routeRequest` | 提取路由匹配 decision 函数 |
| 23 | `pkg/client/chunked.go:doUpload` | 拆分为 init/send/complete 三阶段 |
| 23 | `cmd/sproxy/root.go:runServer` (#244) | 提取 listener 创建和 graceful shutdown |
| 21 | `cmd/sclient/tunnel.go:handleTunnelConn` | 提取连接握手、数据读取循环 |
| 20 | `pkg/server/chunked_upload.go:handleChunkUpload` | 提取分块校验和响应构造 |

### Commit 2.3：go:S3776（剩余 ~20 个 16-19 复杂度）

每个提取 1-3 个辅助函数即可降至 ≤15。涉及文件：
- `cmd/sproxy/root.go` x3, `cmd/sclient/tunnel.go` x2
- `pkg/server/handlers.go` x5, `pkg/server/chunked_upload.go` x2
- `pkg/client/chunked.go` x2, `pkg/client/client.go` x2
- `pkg/tunnel/tunnel_mux.go` x2
- `pkg/server/archive.go` x1

### Commit 2.4：测试文件修复

- `gosecurity:S2083` x8（BLOCKER）— 测试中 `client_test.go`、`benchmark_test.go` 使用 `t.TempDir()` 替换用户输入路径拼接
- `godre:S8193` x8 — hub_test.go、store_test.go、integration_test.go 等去掉多余临时变量
- `godre:S8196` x1 — 单方法接口重命名（`InterfaceXxx`）
- `godre:S8184` x1 — 空白导入加注释

---

## PR #3：index.html 前端重构

> 纯前端变更。预计提交 2 个。

### Commit 3.1：CSS 提取 + 对比度修复

- `style.css` — 从 `index.html` 抽取全部内联 CSS
- 修复 6 处 `css:S7924` 对比度不足：
  - `.btn-primary` `#4a90d9` → `#3a7bc8`（对比度 3.0→4.5+）
  - `.btn-warning` `#f39c12` → `#d68910`（对比度 2.8→4.5+）
  - 其余 `#e74c3c`、`#95a5a6` 同理调整色值

### Commit 3.2：JS 文件拆分 + 问题修复

```
web/static/
├── index.html    # HTML 骨架 (~100行)
├── style.css     # 全部 CSS (~180行)
├── sha256.js     # 纯 JS SHA-256 实现 (~95行)
├── tunnel.js     # 隧道加解密 + 流式下载 (~120行)
├── upload.js     # 分块上传 + 续传管理 (~260行)
└── app.js        # 主逻辑：文件列表、CRUD、批量操作、导航、stats (~280行)
```

JS 文件通过 `<script>` 按依赖顺序加载（sha256 → tunnel → upload → app），全局函数共享。

各文件间接口：
```js
// sha256.js — 全局 class Sha256
// tunnel.js — 全局: getTunnelCryptoKey(), tunnelRequest(), tunnelDownloadStream()
// upload.js — 全局: chunkedUpload(), uploadFiles(), computeSHA256(), 续传管理
// app.js — 全局: refreshList(), searchFiles(), downloadFile(), deleteFile(), ... + 事件绑定
```

**修复问题对照**：
| 规则 | 数量 | 修复方式 |
|------|------|---------|
| S2814 变量重复定义 | 28 | 拆分文件后各文件独立作用域 + `var`→`let`/`const` |
| S2392 外部引用 | 19 | `var`→`let`/`const` + 闭包改为参数传递 |
| S3776 认知复杂度 | 6 | 提取辅助函数 |
| S7767 `\| 0` | 13 | `Math.trunc()` |
| S4138 `for` | 13 | `for...of` |
| S7781 `replace()` | 7 | `replaceAll()` |
| S7762 `removeChild` | 4 | `element.remove()` |
| S2486 空 catch | 3 | 加 error 日志 |
| S7780 `String.raw` | 2 | 使用 `String.raw` 替代反斜杠转义 |
| S6844 `<a>` 作按钮 | 1 | `<a href="...">` → `<button>` |
| InputWithoutLabel | 2 | 加 `<label>` 关联 |
| S1121 赋值表达式 | 1 | `while (!(readResult=await reader.read()).done)` → 拆分为两行 |
| S7773 `parseInt` | 2 | `Number.parseInt()` |
| S7761 `getAttribute` | 2 | `dataset` 属性 |
| S7765 `.indexOf()` | 2 | `.includes()` |
| S7785 `refreshList()` | 1 | 顶层 `await` |

---

## 执行顺序

```
PR #1（安全+机械） ──→ PR #2（Go逻辑） ──→ PR #3（前端）
       │                      │
       │ 预计 3 commit        │ 预计 4 commit
       │ review: 低风险       │ review: 需关注
       │                      │
       └──── 可先合入 ────────┘     └── 独立 review
```

## 验证方式

**每个 commit 后**：
```bash
make build          # 编译验证
go test ./...       # 全量测试
golangci-lint run ./...  # lint 检查
```

**PR 合入前**：
```bash
make check-ci       # CI 全量检查（含覆盖率门禁）
```

**合入后**：
- SonarCloud 自动扫描，确认问题数量减少
- 检查质量门禁状态是否恢复为 Passed
