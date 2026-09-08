<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# 多卷存储 + 卷 ACL 设计（X：本地卷抽象）

> 日期：2026-09-08
> 范围：sproxy「对等多节点存储」路线图的**本地方案 X**。Y（集群统一访问，多节点经 mesh 互访 + 广播定位 + 显式寻址）在 X 之后独立规格。
> 状态：brainstorming 产出，待用户审阅后进实现计划。

## 1. 背景与动机

用户部署形态演进：单节点单块盘 → **单节点多块盘**（不同盘符/未来云盘）→ 远期多节点共同利用各自的硬盘。当前 sproxy 存储是**单 `storage_root` 一块盘**，无法利用多块物理盘；且多用户共享一台时缺少「某块盘开放给哪些用户」的访问控制。

本设计把单节点存储从「单根」升级为**多卷**：每卷是一块独立存储根（可独立挂盘），写入由服务端自动路由（默认客户端无感），读取自动聚合/定位；每卷配 ACL（黑/白名单 × 默认开放/拒绝）控制哪些 owner 可用。

X 完成后，Y 把「卷」作为集群可寻址最小单元做跨节点统一访问——X 是 Y 的宿主侧授权与寻址地基。

## 2. 范围

### 目标

- 单节点多卷存储：每卷独立根 + 独立 `LAYOUT_VERSION` + 内部沿用六桶布局 `<卷根>/<tenant>/{user,cloud,archive,chunk,version,meta}/`。
- 写入**服务端自动路由**（默认客户端无感）：owner 在 ACL 允许的卷内按策略选卷。
- 读取/删除/改名在 owner 卷视图内自动定位；**强制路径唯一**，无跨卷歧义。
- 卷 ACL：`allow`（默认拒绝+白名单）/ `deny`（默认开放+黑名单）双模式，每卷可配。
- 旧单 `storage_root` = **默认卷**（首个卷），存量数据零迁移、行为零回归。
- 用户可**深入查看**（各卷用量/分布）并**主动控制**（显式指定卷写入、跨卷 move）。
- 配额保持 owner 全局语义 + 新增每卷容量维度（双账本）。

### 非目标（本设计不实现）

- 跨节点统一访问 / 多节点互访（Y，后续规格）。
- 文件副本/冗余、多节点容量聚合调度（远期演进）。
- 云盘/对象存储后端（卷的 root 未来可指向挂载的云盘 fs，属部署选择，不在本设计实现新后端）。
- 卷间自动均衡迁移（手动 move 支持，自动搬移不做）。
- 引入 Raft/etcd 或任何集群一致性抽象（见 §17 未来扩展缝：本阶段不建一致性 pkg，只留三个对的接缝）。

### DoD

1. **单卷零回归红线**：未配置 `volumes` 时行为与当前完全一致（全量测试绿 + curl 实证）。
2. 多卷读写端到端：配置多卷 → 路由落卷、list 聚合、download 自动定位、delete 定位、ACL 拒绝。
3. 路径唯一性强制：手动指定卷撞同名被拒（不产生歧义态）。
4. ACL 矩阵测试：allow+白命中放行 / 未命中拒；deny+黑命中拒 / 未命中放行；缺省兼容开放。
5. 配额双账本：owner 全局上限跨卷生效 + 每卷容量上限生效；写满换卷/报满正确；reconcile 双校准。
6. `sclient upload --volume`、`list --volume`、`volumes` 子命令、WebUI 卷仪表与 badge 可用。

## 3. 现状锚点（file:line，供实现对照）

- 布局：`<storage_root>/{user,cloud,archive,chunk,version,meta}` 六桶 + `LAYOUT_VERSION`（`pkg/storage`）。
  - `pkg/storage/root.go:17` `LayoutVersion = "2"`；`OpenRoot`（:32）校验/写入版本标记。
  - `pkg/storage/tenant.go:28-31` `Tenant{ID, root}`；`UserRel`（:61）用户路径→`user/` 桶；`FeatureRel`（:84）特征桶。
  - `pkg/storage/tenant.go:14-24` 功能桶白名单 `user/cloud/archive/chunk/version/meta`。
