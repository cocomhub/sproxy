# 封装卷（Enclosure Volume）设计

> 2026-10-11 · 状态：设计定稿（Stage 1 已落地），Stage 2+ 实施中
> 前置：`2026-10-10-trusted-volume.md`（可信卷/sidecar/容量）、`2026-10-01-secret-volume.md`（加密卷）
> 触发：多角度对抗评审 P0——at-rest 加密卷在 S3/DAV/分享/分块上传面未接线（读密文、写明文）

## 1. 目标与非目标

**目标**

1. 把「某个卷的某个子目录被封装（enclosure）占用」提升为**卷机制级**能力：封装后该子路径在
   源卷侧**拒绝一切直连读写**，客户端只能经封装卷由服务器处理（读明文/写加密）。
2. 提供**通用、可复用的封装卷能力层**：任意卷类型（加密/镜像/压缩/脱敏…）都能作为「封装处理层」
   挂到任意源卷的子目录上，无需各自实现视图、配额、防护。
3. 容量配额按**真实物理占用**记账，逻辑大小不再导致配额失真。

**非目标**

- 不改变既有 `trusted`（sidecar 完整性）/`capacity`（卷容量）/`quota`（账本）职责。
- 不为封装卷引入第二套视图/配额实现（复用为本设计的硬要求）。

## 2. 卷功能定位（正交边界，不重叠）

| 层 | 组件 | 唯一职责 | 明确**不**负责 |
|---|---|---|---|
| 声明 | `volume.Volume` + `registry` | 卷的身份/类型/ACL/上限/声明（含 `extra.source`） | 不做路径映射、不做加解密 |
| 定位 | `volume.Location`（`FSPath()` 唯一拼接点） | owner/bucket/rel → 卷内最终键 | 不知道封装、不感知加密 |
| 视图 | `sync.PrefixFS` | 键空间前缀映射与反转（子目录视图内核） | 不做防护、不做处理 |
| **封装** | **`enclosure`（本设计）** | **封装身份声明、源视图组装、直连防护（封印）、DirectURL 抑制** | 不做加解密算法、不做账本 |
| 处理（wrapper） | `secretdata` 等（可注册） | 对字节做变换（加解密/镜像/压缩） | 不知道源卷是谁、不做配额 |
| 完整性 | `trusted`（TrustedVolumeFS/Guard） | sidecar 计算/校验/失效 | 不做容量、不做路径映射 |
| 容量 | `capacity`（CapacityFS + PoolCounter） | 卷容量上限与真实占用结算 | 不做 ACL、不做视图 |
| 账本 | `quota`（Pool/Scope）+ `storage/capacity.StorageManager` | 预留/结算/对账 | 不知道卷语义 |
| 底座 | `storage.Root` | root 相对安全 IO + at-rest 加解密入口 + **封印** | 不知道业务语义 |

**正交判定**（每条写进代码注释与门禁）：
- 视图只搬键，不改字节；处理只改字节，不搬键；封印只拒绝，不搬运也不变换。
- 配额只记账，不决定「能不能写」（能否写由封印/ACL/上限三者分别裁决，且**封印优先**）。
- 任意两层可独立替换/单测：`EnclosureFS` 可脱离 secretdata 单测；`PrefixFS` 可脱离封装单测。

## 3. 分层架构

```
            ┌────────────────────────── registry.Set ──────────────────────────┐
            │ 声明：volumes[].extra.source="<源卷>:<子路径>" + wrapper/权限/配额      │
            └───────────────┬───────────────────────────────┬──────────────────┘
                    拓扑解析（嵌套/跨外部/环检测/深度上限）      封装注册表 → SealRule[]
                            │                                │
   ┌────────────────────────▼─────────┐            ┌─────────▼───────────────┐
   │ 封装卷 backend.FS()               │            │ 源卷 storage.Root        │
   │  EnclosureFS(  ← 薄层：禁直链/身份  │            │  SetSeals(SealRule[])    │
   │    PrefixFS( 源卷特权 FS, "sub/" ) │            │  ← 拒绝一切直连读写        │
   │  )                               │            └─────────────────────────┘
   │  ↑ 处理层 wrapper（secretdata…）   │                       ▲
   └──────────────┬───────────────────┘                       │
                  │ 特权源视图（封印注入前取得，封装卷自身不受封印影响）  │
                  └───────────────────────────────────────────┘
   容量：CapacityFS 包住封装 backend；封装池 parent=源卷池（默认继承，可配 max_bytes）
        真实占用：PhysSizer 能力探测 → 实测物理字节结算；缺失回落逻辑 size
```

