# 审查：批次 16——secretdata 容器名规范化 / 重复容器治理（12 提交）

- **批次**：16
- **审查基线**：master `origin/master` → 分支 `fix/dup-container`（12 commits，`d92bbbe96`）
- **范围**：`pkg/volume/secretdata`（权威容器选择 / 确定性命名 / 迁移 / 审计 / 导出 / 空容器清理 /
  快照失效与采样校验）+ `pkg/cryptox/shardseal`（`DirNameFor`）+ `cmd/sproxy`（配置接线）
- **维度覆盖**：正确性 / 可靠性 / 完整性 / 稳定性 / 可维护性 / 性能
- **方法**：逐函数源码审查 + 与既有实现对照（`rebaseContainerPathsLocked`、`GC`/`gcMarkIndex`、
  `applyDirMoveLocked`、快照路径）+ 线上实测复现记录核对

## 结论

**总评**：**有条件通过** —— 交付的四条主链路（确定性命名 / 权威选择 / 迁移 / 清理）方向正确、
安全约束到位，但存在 **1 项直接导致功能不可用** 的缺陷与 **3 项状态泄漏/竞态**。

**发现数**：P0 0 / **P1 5** / P2 7 / P3 3

## 发现清单

### [P1-1] 迁移改名后未同步 `metaEntry.dirSeg` ⇒ 同进程内既有文件读/删指向旧路径（功能不可用）

- **位置**：`pkg/volume/secretdata/secretdata.go` `normalizeOneRel` / `commitPartialMoves` /
  `NormalizeContainerNames`（对比既有 `rebaseContainerPathsLocked`，同文件 3655 行起）
- **问题**：迁移把容器**物理改名**，只更新了 `s.dirSegs`（逻辑目录 → 容器）映射，
  **没有更新每个索引条目的 `e.dirSeg` 字段**。而 `e.dirSeg` 是文件 blob 的定位依据
  （`meta_cache.go:228`、`meta_provider.go:221`、`secretdata.go:3827/3859/3865`、
  `inspect.go:326/338/343`、`erasure.go:258/561`）。后果：
  1. **同进程内**（迁移后到下次重启之间）既有文件的读/删/改名都指向**已不存在的旧容器路径**
     ⇒ 读失败（用户可见的功能故障）；
  2. **GC 退化**：`gcContainersLocked()` 取 `s.dirSegs`（**新**名），而 `gcMarkIndex` 用 `e.dirSeg`
     （**旧**名）登记引用 ⇒ 新容器下所有 blob 都"不在索引" ⇒ 走 `gcReconfirmContainer` 的
     **逐容器解密重读**路径（scrypt，写锁内），整卷可阻塞分钟级；虽最终由
     `gcProtectLiveMeta` 兜住引用（**不丢数据**），但代价与风险都不该出现。
- **关键反证**：仓库**已有**同等机制并**明确记录了该故障模式**——
  `rebaseContainerPathsLocked` 注释：「**索引条目也持有容器路径**（每个文件 meta 记录其所在容器
  `dirSeg`）⇒ 必须同步，否则文件读/删会指向已搬走（不存在）的旧路径（实测：目录移动后子树文件
  `dirSeg` 未 rebase ⇒ 读失败）」。迁移路径漏用了这套既有能力。
  （线上验证被掩盖：每次验证前都重启过服务，全量重扫把 `dirSeg` 修正了。）
- **附带**：`startGC` 在 `runNormalizeNamesAction` **之前**启动（`secretdata.go:431-437`），
  即 GC 与迁移可能同进程并发 ⇒ 上述 GC 退化在 `gc_interval > 0` 时是**必然**发生的。
- **建议**：迁移成功后（`commitPartialMoves` 与成功路径）对每个已迁移 rel 调用
  `rebaseContainerPathsLocked(oldSeg, newSeg)`（或用 `newSegs` 差集驱动），并补测试：
  「迁移后 `OpenRead`/`Delete` 既有文件仍成功」+ 变异（不 rebase ⇒ 红）。
  另建议把 `startGC` 移到 `runNormalizeNamesAction` **之后**（运维动作先收口）。

