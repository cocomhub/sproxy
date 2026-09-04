# AccessKeys→Store 凭据架构重塑 + SK 轮换 + 首次连接安全（TOTP）

日期：2026-09-04
状态：多轮头脑风暴收口（草稿，等待逐节对齐后定稿）

## 0. 范围总览

本次设计是**阶段 6 任务 4 的扩大版**——从「SDK 轮换」扩张为「凭据架构从静态 yaml 迁移到 store +
运行时 SK 轮换 + 首次连接安全（TOTP 注册/登录）」。拆分为三个 PR：

- **4A（本轮）**：凭据架构重塑——yaml `access_keys` 废弃改为 store 独立存储（用户 meta 下）、
  SK 多条目模型、运行时 SK 轮换（renew）+ 管理 API + sclient trust 命令、签名 v2、
  插件/存储/加密预留接口。
- **4B（后续 PR）**：首次连接安全——Web 注册（GA/TOTP）+ 登录动态码解密下发 SK +
  sclient login/查看 + AK→多 SK（session SK）+ 服务端控过期。
- **4C（后续 PR）**：密码管理器/KMS 插件、加密静态存储、（实现细节后续 PR 细化）。

> 用户明确：「预留设计给新方案接入」——4A 必须把 4B/4C 需要的钩子（多 SK 模型、WrapScheme、
> meta/审计、服务端控过期、插件/存储接口）一次性定型。

## 1. 背景与动机

当前 `access_keys` 是静态装配期配置：`sclient access-key create` 唯一入口，服务端仅启动时读取一次，
SIGHUP 不重载，hub.Authenticator 持有无锁静态切片。无生命周期管理。

**动机（用户决策）**：定期合规轮换（30/90 天），平滑无感过渡 + TTL 自动过期 + 持久化。
后续再补首次连接安全（TOTP 登录）。

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
| 初始凭据 | **首次启动自动生成 anonymous 用户 SK**，唯一一次明文打印到启动日志，供初始访问。 |
| 禁止注册 | 配置名调整确保**默认为 false=允许注册**；禁止注册需显式 true。 |
| 服务端控过期 | SK 过期时间仅服务端控制，按场景（web/cli）设 TTL；客户端不能生成长期有效 SK 降低安全性。 |
| verify 回退 | **无回退，只查 store/ring**（yaml 已废弃，无静态段）。**无认证凭据时仅回环放行**（ls/stat 等仅跨 127.0.0.1 放行，`allow_insecure_loopback` 默认 false→其余 401）。 |
| renew 客户端不控 TTL | SK 过期仅服务端配置控制（renew 默认 30d 上限；客户端 `--ttl` 移除，仅服务端配）。 |
| GET 只取指定 AK 的 SK 列表 | 不暴露全部 AK 的 SK；缺省按调用方自己的 AK，指定他 AK 需 admin。 |
| 全 AK 列表管理端点仅 admin | 获取所有 AK 列表等管理端点只允许 admin AK 用户；**首个 TOTP 注册用户即 admin**，无法新增其他 admin。 |
| 信令/tunnel/hub Secret | **统一查 store**（单一事实源）。 |
| 自加密 | 信封加密：wrap key 由调用方自己的 SK 派生（HKDF-SHA256），AES-256-GCM 包裹新 SK/逐条 SK。 |

### 2.3 首次连接安全（4B，后续 PR，本次仅预留）

