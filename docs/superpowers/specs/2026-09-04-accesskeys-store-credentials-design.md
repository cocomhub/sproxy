# AccessKeys→Store 凭据架构重塑 + SK 轮换 + 首次连接安全（认证面插件化 + 角色模型）

日期：2026-09-04（4A 落地后对齐更新：2026-09-06）
状态：4A 已落地（commit 1618835f 及之前）；§2.3/§10/§13 按 4A 实际实现事实对齐。4B 设计经 3 轮修订
（2026-09-06，第 3 轮按用户新架构方向 D5/D6/D7 全面重构：**默认 = 简单 AK/SK 管理（4A 语义回归）、
TOTP 作为可强制插件、账号级角色模型 `Key.Role`、认证面插件化嵌入 seam、TOTP 核心 stdlib、QR 走 Web
客户端 JS**，原「TOTP 强制」「admin 锚条目」设计推翻）。4B/4C 计划见
`docs/superpowers/plans/2026-09-06-credentials-4b-totp-login.md` 与
`docs/superpowers/plans/2026-09-06-credentials-4c-kms-plugin.md`

## 0. 范围总览

本次设计是**阶段 6 任务 4 的扩大版**——从「SDK 轮换」扩张为「凭据架构从静态 yaml 迁移到 store +
运行时 SK 轮换 + 首次连接安全（默认简单 AK/SK 注册 + 可强制的 TOTP 登录）」。拆分为三个 PR：

- **4A（本轮，已落地）**：凭据架构重塑——yaml `access_keys` 废弃改为 store 独立存储（用户 meta 下）、
  SK 多条目模型、运行时 SK 轮换（renew）+ 管理 API + sclient trust 命令、签名 v2。
  （插件/存储/加密接口原计划 4A 预留，实际**未落地**——见 §10，4C 第一步补接口。）
- **4B（后续 PR，拆三）**：首次连接安全——认证面插件化（Authenticator seam）+ 账号级角色模型
  （`Key.Role`：user/node/admin）+ **默认简单 AK/SK 注册**（4A 语义回归）+ **可强制的 TOTP 登录**
  （`registration.force_totp`，Web 注册/登录 + 客户端 JS QR）+ sclient trust login + 服务端控过期。
  4B-1 = 认证 seam + Role + 简单 AK/SK 注册（无 TOTP）；4B-2 = TOTP 插件（pkg/otp + login）；
  4B-3 = Web 注册/登录页 + JS QR。
- **4C（后续 PR）**：密码管理器/KMS 插件、加密静态存储、（实现细节后续 PR 细化）。

> 用户明确：「预留设计给新方案接入」——4A 必须把 4B/4C 需要的钩子（多 SK 模型、WrapScheme、
> meta/审计、服务端控过期、插件/存储接口）一次性定型。

## 1. 背景与动机

当前 `access_keys` 是静态装配期配置：旧 `sclient access-key create`（2026-09 凭据 store 化
后已删除，由 `trust ak add` 生成注册取代）唯一入口，服务端仅启动时读取一次，
SIGHUP 不重载，hub.Authenticator 持有无锁静态切片。无生命周期管理。

**动机（用户决策）**：定期合规轮换（30/90 天），平滑无感过渡 + TTL 自动过期 + 持久化。
后续再补首次连接安全（默认简单 AK/SK 注册，TOTP 可强制）。

## 2. 用户决策记录（多轮收口）

### 2.1 已批准核心（早期轮次）

| 决策点 | 结论 |
|--------|------|
| 轮换动机 | **定期合规轮换** |
| 过渡窗口 | **新旧双 SK 并发** |
| 持久化 | **持久化（但改为 store，非 yaml 写回——见 2.2）** |
| AK 是否轮换 | **只轮换 SK（AK 稳定作身份）** |
| 自加密范围 | **rotate + list 全覆盖（信封加密）** |
| pkg 命名 | **`pkg/accesskey`**（Key 类型 + Ring 原子集合；sproxysig 只做签名协议） |
| hub 接入 | **共享同一 Ring 实例**（单一凭据源，零同步） |

### 2.2 深入重塑（后续轮次）——本 PR 的骨架

| 决策点 | 结论 |
|--------|------|
| 凭据持久化源 | **从 yaml 彻底移除 access_keys，改为 store 独立存储**（放对应用户 meta 下）。无需历史兼容。 |
| 数据模型 | **AK→多 SK 条目**（map[AK]→[]SKEntry）。rotate=renew=增量追加新 SK 条目，旧条目自然过期。4A 统一定型。 |
| 命名 | **4A 即定**：端点 `/api/credentials` + sclient `trust` 命令（`renew` / `sk list` / `sk delete` / `sk expire` / `ak list` / `ak add` / `ak delete`）+ 验证函数 `VerifySproxySigMulti`；`access-key` 命令 rename 为 `trust`（内部分 `trust ak`/`trust sk` 子层级）。 |
| 签名协议 | **升级 v2 且废弃 v1**（破坏性，无存量 client 顾虑）。 |
| store 落点 | **storage_root/<tenant>/meta/ 下文件**（如 accounts.json，与用户 meta 共放，多租户隔离自然成立）。 |
| api_keys | **留 yaml**（多用户 Bearer 保留），启用时与 store 凭据**互斥优先**（现状语义保留）。 |
| 初始凭据 | **server 零凭据启动（U3）**：不再自动生成 anonymous 引导 AK/SK；register 是唯一用户入口——首个 admin 经回环注册（U2，4B），此后新用户注册受 `registration.disable` 控制。 |
| 禁止注册 | 配置名调整确保**默认为 false=允许注册**；禁止注册需显式 true。 |
| 服务端控过期 | SK 过期时间仅服务端控制，按场景（web/cli）设 TTL；客户端不能生成长期有效 SK 降低安全性。 |
| verify 回退 | **无回退，只查 store/ring**（yaml 已废弃，无静态段）。**零凭据窗口（U3，首个 admin 注册前）** 内 HTTP 面仅对 127.0.0.1/::1 回环放行——读取类操作放行与否由 `allow_insecure_loopback` 决定（默认 false→除 healthz/version 外全 401）；register 端点本身在无 admin 时恒回环（U2，不受该配置影响）。 |
| renew 客户端不控 TTL | SK 过期仅服务端配置控制（renew 默认 30d 上限；客户端 `--ttl` 移除，仅服务端配）。 |
| GET 只取指定 AK 的 SK 列表 | 不暴露全部 AK 的 SK；缺省按调用方自己的 AK，指定他 AK 需 admin。 |
| 全 AK 列表管理端点仅 admin | 获取所有 AK 列表等管理端点只允许 admin 角色 AK 用户；**首个注册用户即 admin（force_totp 时注册含 TOTP）**，无法新增其他 admin。 |
| 信令/tunnel/hub Secret | **统一查 store**（单一事实源）。 |
| 自加密 | 信封加密：wrap key 由调用方自己的 SK 派生（HKDF-SHA256），AES-256-GCM 包裹新 SK/逐条 SK。 |
| **D5（第 3 轮）** | **登录 nonce 独立端点**：`POST /api/credentials/nonce` 单独发放（服务端每登发放、TTL 60s、单次使用、绑定来源 IP、池上限 4096），register 响应不带 nonce——支持老用户重复登录（已注册过、TOTP 模式下无需再注册即可登录）。 |
| **D6（第 3 轮）** | **复用既有 `pkg/server.RateLimiter`**（`NewRateLimiter(limit int, window time.Duration, logger *slog.Logger)` 三参）挂公开端点限频（register 5/min、login+nonce 10/min per-IP），不新建限频器。 |
| **D7（第 3 轮）** | **TOTP 核心 stdlib 进主 module**（RFC 6238，20B 种子，GA 兼容 SHA1/30s/6 位；`crypto/hmac`+`crypto/sha1`+`encoding/base32`），主 go.mod 零新增；**QR 走 Web 客户端 JS**（非 Go 依赖）；三方 Go 库（服务端渲染 QR / KMS SDK）一律进 ext 插件模块（4C/后续）。 |

