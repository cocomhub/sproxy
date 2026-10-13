# 审查：批次 13——新合并功能（内容索引/cron 调度/QUIC 0-RTT）

- **批次**：13
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `1e7827ac`（#559/#561/#562）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过（无 P0/P1/P2；2 项 P3 记录）

**发现数**：P0 0 / P1 0 / P2 0 / P3 2

## 发现清单

### [P3] QUIC 0-RTT 重放语义（#562）
- **位置**：`pkg/tunnel/xfer/ext/quic/quic.go:565`（Allow0RTT: true）
- **问题**：0-RTT 早数据有重放风险（RFC 9001 §9.3）——但本仓 0-RTT 早数据仅 announceMagic（幂等连接信令）+ 上层隧道帧（内层 AES-GCM 新 nonce/会话语义），重放 = 建立重复连接无副作用，密文不泄露。
- **建议**：可接受；如需严格禁重放可配置关 Allow0RTT。记录为演进约束。

### [P3] 内容索引头部采样语义（#559）
- **位置**：`pkg/files/search_index.go`（sampleTokens）
- **问题**：大文件只索引首 4KiB——正文命中受限（文件中部关键词搜不到）；>4MiB 文件返回 nil 不索引。
- **建议**：可接受（可选开关默认关，权衡索引成本）；如需全文可扩展采样策略。

## 通过项（无问题面）

- **#559 内容索引**：WithContentIndex(true) 可选开关（默认 false 零回归）+ 首 4KiB 抽样词元（字母数字 ≥2 字符，小写）+ 写路径 upsert 与全量构建 walkUserRoot **同源**（OpenDecrypted 抽样——加密卷兼容）+ 搜索命中 base 或 contentTokens（仅 content 开关开时）。rename 保留 contentTokens（内容不变随文件迁移）、remove/removePrefix 清条目——一致性完整。TDD + 变异命中（正文命中条件删→红）。
- **#561 sync schedule cron**：标准库 5 字段 cron 解析（分 时 日 月 周 + */N 步长/区间/逗号列表，零新依赖）+ nextAfter 每分钟检查（严格大于，366 天兜底报错）+ 主循环到点触发串行 + triggerSync 复用（CreateSyncTask 服务端执行，客户端只调度）。TDD + 变异命中（nextAfter 不前进→红）。
- **#562 QUIC 0-RTT**：客户端 DialTLSConfig 装配 LRU 64 session ticket 缓存 + 服务端 Allow0RTT + 二次建连 0-RTT 直发（E2E 二次建连 + 纯本地断言）。变异命中（缓存删→红）。
- **聚焦测试全绿**：`go test -run 'TestContentIndex|TestSearchIndex|TestCron|TestSchedule|TestQUIC'` 全通过。

## 验证方式

- 源码逐路径审查（内容索引 upsert/rename/remove 一致性 + cron nextAfter/主循环 + QUIC 0-RTT 装配）
- 聚焦测试全绿
