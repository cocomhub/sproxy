# 2026-09-24 S3 CompleteMultipartUpload：配额记账（双账本对齐 routeUpload）

## 背景/目标

现状（已核实）：`s3CompleteMultipart` 拼接目标文件时**完全不经配额账本**——owner 全局 Scope（`s3TenantFor` 对应卷租户）与卷容量池都未预留/结算，与普通上传（`files.WriteFile` → `routeUpload` 的「Scope + 卷容量池双 TryReserve → 写 → Commit」）语义分叉：
- owner 配额超限时 complete 依然落盘（配额绕过）。
- 卷容量池（`max_storage_bytes`/`volSet.Pool`）对 S3 分块路径虚高无感知（依赖 reconcile 自愈）。
- 目标 rel 已存在时（覆盖写）不释放旧文件占用（`Adjust(prev, written)` 缺失，双计）。

目标：complete 落盘前按「合计 parts 大小」双账本 TryReserve；写成功后 Commit/Adjust；失败 Release。超限 → **507 Insufficient Storage**（S3 协议语义：`InsufficientStorage`，AWS SDK/rclone 可读）。

## 组件与接口

改动面：仅 `pkg/server/s3_multipart.go` + 一处复用（`pkg/files` 的配额语义经 `Handlers.rt` 暴露，见下）。

新增/调整的私有函数（Handlers 上）：
- `s3QuotaScope(owner, rel string) *quota.Scope`：委托 `h.rt.quotaScope(owner, rel)`（与 `pkg/files` 写/删/rename 同键：按 rel 路径解析子 Scope，父链聚合到 user/租户/全局）；未装配 → nil（**退化为无配额记账**，与 files 包一致——保留旧行为，零回归面）。
- `s3PartTotalSize(root *storage.Root, uploadID string, parts []Part) (int64, error)`：逐个 `root.Open(partRel)` + `Stat`，求和（**Stat 而非读内容**——part 已上传并校验过，避免二次 IO；与 `s3UploadPart` 的 `size.DefaultChunkBodyLimit` 上限配合，总量有界 = parts×64MiB，理论上限 ~640GiB，实际由磁盘决定）。

复用（不新增类型）：
- `quota.Scope.TryReserve(estimate) (*Reservation, error)` / `Commit(actual)` / `Release()`。
- 卷容量池：`h.rt.volSet().Pool(volName)` 的 `TryReserve/Commit/ReleaseCommitted`（**与 files 包同一对象**——`routeUpload` 的卷池双账本在 `pkg/volume/registry`，S3 路径必须操作同一 Pool 实例，否则双计/漏计）。
- 目标 rel 已有旧文件：写前 `root.Stat(rel)` 得 `prev`（覆盖写差分 `Commit(prev, written)` 语义同 `routeUpload`；**本设计用 `Adjust(prev, written)` 的 Scope + `Commit(prev+written)` 的卷池**——对齐 `routeUpload` 的双账本差分口径，见数据流）。

接口不变：`s3Handler` 路由、XML 响应、状态码契约（新增 507 为唯一新状态码）。

## 数据流（在 ETag 片 P1/P2 之后叠加）

1. complete 流程按 ETag 片排序/校验通过后、`OpenFile` 目标前：
2. `total := s3PartTotalSize(root, uploadID, parts)`（任一 part Open/Stat 失败 → 400 part 缺失，同 ETag 片）。
3. **配额预检（双账本 TryReserve，任一失败 → 507）**：
   - `scope, ok := s3QuotaScope(owner, rel)`；ok 且非 nil → `scope.TryReserve(total)`（失败 = `quota.ErrStorageFull` → 507「s3: 配额不足」）。
   - 卷池：`pool := volSet.Pool(volName)`（volName 由 `h.volumeNameForRoot(root)` 语义解析：s3TenantFor 已按 bucket ctx 定位卷，`volSet().Pool(vol)` 取池；无 volSet → 跳过）→ `pool.TryReserve(total)`（失败 → 507「s3: 卷容量不足」；**先 Scope 后卷池，失败时回滚已成功者**——`Release()` 已成功预留）。
4. `prev` 预读：`root.Stat(rel)` 成功 → `prev = stat.Size()`（**注意：预留在 prev 统计之前，沿用 routeUpload「预留新总量、Commit 差分」的同序**——见风险，对齐优先于理论最优）。
5. 拼接 + ETag 校验（ETag 片流程）→ 成功后：
   - 覆盖写（prev>0）：`scope.Adjust(prev, total)`（总占用收敛到新大小）；卷池 `pool.Commit(prev+total)`（卷池账本按「本次实际新增 = total，但差分口径用 prev+total 需与 routeUpload 逐字一致——**两者对齐：routeUpload 卷池 Commit(prev, written) 实际签名是 Adjust 语义**，见风险注）。
   - 新文件（prev==0）：`scope.Commit(total)` + `pool.Commit(total)`。
