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
| 互斥占用 | `volumeLinksRegistry`（`PathsOverlap` 父/子/同目录重叠） | `enclosure.Parse` 的足迹重叠校验（**已补**：`ErrFootprintOverlap`，链内叠加允许、兄弟分支互斥） |
| 写保护 | `checkWrapperOccupiedWrite`（10+ 写面接入点）+ `occupiedGuardFS` / `syncexec.guardedLocalFS` 装饰器 | 源卷 `storage.SealRule`（`SealAt` 单一事实源：写恒拒 `ErrSealed`、读 hidden(404+列举过滤)/readable(只读透视)），封印在 `Root` 与 FS 装饰链两处统一收口；**例外**：S3 预签名直传（不经 FS）→ 签发期 `presignSealedGuard` 按规则拦截 |
| 配额 | `registry.Set.DelegatePool`（底层池 `Scope(subdir,max)` 子池）+ `IsDelegated` 例外（域层单向释放） | `quota.NewPoolWithParent(有效源卷池, max_bytes)`（`volumes_enclosure.go: inheritPools` / 运行期 `SetPool`）——提交进自身池即自然聚合父链，无例外 |
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

- **嵌套 target → enclosure（主体，已完成）**：装配层翻译（`pkg/server/nested_enclosure.go`：
  `TranslateNestedDecl`/`TranslateNestedVolume`，config/API/restore 三入口共用）+ 运行期
  装配/拆除闭环（`AttachEnclosureVolume`/`DetachEnclosureVolume`）。过渡文件与逐面 guard 全删。
  **判断**：翻译只在内存发生（store/config 字节不变、物理落点不变 ⇒ 无数据迁移），用户面
  schema 后收敛；`ParseNestedTarget` 保留为该语法的唯一实现（`pkg/volume.SplitNestedTarget`
  随过渡层删除，`PathsOverlap` 语义上移为 `enclosure` 的足迹校验）。
- **写保护坐标澄清（重要，勿再按旧坐标理解）**：过渡层的 guard 用**用户可见坐标**
  （`<owner>/user/videos/…`），而封装卷字节始终落在**卷根命名空间的子区间**
  （`<基卷根>/videos/…`）——两者不是同一棵树，逐面 guard 实际保护的是没有封装数据的另一
  命名空间。迁移后封印恰好覆盖「字节真正所在」的区间（语义自洽且更强），故
  `TestWrapperMove/Rename_SourceOccupied403`、`_TargetWrapper_ClearMessage` 三个断言旧坐标
  守卫的用例随机制删除（封印级等价覆盖见 `pkg/storage/seal_test.go`、
  `pkg/volume/enclosure/sealfs_test.go`）。

## 3. 剩余项与判断依据

1. ~~**嵌套 `target:` → `enclosure`（主体）**~~ **已完成**（见 §2）：`pkg/sync/sub_fs.go`、
   `pkg/server/volume_links.go`、`pkg/volume/nested_target.go`、`registry.Set.DelegatePool/IsDelegated`、
   `reserveVolumePool` 的 `IsDelegated` 例外、`pkg/files.DelegatedPoolSource`、`adoptNestedDir` 与
   restore 轮次特判（改按声明源卷就绪判定）、R23 的 `occupiedGuardFS`/`guardedLocalFS` 登记、4 处
   gocognit 双标记，以及 DAV/S3/S3-multipart/版本恢复/sync/路由/move/文件域内核/云转存/备份共 10 处
   逐面 guard 全部删除；`secrets` wrapper 已随生产装配注册。

   附带发现（batched 内解决）：**池账本口径**——过渡期封装卷池由路由提交（逻辑字节），迁移后由
   FS 层 `capacity.PoolCounter` 计量（实际落盘字节，含元数据 sidecar）⇒ 数值比旧断言大，用例改按
   「父链聚合等式 + 删除精确回落」断言（跨账本数值相等不再成立，owner 全局 Scope 是逻辑口径）。
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
