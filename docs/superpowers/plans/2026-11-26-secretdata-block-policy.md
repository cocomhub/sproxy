<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# secretdata 分块规则引擎 实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 让 secretdata 卷支持「整文件 = 1 个块」（`mode: single`），按路径正则 + 体积上限自动命中，规则可在 v2 卷详情动态调整并持久化。

**架构：** 新增纯函数能力包 `pkg/cryptox/shardseal/policy`（规则→已编译快照）；`pkg/cryptox/shardseal` 新增 `SinglePlanner`；`pkg/volume/registry` 新增通用「卷设置分区」可选能力（Provider/Writer/Store/StoreSetter）+ 一份 StateStore 持久化实现；`pkg/volume/secretdata` 在写路径选型处接入规则；`pkg/server` 提供详情/设置 HTTP 面；`web/static/v2` 提供详情抽屉 + 规则表。

**技术栈：** Go 1.27（标准库 `regexp`/`encoding/json`/`sync/atomic`）、`pkg/units/sizex`、`pkg/state`、`internal/httpjson`、原生 JS（`node --test` + Playwright）。

**规格：** `docs/designs/2026-11-26-secretdata-block-policy-rules.md`

## 全局约束

- 所有 Go 源文件带 SPDX 头（`make fmt` 自动补，提交前跑一次）。
- 字节大小字段一律 `pkg/units/sizex.ByteSize`，禁止裸 `int64`（硬规则 19）。
- 新增依赖只能是标准库 / `golang.org/x/*` / 既有内部包（sproxy 依赖策略）。
- 新测试一律 `t.Parallel()`（R18）；HTTP 测试必须绑 `127.0.0.1`。
- 所有 JSON 响应必须经 `internal/httpjson`（R54）。
- 禁止 `git commit --no-verify` / `git push --no-verify`。
- 测试与文档禁止真实数据（硬规则 21b）：示例卷名用 `vault`、路径用 `archive/`、`photos/`。
- 每个任务完成后单独 commit；提交信息用中文、多重 `-m`、不加署名行。

## 审查重点（Review Focus）

| # | 输入/条件 | 期待行为 | 钉住它的测试 |
|---|---|---|---|
| 1 | 卷**未配置**任何规则时写入文件 | 产物与改动前**逐字节相同**（blob 名 / 字节 / meta 名） | 任务 4 步骤 1 `TestBlockletPolicyFor_NoRules_Unchanged` |
| 2 | 规则命中但文件大小为 0 | 与既有 `RandomPlanner` 一致的 fail-closed（不是静默写半态） | 任务 2 步骤 1 `TestSinglePlanner_EmptyFailsClosed` |
| 3 | 文件大小恰好等于 `MaxSize` / 超 1 字节 | 等于 ⇒ 命中；超 1 字节 ⇒ 不命中（走卷默认） | 任务 1 步骤 1 `TestCompiled_Resolve_SizeBoundary` + 任务 4 `TestWrite_SingleAtBoundary` |
| 4 | 他人用户卷 / 系统卷非 admin 发起 `PUT settings` | 前者 **404**（防枚举，不泄露存在性），后者 403 | 任务 6 步骤 1 `TestVolumeSettings_PermissionMatrix` |
| 5 | 两个管理员并发 `PUT` 同一卷规则 | 一个成功 200、另一个 **409**（CAS），先写者不被覆盖 | 任务 5 步骤 1 `TestSettingsStore_CASConflict` + 任务 6 `TestVolumeSettings_PutCASConflict` |

---

## 文件结构（先锁定分解决策）

**创建**
| 路径 | 职责 |
|---|---|
| `pkg/cryptox/shardseal/policy/policy.go` | 规则类型 / 常量 / `Compile` / `Compiled.Resolve` / `userVisibleRel` 不在此（属 secretdata） |
| `pkg/cryptox/shardseal/policy/policy_test.go` | 编译校验 + 匹配语义（表驱动） |
| `pkg/cryptox/shardseal/single_planner_test.go` | `SinglePlanner` + `Planner()` 装配 + 确定性 |
| `pkg/volume/registry/settings.go` | 设置分区能力类型与接口 + 哨兵 |
| `pkg/volume/registry/settings_state.go` | `NewStateSettingsStore`（唯一持久化实现） |
| `pkg/volume/registry/settings_state_test.go` | CAS / 版本 / 不存在键 |
| `pkg/volume/secretdata/block_policy.go` | `userVisibleRel` + 规则接入 `blockletPolicyFor` + `SetPolicy` |
| `pkg/volume/secretdata/block_policy_test.go` | 剥离规则 + 命中/未命中 + 两写路径 |
| `pkg/volume/secretdata/backend_settings.go` | `backend` 实现 `Settings` / `UpdateSettings` / `SetSettingsStore` |
| `pkg/volume/secretdata/backend_settings_test.go` | 分区回显、校验拒绝、热生效、注入失败容错 |
| `pkg/server/volume_settings_api.go` | 详情/设置 handler + 权限门 + 审计 |
| `pkg/server/volume_settings_api_test.go` | 路由解析 + 权限矩阵 + 状态码 + 审计 |
| `web/static/v2/volume-settings.js` | 规则表纯函数（增删/排序/校验/序列化） |
| `web/static/v2/volume-settings.test.js` | `node --test` |
| `web/e2e/volume-settings.spec.js` | 详情抽屉 → 规则表 → 保存 |
| `docs/superpowers/plans/2026-11-26-secretdata-block-policy.md` | 本计划 |

