# 设计：通知 RSS/Atom 订阅端点（roadmap 11.7-⑦）

- 状态：本期实施 | 日期：2026-09-24 | 功能编号：11.7-⑦
- 现状：pkg/server/notify.go 的 NotifyCenter 维护有界历史（ring 200，historyEntry{TS,
  Action,Object,Channel,Status,Detail}），仅 JSON 端点 GET /api/notify/history?limit=N
  （notifyHistoryHandler），无机器可读的持续通知流。
- 目标：新增 GET /api/notify/feed，输出 RSS 2.0（默认）/ Atom（?format=atom）最近 N 条
  通知；可选 token 门禁；纯标准库实现（encoding/xml），零第三方依赖。

## 1. 组件与接口

- 数据源零改动：复用 NotifyCenter.History()（最近在前的快照），不新增状态字段。
- 新纯函数（pkg/server 新文件 notify_feed.go，同包）：
  - feedEntries(hist []historyEntry, maxN int) []historyEntry
    过滤噪音（排除 status=debounced/skipped，保留 sent/failed）并截断到 maxN。
  - feedMeta{Title, Link, Desc string; Updated time.Time}
  - renderRSS2(entries []historyEntry, meta feedMeta) ([]byte, error)
  - renderAtom(entries []historyEntry, meta feedMeta) ([]byte, error)
- handler 方法：(*Handlers).notifyFeedHandler(w, r)，挂载 GET /api/notify/feed
  （与 notifyHistoryHandler 并列，RegisterRoutes 内注册）。
- 配置（NotifyConfig 扩展，config.go 引用）：feed_max int（默认 20，上限 50）；
  feed_token string（空=公开，非空=强制门禁）。
- 认证：?token= 查询参数或 Authorization: Bearer <token>；crypto/subtle
  ConstantTimeCompare 防时序侧信道；401 不区分「未提供/错误」（防 token 枚举）。
- Link 构造：请求 Host + scheme（r.TLS!=nil → https，否则 http，允许 X-Forwarded-Proto
  覆盖）拼绝对 URL，供 feed link / item link 使用。
- GUID：fmt.Sprintf("%d-%s-%s-%s-%s", TS.UnixNano(), Action, Object, Channel, Status)
  ——同刻多渠道/多状态不撞。
- 时间格式：RSS pubDate=time.RFC1123Z；Atom updated=time.RFC3339Nano。
- Content-Type：application/rss+xml / application/atom+xml（均 charset=utf-8）。

## 2. 数据流

1. 请求 → feed_token 非空时校验 token（失败 → 401，终止）。
2. notifyCenter==nil → 400（与 history 端点一致）。
3. History() 快照 → feedEntries 过滤+截断 → 生成 feedMeta。
4. 按 format 参数（默认 rss，非法值 400）渲染 XML（xml.Header + body）→ 写响应。

## 3. 错误处理

- notify 未启用：400「notify 未启用」（复用既有文案，零回归）。
- token 缺失/错误：401（空 body）。
- format 非法：400，仅支持 rss|atom。
- 空历史：仍输出合法空 channel/feed XML（阅读器兼容，不报错）。
- 渲染失败（理论上不出现）：500 + slog.Warn。

## 4. 测试与变异点（TDD）

- TestFeedEntries_FiltersNoise：debounced/skipped 排除、sent/failed 保留、最近在前；
  变异：过滤条件翻转 → 红。
- TestRenderRSS2_Structure：encoding/xml 解析断言 channel 标题/条目数/pubDate
  RFC1123Z/guid 唯一；变异：guid 去掉 Channel 分量 → 红（同刻多渠道撞 guid）。
- TestRenderAtom_Structure：xmlns 命名空间/entry updated RFC3339Nano/id 唯一。
- TestFeedTokenGate：httptest.NewRecorder + 桩 notifyCenter——无 token/错 token → 401，
  对 token → 200；变异：删除门禁分支 → 红。
- TestFeedLimit：maxN 截断生效；format 非法 → 400。
- 集成：复用 newTestServer（integration_test.go）断言 Content-Type 与 XML 可解析。

## 5. 片划分

- 片1：feedEntries + 两个渲染器纯函数 + 单测 + 变异验证 → 独立 PR。
- 片2：handler + token 门禁 + 路由 + 配置 + 集成测试 → 独立 PR。
- 片3：docs/config.md 补 feed_max/feed_token → 随片2 或 docs-only PR。

## 6. 风险与零回归

- 不改 NotifyCenter 内部状态/字段，仅新增只读入口 → history 端点与渠道发送零影响。
- 新路由 /api/notify/feed 与既有路径无冲突。
- XML 全部经 encoding/xml 自动转义防注入；token 常量时间比较。
- feed 公开会暴露审计信息：默认与 history 一致公开，文档注明生产建议配 feed_token。
- 双格式增加测试面：共享 feedMeta 与过滤逻辑，渲染器各自单测兜底。