6. 任一失败（拼接 IO / ETag 不匹配 / 目标 OpenFile 失败）：已预留的 `Release()`（双账本各自 Release；目标已创建则 Close+Remove）。
7. **边界：预留成功但 ETag 校验失败** → 两账本均 Release，part 保留（重试 complete 重新走全流程）。

**签名对齐注（关键决策）**：`routeUpload` 的「双 TryReserve → 写 → Commit」中，Scope 用 `Commit(prev, written)`（`quota.Scope.Commit(actual)` 只有单参——差分由调用方传 `written`？核实：`routeUpload` 传 `route.Commit(prev, written)`，其中 route.Commit 是 `Scope.Commit(actual)` + `pool.Adjust(prev, next)` 的组合封装。S3 侧无 route 封装，**直接双调**：`scope.Commit(total)` 或 `scope.Adjust(prev, total)` + `pool.Commit(total)`（新文件）——本设计采用此直调形态，语义等价，见测试变异点「双账本差分」）。

## 错误处理

| 状态 | 场景 | 说明 |
|---|---|---|
| 507 | Scope TryReserve 失败 | 文案 `s3: 配额不足`（S3 InsufficientStorage 语义） |
| 507 | 卷池 TryReserve 失败 | 文案 `s3: 卷容量不足`；先回滚 Scope 预留 |
| 400 | part Stat 失败（合计大小时） | 同 ETag 片 part 缺失语义 |
| 其余 | 与 ETag 片完全一致 | 失败统一 Release 双预留 + 清目标 |

预留生命周期：**从 TryReserve 到成功 Commit 或失败 Release，不留悬空 reserved**（对齐 `quota.Reservation.done` CAS——Commit/Release 至多一次，双账本各自独立句柄）。

## 测试 + 变异点（TDD，先红灯）

装配：复用 ETag 片测试基建；配额通过 `Handlers` 装配（`rt.quotaScope` 配置 owner 全局上限 + 卷池 `SetMaxBytes`）注入。

1. 新文件 complete 超 owner 配额 → 507，目标 rel 不存在，parts 保留。
2. 新文件 complete 超卷池容量 → 507。
3. 覆盖写 complete（目标已存在，内容小于 parts 合计）→ 200 后 `scope.Usage()` 收敛到新大小（无双计）。
4. 覆盖写 complete 缩小（prev > total）→ `Usage` 正确下降（Adjust 负向）。
5. 配额恰好在阈值（合计 == 可用）→ 200（TryReserve 边界不误拒）。
6. 未装配配额（nil scope）→ 200 无记账（旧行为零回归）。
7. 拼接中途失败（注入：ETag 不匹配）→ 507 预留已 Release，`scope.Reserved()` 归零 + 卷池 Reserved 归零。
8. 成功 complete 后 `scope.Usage()` == total（新文件）/ == prev+diff（覆盖写）且卷池 Usage 同步。

变异点（变异后必须红）：
- 删 Scope TryReserve → 测试 1 红。
- 删卷池 TryReserve → 测试 2 红。
- 删覆盖写 Adjust/差分 → 测试 3/4 红。
- 删失败 Release → 测试 7 红（Reserved 残留断言）。
- 删 Commit → 测试 8 红。

## 片划分

独立于 ETag 片（配额只依赖 ETag 片的「排序+校验+单遍拷贝」完成态，可串行接片）：
- P1：合计大小 Stat + 双账本 TryReserve/Commit/Release（新文件路径）。
- P2：覆盖写 prev 差分（Adjust）+ 失败回滚完整化。
- P3：507 文案/状态码契约收尾 + 测试补齐。
建议 PR 顺序：ETag 片先合并（提供校验后的拼接循环），配额片在 ETag 片上接。

## 风险与零回归保证

- **行为变更（有意）**：超配额时 complete 从「成功」变 507——这正是目标（堵配额绕过）。
- **覆盖写双计风险**：旧文件占用若未在 complete 前入账（如经普通上传写入的 rel），prev 差分正确；若旧文件自身来自此前未记账的 S3 complete（升级前历史），首次 Adjust 会少计 prev——**可接受**：账本随后续 reconcile 收敛，且本片后所有 complete 都记账。
- **双账本一致性**：S3 侧无 route 封装，Scope/卷池**分别直调**——需在测试 7/8 同时断言两账本，防只记一边（变异点已覆盖）。
- **并发**：complete 与 abort/并发 complete 无会话级锁（沿用现状，同 ETag 片残余）；配额预留/结算均在单请求内完成，账本原子性由 `quota` 包锁保证。
- **零回归**：未装配配额（默认装配无 quotaScope/volSet）走 nil 分支 = 旧行为；新增 507 不触碰既有 200/400/500 路径；改动局限 `s3CompleteMultipart`。
- **已知残余**：part 文件占盘不随配额记账（parts 在 chunk 桶，不在 user 桶 quota 键内）——abort/GC 策略另片；目标 rel 的 checksum 台账（`recordUploadSuccess` 等价物）S3 complete 一直未接，属独立片。
