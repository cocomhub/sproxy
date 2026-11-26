<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# secretdata 分块规则引擎（按路径正则 + 阈值命中「整文件不分片」）

> 状态：**设计待审**（2026-11-26）。决策人：用户。
> 关联：`pkg/cryptox/shardseal`（分块/加密/派生）、`pkg/volume/secretdata`（写路径选型）、
> `pkg/volume/registry`（后端可选能力）、`pkg/server`（HTTP 面）、`web/static/v2`（卷控制台）。
> 前置：`docs/designs/2026-11-16-deterministic-blob-convergent-encryption.md`（收敛档已成默认算法）。

## 1. 背景与目标

secretdata 卷当前**只有一种块级规划**：`RandomPlanner`（`block_policy.mode: random`，1 MiB–200 MiB 变长）。
对体积很小的文件（图片、短文本、日志、已压缩包），随机分块没有收益，反而带来两个可观测坏处：

1. 一个 ≤20 MiB 的文件被切成多块 ⇒ 底层多个 blob、多次上传/校验调用（外部网盘每块一次 CLI 往返）；
2. 块边界随机 ⇒ 小文件之间块布局差异无实际价值，只增加 meta 体积。

**目标**：支持「整个文件 = 1 个块」（不分 chunk），并让它按**路径正则 + 大小上限**自动命中，
规则可在 v2 Web UI 按卷动态调整，且**同一策略必须产出逐字节相同的文件**（与收敛档既有契约同水平）。

**不做**（用户 2026-11-26 裁定）：不新增**段级** `single`（`blocklet_mode: single`）。块内仍走既有
`fixed` / `video-keyframe` 自动选型 —— 上传单位本就是 chunk，切段只影响随机读反弹与 meta 体积，
砍掉段级 single 可少一个规划器分支与一整套测试矩阵，且外部卷 Range 性能不退化。

## 2. 现状事实（读码核实，设计的事实基础）

| # | 事实 | 位置 |
|---|---|---|
| F1 | 块级规划器只有 `RandomPlanner`；`blockPlanner()` 对未知 mode 返回 nil（fail-closed） | `pkg/cryptox/shardseal/shardseal.go:70`、`block.go:44` |
| F2 | 块内只有 `fixed` / `video-keyframe`；读路径按 `meta.Chunks[].Blocklets` 索引驱动，**不认识块级 mode** | `shardseal.go:87`、`meta.go:290` |
| F3 | 写路径两条（流式 / 内存）都只在 `blockletPolicyFor(rel)` 处选策略，**不碰块级** | `secretdata.go:1597`、`2092`、`helpers.go:38` |
| F4 | `blockletPolicyFor` 签名只有 `name`，**没有 size** | `helpers.go:38` |
| F5 | 写入键恒为 `<owner>/user/<rel>`（`Location.FSPath()`），`secretdata` 内部**不知道 owner** | `external_upload.go:71`、`volume_path.go:98` |
| F6 | 收敛档 `AlgoV2GCM` 已是默认写算法：块长/盐/nonce/首部/间隙按 `(卷密钥, 内容CID, 序号)` 确定性派生 | `secretdata.go:538`、`convergent.go` |
| F7 | 单块加密**整块载入内存**（`chunk := make([]byte, b.Size)`） | `shardseal.go:464` |
| F8 | `PhysBound(logical int64)` **不带 rel**（按卷级 `block_policy.max` 折算上界） | `pkg/server/files_phys.go:96`、`secretdata/physfootprint.go:41` |
| F9 | 既有可选能力先例：`Presigner` / `MetaPrewarmer` / `IndexRefresher`（registry 通用层不含具体后端类型） | `pkg/volume/registry/backend.go:28-69` |
| F10 | 既有建卷表单 schema：`FieldSchema{Key,Label,Type,Required,Options,AllowWrapper}`，`Type ∈ {text,volume-select,enum,bool,number}` | `pkg/volume/registry/backend.go:284` |
| F11 | `state.StateStore`（Put/Get/CAS/List）已装配到 `h.stateStore`，可 mongo 后端 | `deps.go:322`、`state/state.go` |
| F12 | 路由分类 deny-by-default（R38）+ 变更型路由业务审计（R35-b）+ API 文档一致（R55） | `internal/archcheck/*` |

## 3. 规则模型

### 3.1 数据结构（新包 `pkg/cryptox/shardseal/policy`）

