# MCP server 设计（S5-①，roadmap 11.9-①）

## 背景与目标
- 现状：sproxy 已有 95+ HTTP 路由（routes.go 全景：文件/分块/卷/回收站/分享/云端下载/同步/Hub/审计/凭据/通知/WebDAV/S3/隧道），`pkg/client` FileClient SDK 封装了 SproxySig 签名、隧道、分块管线；**无 MCP 协议层**。
- 目标：`cmd/sproxy-mcp/` **独立二进制**（不进 sproxy 主服务，零回归），作为 MCP 服务器暴露 sproxy 文件能力给 AI CLI（Claude Code/Codex 等）——工具后端全部走 FileClient SDK，对服务端零改动。
- 协议：手写 **JSON-RPC 2.0**（initialize / notifications/initialized / tools/list / tools/call / ping / shutdown / exit），**不引第三方 MCP SDK**（先确认生态：标准库 + 手写协议，依赖策略贴合「标准库优先」）。
- 传输：**stdio**（本地 AI CLI 默认）+ **SSE**（远程 HTTP，v2 可选）；认证 Bearer = 复用凭据 SK 的 SproxySig 签名（与 sclient 同构造）。

## 组件与接口
1. **`cmd/sproxy-mcp/main.go`**：cobra 入口，`--server` / `--access-key` / `--access-key-secret` / `--access-key-id` / `--transport=stdio|sse` / `--sse-addr`（默认 `:18900`）/ `--volume`（可选卷上下文）。
2. **`pkg/mcp/protocol.go`**：JSON-RPC 2.0 编解码（`json.Decoder` 逐消息读 + `json.Encoder` 写，行/消息分隔：Content-Length 帧（LSP 风格）或单行 JSON——**v1 定单行 JSON + `\n` 分隔**，简单且 Claude/Codex stdio 均兼容）；`Request/Response/Error{code,message,data}`、`ParseError/-32601 MethodNotFound/-32602 InvalidParams` 等标准错误码。
3. **`pkg/mcp/server.go`**：MCP 会话状态机——`initialize` 校验 protocolVersion（返回 `protocolVersion` + `capabilities.tools`）→ `notifications/initialized` → `tools/list`（静态工具清单）→ `tools/call`（按 name 分派）；未初始化即收到 tools/* → 错误；并发经 session 级串行锁（单用户单会话，简单正确）。
4. **`pkg/mcp/tools.go`**：工具定义表（name/description/inputSchema JSON Schema）与 `ToolHandler func(ctx, args) (any, error)` 分派：
   - `read_file` → FileClient.Download 到内存（限长，如 1 MiB）
   - `write_file` → FileClient.Upload（bytes 直传）
   - `list_files`（subdir 参数）→ FileClient.List
   - `search` → FileClient.Search
   - `stat` → FileClient.Stat
   - `mkdir` / `delete`（校验 checksum 防误删）
   - `share_create` → FileClient 分享创建
   - `sync_create` → FileClient 同步任务创建
   - `notify_send` → 通知中心 API（隧道内层可达即用，v1 可延后）
   - 首批 8-10 个，schema 逐字段对齐 routes.go 的既有参数契约（如 `filename`、`subdir`、`checksum`）。
5. **`pkg/mcp/auth.go`**：SSE 传输的 Bearer → 凭据加载（`--access-key/secret`，SproxySig 签名构造复用 `client.NewFileClient` + `WithAccessKey...`）；stdio 传输经环境变量/flag 注入同一构造（本地 AI CLI 直连模式）。

## 数据流
1. stdio：AI CLI 写 JSON-RPC 到 stdin → 协议层逐帧解码 → 会话校验 → tools/call 分派 → FileClient 调 sproxy HTTP API（SproxySig 签名）→ 结果 JSON 化 → stdout 帧回复。
2. SSE：客户端 `GET /sse` 建立事件流（endpoint 下发）→ `POST /messages` 收请求 → 同分派管线 → 事件流回响应（v2；v1 仅 stdio 时此节为预留）。
3. 认证：每请求签名头由 FileClient 计算（本地密钥，永不上线），服务端零感知——即 MCP 工具 = FileClient 的薄封装。

## 错误处理
- JSON-RPC 错误统一 `{code, message}`：解析失败 -32700、方法未知 -32601、参数校验失败 -32602（含工具 inputSchema 校验）、内部错误 -32603；工具业务失败（HTTP 4xx/5xx、checksum 不匹配）包进 -32603 的 `data.message`。
- stdio 帧损坏：返回 ParseError 后**继续读下一帧**（不崩进程）；EOF = 正常退出。
- 认证失败（401）：工具级错误返回，不泄露 SK。

## 测试与变异点
- **Go 单测（pkg/mcp，纯标准库；httptest + 假凭据，127.0.0.1 loopback 铁律）**：
  1. JSON-RPC `initialize` 握手（protocolVersion 协商 + capabilities）——变异：去掉初始化前置校验 → 红；
  2. `tools/list` 返回完整工具清单（含 schema）——变异：清单缺工具 → 红；
  3. `tools/call read_file` 往返：httptest mock server（模拟 GET /download + X-File-Checksum）→ 返回 base64/文本内容——变异：错误路径（404）被当成功 → 红；
  4. 参数校验：`write_file` 缺 data / `delete` 缺 checksum → -32602——变异：校验分支删除 → 红；
  5. 方法未知 / 未初始化调用 tools → 标准错误码——变异：错误码写错 → 红；
  6. `notifications/initialized` 幂等。
- 变异验证法：逐断言改实现字符串/删分支 → 红（TDD 先行，先写红灯测试再实现）。
- cmd 层 e2e（可后续片）：构建二进制 + 子进程喂 JSON-RPC 帧断言响应（test/ 模式，`-race`）。

## 片划分
- **片 1**：pkg/mcp protocol.go + server.go（JSON-RPC 核心 + initialize/tools/list/tools/call 状态机）+ 单测（含变异）。
- **片 2**：tools.go 工具表与分派 + FileClient 封装（首批 8-10 工具）+ httptest 往返测试。
- **片 3**：cmd/sproxy-mcp 二进制（cobra 入口 + stdio 接线 + 凭据装配）+ e2e 子进程测试。
- **片 4（可选）**：SSE 传输 + `--sse-addr`。

## 风险与零回归
- **独立二进制**：不 import sproxy 主服务装配（仅用 pkg/client + pkg/mcp），主服务/既有 CI 零影响；`make build-all` 需把新 cmd 纳入（build 自动发现 `cmd/*` main 包）。
- 协议兼容风险：MCP 生态演进中，手写协议以 **protocolVersion 协商** + 最小能力子集（tools）先行；若生态强制 SDK 再评估（设计期先确认，不引第三方是初始立场）。
- read_file 内存上限防超大文件 OOM；write_file 限长（如 32 MiB，超限走分块管线 v2）。
- 认证：SproxySig 复用既有凭据，无新密钥面；SK 只在本地签名，符合「永不上线」红线。
- 零回归保证：不改 pkg/server / routes.go / cmd/sproxy；新增包与二进制互不影响既有测试（GOWORK=off 独立构建校验需过）。