- 配额：`pkg/quota` Pool/Scope/Reservation。`Pool.Scope(path,max)` 挂子池（`quota.go:38`）；`Resolve` 路径段下探（:101）。
- 装配容器（`pkg/server/handlers.go`）：
  - `tenantRoots map[string]*storage.Tenant`（:98）—— owner → 租户，持**单一**全局 root（`storage.OpenRoot(storageRoot)` 一次，:572-579；owner 懒建，:218 `tenantFor`）。
  - `quotaScopes map[string]*quota.Scope`（:101）—— `globalPool.Scope("/tenant/"+owner, quotaBytes)`（:383-402），owner_quotas/`*` 语义。
  - `quotaBuckets map[string]map[string]*quota.Scope`（:102）—— 桶分层子池 + `bucket_limits` 挂载（:403-421）。
  - `quotaScopeFor(owner, rel)`（:471）—— 写路径解析到桶/子目录 Scope。
  - `reconcileQuotaScopes`（ReconcileFunc，storage_manager.go:54）—— 磁盘扫描校准 Scope；装配于 cmd/sproxy/root.go:683。
  - cloudMgr 目标桶 `quotaBucketFor(owner,"cloud")`（handlers.go:698）。
- 关键路由（现有端点全部假定单根）：`POST /upload`、`GET /download?filename`、`POST /delete`、`POST /rename`、目录操作、`/api/files`、`/api/batch/*`、分块上传、versioning、cloud、archive、share。

## 4. 架构决策

### AD-1 卷 = 独立存储根

每个卷是一个独立物理根：`volumes: [{name, root}]`，装配时各自 `storage.OpenRoot`（独立 `LAYOUT_VERSION` 校验）。文件寻址 = **(卷, owner, 相对路径)**，相对路径沿用现有桶布局映射（`Tenant.UserRel/FeatureRel` 不变）。旧 `storage_root` 键保留为「默认卷」root（首个卷），追加多卷即增量扩盘。根数量小（个位数到十位级），装配全量打开。

### AD-2 owner 卷视图 = ACL 允许的卷集合（本地权威）

owner 在节点上的「可见/可写卷集合」由各卷 ACL 计算得出（见 AD-6），本节点为唯一属主。owner 不在某卷 ACL 允许内 → 该卷不进其视图（list 不出现、路由不落、显式指定拒）。「anonymous」owner 与其它 owner 同等受卷 ACL 约束（兼容默认卷缺省开放不受影响）。

### AD-3 自动路由：placement 策略，默认默认卷优先

配置级 `placement: prefer-default | spread`（缺省 `prefer-default`）：

- `prefer-default`：owner 视图内**默认卷**（首个卷）有配额余量且 TryReserve 成功即落默认；默认卷满/不允许才依卷序尝试后续卷（每个失败换下一卷）。
- `spread`：按每卷「(容量上限 − 已用)」余量占比均衡选择。

两者都先过 ACL 过滤 + 卷容量 TryReserve + owner 全局 TryReserve（双账本见 AD-7）。`prefer-default` 保证旧单根不搬家；`spread` 供真正多盘利用。

### AD-4 强制路径唯一（owner 卷视图内）

owner 的逻辑文件树（卷视图并集）中**同一相对路径只能存在于一个卷**：

- 自动路由写入：路径由路由唯一决定，天然唯一。
- 显式指定卷写入（`upload volume=v` / 特征桶路由例外见 AD-5）：写前在**目标卷 stat 查重**（目标卷已有同相对路径 → `409`）+ **跨卷检查该路径已存在于其它卷 → 拒**（返回文件实际所在卷提示）。唯一性强制使 download/delete/rename 无需歧义裁决：单卷命中即操作，未命中跨卷查下一卷，全未命中 `404`。

### AD-5 特征桶落卷（跟随 / 例外）