**装配顺序（关键不变量）**

1. 建全部源卷句柄（本地 Root / 外部 backend）；
2. 解析封装声明（拓扑序），为每个封装卷取得**源卷特权 FS 视图**（`PrefixFS`，此时源侧尚无封印）；
3. 组装封装 backend（`wrapper(EnclosureFS(PrefixFS(...)))`）并注册；
4. 向**源卷** `Root.SetSeals(...)` 注入封印规则；DAV/WebDAV-remote 视图注入同一规则；
5. 注入配额父链与容量包装。

顺序错即失效：封印必须在「封装卷已持有特权视图」之后注入。

## 4. 配置（无向后兼容负担，一次性到位）

```yaml
volumes:
  - name: main
    type: local
    root: /data/main

  - name: vault                      # 封装卷
    type: enclosure
    extra:
      source: "main:private/vault"   # <源卷名>:<子路径>；子路径可空 = 整卷封装
      wrapper: secretdata            # 封装处理层（注册名；可扩展）
      secret_url: "secrets://keys/vault"   # wrapper 自身配置透传
      source_read_mode: hidden       # hidden(默认,读404+列举过滤) | readable(只读透视)
      allow_direct_url: false        # 默认 false：禁止 302 直链（禁客户端直连源存储）
      max_bytes: 8GiB                # 封装卷自身上限；缺省=仅受源卷池约束（继承）
```

- 源为外部卷（`baidupcs`/`s3`/…）：子路径按 **owner 运行时**拼接（`<owner>/<bucket>/<sub>`），
  视图在**租户解析期**构造（`Set.Tenant` 链），非装配期静态构造。
- 装配期 fail-closed：`source` 未注册/自封装/成环/深度 > 8/同一 `<卷>:<子路径>` 被两个封装卷声明/
  源卷同时 `extra.encrypt`（双重变换）/`wrapper` 未注册 → 报错拒绝启动。

## 5. 可复用能力层 API（对外只需这四个）

```go
// pkg/volume/enclosure —— 声明与解析
type Decl struct { Wrapper, SourceVol, SourcePath string; ReadMode seal.Mode; AllowDirectURL bool; MaxBytes int64 }
func Parse(vols []volume.Volume) ([]Decl, error)         // 解析 + 环检测/深度/唯一性校验
func (d Decl) SealRules() []storage.SealRule             // 供源卷 Root 注入
func (d Decl) SourceKey(owner string) string             // 源视图前缀（本地卷 vs 外部卷 owner 维度）

// pkg/volume/enclosure —— 视图与防护
type FS struct{ /* PrefixFS + 身份 + DirectURL 策略 */ }
func NewFS(prefixFS syncpkg.FS, d Decl) *FS               // 组合，不复制视图逻辑
// 职责：抑制 DirectURL / 补 MetaExtraUpdater 透传 / 携带身份（错误归因）/ R21 登记

// pkg/volume/enclosure —— wrapper 注册表（处理层可插拔）
type Wrapper interface{ Name() string; Build(ctx context.Context, v volume.Volume, src syncpkg.FS, opts map[string]any) (registry.ExternalBackend, error) }
func RegisterWrapper(w Wrapper)

// pkg/storage —— 封印（已实现）
type SealRule struct{ Path, Wrapper string; Mode SealMode }
func (rt *Root) SetSeals([]SealRule) error               // 写恒拒；读 hidden/readable
```

外部卷/未来卷类型要接封装，只需实现 `Wrapper` 并注册；视图/封印/配额全部复用。

## 6. 错误语义与可观测

