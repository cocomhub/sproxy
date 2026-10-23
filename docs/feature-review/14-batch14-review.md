# 审查：批次 14——新合并功能（68 项，AI/集群/sclient/安全/传输/WebUI）

- **批次**：14
- **审查者**：父会话（实现 agent 合并后直接审查）
- **审查基线**：master `55b20e587`（#564-#639 区间 68 个 feat/fix/test）
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过（无 P0/P1/P2；2 项 P3 记录）

**发现数**：P0 0 / P1 0 / P2 0 / P3 2

## 发现清单

### [P3] VPN 仅 P1 骨架（#607）
- **位置**：`pkg/vpn/`（PlatformProbe + TUNDevice 接口 + Router 主循环）
- **问题**：真设备打开（Linux ioctl TUNSETIFF / Windows wintun.dll）+ MTU/地址装配是 P2/P3 片——当前 `mesh up --tun` 在未支持平台 fail-closed 报错。
- **建议**：可接受（roadmap 明示 P1 骨架，P2 真设备后续片）；记录为演进约束。

### [P3] selfupdate 平台覆盖（#577）
- **位置**：`pkg/selfupdate/`（tar 条目路径校验 / 统一 + 反斜杠拒绝）
- **问题**：Windows 两段式替换需先退出自身再替换——进程退出时序依赖外部（`upgrade` 命令文档提示重启）。
- **建议**：可接受（原子替换 + 校验完整，两段式是 OS 限制）；记录为演进约束。

## 通过项（无问题面）

- **G1 AI 系**（#627-#630 + #573）：InsightCache 落盘 + mtime 失效；VectorStore 余弦 top-k + 幂等 rev；AIQuota 前置门（超限不扣减 + 跨天惰性重置 + overrides）；AIPrivacy fail-closed（未加密卷拒绝 + 加密卷透明写 + purge 删除权）。TDD + 变异全命中（历史 9 变异）。
- **G2 集群系**（#576/#578/#634-#636）：StateStore 接口（Get/Put/CAS 原子 + ErrKeyNotFound 哨兵 + Watch 变更流）；LeaderElector Local flock 恒主（Unix flock / Windows LockFileEx 双平台互斥）+ WriteGuard ErrNotLeader（503）；IndexEnvelope rev 单调 + 副本旧 rev 忽略 + 损坏回退重建；NodeRegistry CAS joining→active 仲裁 + 状态机校验；EventBus 环形缓冲 + Last-Event-ID 重连回放 + 写路径全接线。
- **G3 sclient 系**（#571/#577/#587/#609/#612/#618/#626/#564/#608）：runBatchConcurrent 槽位保序 + panic 隔离 + SIGINT Skipped；selfupdate SHA-256 fail-closed + 原子替换 + tar 路径校验；i18n 字典门禁式防漏译；migrate 幂等/冲突/校验。
- **G4 安全/运维**（#566/#572/#592/#584/#588/#590/#591）：RBAC RoleReader 只读子组；resolveClientIP 信任链（右向左 + 畸形 fail-closed + 上限）；OIDC/LDAP ext module + 宿主注入；audit 轮转（max_size + 归档修剪）；webhook HMAC 出站签名（ts+nonce 防重放）；tagsStore 索引缓存 + store 权威；retention 按卷三维清理。
- **G5 传输/协议**（#624/#625/#623/#600/#602/#617/#597/#598/#607）：compressx 注册表（gzip/zstd/brotli + Parse 显式校验）；限流 per-endpoint + 全局并发（新维度默认关闭零回归）；联邦强一致（conflict_mode fail-fast 装配禁静默降级）；PickExitGroup 纯函数（failover/round-robin/weighted）；SelectRoute 域名/cidr 分流 + --route/--exit-only 互斥。
- **G6 WebUI/生态**（#604/#620/#637-#639/#606/#613/#615/#619/#596/#599）：R10 门禁全过（node 单测 + Playwright e2e）；checksum 巡检告警联动（source=checksum_mismatch）；dup_report 台账快照 + 全仓扫描；MCP 独立二进制（JSON-RPC + stdio + Bearer）。
- **聚焦测试全绿**：6 组 30+ 包 `go test -run 'TestAI|TestState|TestLeader|TestCluster|TestBatch|TestCompress|TestVerify|...'` 全 ok。

## 验证方式

- 源码逐路径审查（AI 配额门/StateStore 原子性/负载均衡纯函数/信任链/校验面）
- 聚焦测试 6 组全绿