- **chunk（分块上传）**：`/upload/init` 阶段即做路由**定卷**（预留目标卷），会话存该卷 `chunk/`；`complete` 同卷把分块合入 `user/`（同根 rename 原子）。chunk 永不与目标 user 文件跨卷。
- **version**：随 user 文件所在卷（保存旧版 / restore 都同卷，`version/` ↔ `user/` 同根）。
- **cloud 下载产物**：无 user 文件上下文 → 落 owner **默认卷** `cloud/` 桶（例外，不含路由）；cloud 任务状态照旧在默认卷 `meta/`。
- **archive 压缩产物**：可能聚合跨卷输入 → `.tar.gz` 落 owner **默认卷** `archive/`（例外）；archive 解压写入用户文件经路由（多输入读取按文件定位）。
- 例外仅 cloud/archive 产物（无单一 user 文件上下文），规格在此明确记录，避免 implementer 自行猜测。

### AD-6 卷 ACL：黑/白名单 × 默认开放/拒绝

每卷配置 `acl: {mode: deny|allow, owners: [...]}`：

| mode | 语义 | 缺省（未显式列 owner） |
|------|------|------|
| `deny` | 默认开放 + **黑名单**（owners 被禁） | 对全部 owner 开放（兼容旧根） |
| `allow` | 默认拒绝 + **白名单**（仅 owners 可用） | 对全部 owner 拒绝（显式关卷） |

- 未配 `acl` 段 = 等同 `deny` + 空 owners = 默认开放。
- ACL 判定为**本地权威**、每请求即时生效（改配置生效策略见 §8）。
- 校验点统一：路由选卷、聚合/定位读、显式指定卷、move 目标卷——owner 未在允许内 → `403`。

### AD-7 配额双账本

- **owner 全局池语义保留**（跨卷）：`owner_quotas` / `*` / `bucket_limits` 仍作用于「owner 在全部允许卷的总占用」。owner 桶分层 Scope 现挂 `globalPool`（handlers.go:383-421）——语义不变，只是占用来自多卷。
- **每卷容量 Pool 新增**：`volumeRootPools map[卷名]*quota.Pool`，上限 `vol_capacity`（该卷所有 owner 占用总和；0 = 不限制）。
- 写入时序：选卷后 **先 owner 全局池 TryReserve 再目标卷池 TryReserve**，两者都成才写，Commit 双记；任一失败回滚已预留并换卷（仅卷容量失败可换卷；owner 全局满 = 全卷满，直接 `ErrStorageFull`）。删除/覆盖双 Release。
- `reconcileQuotaScopes` 逐卷扫描，把磁盘占用同时校准进 owner 全局 Scope（现有回调方向）与卷容量 Pool（新增回调或同回调双目标）。
- **现有 `StorageManager` 全局账本**：单卷模式保留原路径（零回归，`max_storage_bytes` 语义不变）；多卷模式**每卷一个 StorageManager**（装配期据 volumes 数量实例化，各持本卷 Reconciler）。owner 全局配额语义始终由 `quotaScopes`/`globalPool` 承担（跨卷合计），不依赖单根 StorageManager 的原子计数。

### AD-8 客户端无感 + 可选深入

- **无感默认**：现有端点（upload/list/download/delete/stat/…）不加参数 → auto 路由/聚合定位，行为等价「逻辑单存储」。
- **深入（可选）**：`GET /api/volumes`（owner 可见卷 + 用量/容量）；`upload volume=<卷>` 显式；`POST /api/volumes/move` 跨卷移动；list 条目带 `volume` 字段 + `?volume=` 过滤。
- 客户端（FileClient/sclient/WebUI）默认零改动；深入能力显式加（见 §12）。

### AD-9 未来扩展缝（X 只留缝，不建一致性抽象）

结论（brainstorming 分析）：Raft/etcd 属于未来「元数据目录 / 副本组协调」域，**不是文件内容同步器**；只要保持「每个文件/卷单一属主写」，就用不到共识（多点并发写同一状态才需要 Raft）。X 留三缝：