```go
// Rule 是一条分块规则：路径正则 + 体积区间 → 块级模式。
type Rule struct {
    // PathRegex 是对**用户可见相对路径**的 RE2 正则（整串匹配，服务端编译时加 ^(?:...)$）。
    // 例：`^archive/.*\.zip$`、`\.jpg$`、`^photos/`。
    PathRegex string `json:"path_regex"`
    // MaxSize/MinSize 是可选的体积区间（字节）。MaxSize>0 表示仅 size ≤ MaxSize 命中；
    // MinSize>0 表示仅 size ≥ MinSize 命中；两者同时给 = 闭区间。0 = 该侧不限。
    MinSize ByteSize `json:"min_size,omitempty"`
    MaxSize ByteSize `json:"max_size,omitempty"`
    // Mode 是块级模式："single"（整文件 1 块）或 "random"（显式回落到卷默认变长分块）。
    Mode string `json:"mode"`
    // Priority 是排序键（大者先）；同 priority 按声明顺序（稳定排序）。
    Priority int `json:"priority,omitempty"`
    // Note 是可选的人类可读说明（仅回显，不参与匹配）。
    Note string `json:"note,omitempty"`
}

// Policy 是一个卷的分块规则集（有序命中，JSON 形状 = 持久化形状）。
type Policy struct {
    Rules   []Rule `json:"rules"`
    Version int64  `json:"version"` // 持久化版本（CAS + UI 乐观并发提示）
}

// Compiled 是**运行期**形态：规则已排序 + 正则已编译（写路径只做 MatchString，零编译开销）。
// Policy 是不可变值；Compiled 由 Compile 一次性产出，之后只读（可被多 goroutine 并发使用）。
type Compiled struct {
    rules []compiledRule
}

// Compile 校验 + 编译规则集（装配期与热更新各调用一次）。校验失败返回 error（含规则下标）。
// volumeBlockMax 是卷级 block_policy.max（≤0 = 未知，跳过该条比较），见 §3.3。
func Compile(p Policy, volumeBlockMax int64) (*Compiled, error)

// Resolve 返回命中规则的**副本**（调用方不可改内部状态）；未命中返回 (零值, false)。
// 纯函数、无 I/O、无 error 通道；size < 0 时只按路径匹配（忽略体积约束，防御未来调用方）。
func (c *Compiled) Resolve(userRel string, size int64) (Rule, bool)

// Rules 返回规则副本（API 回显用）。
func (c *Compiled) Rules() []Rule

// Version 返回编译时携带的持久化版本。
func (c *Compiled) Version() int64
```

**为什么分 Policy / Compiled 两层**：写路径每写一个文件都会走一次匹配；把正则编译放在写路径
就是每文件一次 `regexp.Compile`（真实开销）。`Compiled` 让「校验 + 编译」只在装配与热更新
发生一次，写路径只剩 `MatchString`（RE2，线性时间）。`nil *Compiled` 表示「无规则」⇒ 走卷默认。

模式常量**只在 `shardseal` 定义**（单一事实源，`policy` 直接引用，不再复制一份同名常量）：

```go
// pkg/cryptox/shardseal/block.go
const (
    BlockModeRandom = "random"  // = 既有 RandomPlanner 的模式名（blockPlanner 的 ""/"random" 分支）
    BlockModeSingle = "single"  // 整文件单块
)
```

上限常量（`policy` 包内导出，供 API/UI 校验复用同一事实源）：

```go
const (
    // HardMaxSingleBytes 是 single 模式的**硬上限**（64 MiB）。
    // 依据 F7：单块加密整块入内存 ⇒ 无上限即 OOM 通道；64 MiB 覆盖「图片/文本/日志/已压缩包」
    // 全部目标场景，且低于默认块上限 200 MiB（F8 的 PhysBound 口径不受影响）。
    HardMaxSingleBytes int64 = 64 << 20
    // MaxRules 是单卷规则条数上限（防 CPU/内存滥用；匹配是每写一次全表扫描）。
    MaxRules = 64
    // MaxRegexLen 是 PathRegex 字符串长度上限（RE2 无回溯爆炸，仅防超大模式占内存）。
    MaxRegexLen = 512
)
```

### 3.2 匹配语义

- 匹配对象 = **用户可见相对路径**：`<owner>/user/<rel>` 剥离后的 `<rel>`（用户 2026-11-26 裁定）。
- 剥离用**形状判据**（因为 `secretdata` 不知道 owner，F5）：

  ```go
  // userVisibleRel 把卷内 FS 键剥离 `<owner>/user/` 前缀。
  // 形状不符（段数 <3，或第二段不是 "user"）⇒ 返回 ok=false ≠ 用户面写入 ⇒ 不套任何用户规则。
  func userVisibleRel(fsKey string) (rel string, ok bool)
  ```

  形状判据而非「找第一个 user/ 段」是**必须的**：文件自身路径含 `user` 段（`a/user/b.txt`）时，
  按段找会误剥；按「首段=owner、第二段必须=user」则可靠。owner 名叫 `user` 时（`user/user/x`）
  同样正确（`parts[0]="user"`、`parts[1]="user"` ⇒ `x`）。
- **排序**：`priority` 降序 + 声明序（`sort.SliceStable`）；**首命中即停**。
- **未命中** ⇒ 卷默认策略（现状行为，零回归）。
- 正则编译在**写入/载入规则时**完成（fail-closed），运行期只做 `MatchString`。

### 3.3 校验（写入与装配共用一份，`policy.Compile`）

