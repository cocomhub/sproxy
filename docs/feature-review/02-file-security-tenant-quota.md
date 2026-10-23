# 审查：多租户 + 配额（os.Root 防穿越 + quota.Scope 双账本）

- **批次**：2
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### 租户隔离（os.Root 防穿越）
- **Root 封装**：`pkg/storage/root.go` 全部文件操作（Open/OpenFile/Stat/Lstat/ReadDir/MkdirAll/Remove/RemoveAll/Link/Rename/Chtimes）委托 `os.Root`——标准库对**每路径分量强制 O_NOFOLLOW**，中间目录符号链接指向 root 外即报错（不逃逸）；最终组件符号链接不跟随。
- **LAYOUT_VERSION**：OpenRoot 校验/写入版本标记（`root.go:55-73`）——布局升级检测。
- **租户六桶**：`Tenant.Buckets()` = user/cloud/archive/chunk/version/meta（`tenant.go:52-54`）；`UserRel`（NormalizeRemote + validSegments + 首段 `__` 拒绝）保证用户路径恒在 user/ 桶内，与顶层功能桶物理隔离。
- **Abs 派生**：`Root.Abs`（`root.go:143-168`）字符串级校验（拒绝绝对/`..`/Windows 卷名 + 前缀校验）供 os.SameFile 交叉验证——**防符号链接逃逸双保险**。

### 配额双账本（quota.Scope）
- **双账本结构**：Scope = 段树（`Pool.EnsureScope`/`Mount`），每层 committed（已确认占用）+ reserved（预留中）双计数；`available = max − (committed+reserved)`。
- **reserveUp/commitUp/releaseUp/releaseCommittedUp**（`quota.go:200-330`）：沿父链逐级累加/对账/释放，**只向上传播本层实际生效量**（release 钳到 0 防祖先负值放行、防连带扣减其它子桶）。
- **Reservation 原子性**：`Commit(actual)`/`Release()` CAS 保证**至多生效一次**（`quota.go:410-425`）——重复调用忽略，防双结算。
- **重启校准**：`reconcileQuotaScopes`（`quota_reconcile.go:37-92`）启动/周期扫描按磁盘实际字节 Adjust 收敛；「在途预留（Reserved>0）或子层 skip 时跳过」双计保护；扫描幂等（读-修正非原子竞态下次自愈）。
- **卷容量池**：`adjustVolumePool` 只按**功能桶顶层键**求和（防嵌套文件 user + user/videos 双计）；预留>0 跳过。
- **写路径结算**：上传 Commit(prev,written) Adjust 差分 / 删除 ReleaseUsage(size) / rename 跨键 TryReserve→Rename→Release / restore reserve-then-commit——全部提前 return 路径有 Release（逐路径核对）。
- **测试**：`quota_settlement_test.go`、`quota_reconcile_test.go` 存在；`go test` 全绿。

## 验证方式

- 源码逐路径审查（Root 封装 + Tenant 映射 + Scope 段树 + reconcile 校准）
- `go test -count=1 -timeout 120s -run 'TestQuota|TestTenant|TestRoot' ./pkg/quota/... ./pkg/storage/...` → **ok**