1. **卷 = 可寻址一等单元**：卷有稳定 `name`（+ 未来集群内 `id`），文件寻址原子 = (卷, owner, 相对路径)——未来全局寻址 = (node, volume) + 元数据目录把逻辑路径解析到 (node, volume)，改动集中。
2. **属主薄接口**：卷装配/路由/ACL 收敛为 `pkg/volume` 的 `VolumeSet`/`Router`/`Authorizer`（装配注入，handlers 不散读全局 config）——未来把「属主 = 本地配置」换成「属主 = 集群一致视图（etcd/hub 订阅）」时改动点集中。
3. **沿用 `pkg/store` 字节 KV 接缝**（现有 meta backend；file 后端已注册）做未来元数据目录 backend 扩展点（etcd 后端未来入 `pkg/store/ext/` 独立 go.mod，符合 ext 政策，接口留主包）。本设计不新建一致性包。

**反模式显式排除**：允许多节点并发写同一文件路径（那才需要 Raft 主写）——Y 已用单主属主 + 广播定位避开。

## 5. 配置模型

```yaml
# 兼容：不配 volumes 时 = 旧行为（storage_root 单根）
storage_root: ./storage            # 默认卷 root（兼容键，配 volumes 后仍可省略 = ./storage）

# 多卷（可选；配了则默认卷 = volumes[0]，其余为追加盘）
placement: prefer-default          # prefer-default | spread，缺省 prefer-default
volumes:
  - name: main                     # 合法段名（ValidSegmentName），首个 = 默认卷
    root: ./storage                # 默认卷 root；可省略，省略则取 storage_root
    vol_capacity: 0                # 0 = 不限；该卷所有 owner 占用总和上限
    acl:                           # 省略 = deny + 空（默认开放）
      mode: deny                   # deny | allow
      owners: []                   # deny=黑名单 / allow=白名单
  - name: disk2
    root: /mnt/disk2               # 可挂不同盘符
    vol_capacity: 2TiB
    acl: { mode: allow, owners: [alice, bob] }   # 仅 alice/bob 可用该盘
  - name: backup
    root: /mnt/backup
    acl: { mode: deny, owners: [guest] }          # 默认开放，仅禁 guest
```

校验规则：

- 卷 `name` 满足 `storage.ValidSegmentName`（fail-closed）；`root` 必须存在（装配 `os.MkdirAll` 后 `OpenRoot`，LAYOUT_VERSION 校验失败即启动失败）。
- `volumes` 非空时：首个卷 root 缺省取 `storage_root`；`storage_root` 缺省 `./storage`。
- 卷 name 重复 → 启动失败；`mode` 非法值 → 启动失败。
- 只配一个卷且 root == storage_root → 与旧行为完全等价（单卷退化）。

## 6. 装配与卷视图（目标形状）

新增 `pkg/volume`（或 `pkg/server/internal/volumes`，由实现计划依工程原则 3 定）提供：

- `VolumeSet`：装配时按配置逐卷 `storage.OpenRoot` + 解析 ACL + 建卷容量 Pool；提供 `Default() *Volume`、`ByName(name)`、`All()`、`Authorizer`（ACL 判定）。
- `Router`：`placement` 策略 + 双账本 TryReserve（见 §7）。
- Handlers 装配改动锚点（handlers.go:98-102 容器）：
  - `tenantRoots map[string]*storage.Tenant` → **`map[owner]map[volume]*storage.Tenant`**（每 (owner, 卷) 一个 Tenant 值，ID 同 owner、root 分卷；懒建缓存）。`Tenant` 结构不变，多卷只是 owner 下多个不同 root 的 Tenant。
  - `quotaScopes`/`quotaBuckets` owner 全局语义保留（跨卷占用），新增 `volumeRootPools map[string]*quota.Pool`。
  - `OpenRoot` 装配点（:572-579）改逐卷（每卷独立 root + LAYOUT_VERSION）。