| 条件 | 结果 |
|---|---|
| `len(Rules) > MaxRules` | 拒（400） |
| `PathRegex` 空 / 长度 > `MaxRegexLen` / 编译失败 | 拒（400），错误里带**规则下标** |
| `Mode ∉ {single, random}` | 拒（400） |
| `Mode == single` 且 `MaxSize == 0` | 拒（400）—— 无上限的「整文件 1 块」= OOM 通道 |
| `MaxSize > HardMaxSingleBytes` | 拒（400），文案带实际上限 |
| `MaxSize > 卷 block_policy.max` | 拒（400）—— 依据 F8：`PhysBound` 按卷级 `max` 折算，规则块超出会**低估物理上界** ⇒ 配额预占失真 |
| `MinSize > MaxSize`（两者均 >0） | 拒（400） |

> 「`MaxSize ≤ 卷 block_policy.max`」这条把 F8 的隐含约束变成显式校验，而不是留一个静默的配额低估。

## 4. `single` 规划器

```go
// pkg/cryptox/shardseal/block.go
const BlockModeSingle = "single"

// SinglePlanner 把整文件规划为**单个块**（mode: single）。
// 纯函数、无可注入随机源 ⇒ 收敛档下同内容同策略产出逐字节相同的 blob 与名（契约见 §6）。
type SinglePlanner struct{}

// Plan 校验 size>0 后返回 [{0, size}]。
// 空文件沿用既有 fail-closed 语义（与 RandomPlanner 一致，见 block.go:64 注释）。
func (SinglePlanner) Plan(size int64) ([]Block, error)
```

接入 `BlockPolicy.blockPlanner()`：

```go
case BlockModeSingle:
    return SinglePlanner{}
```

**读路径零改动**（F2）：单块是既有磁盘格式的合法实例（1 个 `ChunkInfo`，块内 blocklet 索引仍
完整覆盖 `[0, size)`）；`validateMeta` / 随机读 / 窗口规划 / erasure 判定全部按既有逻辑成立。

**关键属性（必须写进代码注释并测试锁定）**：

- `SinglePlanner` 不参与 `injectConvBlockSizes`（该函数只对 `*RandomPlanner` 生效，`convergent.go:115`）⇒ 确定性天然成立。
- 内存峰值 = 文件大小（F7）。规则已用 `MaxSize ≤ min(HardMaxSingleBytes, 卷 block_policy.max)` 封顶。
- `stagingReserveInterleaved(…, policy)` 按 `min(size, policy.Max)` 预占（`secretdata.go:5222`）；
  single 时把 `policy.Max` 定为 `max(fileSize, 1)` ⇒ 预占 = `cipherPeak(fileSize)`，与真实本地峰值一致。
- `Erasure` 不产 parity（`len(Chunks) == 1` 本就走不到 parity 分支）⇒ 若某卷开了 `erasure` 又命中
  single 规则，等于该文件**无冗余**。**装配期告警一次**（不是错误：用户可能正要这个效果）。

## 5. 选型接入（写路径）

### 5.0 `Options` 新增字段

```go
// Options 新增（装配层从 extra.block_rules 解析并 policy.Compile 后注入；nil = 无静态基线）：
Policy *policy.Compiled
// OnBlockModeResolved 见 §5.1 末尾（可观测性回调，nil = 不上报）。
OnBlockModeResolved func(rel, mode string, matched bool)
```

`SecretdataFS` 持有 `policy atomic.Pointer[policy.Compiled]`：构造时从 `Options.Policy` 初始化，
之后由 `SetPolicy`（静态基线 / 运行期规则 / 两者合并后的结果）原子替换。

### 5.1 `blockletPolicyFor` 扩展

```go
// 签名变更：加 size（F4）。两个调用点都已有 wc.size。
func (s *SecretdataFS) blockletPolicyFor(rel string, size int64) (shardseal.BlockPolicy, error)
```

新流程（**先规则、再既有逻辑**，保证零回归）：

```
1. 若已编译规则快照非空 且 userVisibleRel(rel) 形状合法：
     命中 → base := s.opts.Block（副本）；按命中规则覆写 Mode/Max：
              mode=single ⇒ policy.Mode = BlockModeSingle, policy.Max = max(size,1), policy.Min = 1
              mode=random ⇒ policy.Mode = BlockModeRandom（显式回落，仍保留卷 Min/Max）
     记录命中（日志 + 指标），**继续走既有 blocklet 选型**（扩展名/显式配置优先）
2. 未命中 → 完全保持现状逻辑（helpers.go:38-66 不变）
```

要点：

- **size 在两个调用点都恒为已知非负值**（事实依据）：内存路径 `wc.size = int64(len(data))`
  （`secretdata.go:1490`）；流式路径在 `writeFileOpt` 已拒绝 `size < 0`（`ErrUnknownSourceSize`，
  `secretdata.go:1408`）。故规则匹配不需要「大小未知」分支；`Resolve` 仍对 `size < 0` 定义行为
  （视作不满足任何 `MinSize/MaxSize` 约束，只按路径匹配）以防御未来调用方。
- 规则**只覆盖块级**，不覆盖 `BlockletMode`（段级仍自动选型/显式配置）——用户 2026-11-26 裁定。
- `policy.Max` 在 single 时设为 `max(size, 1)`：块规划器只产 1 块，`Max` 唯一作用是让
  `stagingReserveInterleaved` 预留正确（见 §4）与 `physfootprint.blockParams` 口径一致。