| 场景 | 语义 |
|---|---|
| 源卷侧直连封印路径（读） | `fs.ErrNotExist`（404）+ `errors.Is(err, ErrSealed)` 可归因；父目录列举过滤 |
| 源卷侧直连封印路径（写/删/改名/链接） | `ErrSealed`（HTTP 409），**任何配置都不能放开** |
| `source_read_mode: readable` | 读放行（迁移/查看原始存储形态），写仍 409 |
| `allow_direct_url: true` | 仅在显式配置时转发 `DirectURL`（默认禁止；使用时警告「流量不经服务端」） |
| 装配期声明非法 | 启动失败（fail-closed），错误含卷名/源/原因 |

## 7. 复用矩阵

| 能力 | 来源 | 复用方式 |
|---|---|---|
| 子目录视图 | `pkg/sync.PrefixFS` | 直接复用（含反转/全能力转发/`Inner()`） |
| 配额嵌套 | `pkg/quota.Pool{parent}` | 补 `NewPoolWithParent`（`reserveUp` 已支持父链） |
| 容量与对账 | `CapacityFS` + `PoolCounter` + `reconcileVolumePool` | 复用；补 `PhysSizer` 探测 |
| 卷注册/解析 | `registry.RegisterBackend/AddExternalVolume/ResolveURL` | 复用 |
| 封印 | `pkg/storage`（本设计新增） | 新增（无既有能力可复用） |
| 封装身份与防护 | `enclosure.FS`（新增薄层） | 新增（组合 PrefixFS） |
| 声明与拓扑 | `enclosure.Parse`（新增） | 新增（复用 registry 装配入口） |

## 8. 测试矩阵（每项必须含变异验证）

1. 封印：写恒拒 / 读 404+归因 / 列举过滤 / readable 透视 / 嵌套优先 / 零回归（已落地）；
2. 封装视图：`DirectURL` 默认抑制、`MetaExtraUpdater` 透明转发、键空间往返（`Key`/`strip`）、
   身份错误归因、R21 能力断言；
3. 声明：环检测/自封装/深度上限/路径唯一/源不存在/wrapper 未注册/`extra.encrypt` 互斥；
4. 端到端（真实 HTTP/DAV/S3 路径）：封装卷正常读明文写密文；源卷直连四面（HTTP/S3/DAV/share）
   全被拒；分块上传/块写同样被封装卷处理；
5. 单账本：封装卷与源卷池不双计；`max_bytes` 生效；物理字节结算含 pad；
6. 性能：封装读路径不引入整文件回读（与既有口径一致）。

## 9. 分期与验收

| 期 | 内容 | 验收 |
|---|---|---|
| S1 ✅ | `storage` 封印内核（15 入口 + 7 组测试） | 已提交 `6b9f03983` |
| S2 | `enclosure.FS` 薄层 + `PrefixFS` 补 `MetaExtraUpdater` + R21 纳入 + `quota` 父链 | 单测 + 变异 + R21 绿 |
| S3 | `enclosure.Parse` 声明/拓扑 + `type: enclosure` 装配 + 源封印注入 + DAV 视图注入 | 端到端拒绝 + 正常读写 |
| S4 | wrapper 注册表（`secretdata` 迁为 wrapper）+ 跨外部卷 per-tenant 视图 | 加密卷六面接通 |
| S5 | 物理占用记账（`PhysSizer`/`Adjust`/S3 part/`Release` 钳 0） | 配额不可被逻辑放大绕过 |
| S6 | 评审 P0/P1/P2/Minor 全量修复 + 文档/门禁 | 四 lint 面 + archcheck + 全量测试绿 |

## 10. 已知边界（记档，不阻塞）

- 外部卷源的 per-tenant 视图构造在 S4；在此之前外部源装配期**显式拒绝**（禁静默降级）。
- 封印只覆盖经 `storage.Root` 与注入检查的视图；任何**新增直连 os I/O 的面**都必须同时接入
  封印检查（列入 R22/R23 门禁：新增面禁裸 `os.Open*` 于卷根）。
- 迁移场景的「只读透视」会暴露原始存储形态（密文），这是显式取舍（便于卷迁移/带外校验）。