### meta 与全局账本归属（重要：多卷下单点权威）

卷视图引入多 root 后，**owner 的「meta 桶」只落在默认卷一份**（单一权威），不随文件所在卷分散：

- **凭据**（`<owner>/meta/credentials.json`）：装配层恒用**默认卷** root 读写（凭据装配入口不受卷影响）。
- **checksum 账本**（`<owner>/meta/checksums.json`）：owner 逻辑文件树级（跨卷合计、唯一性依据）→ 恒在默认卷 `meta/`。download/delete 校验不因文件所在卷而换 checksum 源。
- **cloud / sync / hub 状态**（`meta/` 各状态文件）：现状单一 → 恒默认卷。
- 各**非默认卷**的 `meta/` 桶仅承载该卷特征操作必需的局部账本（本卷 reconcile 的临时对账痕迹），不得复制凭据/checksum 等全局状态（防分叉）。

推论：默认卷是 owner 的「元数据面」，任意卷是其「数据面」；卷被卸载（未来场景）不影响元数据一致——属 Y/运维演进，本设计仅确立归属不变式。

## 7. 访问语义

### 写（upload）

1. 由请求上下文取 owner（现有 auth 路径）。
2. 计算 owner 卷视图（ACL 允许卷序）。
3. 按 placement 在视图内选卷候选：
   - `prefer-default`：默认卷在视图内则先试默认；不在则依序。
   - `spread`：按余量占比加权选。
4. 双 TryReserve（owner 全局 → 目标卷）成功后写；owner 全局满 = `ErrStorageFull`（不换卷）；卷容量满 → 换下一候选卷（`prefer-default` 才可能有多候选），全满 `ErrStorageFull`。
5. 落盘后双 Commit；失败回滚双预留。

### 读 / stat / download / delete / rename

owner 卷视图依序遍历定位（目标相对路径在每卷 `stat`）：

- 单卷命中 → 操作；`user/` 与 `chunk/version` 特征路径走同卷逻辑。
- 全视图未命中 → `404`（与现状一致）。
- 唯一性保证（AD-4）使多卷命中不可达；若因异常出现（理论），返回 `409` 报歧义——防御而非常规路径。

### 显式指定卷（深入）

- `upload volume=v`：先 ACL 校验（v 不在 owner 视图 → 403）；再查重（AD-4）：目标卷已有同路径 → 409；其它卷已有同路径 → 409 + 提示实际所在卷。
- list/download/delete/stat 支持 `?volume=` 过滤（可选，默认聚合/全视图定位）。

### move（跨卷）

`POST /api/volumes/move?from_volume&to_volume&filename=`（同 owner 同相对路径跨卷）：

- from 与 to 都须在 owner 卷视图内（否则 403/404）；源存在（否则 404）；目标查重：目标卷已有 / 与源同路径已在源即源自身，直接同卷 rename。
- 跨卷 = 流式复制到目标卷临时 + fsync + 原子 rename → 源删除；配额双账本移动（to 双 reserve、from 双 release，先 reserve 后 release 防中间超限）。
- 大文件 move 是异步还是同步？第一版**同步**（错误可见）；超过阈值的自动异步为后续增强（记录，不做）。

## 8. ACL 判定（时机 / 次序）

- 时机与次序：请求带 owner（auth 中间件解析）→ 操作所需卷逐一过 `Authorizer.Allow(owner, volume)`。list 聚合只遍历允许卷（不可见卷绝不列出）；路由候选只从允许卷取；显式卷先验 ACL 再验存在。
- 配置变更生效：ACL/placement/vol_capacity 属「存储装配硬配置」，**启动/重启生效**（SIGHUP 不重建，同 storage_root 现状；`PUT /api/storage/config` 的 max 热更新保持 owner 全局池热调，卷级容量热调为后续增强记录）。规格明确此边界，避免误导。
- 判定纯内存（卷 ACL 装配期载入），无 I/O。

## 9. 配额双账本细则