### 2.3 首次连接安全（4B，后续 PR——4A 已为它定型大部分骨架）

> **本节按 4A 实际落地事实对齐**（2026-09-06，第 3 轮重构）。新增「4A 状态」列：✅=4A 已落地、⬜=待 4B 实现。
> 4B 执行计划见 `docs/superpowers/plans/2026-09-06-credentials-4b-totp-login.md`（含 TDD 任务拆分）。

| 决策点 | 4A 状态 | 结论（4B 定稿） |
|--------|--------|----------------|
| 注册方式 | ✅ AK/SK 生成收归；⬜ 端点/流程 | register 是**唯一用户入口（U3）**：公开注册端点 `POST /api/credentials/register`（免 authMiddleware，仿 renew 引导豁免）。**无 admin 时仅回环可达（U2）**：ring 无任何 `Key.Role=="admin"` 的 AK 且非 127.0.0.1/::1 回环来源 → 403（与 `AddRegistration` 的 admin 判定共享 ring 查询）；有 admin 后恢复正常（受 `registration.disable` 控制）。**双模式（DEC-B）**：`registration.force_totp=false`（默认）→ 服务端生成 AK+SK（`accesskey.GeneratePair`），返回 `{ak, owner, admin, sk, skey_id}`——**SK 仅此一次明文下发**，无 TOTP；**SK 条目 `ExpiresAt = now + ttl`，`ttl` 由 handler 以 `credentialTTLFromCfg()`（credentials_handler.go:63，既有取值模式）注入——默认 30d，与 renew 同源，服务端控原则统一，R3-I3/R4-I1**；`force_totp=true` → 服务端生成 AK + TOTP secret（`pkg/otp.GenerateSecret`），**不创建 SK 条目**，返回 `{ak, owner, admin, otpauth_uri, base32_secret}`（无 sk，用户须经 TOTP 登录拿 session SK）。登录所需 nonce 单独走 `POST /api/credentials/nonce`（D5）。**首个注册用户即 admin（DEC-A）**：经 ring 新原子方法 `AddRegistration(ak, owner, sk, totpSecret, role, ttl)`（I2——写锁内建 Key + 追加 SK 条目（简单模式）或写 `Key.TOTPSecret`（TOTP 模式）+ 「全 ring 无 `Key.Role==admin` → 授 admin」判定，杜绝并发双 admin，D2；签名见 §5.2），**账号级 `Key.Role` 判定，不再用 Meta.Type 锚条目**。`registration.disable=true` → 403。 |
| 登录取 SK | ✅ v2 skey-id 必传、`DeriveWrapKey` 收归 | **仅 TOTP 账号需要登录**（简单 AK/SK 账号有 SK 直接用）。登录端点 `POST /api/credentials/login`（公开）body `{ak, nonce, code, login_type?}`（`login_type`=`web` 缺省 / `cli`，**仅选择服务端控 TTL，不构成 TTL 覆盖**，D3；未知值 → 400，M8）：验证 TOTP 动态码（±1 窗口）→ 用 `accesskey` 新常量 `WrapContextTOTP` + nonce 派生 wrap key → 服务端按 `login_type` 签发 session SK 条目（`KindTOTPWrap`，`WrapKeyID` 置空——wrap 来自 TOTP code+nonce 而非 SK 间包裹，M13；web → `Registration.SessionTTL` 默认 24h、cli → `Registration.CliTTL` 默认 **7d**）→ 响应 `{ak, session_skey_id, session_expires_at, wrapped_session_secret}`（snake_case，M2）。客户端同源派生解出 session SK + skeyID。**per-AK 登录失败锁定（U4）**：连续 5 次失败（TOTP 校验失败或 nonce 无效）锁定该 AK 15 分钟，锁定期内登录该 AK 一律 401（`Registration.LoginFailLimit`/`LoginFailWindow`）。`sclient trust login` 走 `login_type=cli`（D3）。 |
| sclient 登录 | ✅ `access-key` 已删、`trust` 家族已落地 | 4B-2 新增 `sclient trust login`（并入 trust 家族，`--ak` 可手动指定已注册 AK——M6）：TOTP 模式下 GA 手动输入密钥 → 注册（若未注册）→ 登录 → 解 session SK → 回填 `access_key`/`access_key_secret`/`access_key_id`（沿用 renew 回填模式）。**简单 AK/SK 模式无 login 流程**（`trust ak add` 或 register 直接下发 SK）。 |
| AK 多 SK | ✅ Ring AK→多 SKEntry | AK 允许同时多个 SK（不同 session TTL），过期自动失效（`Lookup`/`CoreEntry` 读时过滤）。4B 沿用，无新模型。 |
| 轮换策略 | ✅ renew 已落地 | web 不自动轮换、过期重登；sclient 长期运行定期 renew（既有 `trust renew`）、请求发现过期即重登录。 |
| 服务端控过期 | ✅ `credential_ttl`（renew 默认 30d） | 4B session SK 的 ExpiresAt **仅服务端**签发：按 `login_type` 选 `Registration.SessionTTL`（web，默认 24h）/ `Registration.CliTTL`（cli，默认 **7d**）——**不取自 `credential_ttl`**（那是 renew 默认）；客户端不能传 ttl（登录/注册端点白名单无该字段）。 |
| 登录码机制 | ✅ nonce 防重放池概念已有 | 动态码 + **服务端每登发放的 nonce** 派生 wrap key（限频 + 短 TTL + nonce 单次使用防重放 + ±1 窗口容差）。**per-AK 失败锁定（U4）**：连续 5 次登录失败锁定该 AK 15 分钟（锁定期内正确动态码也 401）。**失败计数语义（R2-N2）**：TOTP 错与「池中存在但无效的 nonce」（重放/已消费/过期/IP 不匹配）计失败；**未知 nonce（池中不存在）不计失败**（防随机垃圾 nonce 低成本锁定 AK 的 DoS）。per-IP 限频 + nonce 单次 + IP 绑定保留作纵深（防分布式爆破）。session SK 明文仅存服务端 ring（为验签），线上只传 wrapped。 |
| session SK 持有窗口 | ✅ 4B 定稿（D3） | 按场景设 TTL（服务端控）：web 登录 = `Registration.SessionTTL`（短，默认 24h，关页即弃）；cli 登录 = `Registration.CliTTL`（默认 **7d**——较 renew 更保守，长命仍靠 `trust renew`）。`sclient trust login` 面向交互式会话，回填 `access_key_secret` 前若已有凭据（如 renew 的长命 SK）需确认覆盖；长期运行 daemon 建议 `trust renew`（D4）。 |
| 角色模型 | ⬜ 4B-1 | **账号级 `Key.Role`（DEC-A）**：`RoleUser="user"`（默认）/ `RoleNode="node"` / `RoleAdmin="admin"`。admin 判定读 `Key.Role`（`getRole` → `ring.GetKey(ak).Role=="admin"`），**不再读**存活条目 `Meta.Type=="admin"`。`Meta.Type` 仅保留为登录来源 provenance（register/login/renew…）。node 账号由 admin 创建（`trust ak add --role node` 或 register body `role` 且调用方为 admin）。**删「永不过期 admin 锚条目」概念（D1 简化）**；仅保留 `akDelete` 拒绝删除 `Role==admin` 的 AK（I6）。 |
| 加密存储/KMS | ⬜ 接口未落地 | **4A 未落地接口**（§10 明示）：`CredentialStorer`/`SecureStorer` 均为 ⬜，4C 第一步接口提取。**`Authenticator` 接口已在 4B-1 定义**（DEC-C，见 §7.2），4C 不再重复提取。4B 不实现 KMS。 |
| 插件化粒度 | ⬜ 4C | TOTP/KMS/平台登录实现放独立子 module / 外部注册，core 只持接口（4C）。4B-2 的 `pkg/otp` 先放主包（纯 stdlib，D7），4C 再决定是否外移为插件（三方 TOTP/QR 库家 = `ext/auth/totp`，见 §10）。 |

