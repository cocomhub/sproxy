# TASK: TestAuth_SaveKeysSigns e2e flake 根治（auth_config e2e 加固）

## 背景
`TestAuth_SaveKeysSigns`（web/e2e/auth_config_e2e_test.go）在 CI 频繁偶发失败（已观察 4 次：本批 #440/#441 各 1 次 + 上批 #434 等 2 次）。
失败模式：`waitTextGone` 超时（30s）——点 refresh 后「请求失败」文本未在时限内从 #file-list 消失。
本地复跑 PASS（4.26s）→ 环境敏感 flake。

## 根因方向（协调者先期调研，实施前复核）
1. `waitTextGone(t, page, "#file-list", "请求失败", 8000)` 用 `InnerText` 轮询 + `testutil.WaitFor`（下限 30s）——**只断言「文本消失」为消极条件**：若 refreshList 请求在保存凭据后仍 401（一次请求失败），文本出现后要等下一次成功请求才消失——CI 高负载下时序放大
2. 潜在干扰：#434 的 eventsStart（SSE）在保存凭据前用空凭据建流（401 停止）——保存后未重建，若 SSE 重连风暴与列表请求并发会争用连接
3. **断言应正向化**：不只看「文本消失」，要看「带签名请求成功返回 + 列表渲染成功」（空态/表格出现）

## 交付内容
1. **TestAuth_SaveKeysSigns 加固**（web/e2e/auth_config_e2e_test.go）：
   - `waitTextGone` 之后加**正向断言**：`#file-list` 不再含「加载中」（列表已渲染完成，出现「暂无文件」空态或表格）
   - 用 `testutil.WaitFor` 等「最后一次带签名请求已成功」（可用 recordSignedRequests 的 waitIncrease 后加响应断言，或等列表非错误态）
   - 明确等待时序：刷新点击后先等「请求失败」可能出现再消失（避免一开始就没失败文本的假绿）
2. **排查同类测试**：web/e2e/ 下其他用 `waitTextGone` 的用例（grep）同样加正向断言
3. **变异验证**：故意让签名凭据错误（如保存错误 SK）→ 测试红（证明测试能抓真认证失败，不只是 flake 免疫）

## 硬约束
- 纯标准库测试；只绑 127.0.0.1；真浏览器 Playwright
- 中文注释，UTF-8 无 BOM；SPDX 头
- 不改生产代码（除非发现 #434 SSE 真干扰 app.js——若有，记 TODO 不阻塞，先加固测试）
- 本地复跑：`go test -count=1 -tags=e2e -run 'TestAuth_SaveKeysSigns' ./web/e2e/ -timeout 5m` 连续 3 次 PASS
- `make web-test`（node 部分）+ `go test ./internal/archcheck/` 绿

## 验收标准
- 本地 TestAuth_SaveKeysSigns 连续 3 次 PASS（含加固后断言）
- 变异验证命中（错误凭据 → 红）
- 全量 e2e 相关不回归
- CI 上不再 flake（观察后续批次）

## 流程
1. 先写红灯测试（现有断言：等文本消失——先复现 flake？本地难复现，用「注入错误凭据」造红证明测试能抓失败）
2. 加固断言（正向化 + 时序明确）
3. 本地连续 3 次验证 + 变异验证
4. 提交：`fix(e2e): TestAuth_SaveKeysSigns 断言正向化（等列表渲染成功而非仅文本消失，根治 CI flake）`
   - 只 git add 本任务文件；多重 -m；禁 Co-authored-by；禁 --no-verify；提交前 export PATH
5. 写 REPORT.md
