# 外部卷容量纳管：独立容量 + 用量查询 + 系统限额 设计文档

> **状态：** 已确认（2026-09-18 用户定案）
> **定案：** ① 卷级计数记账（写入累计 + 删除释放，超限拒绝）；② WebDAV 仅限额维度（不做卷总量查询——无标准 API）。
> **前置：** 用户卷 + 三个外部后端已合并（master `df8b2527`）。

## 问题（用户 2026-09-18 提出）

1. **用户创建的外部卷容量应为独立容量**（相当于让系统纳管）——现状 `UserVolume.Capacity` 存了但**未强制生效**
2. **系统限制本地卷容量因系统资源优先**——外部卷不占本机磁盘，逻辑不同
3. **外部卷最好能直接查询当前容量情况**（卷总量 + 已用量）
4. **用户能限制本系统可用该卷多少容量**（系统可用限额）

## 现状（勘察结论）

| 项 | 现状 | 缺口 |
|---|---|---|
| 用户卷 Capacity | store 持久化（创建时存） | **未强制**（无上传/同步校验） |
| 外部卷 quota.Pool | volumes.go 建 Pool（系统盘+用户卷） | **无人入账**（写路径不记账） |
| 外部卷用量查询 | 无统一接口（FS.ListDir 遍历成本高） | **缺 backend 级用量查询** |
| 系统可用限额 | UserVolume.Capacity 语义模糊 | 需明确为「本系统可用限额」 |

## 策略分析（控制者）

**两层容量模型**：
```
① 外部卷总量（卷本身容量）   — backend 级查询（如 baidupcs 配额 API / S3 bucket 用量）
② 本系统可用限额             — UserVolume.Capacity（用户设，本系统可用该卷多少）
```

- **本地卷**：系统资源优先 → 限额硬性（owner_quotas + 卷容量）
- **外部卷**：不占本机磁盘 → 容量是「**授权额度**」——限制本系统能占用外部卷多少（防单用户打爆外部存储）
- **独立容量**：外部卷限额**独立**（不计 owner_quotas——已有语义 ✓），但需**强制记账**（超限拒绝）

## 设计方案

### 1. backend 用量查询接口（VolumeStats）

```go
// pkg/volume/registry/backend.go 扩展
type VolumeStatsProvider interface {
    // Stats 返回卷当前容量情况（总量/已用/可用；nil = 后端不支持查询）
    Stats(ctx context.Context) (*VolumeStats, error)
}
type VolumeStats struct {
    TotalBytes int64  // 卷总量（0 = 未知）
    UsedBytes  int64  // 已用量（0 = 未知）
}

// ExternalBackend 可选实现：baidupcs（配额 API）、S3（bucket 用量）、WebDAV（无标准 → 不实现）
```

### 2. 系统可用限额强制（记账）

- `UserVolume.Capacity` = **本系统可用该卷限额**（0 = 不限制）
- 写路径记账：**每任务/每上传**按 owner + 卷入账（quota.Pool 已有——WriteFile 前 TryReserve，超限拒绝）
- 与本地卷区别：外部卷 Pool 是「授权额度」非「物理资源」——但记账机制复用

### 3. 用量查询 API

```
GET /api/volumes/user?stats=true   → 每卷带 Stats（总/已用/限额）
GET /api/volumes（系统盘）          → 外部卷带 Stats
```

- Stats 来源：backend 查询（支持时）+ 本系统记账（Pool.Usage）——两维展示
- 本系统已用 = Pool.Usage（记账）；卷总用量 = backend Stats（查询）

### 4. Web/CLI 展示

- Web 卷面板：外部卷显示「卷总量 / 本系统已用 / 限额」
- sclient volume list：加 stats 列

## 实施拆分（待用户确认后）

| 子任务 | 内容 |
|---|---|
| C1 | VolumeStats 接口 + backend 实现（baidupcs 配额/S3 用量；WebDAV 不实现） |
| C2 | 外部卷记账强制（WriteFile 入账 Pool + 超限拒绝）——系统盘 + 用户卷 |
| C3 | 用量查询 API（/api/volumes/user?stats + 系统盘 stats） |
| C4 | Web/CLI 展示 + e2e + 文档 |

## 已确认（用户定案）

1. **记账语义**：卷级计数记账——外部卷写入时累计（卷级计数器 + Pool），删除时释放；上传/同步路径 TryReserve 超限拒绝。复用本地卷 Pool 模式（write_ops.go Commit/ReleaseCommitted）。
2. **用量查询**：卷总量 backend 级（baidupcs 配额 API / S3 bucket 用量）；WebDAV 仅「本系统限额」维度（无标准 API 不做总量）。
3. **本系统已用** = 卷级计数（Pool.Usage 持久化语义——需要卷级账本或计数器持久化，重启不丢）。