**修改**
| 路径 | 改动 |
|---|---|
| `pkg/cryptox/shardseal/block.go` | 新增 `BlockModeRandom` / `BlockModeSingle` 常量与 `SinglePlanner` |
| `pkg/cryptox/shardseal/shardseal.go` | `blockPlanner()` 增加 `single` 分支 |
| `pkg/volume/secretdata/secretdata.go` | `Options` 加 `Policy` / `OnBlockModeResolved`；`SecretdataFS` 加 `policy` 原子指针；两处 `blockletPolicyFor` 调用加 `size` |
| `pkg/volume/secretdata/helpers.go` | `blockletPolicyFor` 签名加 `size`，前缀规则选型 |
| `cmd/sproxy/secret_register.go` | 解析 `extra.block_rules` → `policy.Compile` → `Options.Policy`；装配 `OnBlockModeResolved` |
| `pkg/server/routes_files.go` | 注册 3 条新路由 |
| `internal/archcheck/route_classes.tsv` | 增 3 行分类 |
| `internal/archcheck/mutating_route_audit.tsv` | 增 1 行 `action=volume_settings_update` |
| `internal/archcheck/layers.go` | 登记 `pkg/cryptox/shardseal`(0) 与 `.../policy`(1) 到 Levels，`policy` 入 Managed |
| `pkg/server/routes_setup.go`（或 `cmd/sproxy/root.go`） | `setupStateStore` 后调 `server.AttachVolumeSettingsStores(h)` |
| `web/static/v2/volumes-console.js` | 每行「详情」按钮 + 抽屉渲染 |
| `web/static/v2/index.html` | 详情抽屉 DOM + `<script>` 引入 |
| `Makefile` | `web-test` 增 `volume-settings.js` 的 `node --check` + `node --test` |
| `docs/api.md` | 3 节 API 文档 |
| `docs/secret-volume.md` / `docs/config.md` / `pkg/volume/secretdata/README.md` | `extra.block_rules` 与规则语义 |
| `CLAUDE.md`（sproxy） | 能力矩阵新增一行 |

---

### 任务 1：`policy` 能力包（规则编译与匹配）

**文件：**
- 创建：`pkg/cryptox/shardseal/policy/policy.go`
- 测试：`pkg/cryptox/shardseal/policy/policy_test.go`

