# 阶段 2 迁移：嵌套封装过渡层 → master 底座（enclosure / SealFS / 配额 v3 / Location）

**目的（背景知识，非操作日志）**：本分支（`feat/audit-webui-wt`）为「加密封装卷以已创建卷的子目录作底层」
引入了过渡实现；master 同期落地了更正统的通用机制。本页记录**两者的映射关系、判断依据与剩余项**，
供后续继续迁移时复用，不重复分析。

## 1. 两套机制对位

| 能力 | 过渡层（本分支） | master 底座（目标） |
|---|---|---|
| 声明语法 | `type: secretdata` + `extra.target: "<卷>/<子目录>"` | `type: enclosure` + `extra.source: "<源卷>:<子路径>"` + `extra.wrapper: secretdata` |
| 源视图 | `pkg/sync.SubFS`（子目录前缀视图，可写） | `sync.PrefixFS` + `enclosure.NewFS`（区域相对视图，含直链抑制/能力透传/身份归因） |
| 装配 | `cmd/sproxy.resolveNestedTargetFS` + `adoptNestedDir` 重启收养 | `enclosure.Parse` + `AttachAll`（推迟装配，`sourceFS` 取特权源视图） |
| 互斥占用 | `volumeLinksRegistry`（`PathsOverlap` 父/子/同目录重叠） | `enclosure.Parse` 现只查**精确相等**（同一 `<源卷>:<路径>` 唯一）；**需补**重叠校验 |
| 写保护 | `checkWrapperOccupiedWrite`（8+ 写面接入点）+ `occupiedGuardFS` 装饰器 | 源卷 `storage.SealRule`（`SealAt` 单一事实源：写恒拒 `ErrSealed`、读 hidden(404+列举过滤)/readable(只读透视)），封印在 `Root` 与 FS 装饰链两处统一收口 |
| 配额 | `registry.Set.DelegatePool`（底层池 `Scope(subdir,max)` 子池）+ `IsDelegated` 例外（域层单向释放） | `quota.NewPoolWithParent(有效源卷池, max_bytes)`（`volumes_enclosure.go: inheritPools`）——提交进自身池即自然聚合父链，无例外 |
| 用户卷持久化 | `config_writer.go`（写回 config `volumes:` 段） | `user_volume_store`（`<storage_root>/<owner>/meta/volume/<name>.json` + `ScanRestore`）**已是权威** |
| 键空间 | 过渡期曾给嵌套封装卷开 owner-less 例外（`user/<rel>`） | 统一 `volume.Location.FSPath()`（恒 `<owner>/user/<rel>`） |

## 2. 已完成批次（判断仍有效）

- **键空间统一（遗留①）**：转存（`pkg/cloud.transferRelPath`）写点用 Location 模型，
  而过渡例外让读写/列举用 owner-less 键 ⇒ 转存进封装卷的文件落错位置、列举/下载/删除不可见
  （`du` 同样恒空）。判断：**方向是统一到 Location（enclosure 终态亦为该键空间，非 owner 限定声明
  的调用面键同为 `<owner>/user/<rel>`）**，故就地删例外而非给转存打第四个补丁。副作用：命中
  `files_phys.go` 的 `PhysFootprint`（键原就对不上足迹索引）⇒ 结算从逻辑折算变为实测物理，写/释放同源。
- **移除 config 写回**：同一职责双机制，且写回会改写 operator 的 config 文件（YAML 节点改写、
  删卷时先行写回失败即中止删除）。判断：store 台账是权威（`restoreUserVolumes` 已在走），
  能力等价性由「create → ScanRestore → 重建纯域卷」用例锁定（ACL/键空间不翻转）。

## 3. 剩余项与判断依据

1. **嵌套 `target:` → `enclosure`（主体）**：需一并删除 `pkg/sync/sub_fs.go`、`pkg/server/volume_links.go`、
   `pkg/volume/nested_target.go`、`registry.Set.DelegatePool/IsDelegated`、`reserveVolumePool` 的 `IsDelegated`
   例外、`pkg/files.DelegatedPoolSource`、`adoptNestedDir` 与 restore 轮次特判、R23 的
   `occupiedGuardFS`/`guardedLocalFS` 登记、4 处 gocognit 双标记。**注意**：`secrets` 卷在 enclosure 下
   需要一个 `secrets` wrapper 注册项（当前只有 `secretdata`），否则 `ErrUnknownWrapper`。
   配置兼容建议：装配层把 `target: <卷>/<子目录>` 翻译为 enclosure 声明（先翻译、后收敛 schema），
   避免 Web UI / sclient 建卷语义断裂。
2. **遗留② owner Scope 幻影释放**：cloud 转存直写外部卷 FS（不经 files 路由 `route.Commit`），
   删除该对象时 `releaseExternalDeleteQuota` 仍对 owner 全局 Scope 做「未曾提交」的释放 ⇒ 吃掉同池
   其它占用（少计/过度准入）。根因是**缺「释放==曾提交」的证据**，属设计变更（写入侧落 commit 证据，
   或释放侧按证据跳过），不是可以就地打补丁的分支问题。
3. **e2e/门禁**：`internal/archcheck` 的 R25/R26/R27 与 serial 棘轮随每步跑；`.pi/`、`build/hooks/*.log`
   已忽略。

## 4. 踩过的坑（复用价值）

- `sub_fs.go` 的 owner-less 键是**过渡假设**（「subdir 之下直接是 user 桶」），与卷根布局
  （`<卷根>/<owner>/…`）不符——凡是「为某类卷开例外」的映射都要问：它与 `Location` 全 owner 维度
  是否结构性矛盾？
- 物理布局断言（测试里的 `os.Stat(<子目录>/user/...)`）是键空间变更的**探测网**：统一键空间时
  它们会成簇变红，恰好覆盖「是否还有隐藏的第二套口径」。
