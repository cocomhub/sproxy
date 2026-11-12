# 装饰器能力转发（避免"静默降级"）

> 记录一次真实缺口与处置经验：**透明装饰器漏转发可选能力 ⇒ 上层类型断言恒 false ⇒ 静默降级**。
> 本文是权威记录（`docs/`）；实现入口见 `pkg/sync`（能力枚举单一事实源）与
> `internal/archcheck/capability_parity_gate_test.go`（门禁 R21）。

## 1. 问题模式（为什么会"静默降级"）

`pkg/sync.FS` 是基础接口（7 个方法）。**可选能力**是另外的接口，消费者用**类型断言**探测：

```go
if r, ok := fs.(syncpkg.RangeReader); ok { /* 走区间读 */ }   // 断言失败 ⇒ 退回整文件读（静默）
```

装饰器若**不实现**该接口，装饰后的 `fs` 断言恒 `false` ⇒ 上层以为"底层不支持"，**悄悄降级**
——编译通过、测试可能仍绿，但功能已失效。本仓历史上已为此修过 3 次（staging 门卫恒被跳过、
Provider 探测失败、`RangeReader`/`DirectURLProvider` 落空）。

## 2. 两个能力域与门禁覆盖

| 能力域 | 定义处 | 探测助手 | 门禁覆盖 |
| --- | --- | --- | --- |
| **FS 操作能力** | `pkg/sync`（枚举含 `RangeReader`/`WriteIfAbsent`/`MetaExtraUpdater`/`DirMetaCleaner` …） | `syncpkg.Assert*` | ✅ **R21** 自动枚举 + 要求编译期断言 |
| **卷审查能力** | `pkg/sync/inspect.go`（`StorageInspector` / `TreeInspector`；**2026-11-11 由 `pkg/volume/inspect.go` 下沉**） | `syncpkg.AssertStorageInspector` / `AssertTreeInspector` | ✅ **R21**（下沉后自动纳入：R21 从 `pkg/sync` 源码枚举） |

**初始缺口（2026-11-11 实测）**：`pkg/volume/trusted`、`pkg/volume/capacity`、`pkg/volume/enclosure`、
`pkg/sync`、`pkg/syncexec`、`pkg/volumestate`、`pkg/server/dav` 的 **11 个装饰器**当时
**全部不转发** `StorageInspector` / `TreeInspector` ⇒ 上层断言恒 false。
**已修复**（#1096 下沉 + 全量转发；见 §4/§5 状态）。

## 3. 消费侧的正确做法（**2026-11-12 更新：默认直接断言**）

能力**已下沉 `pkg/sync` 且被 11 个装饰器全量转发**（R21 保证）⇒ 消费侧**默认直接断言装饰后的
视图**，不再需要解包：

```go
if insp, ok := syncpkg.AssertStorageInspector(fsys); ok { /* 装饰链已转发；底层未实现 ⇒ ErrUnsupported */ }
```

仅在**能力尚未下沉/未被转发**（如 `MetaExtraUpdater` 的 `AssertMetaExtraUpdater`）时才用
受认可的 `syncpkg.Innermost(fs)` 下探，而**不是**在业务代码里手写 `Inner()` 链遍历（**R22 门禁**禁止）。

**注意边界**：`Innermost` 会一路解到**最内层**。若需要的能力实现在**中间某层**，解包会
**跳过头** ⇒ 此时只能靠「下沉 + 转发」（见 §4），不能靠解包。

## 3.5 **关键架构约束**（决定了根治的形态，实测确认）

朴素想法是"让每个装饰器转发 `StorageInspector`/`TreeInspector` 就行"。**但这一层做不到**：

- `pkg/sync` 在分层表（`internal/archcheck/layers.go`）中是 **G0 基础层**，其导入集**不含** `pkg/volume`
  （实测：`pkg/sync/*.go` 无一处 import `cocomhub/sproxy/pkg/volume`）；
- 而 `StorageInspector`/`TreeInspector` **定义在 `pkg/volume`**（连同 `Anomaly`/`PathTrace`/
  `ContainerRef`/`BlobRef`/`RealEntry` 等数据结构）；
- ⇒ **位于 `pkg/sync` 链上的装饰器**（`StagingQuotaGateFS`、`PrefixFS`）**无法引用**这些类型
  ⇒ **无法转发**。而它们在**装饰链中间** ⇒ 只要它们不转发，链条末端的断言仍然恒 false。

**⇒ 根治（唯一形态）是把「卷审查能力」（接口 + 数据结构）下沉到 `pkg/sync`**（或新建一个位于其
下方的能力包），`pkg/volume` 侧改为**别名**保持单一事实源（`type StorageInspector = syncpkg.StorageInspector`）。
这属于**架构级改动**：涉及 `Anomaly`/`PathTrace`/`ContainerRef`/`BlobRef`/`RealEntry`/`InspectMode` 等
一批数据结构的搬迁与全仓引用更新 ⇒ **应作为独立立项**，不宜与审计类改动混在一个 PR。