- 规则命中**不修改** `s.opts.Block`（那是共享值）；对 `policy` 只做副本改写（既有
  `blockletPolicyFor` 已是副本语义，`helpers.go:39`）。
- **Dedup 路径不受规则影响**：`writeFileDedup` 自己构造单块策略（`features.go:479`），不经
  `blockletPolicyFor`。这是既有设计，本期不改；文档与日志需说明（避免「规则没生效」的误判）。
- **可观测性经回调注入**（`pkg/volume/secretdata` 不 import metrics —— 与 `IssueReporter` 同模式，
  `Options.IssueReporter` 先例）：

  ```go
  // Options 新增字段（nil = 不上报，零回归）：
  // OnBlockModeResolved 在写路径确定块级模式后调用（命中规则/未命中都调，便于区分「规则没命中」）。
  // mode ∈ {"single","random"}；matched 报告是否由规则命中；ruleNote 是命中规则的 Note（可选）。
  OnBlockModeResolved func(rel, mode string, matched bool)
  ```

### 5.2 运行期替换（热更新）

```go
// SetPolicy 原子替换已编译规则集（nil = 清空，回落卷默认）。写路径读快照，无锁竞争。
// 编译（校验）由调用方经 policy.Compile 完成；本方法不做校验、不返回错误。
func (s *SecretdataFS) SetPolicy(c *policy.Compiled)
```

内部用 `atomic.Pointer[policy.Compiled]`；`blockletPolicyFor` 首行 `c := s.policy.Load()`。

### 5.3 后端可选能力（registry，F9 同模式）

```go
// pkg/volume/registry/backend.go

// VolumeSettings 是一个卷的**设置分区**（可读/可写配置项的通用描述）。
// 与 Presigner/MetaPrewarmer 同模式：registry 通用层不提及任何具体后端类型。
type VolumeSettings struct {
    // Key 是分区标识（URL 段；如 "block_policy"）。
    Key string `json:"key"`
    // Label 是 UI 显示名（如 "分块规则"）。
    Label string `json:"label"`
    // Fields 是分区内的字段 schema（复用 FieldSchema；新增 Type "rules" 表示规则表）。
    Fields []FieldSchema `json:"fields"`
    // Value 是当前值（后端自有 JSON 形状，前端按 Fields 渲染）。
    Value json.RawMessage `json:"value,omitempty"`
    // ReadOnly 标记该卷此分区不可写（如后端只支持声明式配置）。
    ReadOnly bool `json:"read_only,omitempty"`
}

// SettingsProvider 是可选能力：卷提供可读的设置分区。
type SettingsProvider interface {
    Settings(ctx context.Context) ([]VolumeSettings, error)
}

// SettingsWriter 是可选能力：按分区写入设置（payload 由后端自行解析并校验）。
// 不支持的分区/字段 ⇒ 返回明确错误（调用方映射 400/405），不得静默忽略。
type SettingsWriter interface {
    UpdateSettings(ctx context.Context, section string, payload json.RawMessage) error
}

// SettingsStore 是「卷设置分区」的**持久化抽象**（装配层注入后端；nil = 仅内存易失）。
// 与 SettingsProvider/Writer 同包，避免为持久化另造同形接口。
type SettingsStore interface {
    // Load 返回该卷的当前值 + 版本；无记录 → (nil, 0, nil)。
    Load(ctx context.Context, volume string) (json.RawMessage, int64, error)
    // Save 以 CAS 写入（expectedVersion 不符 → ErrSettingsVersionConflict），返回新版本。
    Save(ctx context.Context, volume string, payload json.RawMessage, expectedVersion int64) (int64, error)
    // Delete 清除该卷的持久化设置（幂等）。
    Delete(ctx context.Context, volume string) error
}

// ErrSettingsVersionConflict 是 CAS 冲突哨兵（调用方映射 409）。
var ErrSettingsVersionConflict = errors.New("registry: 卷设置版本冲突")
```

**持久化实现（唯一一份）**：`pkg/volume/registry/settings_state.go` 的 `stateSettingsStore`（包内构造
`NewStateSettingsStore(state.StateStore)`），键 `volume_policy/<卷名>`，值为
`{"version":N,"value":<后端自有 JSON>}`；CAS 用 `state.StateStore.CAS`。装配根在 `h.stateStore`
非 nil 时注入，否则传 nil（纯内存）。

> **装配时序约束（读码核实）**：`assembleVolumes`（在 `RegisterRoutes` 内）构造 secretdata 后端，
> 而 `h.stateStore` 由 `setupStateStore(h)` 在 `RegisterRoutes` **之后**装配（`root.go:1178` → `1286`）。
> ⇒ 后端在**构造期**拿不到 StateStore。做法：后端持有 `atomic.Pointer[registry.SettingsStore]`，
> 提供 `SetSettingsStore` 注入点；服务端在 `setupStateStore` 之后调一次
> `server.AttachVolumeSettingsStores(h)`（遍历卷集、对实现注入点接口的后端注入 + 触发一次 `LoadSettings`）。
> 这样「装配期无 StateStore」与「之后注入」都是**同一份代码路径**，不需要在装配顺序上做特殊处理。

