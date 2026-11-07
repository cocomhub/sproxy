# 统一存储卷模型：按用途封装的本地卷 + 唯一卷访问入口（设计）

> 状态：**设计（待实施）**。来源：用户 2026-10-28 明示 ——
> 「目前本地卷是特例，无法使用 Volume 通用卷接口，例如转存时无法选择本地卷；
> 项目中维护了本地卷和外部卷两大类卷，长期维护复杂，新功能需要实现两份；
> 设计方案统一成一套接口降低维护复杂度。注意必须按卷操作文件，避免直接用裸 FS
> 跳过各种卷功能封装；目标是设计根治方案统一架构设计，避免小修小补治标不治本。」
>
> 前置：`docs/designs/2026-10-16-quota-volume-layer.md` §11（M0-M5 统一存储卷模型
> 的既有分片）——本设计是它的**具体化续篇**（M1 已完成但装配未接线、M3/M5 未做）。

---

## 一、现状取证（2026-10-28）

| 面 | 本地卷 | 外部卷 | 统一度 |
|---|---|---|---|
| 数据面 FS | `NewRootFS(root)`（`rootfs.New`） | `be.FS()`（`ExternalBackend.FS`） | 能力都有，**无统一访问器** |
| 存储身份 | `*storage.Root`（`Set.roots`） | `ExternalBackend`（`Set.external`） | ✗ 双轨（`HasLocalRoot`/`External` 分叉） |
| `StorageVolume` 契约 | ✅ `localBackend` 已实现（M1a） | ✅（`ExternalBackend` 是子集；`capacity.Backend` 内嵌） | ✅ 契约已统一 |
| **装配接线** | 🔴 `assembleLocalVolume` 仍裸 `OpenRoot` 进 `roots`（M1b 注册了 `local` 后端但**没用**） | ✅ `registry.NewBackend` + `WrapStorageVolume` | ✗ |
| 写视图出口 | `externalSinkFor` → `Tenant.Root` | `externalVolumeTransferFS`（`Guard(Wrap(fs))`） | ✗ 两套 recipe |
| 转存目标 | **400 拒绝**（`volSet.External` nil） | ✅ | ✗ |
| URL scheme | 无 | 各类型注册 | ✗ |
| 卷内定位 | `volume://`（`Location.String()` 已存在） | 同 | ✅（读侧） |

**关键判断**：`localBackend`（M1a）+ `RegisterLocalBackend`（M1b）**已存在但装配未接线**——
「注册了却没接上」正是「半成品根治」的形态，本设计把它接完。

---

## 二、目标架构（六层收敛）

**核心原则**：`StorageVolume` 是**唯一卷身份**；`Set.VolumeView(name, use)` 是**唯一取卷入口**
（返回按用途封装的 FS，装配期已套完整装饰链）；消费方**永远拿不到裸 FS**（R27 类门禁根治）。

```
              ┌─────────────────────────────────────────────┐
              │   registry.Set（唯一卷集合）                 │
              │  storageVolumes map[string]StorageVolume     │
              │  （本地 localBackend / 外部 capacity.Backend）│
              └──────────────┬──────────────────────────────┘
                             │ VolumeView(name, use)
        ┌────────────────────┼───────────────────────┐
        ▼                    ▼                        ▼
   user 视图             meta 视图                secrets 视图
  (user 桶, 转存目标)   (控制面, sidecar)       (密钥, R26 白名单)
  └ Guard(Wrap(fs))    └ Guard(fs)             └ NewRootSecretsFS
  └ capacity 记账        └ 不经容量
  └ staging(owner)      └ 不经 staging
```

### 按用途封装（用户裁定方向 1：「考虑按用途封装出本地卷类型」）

本地卷**不是**「一个 FS」，而是**多个用途视图**（FS 7 方法同一套，但装饰链/键空间/能力不同）：

| 用途 | 构造 | 装饰链 | 消费方 |
|---|---|---|---|
| `user`（用户文件） | `PrefixFS(NewRootFS(root), <owner>/user/)` | Guard → Wrap(trusted) → capacity → staging | 上传 Sink / 转存目标 / download / DAV / 同步 |
| `meta`（控制面） | `PrefixFS(NewRootFS(root), meta/)` 或 Root 直读 | Guard（不经容量/staging） | sidecar / checksum / 台账 / 审计 |
| `secrets`（密钥） | `NewRootSecretsFS(root, secrets/, logger)` | at-rest 加解密 + 0600 | 密钥管理器（**唯一绕行点**，R26 白名单） |
| `cloud/archive/chunk/version`（任务桶） | `PrefixFS(NewRootFS(root), <bucket>)` | Guard → Wrap(trusted) | 云任务 / 版本 / 分块 |
| 独立目录（非卷内） | `NewLocalFS(absDir)` | 无卷语义 | 审计归档 / pikpak secretRoot（R26 白名单，与卷无关） |

**API**：`Set.VolumeView(name, use string) (FS, bool)`（或更窄 `Set.UserView(name, owner)` /
`Set.MetaView(name)` 等）——装配期**一次**构造好各用途视图（含装饰链），消费方按名取。
**外部卷**同样适用：`use=user` → `Guard(Wrap(be.FS()))`（与现有 `externalVolumeTransferFS` 同一
recipe）；`use=meta` → 外部卷无 meta 桶（`ReserveSpace` 由容量层兜底）。