**Interfaces（后续任务依赖）**
```go
type Rule struct {
    PathRegex string            `json:"path_regex"`
    MinSize   sizex.ByteSize    `json:"min_size,omitempty"`
    MaxSize   sizex.ByteSize    `json:"max_size,omitempty"`
    Mode      string            `json:"mode"`
    Priority  int               `json:"priority,omitempty"`
    Note      string            `json:"note,omitempty"`
}
type Policy struct {
    Rules   []Rule `json:"rules"`
    Version int64  `json:"version"`
}
type Compiled struct{ /* 未导出 */ }
func Compile(p Policy, volumeBlockMax int64) (*Compiled, error)
func (c *Compiled) Resolve(userRel string, size int64) (Rule, bool)
func (c *Compiled) Rules() []Rule
func (c *Compiled) Version() int64
const HardMaxSingleBytes int64 = 64 << 20
const MaxRules = 64
const MaxRegexLen = 512
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestCompile_Validation(t *testing.T) {
	t.Parallel()
	ok20 := sizex.ByteSize(20 << 20)
	cases := []struct{ name string; rules []policy.Rule; volMax int64; wantErr string }{
		{"valid single", []policy.Rule{{PathRegex: `^archive/.*\.zip$`, MaxSize: ok20, Mode: "single"}}, 0, ""},
		{"valid random", []policy.Rule{{PathRegex: `\.jpg$`, Mode: "random"}}, 0, ""},
		{"single without max", []policy.Rule{{PathRegex: `x`, Mode: "single"}}, 0, "需给 max_size"},
		{"single over hard cap", []policy.Rule{{PathRegex: `x`, MaxSize: sizex.ByteSize(64<<20 + 1), Mode: "single"}}, 0, "硬上限"},
		{"single over volume max", []policy.Rule{{PathRegex: `x`, MaxSize: sizex.ByteSize(20 << 20), Mode: "single"}}, 1 << 20, "卷块上限"},
		{"bad regex", []policy.Rule{{PathRegex: `(`, Mode: "random"}}, 0, "规则 0"},
		{"unknown mode", []policy.Rule{{PathRegex: `x`, Mode: "legend"}}, 0, "规则 0"},
		{"min>max", []policy.Rule{{PathRegex: `x`, MinSize: ok20, MaxSize: sizex.ByteSize(1), Mode: "random"}}, 0, "规则 0"},
		{"too many rules", make([]policy.Rule, policy.MaxRules+1), 0, "条数"},
	}
	for _, c := range cases { /* compile and assert error contains/empty */ }
}

func TestCompiled_Resolve_SizeBoundary(t *testing.T) {
	t.Parallel()
	c, err := policy.Compile(policy.Policy{Rules: []policy.Rule{
		{PathRegex: `^archive/.*`, MaxSize: sizex.ByteSize(20 << 20), Mode: "single", Priority: 100},
	}}, 0)
	// size == 20MiB 命中；size == 20MiB+1 不命中；19MiB 命中
}

func TestCompiled_Resolve_PriorityAndOrder(t *testing.T) {
	t.Parallel()
	// 高 priority 先命中；同 priority 按声明序；首命中即停（低优先级更宽的规则不生效）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race ./pkg/cryptox/shardseal/policy/...`
预期：FAIL —— 包内无 `Compile` 等符号（编译错误）。

- [ ] **步骤 3：实现 `pkg/cryptox/shardseal/policy/policy.go`**

要点（签名与取值已定死，函数体自行实现）：
- `Rule`/`Policy` 字段与 JSON tag 完全按上面 Interfaces；`MinSize`/`MaxSize` 用 `sizex.ByteSize`。
- `Compile`：条数 → 逐条（`PathRegex` 非空 / 长度 / 编译 `/^(?:` + p + `)$/` 整串匹配 / `Mode` ∈ {`shardseal.BlockModeSingle`, `shardseal.BlockModeRandom`} / `single` 必须有 `MaxSize` / `MaxSize ≤ HardMaxSingleBytes` / `volumeBlockMax > 0 && MaxSize > volumeBlockMax` 拒 / `MinSize > MaxSize` 拒），错误用 `fmt.Errorf("policy: 规则 %d …: %w", i, err)`，最后 `sort.SliceStable` 按 `Priority` 降序。
- `Resolve`：顺序遍历已编译规则，`size < 0` 时忽略体积约束；命中返回 `Rule` 副本与 `true`。
- `Rules()` 返回切片副本；`Version()` 返回编译时的 `p.Version`。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/cryptox/shardseal/policy/...`
预期：PASS。

- [ ] **步骤 5：变异命中验证**

把 `sort.SliceStable` 的比较由 `>` 改成 `<`，重跑 `go test -run 'TestCompiled_Resolve_PriorityAndOrder' ./pkg/cryptox/shardseal/policy/`，**预期 FAIL**；改回后 PASS。

- [ ] **步骤 6：Commit**

```bash
git add pkg/cryptox/shardseal/policy/
git commit -m "feat(secretdata): 分块规则能力包（路径正则 + 体积区间 + 优先级）" \
  -m "新增 pkg/cryptox/shardseal/policy：Rule/Policy 为用户面 JSON 形状，Compiled 为
预编译快照（正则只编译一次，写路径只做 MatchString）。

校验与装配共用一份 Compile：条数/正则/mode/体积上限（single 必须有 max_size 且
≤ min(64MiB, 卷 block_policy.max)）全部 fail-closed，错误带规则下标。" \
  -m "验证：go test -race ./pkg/cryptox/shardseal/policy/ 全绿；排序变异必红。"
```

---

### 任务 2：`SinglePlanner`（整文件单块）

**文件：**
- 修改：`pkg/cryptox/shardseal/block.go`（常量与规划器）、`pkg/cryptox/shardseal/shardseal.go:70`（`blockPlanner` 分支）
- 测试：`pkg/cryptox/shardseal/single_planner_test.go`

**Interfaces（后续任务依赖）**
```go
const (
    BlockModeRandom = "random"
    BlockModeSingle = "single"
)
type SinglePlanner struct{}
func (SinglePlanner) Plan(size int64) ([]Block, error)
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestSinglePlanner_Plan(t *testing.T) {
	t.Parallel()
	blocks, err := shardseal.SinglePlanner{}.Plan(1 << 20)
	// 断言：err==nil、len(blocks)==1、Offset==0、Size==1<<20
}

func TestSinglePlanner_EmptyFailsClosed(t *testing.T) {
	t.Parallel()
	for _, size := range []int64{0, -1} {
		if _, err := (shardseal.SinglePlanner{}).Plan(size); err == nil {
			t.Fatalf("size=%d 应 fail-closed", size)
		}
	}
}

func TestBlockPolicy_SingleMode_Planner(t *testing.T) {
	t.Parallel()
	bp, lp := shardseal.BlockPolicy{Mode: shardseal.BlockModeSingle}.Planner()
	// 断言 bp 与 lp 均非 nil；未知 mode 仍为 nil（零回归）
}

func TestSingleMode_DeterministicParity(t *testing.T) {
	t.Parallel()
	// 收敛档（AlgoV2GCM）下，同一数据两次 EncryptShardsBytes（policy.Mode=single）
	// ⇒ blob 名相同、blob 字节相同、meta 名相同；把 mode 改成 random 后必不同。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run 'TestSingle|TestBlockPolicy_Single' ./pkg/cryptox/shardseal/`
预期：FAIL（`SinglePlanner` 未定义）。

- [ ] **步骤 3：实现**

`block.go` 增加两个常量与 `SinglePlanner`（`Plan` 校验 `size <= 0` 返回错误，文案与 `RandomPlanner` 同族「空文件不可分块（size=%d）」；否则返回 `[]Block{{Offset: 0, Size: size}}`）。
`shardseal.go` 的 `blockPlanner()` 增加 `case BlockModeSingle: return SinglePlanner{}`；`BlockModeRandom` 与既有 `"random"`/`""` 分支同值（直接复用它，不新增分支）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/cryptox/shardseal/`
预期：PASS（含既有全部用例，零回归）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/cryptox/shardseal/block.go pkg/cryptox/shardseal/shardseal.go pkg/cryptox/shardseal/single_planner_test.go
git commit -m "feat(secretdata): 新增 single 块模式（整文件 1 块）" \
  -m "RandomPlanner 对小文件无收益却产生多个底层 blob；新增 SinglePlanner 把整文件规划为
单块。纯函数、无随机源 ⇒ 收敛档下同内容同策略产出逐字节相同的 blob 与名（测试锁定）。

读路径零改动：单块是既有磁盘格式的合法实例（1 个 ChunkInfo、blocklet 索引仍完整
覆盖 [0,size)）。空文件沿用既有 fail-closed 语义。" \
  -m "验证：go test -race ./pkg/cryptox/shardseal/ 全绿。"
```

---

### 任务 3：registry 卷设置能力 + StateStore 持久化

**文件：**
- 创建：`pkg/volume/registry/settings.go`、`pkg/volume/registry/settings_state.go`
- 测试：`pkg/volume/registry/settings_state_test.go`
- 修改：`internal/archcheck/layers.go`（`pkg/cryptox/shardseal` 0、`.../policy` 1，`policy` 入 `Managed`）

**Interfaces（后续任务依赖）**
```go
type VolumeSettings struct {
    Key      string          `json:"key"`
    Label    string          `json:"label"`
    Fields   []FieldSchema   `json:"fields"`
    Value    json.RawMessage `json:"value,omitempty"`
    ReadOnly bool            `json:"read_only,omitempty"`
}
type SettingsProvider interface {
    Settings(ctx context.Context) ([]VolumeSettings, error)
}
type SettingsWriter interface {
    UpdateSettings(ctx context.Context, section string, payload json.RawMessage) error
}
type SettingsStore interface {
    Load(ctx context.Context, volume string) (json.RawMessage, int64, error)
    Save(ctx context.Context, volume string, payload json.RawMessage, expectedVersion int64) (int64, error)
    Delete(ctx context.Context, volume string) error
}
type SettingsStoreSetter interface {
    SetSettingsStore(store SettingsStore) error
}
var ErrSettingsVersionConflict = errors.New("registry: 卷设置版本冲突")
var ErrSettingsSectionUnsupported = errors.New("registry: 卷设置分区不支持")
func NewStateSettingsStore(st state.StateStore) SettingsStore
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestSettingsStore_CASConflict(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := registry.NewStateSettingsStore(state.NewLocalStateStore(t.TempDir(), nil))
	v1, err := st.Save(ctx, "vault", []byte(`{"rules":[]}`), 0)   // 断言 v1==1
	v2, err := st.Save(ctx, "vault", []byte(`{"rules":[1]}`), v1) // 断言 v2==2
	_, err = st.Save(ctx, "vault", []byte(`{"rules":[2]}`), v1)   // 断言 errors.Is(err, registry.ErrSettingsVersionConflict)
}

func TestSettingsStore_LoadMissing(t *testing.T) {
	t.Parallel()
	// 无键 ⇒ (nil, 0, nil)，不是错误
}

func TestSettingsStore_DeleteIdempotent(t *testing.T) {
	t.Parallel()
	// 删不存在的键不报错；删后再 Load 回到 (nil,0,nil)
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run TestSettingsStore ./pkg/volume/registry/`
预期：FAIL（`NewStateSettingsStore` 未定义）。

- [ ] **步骤 3：实现**

`settings.go`：上面全部类型与哨兵（`VolumeSettings` 复用既有 `FieldSchema`）。
`settings_state.go`：`stateSettingsStore`，键 `volume_policy/<卷>`，值 `{"version":N,"value":<raw>}`；`Save` 用 `state.StateStore.CAS`（old 为 `version==expectedVersion` 的现存字节，expectedVersion==0 时 old=nil 走 create-only），CAS 失败映射 `ErrSettingsVersionConflict`；`Load` 把 `state.ErrKeyNotFound` 映射为 `(nil,0,nil)`。
`layers.go`：`Levels` 增 `github.com/cocomhub/sproxy/pkg/cryptox/shardseal: 0`（实测只依赖 `pkg/plugin`(0)）与 `github.com/cocomhub/sproxy/pkg/cryptox/shardseal/policy: 1`（依赖 shardseal + `pkg/units/sizex`(0)）；`Managed` 增 `.../policy`。**注意**：`TestManaged_PackagesExistInGraph` 要求 Managed 里的每个包都以**精确的导入路径**出现在 `go list ./...` 的图里——新建空目录不会出现，必须等 `policy.go` 落盘后再跑门禁。`pkg/volume/registry` 新增 `pkg/state` 依赖方向合法（registry=L2、state=L0）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/volume/registry/ && go test -count=1 ./internal/archcheck/ -run 'TestR1|TestR3'`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/registry/settings.go pkg/volume/registry/settings_state.go pkg/volume/registry/settings_state_test.go internal/archcheck/layers.go
git commit -m "feat(volume): 卷设置分区能力与 StateStore 持久化（唯一实现）" \
  -m "后续各卷型（secretdata 为首个）需要在卷详情里暴露可读可写的配置分区。提供通用能力：
SettingsProvider/SettingsWriter（读写一个分区）+ SettingsStore（持久化，CAS 版本）
+ SettingsStoreSetter（StateStore 就绪后注入）。

持久化实现只有一份（stateSettingsStore，键 volume_policy/<卷>），用 state.StateStore.CAS
保证并发不覆盖。" \
  -m "验证：go test -race ./pkg/volume/registry/；archcheck R1/R3 通过。"
```

---

### 任务 4：secretdata 写路径接入规则

**文件：**
- 创建：`pkg/volume/secretdata/block_policy.go`
- 测试：`pkg/volume/secretdata/block_policy_test.go`
- 修改：`pkg/volume/secretdata/helpers.go:38`（签名 + 前缀规则）、`pkg/volume/secretdata/secretdata.go`（`Options` 字段、`policy` 原子指针、两处调用点 `:1597`/`:2092` 传 `wc.size`）

**Interfaces（后续任务依赖）**
```go
// Options 新增
Policy              *policy.Compiled
OnBlockModeResolved func(rel, mode string, matched bool)

// SecretdataFS 新增
func (s *SecretdataFS) SetPolicy(c *policy.Compiled)

// 包内（测试可见）
func userVisibleRel(fsKey string) (rel string, ok bool)
func (s *SecretdataFS) blockletPolicyFor(rel string, size int64) (shardseal.BlockPolicy, error)
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestUserVisibleRel(t *testing.T) {
	t.Parallel()
	cases := []struct{ in string; wantRel string; wantOK bool }{
		{"alice/user/a/b.txt", "a/b.txt", true},
		{"user/user/x", "x", true},
		{"a/user/b.txt", "", false},      // 第二段不是 user ⇒ 不是用户面键
		{"meta/x", "", false},
		{"alice/user/", "", false},       // 空文件名
		{"alice/cloud/x", "", false},
	}
	// 逐个断言
}

func TestBlockletPolicyFor_NoRules_Unchanged(t *testing.T) {
	t.Parallel()
	// 卷无规则：blockletPolicyFor 返回与构造时 s.opts.Block 等值（逐字段比较），
	// 且 Mode 仍为 "random"（或构造值），BlockletMode 走既有扩展名选型。
}

func TestWrite_SingleAtBoundary(t *testing.T) {
	t.Parallel()
	// 规则 {PathRegex:`^archive/.*`, MaxSize:20MiB, Mode:"single"}（卷 block_policy.max=20MiB）
	// 写 archive/a.bin == 20MiB ⇒ meta.Chunks 长度 1 且 meta.Block.Mode == "single"
	// 写 archive/b.bin == 20MiB+1 ⇒ meta.Chunks 长度 > 1 且 meta.Block.Mode == "random"
}

func TestWrite_SingleBothPaths(t *testing.T) {
	t.Parallel()
	// 同一规则、同一内容：一次用可 seek 源（走 writeFileStream），一次用非 seek 源
	// （走 writeFileEncrypted），两者 meta.Chunks 长度均 == 1。
}

func TestSetPolicy_HotSwapAppliesToNewWrites(t *testing.T) {
	t.Parallel()
	// 先写无命中的文件（多块）；SetPolicy 命中后写同名新内容 ⇒ 单块；
	// 旧文件仍可完整读出（读路径不依赖规则）。
}

func TestWrite_SingleWithErasure_NoParity(t *testing.T) {
	t.Parallel()
	// Erasure=true 的卷命中 single 规则 ⇒ meta.Parity 为空、写成功。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run 'TestUserVisibleRel|TestBlockletPolicyFor_NoRules|TestWrite_Single|TestSetPolicy_HotSwap' ./pkg/volume/secretdata/`
预期：FAIL（`userVisibleRel` / `SetPolicy` 未定义；`blockletPolicyFor` 签名不匹配）。

- [ ] **步骤 3：实现**

`block_policy.go`：
- `userVisibleRel`：按 `strings.Split(fsKey, "/")` 形状判据（`len < 3 || parts[1] != "user"` ⇒ false；尾部空段 ⇒ false），返回 `strings.Join(parts[2:], "/")`。
- `SetPolicy`：`s.policy.Store(c)`（`atomic.Pointer[policy.Compiled]`）。
- `resolveRulePolicy(base shardseal.BlockPolicy, rel string, size int64) (shardseal.BlockPolicy, bool)`：取快照 → `userVisibleRel` → `Resolve`；命中时 `base.Mode` 按规则设为 `BlockModeSingle`（并 `base.Max = max(size,1)`、`base.Min = 1`）或 `BlockModeRandom`（保留卷 `Min/Max`）；返回是否命中。

`helpers.go`：`blockletPolicyFor(rel string, size int64)`，先调 `resolveRulePolicy` 得到候选 base，其余逻辑（显式 `blocklet_mode` 优先 / 扩展名自动选型 / fixed 归一）原样保留；结束时若有 `OnBlockModeResolved` 回调则调用一次（`mode` 取最终 `policy.Mode`，`matched` 取是否规则命中；回调 nil 安全）。

`secretdata.go`：`Options` 增两字段 + 文档注释；`SecretdataFS` 增 `policy atomic.Pointer[policy.Compiled]`，`NewFS` 处 `if opts.Policy != nil { fs.policy.Store(opts.Policy) }`；两处 `blockletPolicyFor(wc.rel)` 改为 `blockletPolicyFor(wc.rel, wc.size)`。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/volume/secretdata/`
预期：PASS（既有全量用例零回归，含 `deterministic_parity_test.go`）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/secretdata/block_policy.go pkg/volume/secretdata/block_policy_test.go pkg/volume/secretdata/helpers.go pkg/volume/secretdata/secretdata.go
git commit -m "feat(secretdata): 写路径按规则命中 single 分块" \
  -m "blockletPolicyFor 增加 size 参数并前置规则选型：规则只覆盖块级（Mode/Max），块内
blocklet 仍按扩展名自动选型或显式配置。未命中规则时逻辑与产物逐字节不变（测试锁定）。

SecretdataFS 用 atomic.Pointer 持有已编译规则快照，SetPolicy 支持热替换（新写立即生效，
旧文件读路径不依赖规则）。命中/未命中经 Options.OnBlockModeResolved 上报（nil 安全）。" \
  -m "验证：go test -race ./pkg/volume/secretdata/ 全绿。"
```

---

### 任务 5：secretdata backend 设置分区实现

**文件：**
- 创建：`pkg/volume/secretdata/backend_settings.go`
- 测试：`pkg/volume/secretdata/backend_settings_test.go`

**Interfaces（后续任务依赖）**
```go
// backend（同时实现三者，编译期断言）
var _ registry.SettingsProvider    = (*backend)(nil)
var _ registry.SettingsWriter      = (*backend)(nil)
var _ registry.SettingsStoreSetter = (*backend)(nil)
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestBackend_Settings_ReportsBlockPolicySection(t *testing.T) {
	t.Parallel()
	// Settings(ctx) 返回恰好 1 个分区，Key=="block_policy"，Fields 非空，
	// Value 可 json.Unmarshal 成 policy.Policy 且 Version 与 SetPolicy 时一致。
}

func TestBackend_UpdateSettings_RejectsInvalid(t *testing.T) {
	t.Parallel()
	// {"rules":[{"path_regex":"x","mode":"single"}]}（缺 max_size）⇒ 错误含「需给 max_size」，
	// 且内存策略未变（Settings 回显仍是旧值）。
}

func TestBackend_UpdateSettings_UnknownSection(t *testing.T) {
	t.Parallel()
	// section="other" ⇒ errors.Is(err, registry.ErrSettingsSectionUnsupported)
}

func TestBackend_UpdateSettings_PersistsAndApplies(t *testing.T) {
	t.Parallel()
	// 注入内存 SettingsStore 替身：Save 被调用 1 次（payload 版本递增），
	// 之后 SetPolicy 生效（写一个命中文件 ⇒ 单块）。
}

func TestBackend_SetSettingsStore_LoadFailureKeepsBaseline(t *testing.T) {
	t.Parallel()
	// store.Load 返回错误 ⇒ 不返回错误、保留静态基线（Settings 回显静态基线）。
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run 'TestBackend_Settings|TestBackend_UpdateSettings|TestBackend_SetSettingsStore' ./pkg/volume/secretdata/`
预期：FAIL（方法未实现）。

- [ ] **步骤 3：实现**

`backend_settings.go`：
- `backend` 增字段 `store atomic.Pointer[registry.SettingsStore]`（接口指针安全：存的是接口值的指针）。
- `Settings(ctx)`：读 `b.fs.policy` 快照 → `policy.Policy{Rules: c.Rules(), Version: c.Version()}` 序列化为 `Value`；`Fields` 声明两个：`{Key:"rules", Label:"规则", Type:"rules"}` 与 `{Key:"max_size_limit", Type:"number", Label:"单块上限(字节)"}`（后者仅提示，值由前端从后端常量取）；`ReadOnly: b.store.Load() == nil`。
- `UpdateSettings(ctx, section, payload)`：`section != "block_policy"` ⇒ `ErrSettingsSectionUnsupported`；解析 `policy.Policy`；`policy.Compile(p, volumeBlockMax)`（`volumeBlockMax` 取 `b.fs.opts.Block.Max`）；`store.Save(ctx, b.fs.opts.VolumeName, payload, currentVersion)`；成功后 `b.fs.SetPolicy(c)` 并回填新 `Version`。
- `SetSettingsStore(store)`：保存并按卷 `Load`；`Load` 出错或 `Compile` 失败 ⇒ `slog.Warn` + 保留当前策略，**返回 nil**（不阻断装配）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/volume/secretdata/`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/volume/secretdata/backend_settings.go pkg/volume/secretdata/backend_settings_test.go
git commit -m "feat(secretdata): 后端提供 block_policy 设置分区（读/写/持久化注入）" \
  -m "实现 registry 的三个可选能力：Settings 回显当前规则与字段 schema；UpdateSettings 校验
后 CAS 落盘再热生效（只接受 block_policy，其它分区 405）；SetSettingsStore 在 StateStore
就绪后注入并载入一次——读失败或存量规则编译失败只告警、保留静态基线，不阻断挂载。" \
  -m "验证：go test -race ./pkg/volume/secretdata/ 全绿。"
```

---

### 任务 6：HTTP 面（详情 / 设置）+ 权限 + 审计 + 门禁表

**文件：**
- 创建：`pkg/server/volume_settings_api.go`、`pkg/server/volume_settings_api_test.go`
- 修改：`pkg/server/routes_files.go`（3 条路由）、`internal/archcheck/route_classes.tsv`、`internal/archcheck/mutating_route_audit.tsv`、`docs/api.md`、`cmd/sproxy/root.go`（`setupStateStore` 后 `AttachVolumeSettingsStores`）

**Interfaces（后续任务依赖）**
```go
// pkg/server
func AttachVolumeSettingsStores(h *Handlers)      // 装配期调用：给实现 SetSettingsStore 的后端注入
func (h *Handlers) volumeDetailHandler(w, r)
func (h *Handlers) volumeSettingsGetHandler(w, r)
func (h *Handlers) volumeSettingsPutHandler(w, r)
// 审计动作常量
const auditActionVolumeSettingsUpdate = "volume_settings_update"
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestRouteResolution_VolumeDetailNotShadowed(t *testing.T) {
	t.Parallel()
	// 用 newTestServerWithAllRoutes 起服务后断言：
	//  GET /api/volumes/export → export handler（非 detail，name 不取 "export"）
	//  GET /api/volumes/user   → 用户卷列表
	//  GET /api/volumes/user/detail → detail(name=user)
	//  PUT /api/volumes/user/settings/block_policy → 走用户卷分支
}

func TestVolumeSettings_PermissionMatrix(t *testing.T) {
	t.Parallel()
	// 用户卷（owner=alice）：
	//   alice → 200；bob → 404
	// 系统卷（config 声明）：
	//   reader/user → 403；admin → 200
	// 读：非可见卷 → 404
}

func TestVolumeSettings_PutValidationAndStatus(t *testing.T) {
	t.Parallel()
	// 非法规则 → 400 且 body 含规则下标；未知分区 → 405；卷不存在 → 404
}

func TestVolumeSettings_PutCASConflict(t *testing.T) {
	t.Parallel()
	// store 预置 version=2；请求携带 expected_version=1 ⇒ 409
}

func TestVolumeSettings_AuditOnBothOutcomes(t *testing.T) {
	t.Parallel()
	// 成功与 403 都产生 action=volume_settings_update，且 Detail 不含规则全文
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run 'TestRouteResolution_VolumeDetail|TestVolumeSettings_' ./pkg/server/`
预期：FAIL（handler 未注册、常量未定义）。

- [ ] **步骤 3：实现**

`volume_settings_api.go`：
- `volumeAccess(w, r, name) (vol volume.Volume, be registry.ExternalBackend, ok bool)`：按规格 §8.1 的三步门（可见性 → 归属/admin）。非可见 ⇒ 404；用户卷非本人 ⇒ 404；系统卷非 admin ⇒ 403。拒绝路径也 `RecordAudit`（`result=denied`）。
- `volumeDetailHandler`：`volume` 基础字段 + `capabilities`（`settings` / `block_rules`）+ `settings`（从 `SettingsProvider` 取；未实现 ⇒ `[]`）。
- `volumeSettingsGetHandler`：只返回 `settings`。
- `volumeSettingsPutHandler`：读 body（`http.MaxBytesReader` 1 MiB）→ 后端断言 `SettingsWriter`（未实现 ⇒ 405）→ `UpdateSettings`；错误映射：校验类 400 / `ErrSettingsVersionConflict` 409 / `ErrSettingsSectionUnsupported` 405 / 其它 500。成功 `{"success":true,"version":N}`。全部经 `httpjson.Send`/`SendErr`。
- `AttachVolumeSettingsStores(h)`：遍历 `h.volSet.All()`，`h.volSet.External(v.Name)` 断言 `SetSettingsStore` 则注入 `registry.NewStateSettingsStore(h.stateStore)`（`h.stateStore == nil` 时传 nil store 并跳过）。

`routes_files.go`：注册三条（`ClassReader`/`ClassReader`/`ClassAuth` + `Mutating: true`）。
`route_classes.tsv` / `mutating_route_audit.tsv`：按格式补行。
`docs/api.md`：三节 `### GET /api/volumes/{name}/detail` 等，含请求/响应示例与权限说明。
`root.go`：`setupStateStore` 成功返回前调用 `server.AttachVolumeSettingsStores(h)`。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./pkg/server/ -run 'TestRouteResolution|TestVolumeSettings' && go test -count=1 ./internal/archcheck/`
预期：PASS（含 R38 / R35-b / R54 / R55）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/volume_settings_api.go pkg/server/volume_settings_api_test.go pkg/server/routes_files.go cmd/sproxy/root.go internal/archcheck/route_classes.tsv internal/archcheck/mutating_route_audit.tsv docs/api.md
git commit -m "feat(api): 卷详情与设置分区读写（含权限门与审计）" \
  -m "新增 GET /api/volumes/{name}/detail、GET .../settings、PUT .../settings/{section}。
详情挂 /{name}/detail 而非 /{name}：卷名可合法取 export/user 等值，与既有两段字面路由
在三段形态下结构性无冲突（测试锁定 /api/volumes/export 仍命中 export）。

权限门按序：可见性（他人私有卷 404 防枚举）→ 归属或 admin（系统卷非 admin 403）。
拒绝路径同样记审计。响应一律经 internal/httpjson。" \
  -m "验证：go test -race ./pkg/server/ -run 'TestRouteResolution|TestVolumeSettings'；archcheck 全绿。"
```

---

### 任务 7：v2 前端纯函数模块

**文件：**
- 创建：`web/static/v2/volume-settings.js`、`web/static/v2/volume-settings.test.js`
- 修改：`Makefile`（`web-test`）

**Interfaces（后续任务依赖）**
```js
window.v2volumeSettings = {
  parseSize(text),            // "20MiB" → 字节；非法 → null
  formatSize(bytes),          // 字节 → "20 MiB"
  sortRules(rules),           // 按 priority 降序稳定排序（返回新数组）
  addRule(rules),             // 追加一条空白规则（默认 priority = 现存最大 + 10）
  removeRule(rules, index),
  validateRules(rules, opts), // opts={hardMax, volBlockMax} → {ok, errors:[{index,msg}]}
  toPayload(rules),           // 前端形状 → PUT body {rules:[...]}（max_size 用人类可读串）
  fromValue(value)            // 后端 value → 前端行数组
};
```

- [ ] **步骤 1：编写失败的测试**（`node --test`）

```js
test('validateRules 拒绝 single 缺上限', () => {
  const r = vs.validateRules([{ path_regex: 'x', mode: 'single' }], { hardMax: 64<<20 });
  assert.equal(r.ok, false);
  assert.match(r.errors[0].msg, /max_size/);
});
test('validateRules 拒绝超硬上限与超卷上限', () => { /* 两个断言 */ });
test('sortRules 按 priority 降序且稳定', () => { /* 同 priority 保持原序 */ });
test('parseSize 支持 20MiB / 20971520 / 非法', () => { /* 三个断言 */ });
test('toPayload 输出人类可读 max_size', () => { /* deepEqual */ });
```

- [ ] **步骤 2：运行测试验证失败**

运行：`node --test web/static/v2/volume-settings.test.js`
预期：FAIL（模块不存在）。

- [ ] **步骤 3：实现 `volume-settings.js`**

纯函数、无 DOM 依赖（`typeof module !== 'undefined' && (module.exports = ...)` 与 `window.v2volumeSettings` 双导出，与 `config-console.js` 同模式）。`validateRules` 与后端 `policy.Compile` 同一套判据（缺 `max_size`、超 `hardMax`、超 `volBlockMax`、正则无法编译用 `new RegExp` 试）。

- [ ] **步骤 4：运行测试验证通过**

运行：`node --test web/static/v2/volume-settings.test.js`
预期：PASS（全部用例）。

- [ ] **步骤 5：登记进 `Makefile web-test`**

在 `web-test` 目标里加 `@node --check web/static/v2/volume-settings.js` 与 `@node --test web/static/v2/volume-settings.test.js`（与既有 v2 测试行同位置风格）。

- [ ] **步骤 6：运行 `make web-test` 验证**

运行：`make web-test`
预期：全部 `node --check` 与 `node --test` 通过（含新模块）。

- [ ] **步骤 7：Commit**

```bash
git add web/static/v2/volume-settings.js web/static/v2/volume-settings.test.js Makefile
git commit -m "feat(ui): 卷设置规则表纯函数模块（校验/排序/序列化）" \
  -m "把规则表的增删、优先级排序、校验（与后端 policy.Compile 同判据的常量）、与 PUT body
序列化抽成无 DOM 依赖的纯函数，node --test 单测覆盖边界；已登记进 make web-test（R10）。" \
  -m "验证：node --test web/static/v2/volume-settings.test.js；make web-test 全绿。"
```

---

### 任务 8：v2 卷详情抽屉 + 规则表接线 + Playwright e2e

**文件：**
- 修改：`web/static/v2/volumes-console.js`、`web/static/v2/index.html`
- 创建：`web/e2e/volume-settings.spec.js`

**Interfaces**
```js
// volumes-console.js 新增导出（供 e2e/调试）
window.v2volumes = { init, load, openDetail(name), saveSection(section) }
```

- [ ] **步骤 1：编写失败的 e2e 测试**

```js
test('卷详情抽屉可打开并保存分块规则', async ({ page }) => {
  // 1) 打开 /ui/v2/ → 进「卷」分区
  // 2) 点某 secretdata 卷行内「详情」按钮 → 抽屉可见，含「分块规则」分区
  // 3) 点「新增规则」→ 填 path_regex=^archive/.* max_size=20MiB mode=single
  // 4) 点保存 → 期望提示成功且 version 递增（比对保存前后文本）
});
```

- [ ] **步骤 2：运行测试验证失败**

运行：`npx playwright test web/e2e/volume-settings.spec.js`
预期：FAIL（无「详情」按钮）。

- [ ] **步骤 3：实现**

`index.html`：在卷分区表格增加「详情」列（每行按钮由 JS 生成），新增详情抽屉 DOM（复用 `drawer` 类与既有 `#v2-drawer` 样式），`<script src="volume-settings.js">` 排在 `volumes-console.js` 之前。
`volumes-console.js`：行内按钮 → `openDetail(name)` → `GET /api/volumes/{name}/detail` → 渲染基础信息 + 分区列表；`block_policy` 分区用 `volume-settings.js` 渲染规则表（`path_regex`/`min_size`/`max_size`/`mode` 下拉/`priority`/删除 + 新增 + 保存）；保存前 `validateRules`（错误就地显示），保存 `PUT /api/volumes/{name}/settings/block_policy`（body 由 `toPayload` 生成）；`409` 提示「规则已被他人修改，请刷新后重试」；`403/404` 显示后端 message。提示两条约束（`single` 需上限；改规则不影响已存在文件）。

- [ ] **步骤 4：运行测试验证通过**

运行：`npx playwright test web/e2e/volume-settings.spec.js && make web-test`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add web/static/v2/volumes-console.js web/static/v2/index.html web/e2e/volume-settings.spec.js
git commit -m "feat(ui): 卷详情抽屉与分块规则表（v2）" \
  -m "卷列表每行新增「详情」按钮，抽屉显示基础信息 + 由后端 settings[] 驱动的分区列表
（前端无卷型专有分支，后续卷型新增配置无需改前端框架）。block_policy 分区渲染为规则表，
保存前本地校验、保存后刷新版本；409 提示并发冲突。" \
  -m "验证：npx playwright test web/e2e/volume-settings.spec.js；make web-test 全绿。"
```

---

### 任务 9：配置入口 + 文档收敛

**文件：**
- 修改：`cmd/sproxy/secret_register.go`（`extra.block_rules` 解析）、`docs/secret-volume.md`、`docs/config.md`、`pkg/volume/secretdata/README.md`、`CLAUDE.md`（sproxy 能力矩阵）
- 测试：`cmd/sproxy/secret_register_test.go`（追加用例）

**Interfaces**
```go
// secretdataOptionsFromVolumeWith 内
Policy *policy.Compiled   // 来自 extra.block_rules，经 policy.Compile(…, bp.Max)
OnBlockModeResolved func(rel, mode string, matched bool)  // 接 metrics 计数
```

- [ ] **步骤 1：编写失败的测试**

```go
func TestSecretdataOptions_BlockRulesParsed(t *testing.T) {
	t.Parallel()
	// v.Extra["block_rules"] = []any{map[string]any{
	//   "path_regex":"^archive/.*", "max_size":"20MiB", "mode":"single", "priority":float64(100)}}
	// ⇒ opts.Policy != nil，Resolve("archive/a.bin", 1<<20) 命中 single
}
func TestSecretdataOptions_BlockRulesInvalidFailsClosed(t *testing.T) {
	t.Parallel()
	// max_size 缺省 + mode=single ⇒ NewBackend 返回错误（含「需给 max_size」）
}
```

- [ ] **步骤 2：运行测试验证失败**

运行：`go test -count=1 -race -run TestSecretdataOptions_BlockRules ./cmd/sproxy/`
预期：FAIL（`opts.Policy` 恒 nil）。

- [ ] **步骤 3：实现**

`secret_register.go`：新增 `vcExtraBlockRules(v volume.Volume) (policy.Policy, error)`——解析 `extra.block_rules`（兼容 `[]any`/`[]map[string]any`，数值用既有 `vcPositiveInt` 风格统一 int/int64/float64；`max_size`/`min_size` 支持字符串经 `sizex.ByteSize` 解析）；在 `registerSecretdataBackendWithFS` 里 `policy.Compile(p, bp.Max)`，失败 ⇒ 返回错误（装配 fail-closed）。`OnBlockModeResolved` 接到 metrics 计数（复用 `pkg/server/metrics` 的 labeled counters；若装配层拿不到 metrics 则留 nil）。
文档：`docs/secret-volume.md` §3.2 表增 `block_rules` 行 + 新增小节说明规则语义/优先级/上限/「不影响已存在文件」；`docs/config.md` 同样补 `extra.block_rules` 示例；`pkg/volume/secretdata/README.md` 能力列表增一条；`CLAUDE.md` 能力矩阵增一行（`分块规则引擎 | pkg/cryptox/shardseal/policy | Compile/Compiled.Resolve | write 路径按 rel+size 选 single`）。

- [ ] **步骤 4：运行测试验证通过**

运行：`go test -count=1 -race ./cmd/sproxy/ -run TestSecretdataOptions`
预期：PASS。

- [ ] **步骤 5：全量自检**

依次运行并全部通过：
```bash
export PATH="$PATH:$(go env GOPATH)/bin"
make fmt
gofmt -l . | grep -v '^$' ; goimports -l . | grep -v '^$'
go build ./... && make build-all
make lint && make lint-all
go test -count=1 -race ./pkg/... ./internal/...
make web-test
go test -count=1 ./internal/archcheck/
make check-ci
```

- [ ] **步骤 6：Commit**

```bash
git add cmd/sproxy/secret_register.go cmd/sproxy/secret_register_test.go docs/secret-volume.md docs/config.md pkg/volume/secretdata/README.md CLAUDE.md
git commit -m "feat(secretdata): 配置声明分块规则（extra.block_rules）并收敛文档" \
  -m "配置入口：volumes[].extra.block_rules 作为静态基线，经 policy.Compile 预编译，写错
在装配期 fail-closed。运行期规则（StateStore）优先于它，删光运行期规则自然回落。

文档：secret-volume §3.2 配置表 + 规则语义小节（优先级/上限/不影响已存在文件）、
config.example 同步、secretdata README 能力列表、CLAUDE.md 能力矩阵。" \
  -m "验证：go test -race ./cmd/sproxy/；make fmt/lint/lint-all/web-test/check-ci 全绿。"
```

---

## 计划自检记录

- **规格覆盖**：§3 规则模型→任务 1；§4 SinglePlanner→任务 2；§5.0/5.1/5.2 接入→任务 4；§5.3 后端能力→任务 3+5；§7 持久化→任务 3+5+9；§8 API+权限→任务 6；§9 UI→任务 7+8；§12 门禁→任务 3/6/7/9；§13 测试计划→各任务步骤 1；§15 不做项未排任务（符合预期）。
- **审查重点 5 行**均已落到对应任务的步骤 1 具体用例名。
- **类型一致性**：`policy.Compiled` / `SetPolicy` / `blockletPolicyFor(rel,size)` / `SettingsStore` 方法签名在任务 1、3、4、5、6 间一致。