### [P1-2] 快照抑制检查只在函数入口 ⇒ 陈旧快照仍可写回（原竞态未完全关闭）

- **位置**：`pkg/volume/secretdata/index_snapshot.go` `writeIndexSnapshot`（279 行检查）、
  `tryLoadIndexSnapshotFast`（406-410，异步重写 goroutine）
- **问题**：`snapshotSuppressed` 只在 `writeIndexSnapshot` **入口**检查一次；而从检查到
  `os.Rename` 之间是 **marshal + gzip + AES-GCM 加密 + 写盘**（数十 MB、秒级窗口）。此窗口内落下的
  写操作/迁移（`invalidateIndexSnapshot` 已删文件并置位）**无法阻止**该 goroutine 随后把
  **迁移前/写前**的快照 rename 回原路径 ⇒ 下次启动载入陈旧索引（已删文件"复活"、新文件"不可见"）。
  这正是本分支要修的「异步快照落盘竞态 ⇒ 旧索引回写」，但只关了检查点、未关窗口。
- **影响**：索引陈旧 ⇒ 若该进程后续触发 `GC`（`gc_interval>0`），陈旧索引会缺少新文件的引用
  （新文件 blob 的 meta 仍在盘上 ⇒ 由 reconfirm 保护，未见直接丢数据），主要风险是
  "删除后复活/新增不可见"的**语义错误**与重建成本。
- **建议**：落盘前**再检查一次** `snapshotSuppressed`（紧邻 `os.Rename`，失败则删 tmp 返回）；
  更稳妥用「快照代际号」比对（写入前取 gen、rename 前比对，不一致即放弃）。

### [P1-3] 空容器清理删除物理容器后未失效索引快照

- **位置**：`pkg/volume/secretdata/dup_cleanup.go` `CleanupEmptyDuplicateContainers` /
  `cleanupOneEmptyContainer`
- **问题**：该动作**删除真实物理目录**，却没有调用 `invalidateIndexSnapshot()`。而
  `tryLoadIndexSnapshotFast` 的健全性检查 `snapshotRootMatches` **只比对卷根顶层目录集合**——
  被清理的是**深层**容器时顶层集合不变 ⇒ 陈旧快照可以命中 ⇒ 索引指向**已删除**的容器 ⇒
  读/删报 not found（直到下次全量重建）。
- **建议**：清理成功后（`removed > 0` 且非 dryRun）调用 `invalidateIndexSnapshot()`；补测试
  「清理后快照文件不存在」+ 变异。

### [P1-4] 导出（`exportOneContainer`）临时注册泄漏：用量虚增 + 幻影目录 + 去重池引用永久虚增

- **位置**：`pkg/volume/secretdata/dup_export.go` `exportOneContainer`（defer 清理块）
- **问题**：临时注册走 `loadContainerFileMetas` → `loadContainerFileMeta` →
  `registerLoadedEntryLocked`，其副作用包含：`s.usage += size`、`addDirKeysLocked(s.dirs, rel)`
  （**写入全部祖先目录键**）、`registerPoolEntryLocked`（去重池 `refs++`）、`volVersion` 抬升。
  而 defer 只做三件事：删 `s.index` 中 `tmpRel+"/"` 前缀键、还原/删 `s.dirSegs[tmpRel]`、
  `delete(s.dirs, tmpRel)`。因此泄漏：
  1. **`s.usage` 永久虚增**（导出多少字节就虚增多少；`Usage()` 对外暴露，配额/统计失真）；
  2. **`s.dirs` 幻影键**：`addDirKeysLocked` 写入的是 `rel` 的**祖先**（`__dup_export__`、
     `__dup_export__/X`、`__dup_export__/X/sub`…），defer 只删了 `tmpRel` 对应的那一个
     ⇒ 顶层 `__dup_export__` 与各子键**永久残留** ⇒ `ListDir("/")` 出现**用户可见的幻影目录**；
  3. **去重池 `refs` 永久虚增**（引用计数只增不减 ⇒ 池条目永不回收）；
  4. `volVersion` 被临时条目的 `BaseVersion` 抬升（语义污染）。