**为什么不做「包装 backend」**：`registry.Set.External()` 返回的是**原始 backend 实例**
（`set.go:271`），包一层薄壳会让 `Presigner`/`MetaPrewarmer`/`IndexRefresher`/`URLResolver`
等既有能力断言全部落空（静默降级）。故由**各卷型后端自己实现** `SettingsProvider`/`SettingsWriter`
并组合同一份 `SettingsStore`——单份持久化实现，零包装。

`secretdata` 的 `backend` 实现两者：

- `Settings` 返回单个分区 `block_policy`（`Fields` 声明 `rules` 类型 + 上限元信息），
  `Value` = 当前规则集 JSON（含 `version`）；**纯内存读**（不 I/O）。
- `UpdateSettings("block_policy", payload)`：解析 → `policy.Validate` → `SettingsStore.Save`（CAS）
  → `SetPolicy` 热生效。**只接受 `block_policy`**，其它分区返回 `ErrSettingsSectionUnsupported`。
- `LoadSettings(ctx)`（后端内部方法，非能力接口）：从 `SettingsStore` 读一次并 `SetPolicy`，
  读失败保留静态基线 + 告警（不阻断挂载）。由 `SetSettingsStore`（注入点）与显式重新加载共用同一实现。

**注入点能力（新增，与 `SettingsProvider`/`SettingsWriter` 同风格）**：

```go
// SettingsStoreSetter 是可选能力：装配层在 StateStore 就绪后把持久化后端注入卷后端。
// 实现者应：保存 store → 立即 LoadSettings 一次（读失败仅告警，保留当前策略）。
type SettingsStoreSetter interface {
    SetSettingsStore(store SettingsStore) error
}
```

## 6. 确定性（重入稳定）契约

要求（用户 2026-11-26 明示）：**同一策略必须生成完全一样的文件**，与既有收敛档契约同水平。

论证与测试：

| 维度 | 结论 | 依据 |
|---|---|---|
| 块边界 | 确定性：`SinglePlanner.Plan` 是纯函数 | §4 |
| 盐 / nonce / 随机首部 / 段间隙 | 确定性：`convSealSource` 按 `(卷密钥, CID, 标签)` 派生 | F6、`convergent.go` |
| 块名 / meta 名 | 确定性：由密文哈希派生 | `naming.go` |
| 块内 blocklet | 确定性：`fixed` 由 `(BlockletMin,Max)` 决定；`video-keyframe` 由内容关键帧决定 | `block.go:135`、`video_planner.go` |

⇒ 同内容 + 同规则集 + 同卷密钥 ⇒ **逐字节相同**。测试锁定：同一输入两次写入产物（blob 名 + 字节 + meta 名）
全等；换规则（single ↔ random）产物必不同。

**已知边界（必须写进文档与 UI 提示）**：`mode: single` 与 `mode: random` 的产物不同 ⇒
**改规则不会重写已落盘文件**（既有文件按自身 meta 读，永远正确）；重写后才用新策略。

## 7. 持久化与优先级

- **键**：`volume_policy/<卷名>`（经 `registry.SettingsStore` → `state.StateStore`）。
- **值**：`{"version":N,"value":{"rules":[...]}}`——外层 `version` 是 CAS 版本，`value` 是后端自有
  JSON（`policy.Policy` 形状，其内部 `version` 字段与 CAS 版本同源，避免两个版本号）。
- **写入**：`SettingsStore.Save(vol, payload, expectedVersion)` 内部 CAS；冲突 ⇒
  `ErrSettingsVersionConflict`，HTTP 409 + 提示重读（不覆盖他人修改）。
- **优先级链**（显式、单一顺序）：

  ```
  StateStore 运行期规则  >  config volumes[].extra.block_rules（静态基线）  >  卷默认策略
  ```

  StateStore 无键 ⇒ 用静态基线；两者皆空 ⇒ 卷默认（现状零回归）。删除运行期规则（`PUT {"rules":[]}`）
  ⇒ 自然回落到静态基线/默认，无需「删除键」语义 ⇒ **不会出现「改了删不掉」**。
- **装配**：`cmd/sproxy/secret_register.go` 解析 `extra.block_rules` 作为初始策略注入 `Options.Policy`
  （经 `policy.Compile` 预编译；编译失败 ⇒ **装配 fail-closed**，静态配置写错不该静默）；
  服务端装配完成后（`h.stateStore` 就绪）逐卷 `LoadSettings` 覆盖 ⇒ 重启即生效。
- **失败语义**（生产可用性）：

  | 场景 | 处置 |
  |---|---|
  | StateStore 读失败（网络/后端故障） | **保留静态基线 + 告警**，不阻断挂载（可用性优先；规则是加速项非正确性项） |
  | StateStore 读到的规则编译失败（外部写入/版本不兼容） | 同上：保留基线 + 告警，**不阻断**（否则一个坏规则能让卷挂不上） |
  | 写 StateStore 失败 | 500，**内存策略不变**（不接受「半生效」） |
  | 写成功但热注入失败（理论上不会发生，`SetPolicy` 无失败通道） | 不会发生；`SetPolicy` 为 atomic 交换、无错误返回 |