> **4B 协议前提（来自 4A，勿再造）**：
> - AK = `ak-<mesh>-<32hex>`（`GeneratePair`）；SK = 32B/64hex；skeyID = `skey-<12hex>`（`GenerateID`）。
> - **零凭据启动（U3）**：server 不再自动生成 anonymous 引导凭据（4B 移除 4A 首启行为）；
>   `bootstrapCredentials` 只做「载入 store 快照 + 等待注册」，store 为空时零凭据运行。
> - v2 签名 `skey-id=` **强制必传**（`sproxysig.ParseHeader` 缺段 401）。注册/登录端点本身无凭据，注册在
>   主 mux **不挂 authMiddleware**（与 `/healthz` 同层公开 + 独立限频）；登录签发 session SK 后，
>   后续所有请求带新 `skeyID` 走既有 v2 路径，全链零改动。
> - **admin = `ring.GetKey(ak).Role=="admin"`（账号级，`getRole` 逐请求实时，不缓存）**；不再读
>   存活条目 `Meta.Type=="admin"`（DEC-A）。`Meta.Type` 仅登录来源 provenance。
> - **首注册仅回环可达（U2）**：register handler 开头判定「ring 无任何 `Role==admin` 的 AK 且 非 loopback
>   来源」→ 403；有 admin 后恢复正常（受 `registration.disable` 控制）。该回环判定与
>   `AddRegistration` 的 admin 存在性判定共享 ring 查询，权威判定仍在写锁内（D2）。
> - wrap context 单一事实源在 `pkg/accesskey`：4B-2 新增 `WrapContextTOTP` 常量 + TOTP 包裹/解开
>   （`DecryptSecret` 现只认 `KindSecretWrap`，需在 accesskey 扩展 `KindTOTPWrap` 的包裹路径）。
> - TOTP secret 持久化：随注册写入 `<tenant>/meta/credentials.json` 的对应 AK 条目（`Key.TOTPSecret`），
>   随账号注销清理（§13）。

## 3. 核心原则

### 3.1 SK 轮换 vs AK 轮换
只轮换 SK，AK 稳定作身份锚（Actor/owner 以 AK 为身份，数据连续）。AK 轮换记后续。

### 3.2 单一事实源
Store 是唯一凭据源。启动把 store 载入 Ring；auth/hub/信令/tunnel 全部查 Ring。不维护 cfgPtr.AccessKeys 静态段。

## 4. 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                 accesskey.Ring (运行时权威凭据源)            │
│  map[AK] → Key { AK, Owner, Role(user|node|admin),           │
│                 TOTPSecret(4B-2), Entries []SKEntry }        │
│  SKEntry { SK, Kind, WrapKeyID, CreatedAt, ExpiresAt,        │
│            Status, Meta{type,ip} }                           │
│  原子性：方法级锁；快照 COW；读取时比对当前时间               │
└──────────────────────────┬──────────────────────────────────┘
                          │ 同一实例注入
        ┌─────────────────┴─────────────────────┐
        ▼                                       ▼
  HTTP 面 auth (Authenticator 链)        hub.Authenticator
  [api_keys Bearer] → [RingAuthenticator]  （共享 ring，无 SetAccessKeys 需要）
  → Principal{AK, Owner, Role, Mesh} 入 ctx
  （宿主可经 RegisterRoutesOpts.Authenticators 注入自有实现）

  Store（持久化）：
  storage_root/<tenant>/meta/accounts.json
  ├── accounts[{ak, owner, role, meta(注册信息), totp?}]   // 4B-2: totp secret
  └── keys[{sk, kind, wrap_key_id, created, expires, status, meta}]
  SecureStorer 接口：默认文件实现；KMS/加密 4C 接入
```

## 5. pkg/accesskey 包设计

### 5.1 数据模型（AK→多 SK）

```go
type Kind  string        // plain | secret_wrap | totp_wrap(4B-2)
type Status string       // active | expired | disabled
type Role  string        // user | node | admin（账号级，4B-1 新增，DEC-A）

const (
    RoleUser  Role = "user"    // 默认（普通凭据用户）
    RoleNode  Role = "node"    // mesh/hub/relay 节点（文件操作无 node）
    RoleAdmin Role = "admin"   // 管理角色（首注册授予，无法新增其他 admin）
)

type Key struct {
    AK           string   // 身份锚，ak[-<mesh>]-<32hex>（16B 标准；解析层兼容 legacy <16hex>）
    Mesh         string   // 由 AccessKeyMesh(AK) 导出，不存字段避免漂移
    Owner        string   // 稳定 owner（默认 = AK；4B 注册用户 = 用户名）
    Role         Role     // 账号级角色（4B-1，DEC-A；json:"role"，持久化进 credentials.json）
    TOTPSecret   []byte   // 4B-2：TOTP 注册时生成、账号级，随 Key 序列化（[]byte→base64 明文落盘，M12）
    Entries      []SKEntry // 该 AK 的全部 SK 条目（多 SK 并存）
}

type SKEntry struct {
    SK        []byte    // 明文 SK（服务端内部）
    Kind      Kind
    WrapKeyID string    // Kind==secret_wrap 时包裹本 SK 的 wrap key 的 AK；Kind==totp_wrap 为空
                        // （wrap 来自 TOTP code+nonce，非 SK 间包裹，M13）
    CreatedAt time.Time
    ExpiresAt time.Time // 零值=永不过期（服务端控；普通 SK 条目按 TTL 设值；简单模式注册 SK 同样按
                        // `CredentialTTL` 设值，R3-I3）
    Status    Status
    Meta      Meta
}