| 决策点 | 结论 |
|--------|------|
| 注册方式 | Web 注册选择类型，本只实现 GA/TOTP。服务端生成 AK + TOTP secret，返回 AK 与 otpauth URI 二维码。 |
| 登录取 SK | 先注册后在服务端生成新 SK（session TTL），用 TOTP 动态码 + 服务端 nonce 经 HKDF 派生 wrap key，AES-GCM 加密，页面输入动态码解密保存。 |
| sclient 登录 | 选择 GA 后手动输入密钥；重构旧 access-key 命令提供 login/查看；按 AK 动态码加密取新 SK。 |
| AK 多 SK | AK 允许同时多个 SK（不同 session TTL），过期自动失效。 |
| 轮换策略 | web 不自动轮换、过期重生成；sclient 长期运行定期轮换、请求发现过期即重生成。 |
| 服务端控过期 | 不同场景生成的 SK 过期时间**仅服务端控制**，避免客户端生成长期有效 SK。 |
| 登录码机制 | **确认**：动态码 + nonce 派生 wrap key（限频 + 短 TTL + nonce 每登各异）；SK 明文服务端存储（为验签）。 |
| 文件权限 | 服务端至少限制文件访问权限（配合 SK 过期）。 |
| 加密存储/KMS | **4A 只做设计分析 + 预留接口（SecureStorer/KMS），不实现**；KMS/平台密钥管理器 4C 再接入。 |
| 插件化粒度 | **外部插件注册**：TOTP/KMS/平台登录实现放独立子 module / 外部注册，core 只持接口，避免污染主包 go.mod。 |

## 3. 核心原则

### 3.1 SK 轮换 vs AK 轮换
只轮换 SK，AK 稳定作身份锚（Actor/owner 以 AK 为身份，数据连续）。AK 轮换记后续。

### 3.2 单一事实源
Store 是唯一凭据源。启动把 store 载入 Ring；auth/hub/信令/tunnel 全部查 Ring。不维护 cfgPtr.AccessKeys 静态段。

## 4. 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                 accesskey.Ring (运行时权威凭据源)            │
│  map[AK] → []SKEntry { SK, Kind, WrapKeyID, CreatedAt,      │
│                       ExpiresAt, Status, Meta{type,ip} }     │
│  原子性：方法级锁；快照 COW；读取时比对当前时间               │
└──────────────────────────┬──────────────────────────────────┘
                          │ 同一实例注入
        ┌─────────────────┴─────────────────────┐
        ▼                                       ▼
  HTTP 面 auth                         hub.Authenticator
  VerifySproxySigMulti                （共享 ring，无 SetAccessKeys 需要）
  （信令/tunnel 派生密钥也查 store）

  Store（持久化）：
  storage_root/<tenant>/meta/accounts.json
  ├── accounts[{ak, owner, meta(注册信息), totp?}]      // 4B: totp secret
  └── keys[{sk, kind, wrap_key_id, created, expires, status, meta}]
  SecureStorer 接口：默认文件实现；KMS/加密 4C 接入
```

## 5. pkg/accesskey 包设计

### 5.1 数据模型（AK→多 SK）

```go
type Kind  string        // plain | secret_wrap | totp_wrap(4B)
type Status string       // active | expired | disabled

type Key struct {
    AK        string   // 身份锚，sk[-<mesh>]-<16hex>
    Mesh      string   // 由 AccessKeyMesh(AK) 导出，不存字段避免漂移
    Owner     string   // 稳定 owner（默认 = AK；4B 注册用户 = 用户名）
    Entries   []SKEntry // 该 AK 的全部 SK 条目（多 SK 并存）
}

type SKEntry struct {
    SK        []byte    // 明文 SK（服务端内部）
    Kind      Kind
    WrapKeyID string    // Kind==secret_wrap 时包裹本 SK 的 wrap key 的 AK
    CreatedAt time.Time
    ExpiresAt time.Time // 零值=永不过期（服务端控）
    Status    Status
    Meta      Meta
}