## 8. HTTP API

| 方法 | 路径 | 分类 | 说明 |
|---|---|---|---|
| `GET` | `/api/volumes/{name}/detail` | `ClassReader` | 卷详情：基础信息 + `settings[]`（全部分区 schema + 当前值）+ `capabilities[]` |
| `GET` | `/api/volumes/{name}/settings` | `ClassReader` | 只取 `settings[]`（大卷型只读详情时不拖慢基础信息） |
| `PUT` | `/api/volumes/{name}/settings/{section}` | `ClassAuth` + 归属校验 | 写一个分区（本期仅 `block_policy`） |

**为什么不直接用 `GET /api/volumes/{name}`（实测核实后的修正）**：卷名只受
`storage.ValidSegmentName` 约束（`name.go:29`），`export`/`user`/`refresh`/`move`/`copy`/
`rebalance`/`import` 都是**合法卷名**；而 Go 1.22 ServeMux 中**字面段优先于通配段**（已用最小
用例实测：`GET /api/volumes/export` 仍命中 export handler）。⇒ 若详情挂在 `/api/volumes/{name}`，
名为 `export` 的卷**永远打不开详情**。改用 `/{name}/detail`（三段）与全部既有两段字面路由
**结构性无冲突**，且经实测验证 `GET /api/volumes/user/detail` 正确解析 `name=user`。

**设计理由（用户 2026-11-26 裁定「每个卷新增详情按钮，在 secretdata 卷详细中新增分块规则模块,
后续不同类型的卷新增配置功能都采用类似方式」）**：详情与设置做成**通用容器**（分区列表 + 字段 schema
+ UI `kind`），后续卷型新增配置只实现 `SettingsProvider`/`SettingsWriter` 并声明字段，**不改路由、
不改前端框架**。路径形如 `/api/volumes/{name}/settings/{section}` 而非 `/api/volumes/{name}/policy`,
正是为了让「分块规则」只是第一个分区而非特例。同形态既有先例：`/api/volumes/{name}/meta-audit`、
`/orphans`、`/trace`、`/explain`。

**实施时必须补的路由级测试**（防止未来新增字面路由与 `{name}` 段互相吞掉）：
`GET /api/volumes/export` → export handler；`GET /api/volumes/user` → 用户卷列表；
`GET /api/volumes/user/detail` → 详情(name=user)；`PUT /api/volumes/user/settings/block_policy` → 用户卷分支。

### 8.1 权限门（用户 2026-11-26 裁定，判定顺序不可交换）

```
① 卷存在性 + 可见性（读与写共用）：
     vol, ok := h.volSet.ByName(name)；且 name ∈ egressVisibleView(AllowedVolumes(all, owner), owner)
     不满足 ⇒ 404（防枚举：他人私有卷与「不存在」不可区分）
② 写权限：
     若 name 在**请求者自己的**用户卷内（h.userVolumes.Get(owner, name) 命中）⇒ 允许
     否则若 请求者角色 == admin                                  ⇒ 允许
     否则                                                        ⇒ 403（可读但不可写）
③ 读权限：① 通过即可。
```

要点（安全性）：

- **顺序是关键**：先过 ①「可见性」再判 ②「归属/admin」。他人的用户卷是 owner-only ACL
  （`Mode=Allow` + 单 owner 白名单，`user_volume_store.go:32-47`）⇒ **① 就 404**，
  不会落到 ② 的 403 分支 ⇒ **不泄露「这个卷名存在且属于别人」**。
- ② 用**请求者自己的** store 查（`h.userVolumes.Get(owner, name)`，与 `deleteUserVolumeHandler`
  同源判据 `user_volume_api.go:441`），不做跨 owner 扫描（O(owners) 且会引入新的枚举面）。
- **系统卷/共享卷写必须 admin**：影响所有租户的卷，非 admin 改不了（403 可接受——它本就在
  `GET /api/volumes` 里对该 owner 可见，存在性不是秘密）。
- ① 与 ② 之间不等待 I/O（卷集与 store 均为内存/本地读），无实际 TOCTOU 窗口；写前**从 Set 重新取
  后端实例**，不复用得 ① 的句柄用于构造新状态。
- 审计动作 `volume_settings_update`（与既有 `volume_refresh`/`volume_move`/`user_volume_create`
  同命名风格，`mutating_route_audit.tsv` 既有行见 `:30/46-49/83/93`）。进 `pkg/audit` action 常量 +
  R35-b 表 + 场景测试；记录 `volume` / `section` / `actor` / `result`；
  **不记录规则内容全文**（可能含路径模式，取条数 + sha256 前 8 hex 摘要）。
  **403/404 两条拒绝路径也要记审计**（`result=denied`），否则越权尝试不可见。

### 8.2 响应形状

```jsonc
// GET /api/volumes/vault/detail
{
  "volume": {"name":"vault","type":"secretdata","mode":"","capacity":0,"usage":123,"state":"healthy"},
  "capabilities": ["settings", "block_rules"],
  "settings": [
    {
      "key": "block_policy",
      "label": "分块规则",
      "fields": [
        {"key":"rules","label":"规则","type":"rules","required":false},
        {"key":"max_size_limit","label":"单块上限(字节)","type":"number","required":false}
      ],
      "value": {"version": 3, "rules": [{"path_regex":"^archive/.*\\.zip$","max_size":20971520,"mode":"single","priority":100}]},
      "read_only": false
    }
  ]
}
```