type Meta struct {
    Type string // 登录来源 provenance：config|renew|register|login…（4B 起不再承载 admin 角色
                // 标记——admin 判定改为读 Key.Role，DEC-A）
    IP   string // 生成时来源 IP（审计/溯源）
    // 4B extends: device/user_agent 等
}
```

**持久化兼容（DEC-A + R3-M4）**：旧 credentials.json（4A 无 role 字段）加载时 `Key.Role` 为空串
（`""`）——**`Load`/`Replace`（或 `requireRole`）显式把 `""` 归一为 `RoleUser`**（防 `requireRole("", user)`
误 403）；dev 阶段无存量用户，不提供迁移脚本。

### 5.2 Ring 原子集合

```go
type Ring struct { mu sync.RWMutex; m map[string]*Key; now func() time.Time }
```

- `NewRing(now ...func() time.Time) *Ring`
- `UpsertAK(ak, owner string) error` — 覆盖 AK 条目（保留已有多 SK 条目）
- `AddKey(ak string, sk []byte, opts ...EntryOption) (string, error)` — **追加**一条 SK 条目（要求
  `len(sk)==32`B，否则 `ErrInvalidSecret`；AK 未知→错误）；opts 用
  `WithID/WithKind/WithWrapKeyID/WithExpiresAt/WithMeta`；生成 `skey-<12hex>` ID
  （`accesskey.SkeyIDPrefix`，与 AK 的 `ak-` 前缀对称）
- `DeleteAK(ak string) error` — 删除整个 AK（**`akDeleteHandler` 对 `Role==admin` 的 AK 拒绝调用**，
  见 §7.4）
- `DeleteKey(ak, id string) error` — 删特定 SK 条目（不存在→ErrNotFound/404）
- `ExpireKey(ak, id string, until time.Time) error` — 设某条 SK 过期
- `Lookup(ak string) ([]SKEntry, bool)` — **读取时过滤已过期条目**（now≥ExpiresAt 剔除，`aliveLocked`）
- `CoreEntry(ak string) *SKEntry` — 供信令/tunnel/renew 取当前有效主条目（最新未过期）
- `GetEntry(ak, id string) (SKEntry, bool, error)` — 按 skeyID 精确取（签名 v2 校验用，无试签回退）
- `GetKey(ak string) (*Key, bool)` — 按 AK 取完整 Key 副本（含 `Role`/`TOTPSecret`，深拷贝；不存在→false；
  I1，`getRole` 读 `Key.Role` 与登录 handler 取 `TOTPSecret` 用）
- `Snapshot() []Key` — 全部快照（copy，按 AK 排序）
- `Replace(keys []Key) error` / `Len() int`
- `AddRegistration(ak, owner string, sk, totpSecret []byte, role Role, ttl time.Duration) (granted bool, id string, err error)`
  — 4B 新增（DEC-A/DEC-B/I2）：**方法首部校验「`sk` 与 `totpSecret` 不同时非 nil，双 nil → error」
  （R3-M2，防产生既无 SK 又无 TOTP 的账号）**。写锁内建 Key（含 `Role` 与 `TOTPSecret`）+ 追加 SK
  条目（`sk` 非 nil 时，简单模式，**`ExpiresAt = now + ttl`**——TTL 由调用方注入，默认 30d 与 renew
  同源，R4-I1；**`pkg/accesskey` 不依赖 `pkg/server` 配置，方法内部不读 `cfg.CredentialTTL`**；TOTP
  模式无 SK 条目，`ttl` 忽略）+ 「全 ring 无 `Key.Role==admin` → 授 admin」判定（D2/I2/M5；`aliveLocked`
  语义换为遍历 Key.Role）。**admin 授予由方法内原子判定覆盖**：首注册恒授 `RoleAdmin`（无论入参）；非首注册传
  `RoleAdmin` 拒绝或降级 `RoleUser`。`granted` = 本次注册是否被授予 admin；`id` = 简单模式下新建 SK
  条目 ID（TOTP 模式无 SK 条目 → 空串）。`owner` 空值默认 = AK 字符串（R2-N4）。
  **不可在 handler 里「先 `Snapshot` 判、再 `UpsertAK`+`AddKey`」**——sync.RWMutex 不可重入，
  且两次独立调用会并发双 admin（M5）。

**原子性关键**：所有读加读锁；写加写锁；`Expire`/`Lookup` 共用同一 `now` 基准，
杜绝「已过期仍被验证」窗口。

**getRole 语义注**：`getRole(ak)` 读 `ring.GetKey(ak).Role`（`"user"` 缺省），不再遍历存活条目
`Meta.Type=="admin"`（DEC-A）。4A 现有 `getRole` 实现（credentials_handler.go:81）在 4B-1 修改。

### 5.3 服务端控过期（核心护栏）

- renew（SK 轮换）给新 SK 设 `ExpiresAt = now + ttl`（默认 30d，**上限来自配置**，客户端不能传 ttl，
  仅服务端配）。
- **旧 SK 到期自动失效**：`CoreEntry`/`Lookup` 读到 now≥ExpiresAt 即剔除。
- 多 SK 并存：renew 追加新条目，旧条目保留到各自 ExpiresAt → 平滑过渡窗口。
- 过期条目自动从 ring 清理（可选，有界）；`Snapshot` 保留 Status=Expired。

## 6. 自加密（信封加密）

### 6.1 动机
- renew（rotate）是 SK 明文唯一跨网络时刻；list 若明文下发全部 SK 则持有读取权者可冒充任意身份。
- 按 key 隔离秘密可见性：同 mesh 多客户端只能解出自己的 SK。

### 6.2 wrap key 派生
```go
// 从调用方自己的 SK 派生加密新 SK 的 wrap key（AES-256-GCM）
func wrapKey(sk []byte, context string) ([]byte, error) {
    return deriveKey(sk: 32B, salt: "sproxy-accesskey-wrap/v1\x00"+context, 32B)
}
// 派生原语 = HKDF-SHA256(secret=sk, salt=sproxy-accesskey-wrap/v1+context, info=ak)
```

- renew 响应：`wrapKey = wrapKey(旧SK, mesh)`，`ciphertext = AES-GCM(wrapKey, 新SK)`
- list 响应：每条 `wrapKey_i = wrapKey(key_i.SK, mesh, ak_i)`，`ciphertext_i = AES-GCM(wrapKey_i, key_i.SK)`
- 客户端必须持有对应 SK 才能解密。
- **4B-2 TOTP 登录**：`DeriveTOTPWrapKey(code, ak, nonce)` = `wrapKey(sha256(code), ak,
  WrapContextTOTP+"#"+nonce)`——动态码经 SHA-256 展为 32B HKDF 输入。

### 6.3 失败模式
- 客户端用的旧 SK 与当前核心 SK 不一致 → wrap key 错 → 解密失败 → 报错提示 `--access-key-secret` 更新。
- 服务端权威操作不因客户端解密失败回滚；客户端在 renew 前可先 list 解密当前 SK 校验一致性。
- **4B-2 登录码（仅 TOTP 账号）**：动态码+nonce 派生 wrap key；6 位码熵较低 → 服务端限频 + 短 TTL +
  nonce 每登各异 + ±1 窗口容差 + **per-AK 失败锁定（U4，默认 5 次/15 分钟）**。
  **威胁模型如实表述（M9）**：截获 wrapped 响应后攻击者可**离线爆破 6 位码，不受 30s 服务端窗口
  限制**（码在签发时刻有效即可）；实际保护 = TLS 边界（截获需破 TLS）+ session TTL（web 24h / cli
  7d）+ 爆破 1M 空间的成本 + per-AK 锁定（分布式爆破把攻击面收敛到单个 AK）。

### 6.4 安全边界（诚实声明）
- 服务端为验签必须持明文 SK → 自加密不防服务端泄露，防「同 mesh 内 SK 相互隔离」+「SK 明文不落日志」。
- renew 的「只用旧 SK 包新 SK」：只有持当前有效 SK 者能轮换出可用新 SK。

## 7. 服务端装配（pkg/server + cmd/sproxy）

### 7.1 装配流程（4A）

```
cmd/sproxy/root.go（启动）:
  store := credentialstore.NewFileStore(filepath.Join(cfg.StorageRoot, "_meta_", "credentials.json"))  // 多租户: <tenant>/meta/credentials.json
  ring := accesskey.NewRing()
  // 零凭据启动（U3）：store 为空 → 不生成任何凭据，仅等待注册（首 admin 经回环注册）
  ring.Replace(loadFromStore(store))
  h := RegisterRoutes(..., CredentialRing: ring, CredentialStore: store)   // HTTP 认证查 ring
  hubAuth := hub.NewAuthenticator(ring)                                     // 共享同一实例
