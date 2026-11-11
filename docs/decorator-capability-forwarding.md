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

## 2. 两个能力域（**关键**：门禁只覆盖了其中一个）

| 能力域 | 定义处 | 探测助手 | 门禁覆盖 |
| --- | --- | --- | --- |
| **FS 操作能力** | `pkg/sync`（枚举 14 个） | `syncpkg.Assert*` | ✅ **R21** 自动枚举 + 要求编译期断言 |
| **卷审查能力** | `pkg/volume/inspect.go`（`StorageInspector` / `TreeInspector`） | `volume.AssertStorageInspector` / `AssertTreeInspector` | ❌ **未覆盖**（R21 只看 `pkg/sync`） |

**已实测确认**：`pkg/volume/trusted`、`pkg/volume/capacity`、`pkg/volume/enclosure`、
`pkg/sync`、`pkg/syncexec`、`pkg/volumestate`、`pkg/server/dav` 的 **11 个装饰器**
**全部不转发** `StorageInspector` / `TreeInspector`。

## 3. 消费侧的正确做法（**权威解包入口**）

需要穿透装饰链取到"具备某能力的真实实现"时，**必须**用受认可的
`syncpkg.Innermost(fs)`，而**不是**在业务代码里手写 `Inner()` 链遍历（**R22 门禁**禁止）。
既有范例：`pkg/server/volume_inspect.go`：

```go
insp, ok := volume.AssertStorageInspector(syncpkg.Innermost(fsys))
```

**注意边界**：`Innermost` 会一路解到**最内层**。若需要的能力实现位于**中间某层**
（例如封装卷的 `secretdata` 实现位于其内层网盘之前），解包会**跳过头** ⇒ 此时**不能**用
`Innermost`，需要该能力**被装饰器转发**（见 §4）。

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

## 4. 根治方案（**待实施**：完整能力集接口 + 门禁扩展 + 逐装饰器补转发）

1. **提供"完整能力集"接口**（单一事实源）：把装饰器应当转发的**全部**可选能力（`pkg/sync` 的
   FS 能力 + `pkg/volume` 的审查能力）聚合为一个接口，例如
   `type fsCapabilities interface { syncpkg.RangeReader; …; volume.StorageInspector; … }`，
   供装饰器**一次性编译期断言**：
   ```go
   var _ fsCapabilities = (*MetaBucketGuard)(nil)
   ```
   好处：新增/遗漏能力时**编译期**即可发现，而不是等到运行期断言恒 false。
2. **扩展 R21 门禁覆盖 `pkg/volume`**：除 `pkg/sync` 外，**同时枚举** `pkg/volume` 中属于
   **FS 能力**的接口（`StorageInspector` / `TreeInspector`；`Footprinter`/`PhysBound`/
   `CapacityOwner`/`StatsProvider`/`StateGeneration` 是**卷级**能力，装饰器不适用，应显式排除）
   ⇒ 与 `pkg/sync` 同样要求 `var _ <能力> = (*<类型>)(nil)` 或显式豁免 + 理由。
3. **逐个装饰器补转发**（11 个，见 §2 清单），并对**无法同时装成多接口**的情况说明原因
   （不得静默漏掉）。`BlockAccessor` 是既有的合理豁免先例（引擎回退语义需 Capability 探测）。
4. **消除消费侧解包**：上述完成后，`pkg/server` 侧即可统一走 `volSet.VolumeView(…, ViewUser)`
   **直接断言**能力，不再需要 `Innermost`，也不再产生"外部卷 FS 直连点"（R27 登记项随之消除）。

## 5. 现状与本次处置

- **启动存储审查**（`pkg/server/issues_scan.go`）：已改用
  `syncpkg.Innermost(volSet.VolumeView(…, ViewUser))` —— 与 `volume_inspect.go` **同一正统做法**，
  不再直接取后端视图，`external_fs_calls.tsv` 中的登记项已消除。
- **待处理事项的执行器**（`cmd/sproxy/root.go`）：仍需**中间层**能力（封装卷的
  `CleanupStaleDirMeta` 实现在 `secretdata`，`Innermost` 会跳到更内层的网盘视图而**取不到**）
  ⇒ 暂时保留后端视图取用并在 `external_fs_calls.tsv` **登记**（评估结论：只删本卷自身的残留
  dir-meta blob，不读写用户内容、不访问被封装的源子目录 ⇒ **非封印绕过面**）；按 §4 完成后消除。

## 6. 检查清单（新增能力接口或装饰器时）

- [ ] 新能力接口登记在**单一事实源**（`pkg/sync` 或 `pkg/volume`），并进入"完整能力集"聚合接口；
- [ ] **每个**装饰器补 `var _ <能力> = (*<类型>)(nil)`；确实不转发 ⇒ 在 R21 豁免表写明理由；
- [ ] 跑 `go test ./internal/archcheck/`（R21 + R27 + R22）；
- [ ] 若消费侧必须穿透装饰链 ⇒ 用 `syncpkg.Innermost`，**不要**手写 `Inner()` 遍历（R22 会拦）。