type Meta struct {
    Type string // login_type: config|renew|register|login… (4B: web/cli/register)
    IP   string // 生成时来源 IP（审计/溯源）
    // 4B extends: device/user_agent 等
}
```

### 5.2 Ring 原子集合

```go
type Ring struct { mu sync.RWMutex; m map[string]*Key; now func() time.Time }
```

- `NewRing(now ...func() time.Time) *Ring`
- `UpsertAK(k *Key) error` — 覆盖 AK 条目（保留已有多 SK 条目）
- `AddKey(ak string, e SKEntry) error` — **追加**一条 SK 条目（AK 未知→错误）；生成 `SKEntry.ID`（`sk-<12hex>`）
- `DeleteAK(ak string) bool` — 删除整个 AK
- `DeleteKey(ak, skID string) error` — 删特定 SK 条目（skID 不存在→错误/404）
- `ExpireKey(ak, skID string, until time.Time) error` — 设某条 SK 过期
- `Lookup(ak string) ([]SKEntry, bool)` — **读取时过滤已过期条目**（now≥ExpiresAt 剔除）
- `CoreEntry(ak string) *SKEntry` — 供信令/tunnel/renew 取当前有效主条目（最新未过期）
- `GetEntry(ak, skID string) (*SKEntry, error)` — 按 entryID 精确取（签名 v2 校验用）
- `Snapshot() []*Key` — 全部快照（copy，按 AK 排序）
- `Replace(ks []Key)` / `Len()`

**原子性关键**：所有读加读锁；写加写锁；`Expire`/`Lookup` 共用同一 `now` 基准，
杜绝「已过期仍被验证」窗口。

### 5.3 服务端控过期（核心护栏）

- renew（SK 轮换）给新 SK 设 `ExpiresAt = now + ttl`（默认 30d，`--ttl` 可调，**上限来自配置**，
  客户端不能自行设超过服务端上限）。
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

### 6.3 失败模式
- 客户端用的旧 SK 与当前核心 SK 不一致 → wrap key 错 → 解密失败 → 报错提示 `--access-key-secret` 更新。
- 服务端权威操作不因客户端解密失败回滚；客户端在 renew 前可先 list 解密当前 SK 校验一致性。
- **4B 登录码**：动态码+nonce 派生 wrap key；6 位码熵较低 → 服务端限频 + 短 TTL + nonce 每登各异 + ±1 窗口容差。

### 6.4 安全边界（诚实声明）
- 服务端为验签必须持明文 SK → 自加密不防服务端泄露，防「同 mesh 内 SK 相互隔离」+「SK 明文不落日志」。
- renew 的「只用旧 SK 包新 SK」：只有持当前有效 SK 者能轮换出可用新 SK。

## 7. 服务端装配（pkg/server + cmd/sproxy）

### 7.1 装配流程（4A）

```
cmd/sproxy/root.go（启动）:
  store := credentialstore.NewFileStore(filepath.Join(cfg.StorageRoot, "_meta_", "credentials.json"))  // 多租户: <tenant>/meta/credentials.json
  ring := accesskey.NewRing()
  // 首启：store 为空 → 自动生成 anonymous 初始 SK，写 store + 日志明示
  ring.Replace(loadFromStore(store))
  h := RegisterRoutes(..., CredentialRing: ring, CredentialStore: store)   // HTTP 认证查 ring
  hubAuth := hub.NewAuthenticator(ring)                                     // 共享同一实例
