# TASK: WebUI SSE 实时刷新（roadmap 2.3 P1 服务端事件通知的 Web UI 验收）

## 背景
#433 已实现 `/api/events` SSE 端点（EventBus 环形缓冲 + Last-Event-ID 游标回放 + per-owner 订阅），
但 WebUI（web/static/app.js）仍是 `refreshList()` 轮询/手动刷新，未接事件流。
本任务把 WebUI 文件列表改为 EventSource 订阅 `/api/events` 增量刷新。

## 交付内容
1. `web/static/` 新增事件流模块（如 `events.js`，与 app-render.js 同隔离原则：纯函数/无 DOM 副作用，
   UMD 挂全局 + module.exports 可 require），负责：
   - EventSource 连接 `/api/events`（owner 从当前认证上下文取，参考 login.js 现有模式）
   - 维护 lastEventID（localStorage 持久化，重连时 Last-Event-ID 回放）
   - 事件类型过滤（upload/rename/delete/mkdir/rmdir）→ 调 app.js 的刷新钩子
   - 断线自动重连（指数退避，上限 30s）
2. `web/static/app.js`：`refreshList()` 保留（手动/错误回退），接入事件流：收到事件 → 增量刷新列表
   （简单做法：直接调 refreshList()；进阶：按事件类型做局部更新，但**局部更新必须同样过 e2e**）
3. **纯函数 node --test 单测**：`web/static/events.test.js`（事件解析、lastEventID 更新、退避计算、过滤逻辑）
4. **Playwright e2e**（`web/e2e/`）：真浏览器验证「上传/删除文件后列表不刷新也自动更新」
   （EventSource 推送 → 列表出现新文件；现有 e2e 用例模式参考 `web/e2e/` 现有文件）
5. 新 JS 文件必须登记进 Makefile `web-test`（门禁 R10：`internal/archcheck/web_assets_test.go` 会拦）
6. 若无 SSE 支持环境（e2e 用 fetch mock / EventSource 不可用时）优雅降级到轮询（**不得静默坏掉现有刷新**）

## 硬约束
- 纯标准库测试（node --test）；不引入第三方前端框架
- 中文注释，UTF-8 无 BOM；SPDX 头 `Copyright 2026 The Cocomhub Authors. All rights reserved.` + `SPDX-License-Identifier: Apache-2.0`
- Web UI 改动硬要求：**必须** node --test 单测 + **必须** Playwright 真浏览器 e2e
- 门禁：`make web-test`（node --test 全绿）；`internal/archcheck/web_assets_test.go`（新 JS 登记 Makefile）
- 不改服务端（/api/events 已就绪）；若发现端点缺陷，记 TODO 不阻塞

## 验收标准
- `make web-test` 全绿（含新增 events.test.js）
- Playwright e2e：上传 → 不手动刷新 → 列表自动出现新文件（SSE 推送）
- `go test ./internal/archcheck/` 绿（R10 门禁）
- 手动刷新/轮询回退路径仍工作（回归）

## 流程
1. 先写红灯测试（node --test 断言事件解析/退避/过滤）
2. 实现 events.js + app.js 接入
3. 真浏览器验证（Playwright，可本地起服务 + chromium）
4. 全量验证：make web-test + 相关 go 门禁
5. 提交：`feat(web): WebUI SSE 实时刷新（EventSource 订阅 /api/events 增量刷新，断线重连+游标回放）`
   - 只 git add 本任务文件；多重 -m 写清「做了什么/怎么做/为什么」
   - **禁 Co-authored-by；禁 --no-verify；禁 git add -A**
6. 写 REPORT.md（交付内容/验证证据/残余）