- **建议**：把「临时注册」做成**成对的事务**（保存/还原 `usage`、`dirs` 全量差集、池 refs、
  `volVersion`），或改为**不经过索引**的专用读取路径（直接用 `readFileMetaFast` + `OpenRead`
  按显式容器路径解密导出）；补测试：导出前后 `Usage()`、`ListDir("/")`、池 refs 不变 + 变异。

### [P1-5] 争议判据用**物理条目名**比对 ⇒ 含子容器的重复组必然被判定为争议，永久跳过迁移

- **位置**：`pkg/volume/secretdata/secretdata.go` `auditOneGroup` + `containerEntries` +
  `normalizeBlockedRels`
- **问题**：`containerEntries` 用 `s.inner.ListDir` 返回容器下**全部直接条目名**（含子容器目录、
  目录 meta、文件 meta），`auditOneGroup` 以「落选容器存在权威容器没有的条目名」判定
  `Conflicted`。而历史**子容器名是随机**的（正是本仓库要治理的问题本身）⇒ 只要重复组里有 ≥2 个
  子容器，两侧子容器名几乎必然不同 ⇒ `Conflicted=true` ⇒ `normalizeBlockedRels` **永久跳过该组**。
  结果是：**最需要治理的"嵌套重复容器"这一类永远无法自动迁移**，与设计目标（"迁移前审计争议、
  非争议自动迁移"）相悖。
- **说明**：保守判定本身符合用户"有争议先记录等人工确认"的要求，问题在**判据粒度过粗**——
  把"重复容器的正常形态（同一份目录 meta 派生的随机子容器名）"误判为"内容差异"。
- **建议**：比较**逻辑结构**而非物理名：对两侧子容器分别解析目录 meta（`DirID`/逻辑 rel）后比对；
  或比对时先对子容器名做**规范化/归一化**（如按父容器 + 逻辑路径重算 `DirNameFor`）；至少在
  `DupContainerConflict` 中区分「多出条目类型」（子容器 / 文件 meta）并在日志与审计输出中标注，
  避免运维误判。

## P2（可用性/稳定性/可维护性）

### [P2-1] 装配期运维动作无超时，可长时间阻塞挂载
- **位置**：`runNormalizeNamesAction`（`context.Background()`）
- **问题**：迁移/审计/导出/清理全部使用无超时 ctx，且都在 `NewFS` 内**同步**执行 ⇒ 外部限流
  或大卷场景下启动被阻塞（实测从 32s 变为数分钟，多轮超时失败）。失败虽只告警不阻断（设计正确），
  但**挂载完成时间不可控**。
- **建议**：加可配置超时（如 `normalize_timeout`，默认几十秒~几分钟），超时即收口为"部分完成 +
  可续跑"（当前已具备幂等续跑能力）。

### [P2-2] `snapshotSuppressed` 是一次性闩锁，永不复位（注释与语义不符）
- **位置**：`index_snapshot.go:342`
- **问题**：注释写"抑制**本次装配**的异步落盘"，实际是进程级闩锁：进程内**任何**写操作置位后，
  该进程**再也不会**写出快照（含未来可能新增的周期性快照写入）。当前无功能影响（写路径本就
  应删快照），但语义脆弱、易被后续改动踩到。
- **建议**：改为"代际号比对"（写入前取 gen，rename 前比对），或显式命名
  `snapshotWriteDisabled` 并注释进程级语义。