```

- `RegisterRoutesOpts.CredentialRing *accesskey.Ring`（新字段，nil 时内部自建并灌 store/首启生成）。
- `hub.NewAuthenticator(r *accesskey.Ring)`（改签名：从 []AccessKey 改接收 ring）。

### 7.2 auth.go 认证改造（统一查表）
- `verifySproxySig` → `VerifySproxySigMulti`：先查 ring（多条目），`CoreEntry` 取当前有效主条目
  验签；**无 cfgPtr.AccessKeys 回退**（yaml 已废弃）。
- 签名 v2（见 8）升级后 verify 用 v2 canonical；不再有 v1 兼容。
- authMiddleware 的 api_keys Bearer 优先互斥保留（Enabled → api_keys 路径，不查 store）。

### 7.3 hub.Authenticator 改造
- 结构改为 `{ ring *accesskey.Ring; noncePool }`（替换 accessKeys []AccessKey）。
- `Authenticate` → ring 查（含过期检查）+ nonce 防重放 + HMAC proof。
- 无需 SetAccessKeys：共享同一 ring 实例。

### 7.4 管理端点（主 mux + authMiddleware 保护，仿 PUT /api/config 样板）

> **模型前提**：AK=用户身份（稳定），SK=该身份下的多条目（可过期/删除）。因此「删除」语义
> 必须落在 **SK 条目**上（普通用户层面），而非整个 AK。**账号（AK）注销暂不支持**（范围外，
> 见 13 节）；admin 才可删除整个 AK，且需**二次确认**。

> **权限分档**：
> - **普通凭据用户（role=user）**：可 `renew` / `GET` 自己的 AK 的 SK 列表 / `expire` 自己的 SK /
>   `DELETE` 自己的 SK 条目。**不能**看全量、**不能**删 AK、**不能**注销账号。
> - **admin（4B 首个 TOTP 注册用户，无法新增其他 admin）**：除普通能力外，可 `GET` 全部 AK 列表、
>   可 `POST /api/credentials` 新增 key、可 `expire`/`DELETE` 任意 AK（删除 AK 需二次确认）。
>   admin 判定：凭据条目 `Role=admin`（4A 阶段无 admin——anonymous 为 user，admin-only 端点 4A 返回 403）。

| 端点 | 权限 | 行为 |
|------|------|------|
| `POST /api/credentials/{ak}/renew` body `{mesh?}` | 本人 | 调用方当前 AK 追加新 SK 条目（多 SK 模型：旧条目保留至各自 ExpiresAt）。用调用方 SK 派生 wrap key 加密新 SK（Kind=secret_wrap），store 持久化 + RecordAudit(credential_renew)。**TTL 仅服务端配置控制（客户端不可传）**。返回 `{ak, sk_id, wrapped_secret(b64), kind, wrap_key_ak, expires_at}` |
| `GET /api/credentials/{ak}/sk` | 本人（`{ak}`=自己）；admin（任意） | 返回**指定 AK 的 SK 列表**（含行 `sk_id/created/expires/status`，每条需各自 wrap key 解密）。不暴露全部 AK 的 SK；缺省=调用方自己 |
| `DELETE /api/credentials/{ak}/sk/{skID}` | 本人；admin（任意） | **删除单个 SK 条目**（普通用户注销 SK 的唯一手段；删除后该 SK 立即失效，不再可信）。幂等（不存在返回 404）。RecordAudit(credential_sk_delete)。**不支持账号注销（无 AK 级删除入口给普通用户）** |
| `POST /api/credentials/{ak}/sk/{skID}/expire` body `{until}` | 本人；admin（任意） | 设**单个 SK 条目**过期（RFC3339；空=永不过期）；用于双 SK 并存窗口裁切。RecordAudit(credential_expire) |
| `GET /api/credentials`（无参，admin-only） | admin | 全部 AK 列表（admin 才能看全量；每个 AK 含其活跃 SK 数/摘要，不下发明文 SK） |
| `POST /api/credentials` body `{ak, owner, role?}` | admin | 新增 AK（4B 注册用；4A 仅 admin 预创建可选） |
| `DELETE /api/credentials/{ak}` | **admin 专属 + 二次确认** | 删除整个 AK（身份+全部 SK 条目+关联 meta）。body 须带 `{confirm: "<ak>", force?: bool}`（确认串=目标 AK 名）；未提供/不匹配 → 400。RecordAudit(credential_ak_delete)。**普通用户无此能力** |

> **二次确认设计**：`DELETE /api/credentials/{ak}` body 必须含 `confirm` 字段等于目标 AK 字符串；
> 且若该 AK 仍有活跃（未过期）SK 条目，默认拒绝，需 `force: true` 才允许（防误删在用的凭据）。
> 确认串为 AK 名本身——要求调用方明确显式意图，防脚本/误触。

全部变更 `RecordAudit`（action=credential_renew / credential_add / credential_ak_delete /
credential_sk_delete / credential_expire）。调用方须为合法持有者（authMiddleware 验签通过即证持
SK；admin 校验交给 handler，按 `ActorFrom(ctx)` 查其 AK 的 role）。

### 7.5 持久化（store 文件）
- 写入时机：renew/add/expire/delete 后 → 同步 store（COW 更新 ring 后写 store）。
- 多租户：`storage_root/<tenant>/meta/credentials.json`（沿用 meta 桶），账户与密钥同文件
  （或拆 accounts/keys 两文件，视 4B）。
- **写原子替换**（临时文件 + rename），失败 RecordAudit(credential_persist_error) + 不丢内存态。
- **SIGHUP**：不再重载 access_keys（yaml 已废弃），该重载逻辑删除；CLAUDE.md 更新。
- **首启自动生成 anonymous**：store 为空 → 生成（`registration.disable: false` 亦生成——服务端已有
  凭据才能被客户端信任；然后提示以 anonymous AK 登录/注册）。anonymous 为**非 admin**（role=user），
  仅可访问自身 AK 的 SK 列表，不能看全量。
- **admin 角色 4B 产生**：4A 阶段 store 无 admin（anonymous=user，renew 只产生群组），admin-only
  管理端点（全量列表/加 key）在 4A 暂对 non-admin 拒绝（返回 403）——首个 TOTP 注册用户即 admin（4B）。
- **凭据按租户隔离**：`<tenant>/meta/credentials.json` 每租户独立；AK 属于单一租户，认证后 ctx 带
  租户 scope，文件操作限该租户（multi-tenant 隔离自然成立）。

### 7.6 无认证凭据兜底（allow_insecure_loopback）
- 无任何凭据（store 为空、api_keys 未启用、未生成 anonymous——如显式禁用）时，HTTP 面**仅对
  127.0.0.1/::1 回环放行**（本地运维 ls/stat/healthz），其余请求 401。
- 新配置 `allow_insecure_loopback`（默认 **false**：连回环也只放行 healthz/version；显式 true 才
  放行读取类操作如 ls/stat）。无凭据时若 `allow_insecure_loopback=true` → 回环放行读操作；否则全 401。

> 语义澄清：默认 `false` = 最严格（无凭据时回环也不放读取，仅 healthz）；运维本地调试显式开 true
> 才获得回环读取放行。authMiddleware 在「无任何凭据可查」时走此分支。

## 8. 签名 v2

### 8.1 协议（v2 定稿）

**Header**：
```
SproxySig v=2 ak=<AK> sk=<entryID> ts=<unix_ms> exp=<unix_ms> nonce=<16B hex> body_sha256=<hex|UNSIGNED> sig=<hex>
```

**entryID = SK 条目 ID**：`sk-<12hex>`（SKEntry.ID，ring 条目唯一标识，创建时生成）。
多 SK 并存时精确锁定被验条目；服务端先按 `ak`+`sk` 定位条目再取该条 SK 验签。

**Canonical（v2）**：
```
sproxy-sig/v2
<ak>
<sk>             ← 新增条目 ID 段（定位精确条目，避免歧义）
<ts>
<exp>
<nonce>
<method>
<path>
<query>
<body_sha256>
```
签名仍 = `HMAC-SHA256(SK, canonical_v2)`。**废弃 v1**（无存量 client，破坏性升级可接受）。

> 备注：`sk`（entryID）段是唯一相对 v1 的 canonical 增加；entryID 键名与位置可在 writing-plans
> 前据实现微调，但「v2 + entryID 精确定位」已定稿。

### 8.2 升级后服务端校验
- 解析 v2 头 → `ak`+`sk` 精确取 SKEntry → 用该条 SK 验签 → 成功即用其 Secret/mesh/meta。
- 无 v1 校验路径（决定：废弃 v1）；sproxysig.Verify 改收 SKEntry 或 (sk, entryID)。

## 9. sclient 命令

| 命令 | 行为 |
|------|------|
| `trust renew [--mesh M]` | 调 `POST /api/credentials/{ak}/renew`（**无 ttl 参数——服务端配置控制**），用本端当前 SK 解 wrap 得到新 SK，`config set access_key_secret <newSK>`，打印新 `sk_id` 摘要 |
| `trust sk list [--ak A]` | 调 `GET /api/credentials/{ak}/sk`（缺省自己），只解密自己的 key 显示，其余 masked |
| `trust sk delete <skID>` | 调 `DELETE /api/credentials/{ak}/sk/{skID}`（普通用户注销某 SK；删除后立即失效） |
| `trust sk expire <skID> [--until <RFC3339>]` | 调 `POST /api/credentials/{ak}/sk/{skID}/expire`（设单条 SK 过期） |
| `trust ak list`（admin） | 调 `GET /api/credentials`（全量 AK 列表，admin-only） |
| `trust ak add [--owner O]`（admin） | 调 `POST /api/credentials`（admin 预创建 4B 前的账号） |
| `trust ak delete <ak> [--force]`（admin） | 调 `DELETE /api/credentials/{ak}`（**二次确认**：命令交互确认 AK 名；`--force` 跳过"仍有活跃 SK"检查） |

- `pkg/client/accesskey.go`（新）领域 API（coreRequest 签名）。
- 4B 追加 `trust login`/查看（GA 登录取 SK）。

## 10. 插件化与存储/KMS（4C 预留，4A 仅接口）

- **core 只持接口**：`CredentialStorer`（Load/Save）+ `AuthenticatorPlugin`（注册/登录/验证）。
- TOTP / KMS / 平台密钥管理器实现放**独立子 module（外部插件注册）**，避免污染主包 go.mod。
- 4A 仅文件 store（core 内默认实现）+ 接口 + 注入链就绪；不实现 KMS/加密。
- **设计分析（4A 附）**：加密静态 vs 内存威胁 vs KMS 必要——结论不阻塞 4A 实现。

## 11. config 变更

- **移除** `access_keys` 配置段（yaml 废弃）。authMiddleware 由 store 驱动。
- **新增** `registration.disable`（默认 **false** 表「允许注册」；禁止注册需显式 `true`——`disable=false`=允许注册，字段名表达的是「是否禁用注册」而非「是否允许」，避免"allow=false 表示允许"的反直觉）
  与 `registration.admin` 概念（首个 TOTP 注册用户即 admin，无法新增其他 admin）。
- `api_keys` 保留（Bearer，独立），互斥优先。
- `allow_insecure_loopback`（默认 false：无凭据时回环也仅 healthz；true 放行回环读取）。
- 8.x 服务端 TTL 上限配置（renew 默认 30d，上限可配；客户端不可传 ttl）。

## 12. 验证

- `go test -race ./pkg/accesskey/... ./pkg/server/... ./pkg/tunnel/hub/... ./cmd/sproxy/... ./cmd/sclient/...`
- `make lint`（含子 module）+ `make build-all` + `make test-all` + `make check-loopback`
- 手测：首启 anonymous 凭据 + `allow_insecure_loopback` 兜底；renew 出新旧 SK 并发、客户端不可传 ttl；
  GET 只取指定 AK 的 SK；全量列表 admin-only；旧 SK 过期自动失效；SIGHUP 不再重载 access_keys；
  hub 注册用新 SK 成功；浏览器/CLI trust 命令全流程。

## 13. 范围外（后续项）

- **4B**：TOTP 注册/登录、Web 界面、sclient login、session SK、服务端控过期细化。
- **账号注销（本 PR 不支持）**：普通用户无 AK 级删除入口，仅可删除自己的 SK 条目；AK（账号）删除
  是 admin 专属 + 二次确认。完整「注销账号」流程（含 TOTP secret 清理、meta 保留策略）列为后续项。
- **4C**：KMS/密钥管理器插件、加密静态存储、（账号存储与 meta 细节）。
- AK 轮换（owner 延续）、SK 到期告警。

## 14. 与现有模式一致性

| 项目惯例 | 本设计落点 |
|----------|-----------|
| configMu + COW + Store | renew/expire/delete 后 ring COW + store 写回 |
| authMiddleware 保护管理端点 | /api/credentials/* 全部挂 authMiddleware |
| RecordAudit | credential_renew/expire/delete 入审计 |
| 纯 stdlib + slog | pkg/accesskey 纯 stdlib（crypto/aes+cipher/hkdf） |
| cmd 薄逻辑 | sclient trust 只做 flag+IO+解密，领域 API 在 pkg/client |
| 插件化 | 4C 外部插件注册；4A 接口 + 默认文件实现 |
