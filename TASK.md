# TASK: WebUI 卷健康仪表（roadmap 3.3 P1 残余）

## 背景
#432 已实现卷健康指标入 /metrics（`sproxy_volume_io_total{volume,op}` + `sproxy_volume_io_failures_total{volume,op}`，
每卷读写延迟/失败率，volume+op 标签）。roadmap 3.3 P1「卷健康/迁移仪表」残余：
**WebUI 卷仪表展示**（面板可见每卷健康与 rebalance 进度；失败卷告警）。

## 交付内容
1. **web/static/ 卷健康面板模块**（如 `web/static/volume-health.js`，与 app-render.js 同隔离原则：
   纯函数/无 DOM 副作用，UMD 挂全局 + module.exports 可 require）：
   - 拉取 `/metrics`（文本 Prometheus 格式）→ 解析 `sproxy_volume_io_*` 系列 → 结构化卷健康数据
   - `parseVolumeMetrics(text)`：纯函数解析 metrics 文本 → [{volume, op, total, failures, fail_rate}]
   - `renderVolumeHealth(vols)`：生成面板 HTML（每卷一行：卷名/操作/总次数/失败率/健康状态徽标）
   - 健康判定：失败率 0 = healthy；>0 但 <5% = warning；≥5% = degraded（阈值常量可测）
2. **web/static/app.js / index.html**：卷面板挂载（现有 user-volumes.js 面板模式参考）：
   - 卷列表加载后渲染健康面板；定时刷新（如 30s，对齐 /metrics 拉取频率）
   - 失败卷（degraded）高亮/告警徽标
   - **rebalance 进度条**：从 /metrics 卷健康数据或既有迁移状态接口取进度（#432 残余中明确
     「迁移进度条」——若服务端无进度字段，记 TODO 只做健康仪表部分）
3. **纯函数 node --test 单测**：`web/static/volume-health.test.js`（parseVolumeMetrics 边界：
   空文本/无指标/标签顺序/失败率计算；renderVolumeHealth 徽标判定）
4. **Playwright e2e**（web/e2e/）：真浏览器验证卷面板渲染（有 metrics 数据 → 面板出现健康行；
   无数据 → 面板显示空态不报错）
5. 新 JS 文件登记 Makefile web-test（门禁 R10：internal/archcheck/web_assets_test.go 会拦）

## 硬约束
- Web UI 改动硬要求：**必须** node --test 单测 + **必须** Playwright 真浏览器 e2e
- 纯标准库测试（node --test）；不引入第三方前端框架
- 中文注释，UTF-8 无 BOM；SPDX 头
- **不改服务端**（/metrics 已就绪）；若缺 rebalance 进度数据源，记 TODO 不阻塞
- `make web-test` 全绿；`go test ./internal/archcheck/` 绿

## 验收标准
- `make web-test` 全绿（含新增 volume-health.test.js）
- Playwright e2e：卷面板在有/无 metrics 数据两种场景下正确渲染
- `go test ./internal/archcheck/` 绿（R10 门禁）
- 现有 UI E2E 不回归（卷管理/文件列表正常）

## 流程
1. 先写红灯 node --test 测试（parseVolumeMetrics 解析错误/失败率计算错 → 红）
2. 实现 volume-health.js + app.js 挂载
3. Playwright 真浏览器验证
4. 全量验证（make web-test + archcheck + 相关 e2e 不回归）
5. 提交：`feat(web): WebUI 卷健康仪表（/metrics volume_io 解析渲染，健康/警告/降级徽标）`
   - 多重 -m：做了什么/怎么做/为什么 + 验证证据
6. 写 REPORT.md（若 rebalance 进度条因缺数据源未做，明确说明残余）