### [P2-3] 采样校验在"父容器未迁移"时必然报漂移 ⇒ 每次启动固定刷告警
- **位置**：`sampleVerifySnapshot`
- **问题**：`want = DirNameFor(父容器当前值, rel)`，父未迁移时子容器的期望名与现值必然不同 ⇒
  未迁移卷**每次启动**固定输出 N 条逐 rel WARN + 1 条汇总 WARN（实测 `sampled=8 drift=8`）。
  `drift` 计数不代表真实问题数（父子依赖被重复计数），长期会造成告警疲劳。
- **建议**：漂移按"最浅未迁移祖先"归并计数；或在已知未迁移（`dirSegs` 中存在非确定性名）时
  降级为 Debug/单条汇总。

### [P2-4] 迁移退避偏弱 + 每次重试附一次远程列举
- **位置**：`moveContainerIdempotent`（`maxAttempts=3`，线性 2s/4s）+ `containerMovedByPeer`
- **问题**：外部限流（实测百度 31034）持续时间远长于 ~6s 总退避 ⇒ 多轮仍失败；
  `containerMovedByPeer` 每轮失败都做一次 `listInner`（该调用本身也受限流影响），
  放大 RTT 与限流概率。
- **建议**：退避改为指数 + 可配置上限（并与 `normalize_move_interval` 联动）；
  幂等判定可缓存父目录列举结果（同一父的多个子容器复用一次列举）。

### [P2-5] 无 CLI/API 入口；分批迁移需人工多次重启
- **问题**：`normalize_names` 只能改配置 + 重启触发；`normalize_max_moves` 分批后需**多次重启**
  才能收敛（本次实测正是如此）。运维不友好，且每次重启都要付一次全量容器树列举成本。
- **建议**：提供 `sclient volume normalize --action=apply --max-moves=N`（或管理 API），
  在一次进程内循环分批直到收敛/达上限。

### [P2-6] `exportOneFile` 返回两个相同值；注释重复；索引全量扫描
- **位置**：`dup_export.go`
- **问题**：`exportOneFile` 返回 `(int64, int64, error)` 但两值恒相同（语义不明）；函数头注释
  重复两行（复制粘贴残留）；`indexKeysUnder` 与 defer 清理各做一次**全索引扫描**
  （O(节点数 × |index|)）。
- **建议**：收敛返回值为单值；去重注释；用一次收集（`tmpRel` 前缀键列表）复用两次。

### [P2-7] `dupGroupsSnapshot` 浅拷贝（`others` 切片与内部共享）
- **问题**：`copy(out, s.dupGroups)` 复制的是结构体，`Others []string` 仍指向内部切片 ⇒
  调用方误改会污染内部状态（当前调用方只读，属防御性问题）。
- **建议**：深拷贝 `others`，或改返回只读视图。

## P3

### [P3-1] `alnumFromBytes` 的取模写法
`b[i%len(b)]`：`len(b)==0` 会 panic（当前不可达，SHA-256 恒 32B）；`% len(charset)` 引入轻微
取模偏置（熵略降，16 字符 ≈ 82 bit 量级，影响可忽略）。建议加长度断言并改用无偏取样。

### [P3-2] `DirNameFor` 分隔符写法
`string(rune(0))` 表意不清，应与同函数内的 `[]byte{0}` 统一为 `"\x00"`（分隔符无歧义本身已确认：
父容器名与逻辑路径均不含 NUL）。

### [P3-3] `containerEntries` 丢弃 `IsDir` 信息
审计输出无法区分"多出的子容器"与"多出的文件 meta"，降低人工处置可操作性（与 P1-5 同源）。

## 通过项（已验证正确，无问题面）

- **确定性命名 `DirNameFor`**：HMAC-SHA256 以**卷密钥**为 key（无密钥者不可枚举/关联）；域分隔符
  `sproxy/dir-name/v1\x00` + 父容器 + `\x00` + 逻辑路径 ⇒ 父名与路径均不含 NUL ⇒ 无拼接歧义；
  长度 16 落在既有随机名 5..30 外观区间内；**空密钥 fail-closed**。
