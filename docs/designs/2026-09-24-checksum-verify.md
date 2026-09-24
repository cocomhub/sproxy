# 全仓 checksum 巡检（11.3-①）

## 背景 / 目标
- 现状：`pkg/server/checksum.go` 只有 `Checksum`/`FileChecksum`/`FileChecksumRoot`/`verifyChecksum` 工具函数（per-request 校验用）；`Handlers.checksumStoreFor(owner)` 有 per-tenant `checksum.ChecksumStore` 台账（`<tenant meta>/checksums.json`，上传时登记）。**无**全卷一致性审计端点（对比台账 vs 实际文件重算）。
- 目标：`POST /api/verify`（重算全部文件 checksum 比对台账 → 坏文件隔离/报告）+ 定时巡检（interval 配置）+ 告警联动（`alertEngine` 挂点，参考既有 volume degraded 模式）。

## 组件与接口
- `pkg/server/verify.go`（新文件，同包）：`verifyHandler`：
  - 请求：`POST /api/verify`，body `{volume?, force?}`（空 = 默认卷全租户）。
  - 数据源：per-tenant `checksumStoreFor(owner)` 的台账快照（文件名 → checksum），加 `storage.ListOwners` 枚举租户（复用 `listTenantIDs`）。
  - 执行：`FileChecksumRoot(root, rel)` 重算每文件（`root.Open` 防符号链接逃逸，全程 root 内）。
  - 报告：`{total, ok, mismatched: [{path, expected, actual}], missing: [...]}`；`mismatched` 文件默认**隔离**（rename 到 `<tenant meta>/quarantine/` 保留现场，删除前需人工确认——遵守「删除操作先确认」红线，巡检只隔离不删）。
- 定时巡检：`Handlers` 增 `verifyStop/verifyWg`（与 `versionGC`/`trashGC` 同构周期 goroutine）；`cfg.Verify.Interval > 0` 时挂载；结果写审计 + `RecordAudit`。
- 告警联动：不一致数 > 0 → `alertEngine` 挂点（若已启用），通知中心 dispatch。

## 数据流
1. 手动 `POST /api/verify`（或定时触发）→ 枚举租户 → 读台账。
2. 逐文件 `FileChecksumRoot` 重算 SHA-256 → 与台账 `checksum.Equal` 比较。
3. 一致 → `ok++`；不一致 → `mismatched` 列表 + 隔离 rename；台账有但文件缺失 → `missing`。
4. 报告落审计 + 告警；返回 JSON。

## 错误处理
- 文件读取中途 I/O 错误 → 记入 `errors` 列表（区分 checksum 不符 vs 读失败，不混淆）。
- 台账损坏/缺失 → 该租户记 warn + 跳过（不因单租户失败中止全卷巡检；结果含 `skipped` 计数）。
- 巡检进行中并发上传 → 文件被重算期间变化：以「读时刻」为准，结果可能瞬时不一致 → 报告标注 `concurrent` 提示重跑（不做锁；文档说明）。
- 隔离 rename 失败（跨桶/权限）→ 记错误，文件留在原位但列入报告（不静默）。
- 定时巡检与前一次重叠 → 跳过本次（busy 标记，防堆叠）。

## 测试 + 变异点
- `TestVerify_AllConsistent`：台账与文件全一致 → `{ok=N, mismatched=[]}`（变异：比较逻辑反/不重算 → 红）。
- `TestVerify_MismatchReported`：篡改一文件内容 → 该文件进 mismatched + 被隔离（变异：不隔离/漏报告 → 红）。
- `TestVerify_MissingFile`：删文件 → 进 missing（变异：missing 统计缺失 → 红）。
- `TestVerify_TimerTriggered`：interval>0 → 周期 goroutine 触发执行（变异：不触发 → 红）。
- `TestVerify_ConcurrentSkip`：busy 时第二次触发 → 跳过（变异：重叠执行 → 红）。
- 变异验证核心：把 `verifyChecksum` 改成恒 true（假绿）→ 测试红。

## 片划分
- P1：`POST /api/verify` 手动端点 + 报告 + 隔离。
- P2：定时巡检 goroutine + 审计/告警联动。
- P3：大卷并发巡检（worker 池 + 进度）+ 巡检结果持久化。

## 风险与零回归
- 新增路由默认装配（POST /api/verify 只读审计，不改变任何写路径）；隔离仅 rename 到 quarantine，不删除任何数据。
- 零回归：所有既有文件操作路径不动；新端点与定时器独立挂载，`Verify.Interval=0` 时行为与现状完全一致。
- 隔离目录落租户 meta（非用户可见桶），避免污染用户卷视图。