> **已落地（2026-11-11 / 11-12）**：审查能力经 `pkg/{volume => sync}/inspect.go` 整体搬迁完成
> （#1096）；消费包自定义能力 `DirMetaCleaner` 按同一判据下沉（2026-11-12）。
> **未采用别名**：`pkg/volume` 为别名而 import `pkg/sync` 会与既有 `pkg/sync → pkg/files/meta →
> pkg/volume` 形成**环**（实测确认）⇒ 调用点直接改用 `syncpkg.X`。

## 4. 根治方案（完整能力集接口 + 门禁覆盖 + 逐装饰器补转发）

1. **提供"完整能力集"接口**（单一事实源）：把装饰器应当转发的**全部**可选能力（`pkg/sync` 的
   FS 能力 + 下沉后的审查能力）聚合为一个接口，供装饰器**一次性编译期断言**：
   ```go
   var _ fsCapabilities = (*MetaBucketGuard)(nil)
   ```
   好处：新增/遗漏能力时**编译期**即可发现，而不是等到运行期断言恒 false。
   **状态（2026-11-12）**：未实施（现依赖 R21 逐接口断言，效果等价但需逐个列）。
2. ~~**扩展 R21 门禁覆盖 `pkg/volume`**~~：**已由下沉替代**（2026-11-11）：审查能力移入
   `pkg/sync` 后，R21 **自动枚举**即覆盖（无需改门禁）。
3. **逐个装饰器补转发**（11 个，见 §2 清单），并对**无法同时装成多接口**的情况说明原因
   （不得静默漏掉）。`BlockAccessor` 是既有的合理豁免先例（引擎回退语义需 Capability 探测）。
   **状态（2026-11-12）**：`StorageInspector`/`TreeInspector`（#1096）与 `DirMetaCleaner`
   已完成 11 个装饰器全量转发 + 编译期断言。
4. **消除消费侧解包**：能力被全量转发后，消费侧即可统一走 `volSet.VolumeView(…, ViewUser)`
   **直接断言**，不再需要 `Innermost`，也不再产生"外部卷 FS 直连点"（R27 登记项随之消除）。
   **状态（2026-11-12）**：三条消费路径已完成——启动审查（#1101）、卷审查 API（#1101）、
   事项修复执行器 `CleanupTargetResolver`（2026-11-12）。

## 5. 现状（2026-11-12 更新）

- **能力已下沉 `pkg/sync`**：`StorageInspector`/`TreeInspector`（#1096，含数据结构整体搬迁，
  调用点 `volume.X` → `syncpkg.X`）、`DirMetaCleaner`（2026-11-12，实现方仍在
  `pkg/volume/secretdata` 的 `SecretdataFS`）。
- **11 个装饰器全量转发 + 编译期断言**（R21 自动枚举）：`trusted.TrustedVolumeFS`/
  `MetaBucketGuard`、`capacity.CapacityFS`、`sync.StagingQuotaGateFS`/`PrefixFS`、
  `server/dav.davMetaFS`、`enclosure.FS`/`SealFS`/`ownerFS`、`syncexec.quotaLocalFS`、
  `volumestate.CacheFS`。底层未实现 ⇒ 转发层返回 `ErrUnsupported`（断言**仍成立**，由调用方
  按"未实现"处置）。
- **消费侧已收口为「直接断言装饰后的视图」**：`pkg/server/issues_scan.go`（启动审查扫描）、
  `pkg/server/volume_inspect.go`、`pkg/server/issues_executors.go` 的 `CleanupTargetResolver`
  （事项修复目标；原 `external_fs_calls.tsv` 登记项已消除）。
- **仍未下沉、仍靠 `Innermost` 探测的能力**（另行收口）：`pkg/sync/metaextra.go`
  （`AssertMetaExtraUpdater`）、`pkg/sync/range_seek.go`、`pkg/sync/staging_gate.go`
  （staging 判据必须基于最内层，否则会被透传接口恒真击穿）、`pkg/server/files_phys.go` /
  `files_service.go`（`Copier`/range 探测）、`pkg/volume/capacity/phys.go`、
  `pkg/volumestate/wrap.go`。

## 6. 检查清单（新增能力接口或装饰器时）

- [ ] 新能力接口登记在**单一事实源**（`pkg/sync`；`pkg/sync` 不能引用的能力包不满足条件）；
- [ ] **每个**装饰器补 `var _ <能力> = (*<类型>)(nil)`（漏一个 ⇒ R21 红）；确实不转发 ⇒ 在 R21 豁免表写明理由；
- [ ] 跑 `go test ./internal/archcheck/`（R21 + R27 + R22）；
- [ ] 消费侧**默认直接断言装饰后的视图**（不得先解包）；仅对未下沉能力保留 `syncpkg.Innermost`，
      **不要**手写 `Inner()` 遍历（R22 会拦）；
- [ ] 转发层对「底层未实现」统一返回 `ErrUnsupported`，并在调用侧转成可读的"不支持/需人工"原因。