```

- `RegisterRoutesOpts.CredentialRing *accesskey.Ring`（新字段，nil 时内部自建并载入 store 快照；
  store 为空 → 零凭据等待注册，**不再自动生成** anonymous 引导凭据，U3）。
- **`RegisterRoutesOpts.Authenticators []Authenticator`（4B-1 新增，DEC-C）**：认证面插件化嵌入点——
  **nil → 默认装配 `[]Authenticator{RingAuthenticator{ring}}`；非 nil → replace 默认链**（宿主全权掌控，
  需含 RingAuthenticator 则自行加入链）。外部宿主可注入自有实现（映射自有用户/会话 → `Principal`），
  **文件操作按 `Principal.AK` 落桶**（宿主把目标桶 ID 放入 `Principal.AK`，保持 4A 按 AK 落桶现状零
  回归，无需关心 SK 来源；`Principal.Owner` 仅作元数据/审计）。
- `hub.NewAuthenticator(r *accesskey.Ring)`（改签名：从 []AccessKey 改接收 ring）。

### 7.2 auth.go 认证改造（Authenticator 链，4B-1）

**接口定义（`pkg/server/auth.go` 新增，DEC-C）：**

```go
type Principal struct {
    AK    string // 身份锚 + 文件桶 ID（RingAuthenticator 时 = AK；宿主注入者把目标桶 ID 放此，R3-I1）
    Owner string // 元数据/审计（宿主可映射自有用户名；不参与落桶）
    Role  string // user|node|admin
    Mesh  string
    Secret []byte // 明文 SK（R5-I1）：RingAuthenticator 验证成功填充（供 /tunnel 密钥派生）；
                  // 宿主注入的 Authenticator 留空——无 SproxySig 凭据不派生隧道密钥；
                  // SK 仅请求内传递、不落日志、不持久化
}

type Authenticator interface {
    Name() string
    Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}
