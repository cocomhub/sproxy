# 审计日志轮转（11.5-⑥）设计

## 背景 / 目标
- 现状（audit_store.go 实证）：AuditStore 双写「内存全量 + append-only JSON lines」，`<persist_dir>/audit.log` 只增不删，长期运行磁盘无界增长；NewAuditStore 启动全量载入。
- 目标：`audit.persist_dir` 开启时可选 `max_size` 触发轮转 + 保留 N 个归档（冷数据）；内存/查询语义完全不动；未配置时零回归。

## 组件与接口
- 配置扩展（pkg/server/config.go）：
  - `audit.max_size`（ByteSize，默认 0 = 关闭轮转，行为与现状逐字节一致）
  - `audit.max_archives`（int，默认 3；0 = 轮转即删不留档）
- AuditStore 新增字段 `maxSize int64` / `maxArchives int`；新增私有方法：
  - `maybeRotateLocked()`：仅在已持有 `s.mu` 时调用（Append 持锁，轮转天然串行，无并发 Rename）。
  - `rotateLocked()`：`file.Close()` → 归档依次移位 `audit.log → audit.log.1 → … → audit.log.N`（超 maxArchives 删除最旧）→ `OpenFile` 重开新 logPath。
- 恢复语义固化：NewAuditStore 只载入当前 `audit.log`（内存 = 热历史有界）；归档为离线合规冷数据不载入内存（防重启内存暴涨）。

## 数据流
`Append(evt)` → 持锁 → 追加内存 → 若 maxSize>0 且 `当前文件 size + len(line) > maxSize` → rotateLocked → appendLine 写新文件 → 释放锁。

## 错误处理
- 轮转失败（Close/Rename/Open 任一失败）：记日志 + 不轮转，继续 append 原文件（尽力而为，与现有「写盘失败记日志跳过」语义一致，绝不阻断业务）。
- 归档移位删除失败：Warn 继续，残留由下次轮转重试清理。
- 文件 stat 失败：跳过轮转检查直接写（退化 = 现状）。

## 测试 + 变异点
1. 边界：size 恰 == maxSize 不轮转、> maxSize 轮转（变异：`>`/`>=` 互换 → 红）。
2. 轮转后新事件进新文件，旧事件仍在归档文件（变异：漏 Rename → 红）。
3. maxArchives 修剪：N+1 次轮转后最旧档被删（变异：不删 → 红）。
4. max_size=0 恒不轮转（零回归断言；变异：无条件轮转 → 红）。
5. 轮转后 NewAuditStore 只载入新文件历史，Len 不含归档（变异：load 读归档 → 红）。
6. 并发 Append + 轮转不丢行（-race，≥1000 事件）。

## 片划分
- P1：配置 + maybeRotate/rotate + 单测（边界与归档修剪）。
- P2：归档移位清理健壮性 + load 语义固化 + 文档（docs/audit.md）。

## 风险与零回归保证
- 与外部 logrotate 双轮转冲突：文档明示「启用内置轮转时关闭外部 logrotate」。
- 零回归：max_size 默认 0 → 无任何行为变化；内存/查询逻辑不动；轮转全程持 s.mu 单线程，无 Windows 并发 Rename 风险。