`PUT /api/volumes/vault/settings/block_policy` body：

```jsonc
{"rules":[{"path_regex":"^archive/.*\\.zip$","max_size":"20MiB","mode":"single","priority":100}]}
```

- 成功 `{"success":true,"version":4}`；校验失败 400（带**规则下标**与原因）；CAS 冲突 409；
  卷不存在/无权 404；分区不支持 405。
- 全部 response 走 `internal/httpjson`（R54）；`docs/api.md` 补 3 条 `### ` 标题（R55）。

## 9. UI（v2 卷分区）

`web/static/v2/volumes-console.js` + `index.html`：

1. 卷表每行新增「详情」按钮 → 右侧 drawer（复用既有 `#v2-drawer` 模式）显示卷基础信息 + 分区列表；
   **分区列表由后端 `settings[]` 驱动**（无分区的卷显示「此卷暂无可调配置」），前端不含任何卷型专有分支。
2. `block_policy` 分区渲染为**规则表**：`path_regex` / `min_size` / `max_size` / `mode`(下拉) / `priority` / 删除，
   底部「新增规则」+「保存」；表头显示 `version`（保存成功后更新）。
3. 保存前**前端预校验**（复用后端同一套判据的常量：`HardMaxSingleBytes`、`MaxRules`），
   后端仍二次校验（前端校验只是体验，不是安全边界）。
4. 顶部提示两条约束：① `single` 需填上限、上限 ≤ min(64MiB, 卷块上限)；② 改规则**不影响已存在文件**。
5. 纯函数（规则表增删/排序/校验/序列化/用户可见路径提示）抽成独立可测模块
   `web/static/v2/volume-settings.js`：`node --test` 单测 + 登记 `Makefile web-test`（R10）；
   交互走 Playwright e2e（必检项 `UI E2E Tests`）。

## 10. 安全模型（逐条）

| 威胁 | 处置 |
|---|---|
| 越权改他人卷规则 | §8.1 权限门（用户卷按 owner、系统卷需 admin，跨 owner 404） |
| 用宽松规则（如 `.*`）把整个卷改成 single | `single` 必须给 `MaxSize`；`MaxSize ≤ min(64MiB, 卷块上限)`；超出体积的文件自动不命中 |
| 内存 DoS | 单块整块入内存（F7）⇒ 硬上限 64 MiB + 规则数 ≤64 + 正则长度 ≤512 |
| 正则 DoS | Go `regexp` = RE2，无回溯爆炸；仅防超大模式（长度上限） |
| 规则内容泄露路径结构 | 审计只记 section + 摘要，不记全文 |
| 静默失效（规则不生效但没人知道） | 命中/未命中经 `Options.OnBlockModeResolved` **回调**上报（装配层接到 metrics 计数）；`Erasure`+single 组合装配期告警 |
| 半生效 | 持久化成功即内存生效（顺序：先 `SettingsStore.Save` 成功，再 `SetPolicy`——后者是无失败的 atomic 交换）⇒ 不存在「落盘了但内存没生效」；落盘失败 ⇒ 500 且内存不变 |

## 11. 兼容性与零回归

- 未配置规则的卷：规则快照为 nil ⇒ `blockletPolicyFor` 走原逻辑，**逐字节同现状**。
- 已落盘文件：读路径完全按 `meta` 驱动（F2）⇒ 单块与多块文件共存、互不影响。
- `mode: random` 规则与「不命中」等价（只用于显式给某目录压制更宽的 single 规则）。
- 回滚：删规则 `/ PUT {"rules":[]}` 即回到静态基线/默认；不涉及数据迁移。

## 12. 门禁清单（本 PR 会触及的每一条）

| 门禁 | 动作 |
|---|---|
| R1/R3 分层 | `pkg/cryptox/shardseal/policy` 登记进 `internal/archcheck/layers.go`（Managed + Levels；只依赖 stdlib + sizex ⇒ 与 `cryptox/shardseal` 同层或更低）。`pkg/volume/registry` 新增 `pkg/state` 依赖（L0 ⇒ L2，向下合法） |
| R6 server 作用域 | `pkg/server` 不 import `pkg/volume/secretdata`（只经 `registry.SettingsProvider/Writer/StoreSetter` 断言） |
| R38 路由分类 | `route_classes.tsv` 增 3 行（`GET /api/volumes/{name}/detail`、`GET /api/volumes/{name}/settings` 为 reader；`PUT /api/volumes/{name}/settings/{section}` 为 auth） |
| R35-b 变更路由审计 | `mutating_route_audit.tsv` 增 `PUT /api/volumes/{name}/settings/{section}` → `volume_settings_update` + 场景测试 |
| R54 JSON 响应 | 全走 `internal/httpjson` |
| R55 API 文档 | `docs/api.md` 增 3 节 |
| R52 Deps 字段上限 | 新增依赖进 `deps.go` 既有分组，不往 `Handlers` 加扁平字段 |
| R43 配置默认值 | 新增 `extra.block_rules` 默认空（不登记） |
| R18 测试并发 | 新测试一律 `t.Parallel()`，不登记 `serial_budgets.tsv` |
| R10 web-test | `web/static/v2/volume-settings.js` 登记进 `Makefile web-test` |
| R21/R24 装饰器能力 | 不改 `pkg/sync` 能力接口（单块不引入新能力） |
| 能力矩阵 | `sproxy/CLAUDE.md` 能力矩阵新增一行（分块规则引擎） |
| 文档 | `docs/secret-volume.md` §3 配置表 + `docs/config.md` + `pkg/volume/secretdata/README.md` + `docs/api.md` |