```

- **`/tunnel` 密钥派生改读 Principal（R5-I1）**：链成功路径 authMiddleware 以
  `PrincipalFrom(ctx).Secret + PrincipalFrom(ctx).Mesh` 执行既有 `/tunnel` 密钥派生
  （auth.go:393-404 原样保留，`h.tunnelDerivedKey(secret, mesh)` → `tunnel.SetTunnelKey`，仅把
  `cred.secret` 改为读 `Principal.Secret`）。**宿主 Authenticator 的 `Secret` 留空 → `/tunnel` 不派生**
  （401 或明确错误——无 SproxySig 凭据不建立隧道）。

- **`RingAuthenticator`（默认实现）**：包装现有 SproxySig v2 验签（`verifySproxySigFromRing`），
  返回 `Principal{AK: cred.ak, Owner: Key.Owner, Role: string(Key.Role), Mesh: cred.mesh, Secret: cred.secret}`；无
  Authorization 头或验签失败 → error（401 语义）。**失败不写响应（R4-I3）**：实现时先重构
  `verifySproxySigFromRing`（auth.go:232-296）为「返回 `(*verifiedCredential, error)`、失败不写响应」，
  `http.Error` 收敛到 authMiddleware 链全失败后统一输出——否则链前/链中失败已写 401，后续 authenticator
  被短路。**文件桶 = `Principal.AK`**——RingAuthenticator 时即 AK，完全保持 4A 按 AK 落桶行为（R3-I1）。
- **authMiddleware 改走 Authenticator 链**：`Handlers` 增 `authenticators []Authenticator`（默认
  `[RingAuthenticator]`）；**链先跑，任一成功 → `Principal` 入 ctx**（新增 `PrincipalFrom`），全部失败
  才进入兜底。api_keys Bearer 路径保留为链前独立检查（`cfg.APIKeys.Enabled` 时优先走既有
  `authenticateAPIKey`，不查 store）——取更小改动，不把 api_keys 实现为 Authenticator。
- **`handleNoCredentials` 降为最终兜底（R3-I2）**：仅当「链全失败 且 ring 空」时进入
  `handleNoCredentials`（allow_insecure_loopback 兜底，语义不变——**不再前置拦截链**）。宿主注入的
  Authenticator 与 ring 无关，零凭据窗口（U3 首启状态）内仍可认证。
- **回环直通路径合成最小 Principal（R4-I2）**：`handleNoCredentials` 的回环放行分支（auth.go:318-329，
  `allow_insecure_loopback=true` 本地读取）在 `next(w,r)` 前**合成 `Principal{AK:"", Owner:"", Role:"user"}`
  写入 ctx**（`withPrincipal`），使 `requireRole(principal, user)` 放行该路径（4A 回环读取语义零回归）；
  链全失败且非回环来源 → 维持 401。`requireRole` 在 `principal==nil` 时对**非文件路由**（healthz/version
  等裸路由，不挂 authMiddleware）不拦截。
- 现有 `ActorFrom`/`MeshFrom`/`EntryIDFrom` ctx 保留（4A 消费方零改动）；**`withActor` 在认证成功
  路径统一写 `Principal.AK`**（RingAuthenticator = `cred.ak`，宿主 = 其放入的桶 ID）；`Principal.Owner`
  仅作元数据/审计。
- `verifySproxySigFromRing` 内部逻辑不变（仍写 mesh/actor/entryID ctx）；**`getRole` 改读
  `ring.GetKey(ak).Role`**（DEC-A，见 §5.2）。
- **`requireRole` 门禁钩子**：`requireRole(principal *Principal, minRole string) error` 辅助函数 +
  文件操作路由组（upload/download/delete/list/mkdir/rmdir/search/batch…）要求 `Role∈{user,admin}`；
  mesh/hub/relay 路由要求 `Role∈{node,admin}`。本计划至少接文件组 + 注明 mesh 组接线点；完整
  mesh node 用户类型（node 账号文件访问策略）列后续 PR（§13）。
- **公开端点豁免不变**：register/nonce/login 挂主 mux **不包 authMiddleware**（仿 `/healthz` 层公开 +
  独立限频）。

### 7.3 hub.Authenticator 改造
- 结构改为 `{ ring *accesskey.Ring; noncePool }`（替换 accessKeys []AccessKey）。
- `Authenticate` → ring 查（含过期检查）+ nonce 防重放 + HMAC proof。
- 无需 SetAccessKeys：共享同一 ring 实例。

### 7.4 管理端点（主 mux + authMiddleware 保护，仿 PUT /api/config 样板）

> **模型前提**：AK=用户身份（稳定），SK=该身份下的多条目（可过期/删除）。因此「删除」语义
> 必须落在 **SK 条目**上（普通用户层面），而非整个 AK。**账号（AK）注销暂不支持**（范围外，
> 见 13 节）；admin 才可删除整个 AK，且需**二次确认**。

> **权限分档（4B-1 起按 `Key.Role` 三分，DEC-A）**：
> - **user（普通凭据用户，默认角色）**：可 `renew` / `GET` 自己的 AK 的 SK 列表 / `expire` 自己的 SK /
>   `DELETE` 自己的 SK 条目。**不能**看全量、**不能**删 AK、**不能**注销账号。
> - **admin（首个注册用户，无法新增其他 admin）**：除普通能力外，可 `GET` 全部 AK 列表、
>   可 `POST /api/credentials` 新增 key（含创建 `role=node` 的账号）、可 `expire`/`DELETE` 任意 AK
>   （删除 AK 需二次确认；**`Role==admin` 的 AK 拒绝删除**，I6）。
> - **node（mesh 节点账号）**：admin 创建（`trust ak add --role node` / register body `role=node` 且
>   调用方为 admin），Role=node；供 mesh/hub/relay 面认证（`Role∈{node,admin}` 门禁），**不参与文件
>   操作**（文件组门禁 `Role∈{user,admin}`）。
> - admin 判定：`ring.GetKey(ak).Role=="admin"`（`getRole` 逐请求实时，不缓存；4A 阶段无 admin——
>   admin-only 端点 4A 返回 403；首个注册用户即 admin，见 7.5）。

| 端点 | 权限 | 行为 |
|------|------|------|
| `POST /api/credentials/{ak}/renew` body `{mesh?}` | 本人 | 调用方当前 AK 追加新 SK 条目（多 SK 模型：旧条目保留至各自 ExpiresAt）。用调用方 SK 派生 wrap key 加密新 SK（Kind=secret_wrap），store 持久化 + RecordAudit(credential_renew)。**TTL 仅服务端配置控制（客户端不可传）**。返回 `{ak, sk_id, wrapped_secret(b64), kind, wrap_key_ak, expires_at}` |
| `GET /api/credentials/{ak}/sk` | 本人（`{ak}`=自己）；admin（任意） | 返回**指定 AK 的 SK 列表**（含行 `sk_id/created/expires/status`，每条需各自 wrap key 解密）。不暴露全部 AK 的 SK；缺省=调用方自己 |
| `DELETE /api/credentials/{ak}/sk/{skID}` | 本人；admin（任意） | **删除单个 SK 条目**（普通用户注销 SK 的唯一手段；删除后该 SK 立即失效，不再可信）。幂等（不存在返回 404）。RecordAudit(credential_sk_delete)。**不支持账号注销（无 AK 级删除入口给普通用户）** |
| `POST /api/credentials/{ak}/sk/{skID}/expire` body `{until}` | 本人；admin（任意） | 设**单个 SK 条目**过期（RFC3339；空=永不过期）；用于双 SK 并存窗口裁切。RecordAudit(credential_expire) |
| `GET /api/credentials`（无参，admin-only） | admin | 全部 AK 列表（admin 才能看全量；每个 AK 含其活跃 SK 数/摘要，不下发明文 SK） |
| `POST /api/credentials` body `{ak, owner, role?}` | admin | 新增 AK（4B 注册用；4A 仅 admin 预创建可选；`role` 支持 `node`，默认 `user`）。**admin AK 不可经此创建**（Role 强制 user/node） |
| `DELETE /api/credentials/{ak}` | **admin 专属 + 二次确认** | 删除整个 AK（身份+全部 SK 条目+关联 meta）。body 须带 `{confirm: "<ak>", force?: bool}`（确认串=目标 AK 名）；未提供/不匹配 → 400。**`Role==admin` 的 AK 拒绝删除（400「admin 角色 AK 不可删除」，I6）**。RecordAudit(credential_ak_delete)。**普通用户无此能力** |

> **二次确认设计**：`DELETE /api/credentials/{ak}` body 必须含 `confirm` 字段等于目标 AK 字符串；
> 且若该 AK 仍有活跃（未过期）SK 条目，默认拒绝，需 `force: true` 才允许（防误删在用的凭据）。
> 确认串为 AK 名本身——要求调用方明确显式意图，防脚本/误触。

> **admin AK 删除保护（I6）**：`Role==admin` 的 AK **不可删除**——`akDeleteHandler` 定位目标 AK 后
> 若 `Key.Role=="admin"` → 400。防唯一 admin 自删（或持其 session SK 的攻击者删除）导致
> 系统无 admin、下一次注册者即 admin 的接管竞态。node/user AK 可删。

全部变更 `RecordAudit`（action=credential_renew / credential_add / credential_ak_delete /
credential_sk_delete / credential_expire）。调用方须为合法持有者（authMiddleware 验签通过即证持
SK；admin 校验交给 handler，按 `ActorFrom(ctx)` 查其 AK 的 role）。

### 7.5 持久化（store 文件）
- 写入时机：register/renew/add/expire/delete 后 → 同步 store（COW 更新 ring 后写 store）。
- 多租户：`storage_root/<tenant>/meta/credentials.json`（沿用 meta 桶），账户与密钥同文件
  （或拆 accounts/keys 两文件，视 4B）。
- **写原子替换**（临时文件 + rename），失败 RecordAudit(credential_persist_error) + 不丢内存态。
- **SIGHUP**：不再重载 access_keys（yaml 已废弃），该重载逻辑删除；CLAUDE.md 更新。
- **零凭据启动（U3）**：store 为空 → **不生成任何凭据**，仅等待注册（`bootstrapCredentials` 不再
  生成 anonymous 引导 AK/SK）；register 是唯一用户入口，首个注册用户经回环（U2）成为 admin。
  启动日志提示「首次注册经回环，将成为 admin」（S2）。
- **admin 角色 4B-1 产生**：4A 阶段 store 无 admin（无自动生成凭据），admin-only 管理端点（全量列表/
  加 key）在 4A 暂对 non-admin 拒绝（返回 403）——首个注册用户即 admin（4B-1，DEC-A，无法新增其他 admin）。
- **admin 恢复途径（R3-I3 + R3-S1 + R4-M2，运维操作指引）**：简单模式的 admin SK 仅一次下发，丢失/过期后无法
  renew（renew 需有效 SK）。恢复途径二选一：
  - **简单模式（force_totp=false）**：停服 → 编辑 `<tenant>/meta/credentials.json` 删除该 `Key`
    （或清空其 `Entries`）→ 重启 → 因 ring 无 `Role==admin` 的 AK，**重新注册即成为 admin**（register
    的 admin 判定按 Key.Role 遍历，无需保留原 Key）。后续用新 admin AK 管理（可 `trust ak add` 重建
    其它账号）。**SK 过期后无续期路径（R4-M2）**——该 AK 数据仅可在停服维护时导出（旧桶文件保留在
    `<tenant>/user/<old-ak>/`）；如需继续使用，重注册新 AK 后将旧桶文件迁入新 AK 桶（
    `<tenant>/user/<new-ak>/`）。
  - **TOTP 模式（force_totp=true）**：TOTPSecret 持久化在 `Key.TOTPSecret`，admin 经 `trust login`
    随时重登拿 session SK 恢复，无需停服。
  > 约束：恢复操作需要存储文件写权限（运维/部署角色），属带外管理路径，与 register 回环门禁（U2）
  > 互补——恢复后 ring 已有 admin，远程注册恢复受 `registration.disable` 正常管控。
- **凭据按租户隔离**：`<tenant>/meta/credentials.json` 每租户独立；AK 属于单一租户，认证后 ctx 带
  租户 scope，文件操作限该租户（multi-tenant 隔离自然成立）。

### 7.6 无认证凭据兜底（allow_insecure_loopback）
- **零凭据窗口（U3，首个 admin 注册前）**：server 零凭据启动后、首 admin 经回环注册前，HTTP 面
  **仅对 127.0.0.1/::1 回环来源可达**（本地运维 ls/stat/healthz），其余请求 401。
- 新配置 `allow_insecure_loopback`（默认 **false**：零凭据窗口内连回环也只放行 healthz/version；显式
  true 才放行读取类操作如 ls/stat）。`allow_insecure_loopback=true` → 零凭据窗口内回环读取放行；
  否则全 401。
- **register 端点恒回环（U2，不受 `allow_insecure_loopback` 影响）**：无 admin 时 register 仅对
  127.0.0.1/::1 回环放行（远程 → 403）；有 admin 后恢复正常（受 `registration.disable` 控制）。
  > 运维约束（I4b）：`registration.disable=true` **仅适用于已有 admin 的存量部署**——无 admin 时即使
  > 回环来源，`disable=true` 仍 403（loopback 预检只挡远程，disable 检查照常），系统将**永无 admin**
  > （4B 起无任何带外引导，register 是唯一入口）。新部署须保持 `disable=false` 直至首个 admin 注册完成。

> 语义澄清：默认 `false` = 最严格（零凭据窗口内回环也不放读取，仅 healthz）；运维本地调试显式开 true
> 才获得回环读取放行。authMiddleware 在「无任何凭据可查」时走此分支；**回环放行分支合成最小
> `Principal{AK:"", Owner:"", Role:"user"}` 写入 ctx**（R4-I2）——`requireRole(principal, user)` 放行
> 该路径（4A 回环读取语义零回归）；register 端点的回环判定独立于
> 该配置（无 admin 时恒回环，U2）。

## 8. 签名 v2

### 8.1 协议（v2 定稿）

**Header**：
```
SproxySig v=2 ak=<AK> skey-id=<skeyID> ts=<unix_ms> exp=<unix_ms> nonce=<16B hex> body_sha256=<hex|UNSIGNED> sig=<hex>
```

**skeyID = SK 条目 ID**：`skey-<12hex>`（`accesskey.SkeyIDPrefix`，SKEntry.ID，ring 条目唯一标识，创建时生成）。
v2 协议 **强制必传**（`sproxysig.ParseHeader` 缺段 ErrMalformed→401）；服务端先按 `ak`+`skeyID`
精确取条目（`ring.GetEntry(ak, skeyID)`，**无试签回退**）再取该条 SK 验签。唯一例外：自 renew 引导
（`sproxysig.ParseHeaderAllowMissingSkeyID`，仅允许「该 AK 唯一存活条目」定位；4B 注册/登录端点
因无凭据同理豁免——它们不挂 authMiddleware）。

**Canonical（v2）**：
```
sproxy-sig/v2
<ak>
<skeyID>         ← 新增条目 ID 段（定位精确条目，避免歧义；v2 强制必传）
<ts>
<exp>
<nonce>
<method>
<path>
<query>
<body_sha256>
```
签名仍 = `HMAC-SHA256(SK, canonical_v2)`。**废弃 v1**（无存量 client，破坏性升级可接受）。

> 备注：`skey-id`（skeyID）段是唯一相对 v1 的 canonical 增加；键名定稿为 `skey-id=`（前缀
> `skey-<12hex>`，见 4A 实际实现 `pkg/sproxysig` / `pkg/accesskey`），不再微调。

### 8.2 升级后服务端校验（已落地）

- 解析 v2 头（`sproxysig.ParseHeader`，`skey-id` 强制必传）→ `ak`+`skeyID` 精确取 SKEntry
  （`ring.GetEntry(ak, skeyID)`，`verifySproxySigFromRing`）→ 用该条 SK 验签 → 成功即用其
  Secret/mesh/meta，并写入 ctx（`ActorFrom`/`MeshFrom`/`EntryIDFrom` 供 renew 等端点用）。
- 无 v1 校验路径（决定：废弃 v1）；`sproxysig.Verify` 按 `h.Version` 分支（只接受 v2）。
- 唯一 skey-id 缺省豁免：自 renew 引导（`ParseHeaderAllowMissingSkeyID` + 「该 AK 唯一存活条目」）。

## 9. sclient 命令

| 命令 | 行为 |
|------|------|
| `trust renew [--mesh M]` | 调 `POST /api/credentials/{ak}/renew`（**无 ttl 参数——服务端配置控制**），用本端当前 SK 解 wrap 得到新 SK，`config set access_key_secret <newSK>`，打印新 `sk_id` 摘要 |
| `trust sk list [--ak A]` | 调 `GET /api/credentials/{ak}/sk`（缺省自己），只解密自己的 key 显示，其余 masked |
| `trust sk delete <skID>` | 调 `DELETE /api/credentials/{ak}/sk/{skID}`（普通用户注销某 SK；删除后立即失效） |
| `trust sk expire <skID> [--until <RFC3339>]` | 调 `POST /api/credentials/{ak}/sk/{skID}/expire`（设单条 SK 过期） |
| `trust ak list`（admin） | 调 `GET /api/credentials`（全量 AK 列表，admin-only） |
| `trust ak add [--owner O] [--role R]`（admin） | 调 `POST /api/credentials`（admin 预创建账号；`--role` 支持 `node`，默认 `user`，4B-1；admin AK 不可经此创建） |
| `trust ak delete <ak> [--force]`（admin） | 调 `DELETE /api/credentials/{ak}`（**二次确认**：命令交互确认 AK 名；`--force` 跳过"仍有活跃 SK"检查；admin 角色 AK 拒绝删除，I6） |
| `trust login`（4B-2，TOTP 模式） | 调 `POST /api/credentials/register|nonce|login` 全流程，解 session SK 回填三件套（详见 4B plan 任务 ⑩） |

- `pkg/client/accesskey.go`（新）领域 API（coreRequest 签名）。
- 4B-2 追加 `trust login`/查看（GA 登录取 SK），并入 trust 家族。

### 9.1 pkg/client RequestSigner seam（4B-1，DEC-D）

- `pkg/client` 新增 `type RequestSigner interface { Sign(ctx context.Context, req *http.Request) error }`——
  宿主可复用自有凭据源/签名器。
- 默认 `ConfigSigner`：用 config 的 `access_key`/`access_key_secret`/`access_key_id` 签名（当前
  `signRequest`/`sigRoundTripper` 行为，含 `WithAccessKey`/`WithAccessKeyID` 等 option 注入的字段；
  `accessKeySecret==""` 时不签名——公开端点直达）。
- **ConfigSigner 逐条承接现状 `sigRoundTripper` 全部行为（R3-M5）**：
  1. **v2 skey-id 强制**：配置了 `access_key` 但缺 `access_key_id` 且非引导态 → 报错
     「access_key_id 未配置（v2 skey-id 必传）」（client.go:1353/1436 既有语义）；
  2. **renew 引导缺段**：`allowMissingEntryID` 一次性开关放行「该 AK 唯一存活条目」引导路径
     （首次 renew 尚无 access_key_id，ParseHeaderAllowMissingSkeyID 对应语义）；
  3. **nonce + body hash（客户端计算）**：每次签名生成 16B nonce + 预计算 `body_sha256`（带 body 请求
     计算哈希，UNSIGNED 标记缺省体）；服务端流式校验属既有 authMiddleware 行为，**非 ConfigSigner 承接**（R4-M3）；
  4. **隧道 UNSIGNED body**：/tunnel 路径 body 标记 UNSIGNED（隧道 metadata 已加密，不预计算哈希）；
  5. 直连与隧道两路径（`signRequest`/`sigRoundTripper`）共用同一签名语义。
- `FileClient` 支持注入自定义 Signer（`WithRequestSigner(s RequestSigner)` option）；未注入时 =
  现状 ConfigSigner，零行为变化。**隧道路径（xfer）同样走注入的 Signer**（4B-1 补测试断言）。

## 10. 插件化与存储/KMS（4C）

> **4A 落地后现状（2026-09-06）**：`CredentialStorer`/`SecureStorer` 接口**均未实现**——4A 只有具体
> `server.CredentialStore`（文件 store）。这是原计划「4A 预留接口」的未落地缺口，**4C 第一步必须是
> 接口提取**（core 只持接口 + 默认文件实现 + 注入链改持接口），再叠加加密静态存储与 KMS 插件。
> **`Authenticator` 接口已在 4B-1 定义**（DEC-C，§7.2），4C 不再重复提取认证面——注册表只管
> storer/secure 类。4C 执行计划见
> `docs/superpowers/plans/2026-09-06-credentials-4c-kms-plugin.md`。

- **第一步（接口提取，纯重构零回归）**：
  - `CredentialStorer` 接口（`Load() ([]accesskey.Key, error)` / `Save(keys []accesskey.Key) error`）——
    放 `pkg/accesskey`（类型只依赖 accesskey.Key，core 域自包含）；`server.CredentialStore` 收敛为
    **默认文件实现**（`NewCredentialStore(metaDir)` 实现该接口，零行为变化）。
  - `SecureStorer` 接口（加密静态存储：`Encrypt`/`Decrypt` 或更上层 Save/Load）+ **明文默认实现**。
  - 注入链 `bootstrapCredentials`/`RegisterRoutesOpts.CredentialStore`/`Handlers.credentialStore`
    由 `*CredentialStore` 改为接口类型。
  - **4C 不定义 `AuthenticatorPlugin`**——4B-1 已定义 `Authenticator` 接口，宿主经
    `RegisterRoutesOpts.Authenticators` 注入；注册表只管 `CredentialStorer`/`SecureStorer` 类。
- **第二步（加密静态存储）**：`SecureStorer` 加密实现——master key 派生（HKDF，key 来自配置/环境/
  KMS）、信封加密（AES-256-GCM）；credentials.json 落盘加密（可选开）。
- **第三步（外部插件注册）**：TOTP/KMS 实现放独立子 module（仿 `pkg/tunnel/xfer/ext/*` 结构，
  独立 go.mod + `replace github.com/cocomhub/sproxy => ../../../..`，并加入根 `go.work`），
  core 只持接口。4C 提供 KMS 插件骨架（`pkg/accesskey/ext/kms/`）。**三方 TOTP/QR 库的家 =
  `pkg/accesskey/ext/auth/totp/`**（服务端渲染 QR 等三方依赖进该子 module，主 go.mod 零新增，D7）。
- **调用面纪律**：4C 加密/KMS 插件的生成/解析/包裹一律只调 `pkg/accesskey` 导出
  （`DeriveWrapKey`/`EncryptSecret`/`DecryptSecret` 等），禁止自行实现。
- **设计分析（4A 附，结论不阻塞实现）**：加密静态 vs 内存威胁 vs KMS 必要——credentialstore.json
  已含明文 SK，4B-2 起另有 **TOTPSecret** 与 SK 同级明文落盘（base64，M12）；静态加密主要防磁盘泄露。
  KMS 解决 key 管理（轮换/吊销/审计），单机自签部署非必需。4C `SecureStorer` 加密实现**一并覆盖
  TOTPSecret**——加密的是整个 `credentialsFile`（含 TOTPSecret），见 4C plan §0 fact #6。

## 11. config 变更

- **移除** `access_keys` 配置段（yaml 废弃）。authMiddleware 由 store 驱动。
- **新增** `registration.disable`（默认 **false** 表「允许注册」；禁止注册需显式 `true`——`disable=false`=允许注册，字段名表达的是「是否禁用注册」而非「是否允许」，避免"allow=false 表示允许"的反直觉）
  与 `registration.admin` 概念（首个注册用户即 admin，无法新增其他 admin）。
- **新增（4B-1，DEC-B）** `registration.force_totp`（默认 **false**）：false=register 双模式中的**简单
  AK/SK 模式**（返回明文 SK）；true=TOTP 模式（无 SK，走登录）。**切换只影响后续注册；既有账号凭据
  形态不变（TOTP 账号继续登录取 SK，简单账号继续用 SK，不互转，R3-M3）**。
- **新增（4B-2）** `registration.session_ttl`（默认 24h，web 登录 session SK）与 `registration.cli_ttl`（默认 **7d**，cli 登录 session SK——较 renew 更保守，长命仍靠 `trust renew`）；TTL 仅服务端控——登录端点白名单无 ttl 字段，`login_type` 仅选择服务端控 TTL（未知值 → 400）。
- **新增（4B-2，U4）** `registration.login_fail_limit`（默认 5，per-AK 连续登录失败上限）与 `registration.login_fail_window`（默认 15m，锁定窗口）——某 AK 在窗口内连续失败达上限后，锁定期内该 AK 登录一律 401（含正确动态码）。
- `api_keys` 保留（Bearer，独立），互斥优先。
- `allow_insecure_loopback`（默认 false：零凭据窗口内回环也仅 healthz；true 放行回环读取；U3——register 端点无 admin 时恒回环，不受本配置影响）。
- `credential_ttl`（renew 默认 30d，上限可配；客户端不可传 ttl。4A 曾同时作为首启 anonymous 引导凭据有效期，4B 已移除该用途，U3）。

## 12. 验证

- `go test -race ./pkg/accesskey/... ./pkg/server/... ./pkg/tunnel/hub/... ./cmd/sproxy/... ./cmd/sclient/...`
- `make lint`（含子 module）+ `make build-all` + `make test-all` + `make check-loopback`
- 手测：零凭据启动（U3）+ 首 admin 经回环注册（U2）+ `allow_insecure_loopback` 兜底；简单 AK/SK 注册
  （force_totp=false 下发明文 SK）；TOTP 注册/登录（force_totp=true，GA 扫码 + session SK）；renew 出新旧 SK 并发、客户端不可传 ttl；
  GET 只取指定 AK 的 SK；全量列表 admin-only；旧 SK 过期自动失效；SIGHUP 不再重载 access_keys；
  hub 注册用新 SK 成功；浏览器/CLI trust 命令全流程。

## 13. 范围外（后续项）

- **4B（plan 已出，拆三 PR）**：4B-1 = 认证 seam + Role + 简单 AK/SK 注册；4B-2 = TOTP 插件
  （`pkg/otp` + force_totp + register TOTP 分支 + nonce/login + trust login）；4B-3 = Web 注册/登录页 +
  客户端 JS QR。
- **账号注销（4B 附带）**：普通用户无 AK 级删除入口，仅可删除自己的 SK 条目；AK（账号）删除是 admin
  专属 + 二次确认。完整「注销账号」流程（含 **TOTP secret 清理**——随账号注销删除 `<tenant>/meta/
  credentials.json` 中的 TOTP 字段、meta 保留策略）列为 4B 收尾或后续项。
- **mesh node 用户类型完整实现**：4B-1 仅接 requireRole 文件组门禁（`Role∈{user,admin}`）；node 账号
  的 mesh/hub 路由门禁（`Role∈{node,admin}`）标注接线点，完整 mesh node 用户类型（node 账号文件访问
  策略、发现/中继权限细分）列后续 PR。
- **4C（plan 已出）**：接口提取（CredentialStorer/SecureStorer，第一步；Authenticator 已在 4B-1 定义）、
  KMS/密钥管理器插件（`pkg/accesskey/ext/kms/`）、三方 TOTP/QR 库家（`pkg/accesskey/ext/auth/totp/`）、
  加密静态存储（SecureStorer 加密实现）、（账号存储与 meta 细节）。
- AK 轮换（owner 延续）、SK 到期告警。

## 14. 与现有模式一致性

| 项目惯例 | 本设计落点 |
|----------|-----------|
| configMu + COW + Store | renew/expire/delete 后 ring COW + store 写回 |
| authMiddleware 保护管理端点 | /api/credentials/* 全部挂 authMiddleware（register/nonce/login 公开豁免） |
| RecordAudit | credential_register/renew/expire/delete/login 入审计 |
| 纯 stdlib + slog | pkg/accesskey 纯 stdlib（crypto/aes+cipher/hkdf）；pkg/otp 纯 stdlib（D7） |
| cmd 薄逻辑 | sclient trust 只做 flag+IO+解密，领域 API 在 pkg/client |
| 插件化 | 4C 外部插件注册；4A 接口 + 默认文件实现；4B-1 Authenticator 链 + 宿主注入 |