### 渐进迁移（用户裁定方向 2：「逐步迁移」）

| 片 | 内容 | 行为变更 | 验收 |
|---|---|---|---|
| **U1** | `Set` 内部加 `storageVolumes map[string]StorageVolume`；`assembleLocalVolume` 改走 `NewLocalBackend`（不再裸 OpenRoot），roots 保留仅服务默认卷 tenant/globalRoot | 无 | 全量绿 + e2e 不变 |
| **U2** | `Set.VolumeView(name, use)` 统一访问器 + 各用途视图构造（user/meta/secrets/任务桶）；外部卷并入 | 无 | 编译断言 + 单元测试 |
| **U3** | 转存目标改走 `VolumeView(name, "user")`（本地卷现可用）；`checkTransferVolumePreflight` 放宽（有 FS 即可）；前端 `isTransferTargetVolume` 放行 local | **有**（本地卷成为转存目标） | 转存到本地卷 e2e + 断言语义 |
| **U4** | `local://` scheme 声明（`RegisterBackend(TypeLocal, ..., "local")`）；转存 URL 一致化；`ResolveURL` 经 `VolumeView` 读本地 | 有（新增能力） | URL 生成/解析单测 |
| **U5** | R28/R27 门禁：`Set.Root`/裸 `be.FS()` 调用点收缩（能迁的迁到 `VolumeView`）；确需 Root 32 方法的（写内核 OpenFileEncrypted/AtomicRename、SealAt 封印、ListOwners）保留逐条理由 | 无 | 门禁预算只降不升 |

### 关键不动边界（避免过度设计）

1. **本地卷写内核仍用 `Root.OpenFileEncrypted` + `AtomicRename`**——原子写/加密语义只在 Root 上
   完备（M3 复核既有结论）；统一的是**身份与访问入口**，不是把 Root 32 方法塞进接口。
2. **默认卷 `globalRoot`/`tenantFor` 保持 `storage.Root`**（租户布局语义），经统一入口暴露内部不改。
3. **`volume://` 与 `local://` 并存**（用户裁定方向 3）：`volume://` 是卷内定位序列化
   （`Location.String()`，已存在）；`local://` 是协议寻址（转存 URL）。文档明确分工。
4. **trusted 装饰对本地卷启用**（用户裁定方向 4）：本地卷也成可信卷（meta sidecar + 校验），
   与外部卷一致；老文件无 sidecar → 读侧跳过校验（不破坏）。

---

## 三、安全性审查（新面）

| 面 | 风险 | 对策 |
|---|---|---|
| `VolumeView(name, "secrets")` | 泄露密钥视图 | secrets 视图**不入通用表**——只在装配期按需构造（R26 白名单维持）；通用 `VolumeView` 拒绝 `secrets` use |
| `VolumeView(name, "meta")` | 绕 meta 桶隔离 | meta 视图仅控制面消费（sidecar/checksum/台账），用户读路径不得用 |
| 本地卷 trusted 启用 | 老文件无 sidecar 读校验跳过 | 读侧 fail-open（不校验不失败），写侧生成——与外部卷既有语义一致 |
| `local://` URL 解析 | 路径穿越 | `ResolveURL` 经 `volume.ResolveUserLocation`（权限门 + 路径安全）——与读路径同入口 |
| 装配接线 | 本地卷容量记账漏接 | `assembleLocalVolume` 改走 `NewLocalBackend` 后**同样套 capacity**（本地卷也走 `WrapStorageVolume` 或等价记账）——确保「本地卷用户写入也计数」 |

---

## 四、验收矩阵（分片）

| 片 | 验收 |
|---|---|
| U1 | `go test ./pkg/volume/registry/ ./pkg/server/` 全绿；`Set.StorageVolume(name)` 对本地卷返回 non-nil；`storage.Root` 非测试调用点不增（R28 预算不变） |
| U2 | `VolumeView(name,"user")` 对本地/外部返回同形状 FS；secrets use 拒绝；装饰链断言（Guard/Wrap 存在） |
| U3 | 转存到本地卷 e2e：`target_volume=本地卷` 创建任务 → 产物落 `<owner>/user/<rel>` → `/download` 命中；前端下拉含 local；preflight 不再 400 |
| U4 | `local://<卷>/<rel>` URL 生成/解析往返一致；`ResolveURL` 读本地卷返回内容 |
| U5 | R28/R27 门禁预算收缩（能迁的迁完）；`make check-ci` 全绿 |

---

## 五、不覆盖（划界）

- **不**把 `storage.Root` 的 32 个方法塞进 `StorageVolume` 接口（写内核保持 Root）。
- **不**改默认卷的 `globalRoot`/`tenantFor` 语义（租户布局不动）。
- **不**做「外部卷也套 trusted 装饰」的行为变更（外部卷已如此，本地卷对齐它）。
- **不**改 `volume://` 序列化（与 `local://` 并存，文档分工）。
- **不**在本次迁移 `pkg/files` 写内核（U5 只收缩访问器调用点，不重写内核）。
