# REPORT — WebUI SSE 实时刷新（roadmap 2.3 P1 服务端事件通知的 Web UI 验收）

## 交付内容

1. **`web/static/events.js`（新增）**：文件变更事件流前端模块（UMD：浏览器挂 `webEvents`，Node 可 require），
   与 app-render.js 同隔离原则（纯函数部分零 DOM/网络副作用）。提供：
   - `parseSSE`：SSE 文本流 → 事件数组（id/data 解析、空行分隔、注释行心跳容错、非 JSON data 置 null 不抛）
   - `isRefreshableAction`：事件 action → 是否刷新文件列表（upload/delete/rename/mkdir/rmdir/version）
   - `nextCursor`：Last-Event-ID 游标推进（忽略乱序/非法值）
   - `backoffDelay`：断线重连指数退避（1s→2s→4s→…→30s 封顶）
   - `buildEventsUrl` / `buildEventsHeaders`：/api/events URL 与认证头（有凭据 → SproxySig 签名头；无凭据 → 空）
2. **`web/static/app.js`（接入）**：
   - `eventsOwner()`：AK → `sclientTransport.accessKeyMesh` 解析 owner（无 AK → anonymous）
   - `eventsStart()`：fetch 流式读 SSE（**浏览器 EventSource 无法携带自定义 Authorization 头**，认证态直连
     需要 SproxySig 签名头 → 用 fetch + ReadableStream）；事件 → 记录游标（localStorage 持久化）+ 文件相关动作
     300ms 去抖后 `refreshList()`
   - 断线指数退避重连 + Last-Event-ID 游标回放（重连不丢事件）；401/403 静默停止（认证问题无意义重连）
   - 顶层直接调用（脚本在 </body> 前同步加载，DOMContentLoaded 可能已派发）+ DOMContentLoaded 双保险（幂等）
   - `beforeunload` 断开连接（浏览器保活悬挂防护）
   - **优雅降级**：事件流失败/停止不影响既有刷新路径（按钮/交互/轮询照常工作）
3. **`web/static/events.test.js`（新增）**：8 个纯函数单测（parseSSE/isRefreshableAction/nextCursor/backoffDelay/buildEventsUrl/buildEventsHeaders 签名头）
4. **`web/static/index.html`**：events.js 加载（app-render 之后、app.js 之前）
5. **`Makefile`**：web-test 目标登记 events.js（node --check）+ events.test.js（node --test）——门禁 R10 要求
6. **`web/e2e/events_e2e_test.go`（新增）**：Playwright 真浏览器 E2E：
   - `TestEvents_AutoRefreshAfterUpload`：导航前装 /api/events 请求监听 → 断言订阅建立 → 外部 multipart 上传
     → **不点击任何刷新按钮**，文件表自动出现新文件（SSE 事件驱动 refreshList）
   - `TestEvents_AutoRefreshAfterDelete`：种子文件初始可见 → 外部删除 → **列表自动消失**（delete 事件驱动）
   - 辅助：`watchEventsRequested`（导航前装监听——SSE 请求随页面加载发出）、`uploadFileToVolume`、
     `deleteFileToVolume`（testutil.IsolatedClient 隔离连接池，符合硬规则 17）

## 验证证据

| 验证 | 结果 |
|------|------|
| `node --test web/static/events.test.js` | 8 pass / 0 fail（TDD：先写红灯——模块不存在红 → 实现后绿） |
| `make web-test` | 56 tests pass / 0 fail（含新增 events.test.js） |
| `go test ./internal/archcheck/` | ok（R10 门禁：events.js 已在 web-test 覆盖） |
| `go test web/e2e -run TestEvents_` | ok（上传自动刷新 + 删除自动消失，真 Chromium） |
| `go test web/e2e` 全量 | ok（97.8s，零回归） |
| `golangci-lint run ./web/e2e/...` | 0 issues |
| `go build ./...` | 0 |
| gofmt / node --check | 干净 |

## 关键设计决策

1. **fetch 流式读 SSE 而非 EventSource**：EventSource 无法携带自定义 header，而直连认证态
   /api/events 外层挂在 authMiddleware（需 SproxySig 签名头）——fetch + ReadableStream.getReader
   同时支持认证头 + Last-Event-ID 头（EventSource 也能带 Last-Event-ID 但给不了 Authorization）。
2. **游标持久化 localStorage**：重连时 Last-Event-ID 回放（服务端 #433 的 Replay 语义），关页重开不丢。
3. **去抖刷新**：多分块/多事件风暴 300ms 合并一次 refreshList（幂等，避免列表抖动）。
4. **无凭据场景**：服务端 AllowInsecureLoopback 兜底放行（e2e 无凭据前提），事件 owner=anonymous 与
   页面列表请求一致。
5. **优雅降级**：事件流是增量加速器，401/网络错误不阻塞既有轮询/手动刷新。

## 残余

- 认证态（有凭据）SSE 实时刷新未跑真浏览器 E2E（e2e 全是无凭据场景；认证态由 events.js 签名头 +
  服务端 #433 events_test.go 覆盖）——如需可在后续接入带凭据的 E2E。
- 局部更新（按事件类型只改受影响行）未做——统一 refreshList 全量刷新（简单可靠，满足验收）。
- 卷仪表/传输页不接事件流（本任务只覆盖文件列表页）。