## 13. 测试计划（TDD，先红后绿）

**A. `pkg/cryptox/shardseal/policy`（纯函数，全表驱动）**
1. `Compile`：每条校验分支各一例（含 `single` 缺 `MaxSize`、超硬上限、超卷上限、非法正则、规则数超限）。
2. `Compiled.Resolve`：优先级/声明序/首命中即停；`MinSize`/`MaxSize` 闭区间边界（=边界命中、边界±1 不命中）。
3. 变异命中：故意反转排序比较 ⇒ 用例必红。

**B. `pkg/cryptox/shardseal`（规划器）**
4. `SinglePlanner.Plan`：`size>0` → 1 块且覆盖 `[0,size)`；`size<=0` fail-closed。
5. `BlockPolicy{Mode:"single"}.Planner()` 返回非 nil 两件；未知 mode 仍 nil。
6. **确定性回归**：同输入两次 `EncryptShardsBytes`（收敛档）产物字节 + blob 名 + meta 名全等；
   换 mode 后必不同。

**C. `pkg/volume/secretdata`（接入）**
7. `userVisibleRel`：`alice/user/a/b.txt` → `a/b.txt`；`a/user/b.txt`（无 owner 段）→ 不适用；
   `user/user/x` → `x`；`meta/x` → 不适用。
8. 命中规则 ⇒ meta 只有 1 个 chunk，`block.Mode == "single"`；未命中 ⇒ 与现状逐字段相同（零回归）。
9. 流式 + 内存两条写路径都命中规则（`canStream` 两分支）。
10. `SetPolicy` 热替换后**新写**立即生效（同卷旧文件读不出错）。
11. `Erasure` + single 命中：不产 parity、装配告警一次。
12. `Dedup` 路径不受规则影响（显式锁定既有行为）。

**D. `pkg/server`（API + 权限 + 路由）**
13. 路由解析：`/api/volumes/export`→export、`/api/volumes/user`→用户卷列表、
    `/api/volumes/user/detail`→详情(name=user)、`PUT /api/volumes/user/settings/block_policy`→用户卷分支。
14. `GET /api/volumes/{name}/detail`：不含 settings 能力的卷 → `settings: []`；secretdata 卷 → 1 个分区。
15. 权限矩阵：用户卷本人 200 / **他人用户卷 404（不是 403，防枚举）** / 系统卷非 admin 403 / 系统卷 admin 200；
    `PUT` 拒绝路径（403/404）都要有对应审计断言。
16. `PUT` 校验失败 400（带下标）、CAS 冲突 409、未知分区 405、卷不存在 404。
17. 审计：`volume_settings_update` 在成功与 403 两条路径都发出，且不含规则全文。
18. StateStore 读失败 ⇒ 静态基线生效 + 告警（不阻断）。

**E. 前端**
19. `volume-settings.js` 纯函数 `node --test`（增删/排序/校验/序列化/上限提示）。
20. Playwright：详情抽屉打开 → 规则表渲染 → 新增规则 → 保存 → version 递增。

## 14. 验收标准

- 未配置规则的卷：现有全量测试全绿（零回归）。
- 配置 `^archive/.*\.zip$` + 20 MiB + `single`：20 MiB 以下的 zip 落盘为 **1 个 blob**，
  meta `chunks` 长度 = 1，`block.mode = "single"`；21 MiB 的 zip 仍是多块。
- 同内容同规则两次写入 **逐字节相同**（blob 名/字节/meta 名全等）。
- v2 卷详情能列出规则、能增删改排序、保存后立即对新写生效、重启后仍生效。
- 越权：他人用户卷 404、系统卷非 admin 403（均有测试）。
- `make lint-all` / `make web-test` / `go test ./internal/archcheck/` / `make check-ci` 全绿。

## 15. 未决 / 明确不做

- **不做**段级 `single`（用户裁定）。
- **不做**「改规则后自动重写已存在文件」（需要重构加密产物，属独立能力；本期只保证新写生效）。
- **不做**按 owner 维度的规则（规则是卷维度；owner 差异由路径正则表达，如 `^alice/`）。
- **不做**规则命中统计的历史持久化（只在进程内计数 + 指标）。
- `PhysBound` 不带 rel（F8）⇒ 上界仍按卷级参数；已用「`MaxSize ≤ 卷块上限`」校验消解低估风险，
  不改 `PhysBound` 签名（会牵动配额层）。