- **权威容器判据**：`子容器数 → hasFiles → mtime → DirID`。`hasFiles` 位序正确（子容器数相同时
  空容器不得胜出——重复容器常由**复制目录 meta** 形成，mtime/DirID 完全相同，仅靠它们会任选）。
- **幂等判定**：`containerMovedByPeer` 用**父目录列举**（源不在 + 目标在）判定，明确否定了
  `Stat(want)==nil` 的宽松语义（该写法曾导致"迁移静默不生效"）；并以 `decided` 区分
  "无法判定（限流）"与"确定未完成"。
- **深度升序 + 父路径重算**：`segmentOf(segs, newSegs, parentDirOf(rel))` ⇒ 父先迁移、子按父的
  **新**路径派生目标名 ⇒ 迁移结果与"从零重建"同构（`DirNameFor` 依赖父容器，若不重算则深层必错）。
- **快照让路**：`allowSnapshot := opts.NormalizeNames == ""` ⇒ 审计/导出/清理/迁移均走全量扫描
  （否则 `dupGroups` 恒空 ⇒ 审计恒 0）。
- **部分完成的收口**：迁移的"失败/中断/达上限"三条路径都提交已完成部分并失效快照
  （`commitPartialMoves`）。
- **空容器清理的安全约束**：只处理 `others`（权威容器绝不动）；逐容器校验整棵子树**无文件 meta**
  才删；目录 meta 解密 JSON 留证；**自底向上**删除且先清空容器内条目再删目录。
  线上实测：8 个落选容器全部为空（0 个含文件），留证 10 份 JSON。
- **GC 兜底正确**：`gcReconfirmContainer` 对"不在索引"的 meta 重读+解密后调用
  `gcProtectLiveMeta` 补齐引用 ⇒ P1-1/P1-2 造成的引用错位**不会误删数据**（fail-closed），
  只退化为性能与语义问题。
- **测试覆盖**：本分支新增 `normalize_names_test.go`(607) / `dup_container_test.go`(142) /
  `dup_cleanup_test.go`(185) / `dup_export_test.go`(155) / `dirname_for_test.go`(81)，关键路径
  均有断言与变异记录（含本次审查期间新增的分批上限与失败失效快照两项）。

## 验证方式

- 源码逐函数审查：`DirNameFor`/`pickCanonicalContainer`/`containerScoreOf`/`normalizeOneRel`/
  `moveContainerIdempotent`/`containerMovedByPeer`/`normalizeAllRels`/`commitPartialMoves`/
  `auditOneGroup`/`containerEntries`/`cleanupOneContainer`/`deleteContainerTree`/
  `exportOneContainer`/`exportOneFile`/`sampleVerifySnapshot`/`trySnapshotFastPath`/
  `registerCanonicalContainers`/`writeIndexSnapshot`/`invalidateIndexSnapshot`
- 对照审查：`rebaseContainerPathsLocked`、`applyDirMoveLocked`、`loadContainerFileMeta`/
  `registerLoadedEntryLocked`、`addDirKeysLocked`、`gcMarkIndex`/`gcReconfirmContainer`/
  `gcContainersLocked`、`gcProtectLiveMeta`
- 线上实测日志核对（迁移失败窗口、采样 `sampled=8 drift=8`、清理 8/0 含文件）
- 聚焦测试：`go test -count=1 -run 'TestNormalizeContainerNames|TestDup|TestDirNameFor' ./pkg/volume/secretdata/... ./pkg/cryptox/shardseal/...`

## 建议处置顺序

1. **P1-1**（功能不可用 + 既有 helper 可直接复用）→ 立即修 + TDD + 变异
2. **P1-3 / P1-2**（状态一致性/竞态；改动小）→ 同批修
3. **P1-4**（导出泄漏；改动中等）→ 同批或紧随
4. **P1-5**（判据粒度；需产品口径确认"争议"定义）→ 设计确认后修
5. P2/P3 → 记录排期