- 容器：owner 全局 `quotaScopes`（语义不变，跨卷合计）+ `volumeRootPools`（每卷）。
- 时序（写路径，单函数保证）：`globalScope.TryReserve(size)` → 成功 → `volPool.TryReserve(size)` → 成功 → 写盘 → 双 `Commit`。任一失败：回滚已 reserve（`Reservation` 句柄 release），返回换卷或 ErrStorageFull。
- bucket_limits 分层（现挂 owner 桶 Scope）不变，落在全局树；卷层无 bucket 分层（卷容量是粗粒度总和）。
- reconcile：`ScanAndRecalculate` 逐卷扫，回调把每卷占用归集：(a) owner 全局 Scope（现有语义）(b) 该卷容量 Pool（重算卷 committed=卷上全部文件占用）。现 `ReconcileFunc(tenantBuckets map[tenant]map[bucket]int64)` 签名需扩到带卷维度或新增回调。
- StorageManager：单卷模式（旧）为兼容保留；多卷模式各卷自己的 StorageManager/Reconciler，`max_storage_bytes` 语义迁移到卷容量（计划阶段精确定，先不破坏现有单卷）。

## 10. API 与可见性

全部现有端点默认 auto（无感），新增：

- `GET /api/volumes`（auth + per-owner）：`{volumes: [{name, mode, capacity, usage, allowed}]}` —— 仅该 owner 可见卷；满足「深入看到使用情况」。
- list/stat/download 等**可选** `?volume=`（过滤单卷）+ 返回项带 `volume` 字段。
- `upload` 可选 `volume=` 表单/参数。
- `POST /api/volumes/move`（参数如上 §7）。
- 响应沿用现有 JSON/错误形态（403/404/409/ErrStorageFull）。

## 11. 特征桶跟随细则（AD-5 展开）

- 分块上传：`init` 路由定卷 → 会话写该卷 `chunk/<id>/` → `complete` 同卷合入 `user/`。状态/进度照旧（meta 在 owner 默认卷）。
- versioning：`version/` 与目标 `user/` 同卷；`restore`/`delete` 定位 = user 文件所在卷。
- cloud 下载：落 owner **默认卷** `cloud/`（任务状态在默认卷 `meta/`）；cloudMgr 现 `quotaBucketFor(owner,"cloud")` 改绑默认卷对应 Scope。
- archive：压缩产物 `.tar.gz` 落默认卷 `archive/`；解压输入按文件定位读取、产物经路由。
- share：不存文件（token→文件引用），引用 = (卷, 路径) 定位。

## 12. 客户端表面

- FileClient：新 `Volume` 上下文（可选字段），零值 = auto；`List` 返回条目带 Volume；`Volumes()`/`Move` 方法（Move 对应服务端 `/api/volumes/move`）。
- sclient：`upload --volume`、`list --volume`、`stat`/`download` 可选 `--volume`；新增 `volumes` 子命令（列出可见卷 + 用量）；`mv` 增加 `--to-volume <卷>`（把文件迁到目标卷同相对路径，内部调 move API，跨卷写前查重）。
- WebUI：文件行卷 badge（list 条目 volume 字段）；卷容量仪表（/api/volumes）；高级上传下拉「卷」。
- 向后兼容：缺省全部 auto，旧客户端/旧 UI 无感。

## 13. 边界与安全面

- ACL 判定 fail-closed：未配置 = 开放仅限 `deny`+空（向后兼容语义，显式表达）；`allow` 未命中即拒。绝无「显式配置了 ACL 却绕过」路径。
- 卷名 `ValidSegmentName`；root 经 `storage.OpenRoot`（os.Root 防穿越/符号链接逃逸逐卷独立保证）。
- 显式指定卷不可越权：用户只能指到 ACL 允许卷。
- 路径唯一性拒绝返回文件实际卷（提示不泄其它 owner 信息——仅本 owner 视图内）。
- 多租户隔离不因多卷变弱：owner 仍只见自己视图；reconcile 按 owner 归集不跨租户。

## 14. 错误处理

| 情形 | 行为 |
|------|------|
| 卷 root 不可用/LAYOUT_VERSION 不匹配 | 启动失败（fail-fast，不静默跳过该卷） |
| ACL 未命中（allow）/ 命中黑名单（deny） | `403 volume not allowed` |
| 显式指定卷不存在或不在视图 | `404`（列表/过滤）或 `403`（写入指定） |
| 目标卷已有同路径 / 其它卷已有同路径 | `409` + 实际所在卷提示 |
| owner 全局配额满 | `ErrStorageFull`（不换卷） |
| 卷容量满 | `ErrStorageFull`（`prefer-default` 时换下一候选卷；全满才报） |
| 全视图未命中文件 | `404`（与现状一致） |

## 15. 测试策略

- **单卷零回归**：未配 volumes = 现行为——全量测试 + 关键端点 curl（upload/download/delete/rename/list/cloud/archive/chunk/version）。
- 路由：prefer-default 顺序、spread 均衡、换卷、owner 全局满 vs 卷满区分。
- 唯一性：manual 撞同名跨卷拒绝（target 有 / 其它卷有）。
- ACL 矩阵（表驱动）：allow×命中/未命中、deny×命中/未命中、缺省开放、403。
- 特征桶：chunk init 定卷 → complete 同卷原子；version 同卷；cloud/archive 落默认卷。
- 双账本：owner 全局跨卷超限、卷容量超限、写满换卷、删除双 release、reconcile 逐卷双校准。
- API：/api/volumes per-owner、?volume= 过滤、move 跨卷。
- 集成：多卷 mock 根（临时目录 + 假盘）端到端；覆盖 http 全路径。
- 客户端：FileClient Volume 上下文、sclient volumes/move/--volume、WebUI badge 渲染（make web-test）。

## 16. DoD 分块（计划级顺序，每块独立可测可审）

- **A** config 卷模型 + Validate + 兼容默认卷解析 + ACL 解析（含 unit 测试）。
- **B** `pkg/volume` VolumeSet/Authorizer 装配 + owner 卷视图容器改造（单卷退化零回归）。
- **C** 配额双账本：卷容量 Pool + 写/删双 reserve/commit/release + reconcile 逐卷双校准。
- **D** handlers 卷感知：upload 路由、list/stat/download/delete/rename 定位、显式 volume 参数与校验、`/api/volumes`、`POST /api/volumes/move`。
- **E** 特征桶跟随：分块 init 定卷、version 同卷、cloud/archive 落默认卷绑定。
- **F** 客户端表面：FileClient Volume 上下文 / sclient volumes + `--volume` / WebUI badge 与卷仪表。
- 每块含对应测试；A 单卷零回归为全阶段红线；F 完成后全量回归 + curl 实证。

## 17. 未来扩展缝（三缝展开，见 AD-9）

- 卷 id：装配期为每卷派生稳定标识（name 即 id，Y 阶段与节点 id 复合为 `(node, volume)` 全局寻址）。**X 不引跨节点 id**，仅保证卷名合法、稳定、可序列化进路径/元数据。
- 属主薄接口：`VolumeSet/Router/Authorizer` 接口化，`file` 实现为当前本地配置属主；未来实现「集群一致视图属主」经注入替换。
- 元数据 backend：本设计任何「文件→卷」定位均**不在本地新增持久索引**（靠卷视图逐卷 stat，规模小；Y 需要目录时经 `pkg/store` 加 backend）。避免现在建一个未来要丢的本地索引。

## 18. 参考

- 多租户布局：`docs/superpowers/specs/2026-09-01-multitenant-storage-layout-design.md`
- 配额：`docs/superpowers/specs/2026-09-02-quota-disk-enforcement-design.md`
- 集群路线（Y 前身调研）：`docs/superpowers/specs/2026-08-29-sproxy-fullmesh-roadmap.md`、filesync/virtualip stage4 specs
- ext 政策：memory `ext-submodule-policy`
