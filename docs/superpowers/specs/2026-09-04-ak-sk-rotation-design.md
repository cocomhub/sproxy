# AK/SK 轮换（SK 轮换 + 自加密传递）设计

日期：2026-09-04
状态：已批准（用户三轮决策收口）

## 1. 背景与动机

sproxy 生产可用性阶段 6 任务 4（PR-D）。当前 `access_keys` 是静态装配期配置：
旧 `sclient access-key create`（2026-09 凭据 store 化后已删除，由 `trust ak add` 生成注册取代）
唯一入口，服务端仅启动时读取一次，SIGHUP 不重载 access_keys，
hub.Authenticator 持有无锁静态切片。无任何 rotate / expire / delete / 多密钥生命周期管理。

**动机（用户决策）**：定期合规轮换（30/90 天换钥），而非泄密应急响应。因此设计重心在
「平滑无感过渡窗口 + TTL 自动过期 + 持久化写回」，而非「紧急吊销的最短路径」（吊销能力仍提供，但非主角）。

## 2. 用户决策记录（头脑风暴三轮收口）

| 决策点 | 结论 | 依据 |
|--------|------|------|
| 轮换动机 | **定期合规轮换** | 重点：TTL 自动过期、过期提醒、双 SK 平滑过渡 |
| 过渡窗口 | **新旧双 SK 并发** | rotate 后新旧同时有效，旧 key 期满自动失效，sclient/服务端无需精确同步 |
| 持久化 | **持久化写回配置** | rotate 写回 access_keys 配置（YAML），重启存活 |
| 架构 | **方案 A：配置段即凭据源** + **新增独立 pkg 管理 key 类型** | 用户要求提炼 pkg 包管理 key 类型，原子性使用，hub 与 HTTP 统一接入，避免凭据源不一致 |
| pkg 命名 | **`pkg/accesskey`** | 容纳 Key 类型 + Ring 原子集合；sproxysig 继续只做签名协议，职责互补 |
| hub 接入 | **共享同一实例** | `NewAuthenticator(r *accesskey.Ring)`——hub 与 HTTP 认证持有物理同一个 Ring，单一凭据源，零同步成本、零不一致窗口 |
| AK 是否轮换 | **只轮换 SK（AK 稳定作身份）** | 轮换后数据/审计/存储按 AK 视角连续可见；换 AK 破坏数据连续性；SK 才是秘密，轮换 SK 已足够保证安全 |
| 自加密范围 | **rotate + list 全覆盖** | rotate 返回的新 SK 用调用方旧 SK 派生 wrap key 加密；list 每条 SK 用该 key 自身 wrap key 加密（无对应 SK 无法解密，同 mesh 多客户端互不见彼此 SK） |

## 3. 核心原则：SK 轮换 vs AK 轮换

**只轮换 SK，AK 保持稳定作为身份锚。**

- `Actor`（审计）、`owner`（多租户存储/配额）均以 **AK 字符串**为身份。AK 不变 → 轮换后
  旧数据按 AK 视角连续可见，零迁移成本。
- SK 是秘密，AK 是公开标识符。安全来自换 SK；AK 轮换只在「AK 本身被泄露 / 需退役」时才有意义，
  代价是数据连续性。**本轮不实现 AK 轮换**（range 外，记录为后续项）。

## 4. 架构总览

```
┌─────────────────────────────────────────────────────────────┐
│                 accesskey.Ring (唯一权威凭据源)             │
│  ┌───────────────────────────────────────────────────────┐  │
│  │  Key{ AK, SK, Mesh, Owner, Status, CreatedAt,         │  │
│  │       ExpiresAt }  +  RWMutex                          │  │
│  │  Upsert / Delete / Expire / Lookup / Snapshot /       │  │
│  │  Replace / Len / Map                                   │  │
│  └───────────────────────────────────────────────────────┘  │
│  原子性：方法级锁；快照 COW；Expire/Lookup 读取时比对当前时间  │
└──────────────────────────┬──────────────────────────────────┘
                          │ 同一实例注入
        ┌─────────────────┴─────────────────────┐
        ▼                                       ▼
  HTTP 面认证                              hub.Authenticator
  auth.go verifySproxySig                 (pkg/tunnel/hub/auth.go)
  keyring.Lookup 统一查表                 Authenticate 实时 ring.Lookup
  （过期检查内建）                         （无锁静态切片 → 共享 ring）
```

**关键设计决策**：不维护「cfgPtr.AccessKeys 静态段 + KeyRing 动态段」两套表做二次查询；
而是**以 Ring 为唯一权威表**——启动时把静态 `cfg.AccessKeys` 灌入 Ring，此后所有
rotate/expire/delete 直接改 Ring；`cfgPtr` 仅作为持久化镜像（写回 YAML 用）与
config 响应展示。auth/hub 都只查 Ring，**不存在两个凭据源，物理上杜绝不一致**。

## 5. pkg/accesskey 包设计

### 5.1 Key 类型

```go
// Kind 明文 / SecretWrap 两个枚举，见「SK 自加密传递」。
type Key struct {
    AK        string    // 身份锚：sk[-<mesh>]-<16hex>，永不轮换
    Mesh      string    //          mesh，AccessKeyMesh(AK) 导出，不存字段避免漂移
    Owner     string    //         稳定 owner（默认 = AK）
    Secret    []byte    //         32B 明文 SK（仅供服务端内部）；对外序列化时经加密
    Kind      Kind      //         Plain | SecretWrap
    WrapKeyID string    //         若 Kind==SecretWrap，包裹本 SK 的 wrap key 的 AK
    CreatedAt time.Time //         创建时间
    ExpiresAt time.Time //         过期时间（零值 = 永不过期）
    Status    Status    //         Active | Expired | Disabled
}
```

- **不存 Mesh 字段**：Mesh 由 `tunnel.AccessKeyMesh(AK)` 从 AK 派生，避免配置漂移。
- **Owner 默认 = AK**：向后兼容现有数据（审计 actor/存储 owner 都是 AK 字符串）。
  跨 AK 轮换未来落地时，新建 key 可显式 owner。

### 5.2 Ring 原子集合

```go
type Ring struct { mu sync.RWMutex; m map[string]*Key; now func() time.Time }
```

方法（全部加锁；`now` 可注入便于测试过期）：

- `NewRing(now ...func() time.Time) *Ring`
- `Upsert(k *Key) error` — 重复 AK 覆盖；SK 必须 32B；AK 格式校验
- `Delete(ak string) (removes bool)` — 删除 AK；不存在返回 false（幂等）
- `Expire(ak string, at time.Time) error` — 设置过期时间；`at.IsZero()` = 永不过期
- `Lookup(ak string) (*KeySnapshot, bool)` — **读取时比对当前时间**：

  ```go
  if k.ExpiresAt 非零 且 现在 ≥ ExpiresAt → 按 Expired 处理（不返回 / 标记）
  ```

- `Snapshot() []*KeySnapshot` — 全部快照（copy，按 AK 排序）
- `Replace([]Key)` / `Len()`
- `Upsert` 内部全程持写锁，杜绝部分更新可见

**关键点（保证原子性）**：所有读取（Lookup/Snapshot）加读锁；所有写入加写锁；
`Expire` 的"设过期"与 `Lookup` 的"判过期"共用同一 `now` 基准，
杜绝「一个 goroutine 已过期、另一个仍在验证」的窗口。

### 5.3 过期语义（定期合规轮换的核心护栏）

- `rotate`（SK 轮换）时给新 SK 设 `ExpiresAt = now + ttl`（默认 30d，`--ttl` 可调，上限从配置来）。

  **rotate 的精确语义（AK 稳定、换 SK）**：`POST /api/access_keys/rotate {mesh, ak?}`——在**已存在的该 AK** 上置换 SK：
  生成新 SK、`ring.Upsert` 同一 AK（覆盖 SK 字段，保留 Owner/CreatedAt 原值），旧 SK 立即从 ring 移除；
  新 SK 的 `ExpiresAt = now + ttl`。**不产生第二个 AK、不新增身份**；mesh 从调用方当前 AK 派生，`ak` 字段留空即轮换自身。

- **旧 SK 到期自动失效**：`Lookup` 读到 `now ≥ ExpiresAt` 即拒绝验证 → 无需人工 expire 调用。
- **双 SK 并发**：（正统双 SK 场景）rotate 前先 `expire 旧AK <until>` 保留旧 key 到 until，
  再 rotate 提新 SK——两者并存（各自 ExpiresAt），`Lookup` 都能命中 → 平滑过渡窗口。
  （注：单一 AK 自身无法同时持有两个 SK，因为 SK 绑定在 AK 条目上；双 SK 并发 = 新旧 **AK** 并存，
  各自的滚轮窗口由它们各自的 ExpiresAt 裁切，从而实现无停机轮换。）
- **过期的 key 自动从 ring 清理**（可选，ring 保持有界）；`Snapshot` 返回含 Status=Expired。

## 6. SK 自加密传递（信封加密）

### 6.1 动机

- `rotate` 是 **SK 明文唯一跨网络时刻**。即使 TLS 保护，仍可能被日志/代理/抓包/中间层截获明文。
- `list` 若明文下发全部 SK，持有列表读取权（如 API key 用户）即可拿到**所有** SK → 无 AK 的
  bearer 用户也能冒充任意身份。**按 key 隔离秘密可见性**是真实收益。

### 6.2 wrap key 派生

```go
// 从调用方自己的 SK 派生加密新 SK 的 wrap key（AES-256-GCM）
func wrapKey(sk []byte, context string) ([]byte, error) {
    return deriveKey(sk: 32B, salt: "sproxy-accesskey-wrap/v1\x00"+context, 32B)
}
// 派生原语 = HKDF-SHA256(secret=sk, salt=sproxy-accesskey-wrap/v1+context, info=ak)
```

- rotate 响应：`wrapKey = wrapKey(旧SK, mesh)`，`ciphertext = AES-GCM(wrapKey, 新SK)`
- list 响应：每条 `wrapKey_i = wrapKey(key_i.SK, mesh)`，`ciphertext_i = AES-GCM(wrapKey_i, key_i.SK)`
- **客户端必须持有对应 SK 才能解密** → 同 mesh 多客户端只能解出**自己的**新 SK，无法解他人 SK。
- 密文通过 JSON 下发，base64 编码。

### 6.3 失败模式（必须处理）

**旧 SK 过早过期 → 新 SK 永久不可解**（客户端在解出前可能已换 SK？不——解出发生在 rotate 响应当下，
此时旧 SK 仍有效。真正的失败模式是）：

- **客户端调 rotate 时用的"旧 SK"与服务端当前 Active SK 不一致**（客户端配置已过期/被轮换过）→ wrap key 错 → 解密失败 → 客户端应报错并提示 `--access-key-secret` 需更新。
- **确认路径**：轮换是服务端权威操作（追加新 SK 进 ring），**不因客户端解密失败而回滚**——
  服务端已持久化新 SK，客户端需用正确 SK 重新调用。设计上客户端在 rotate 前可先 `list`
  解密当前 SK 校验一致性（提前发现 mismatch），避免在 rotate 响应当天才发现。

### 6.4 安全边界（诚实声明）

- 服务端为验签必须持明文 SK → 自加密**不防服务端泄露**，是「同 mesh 内 SK 相互隔离」+「SK 明文
  不落日志/代理」的纵深防御，叠加在 TLS 之上。
- rotate 的"只用旧 SK 包新 SK"意味着：**只有持当前有效 SK 者能轮换出可用新 SK**。

## 7. 服务端装配（pkg/server + cmd/sproxy）

### 7.1 装配流程

```
cmd/sproxy/root.go（启动）:
  cfg := LoadFromProvider(...)
  ring := accesskey.NewRing()
  ring.Replace(cfg.AccessKeys → []accesskey.Key)   // 静态段灌入唯一权威表
  h := RegisterRoutes(..., AccessKeyRing: ring)     // HTTP 认证查 ring
  hubAuth := hub.NewAuthenticator(ring)             // 共享同一实例
```

- `RegisterRoutesOpts.AccessKeyRing *accesskey.Ring`（新增字段，nil 时内部自建并灌静态段，
  向后兼容测试/其他调用方）。
- `hub.NewAuthenticator(r *accesskey.Ring)`（改签名：从 `[]AccessKey` 改接收 ring；
  现有调用方只有 cmd/sproxy/root.go 与 hub 测试，需同步改）。

### 7.2 auth.go 认证改造（verifySproxySig 统一查表）

- `auth.go` 现 `for i := range cfg.AccessKeys` 线性遍历 → 改为 **先查 ring**：
  `if k, ok := h.accessKeyRing.Lookup(ak); ok { secret = k.Secret; mesh = k.Mesh }`
- **回退**：ring 未命中时回退静态 `cfg.AccessKeys`（**双查表过渡**——仅用于 SIGHUP 后 ring
  尚未同步的动态窗口保护；启动灌入后通常不触发）。`Lookup` 内建过期检查。
- authMiddleware 的 api_keys Bearer 路径不变（api_keys 与 access_keys 互斥语义保留）。

### 7.3 hub.Authenticator 改造

- 结构 `Authenticator{ ring *accesskey.Ring; noncePool }`（替换 `accessKeys []AccessKey`）。
- `Authenticate` → `ring.Lookup`（含过期检查）+ 既有 nonce 防重放 + HMAC proof 比对。
- **关键：无需 SetAccessKeys**——hub 持有与 HTTP 同一个 ring 实例，rotate 后自动可见，
  物理杜绝"HTTP 面新 key 可签、hub 注册被拒"。

### 7.4 管理端点（主 mux + authMiddleware 保护，仿 PUT /api/config 样板）

| 端点 | 行为 |
|------|------|
| `POST /api/access_keys/rotate` body `{mesh, ak?, ttl}` | **在调用方当前 AK 上置换 SK**（SK 轮换：同一 AK，Owner/CreatedAt 保留，旧 SK 移除，新 Key ExpiresAt=now+ttl）。用调用方旧 SK 派生 wrap key 加密新 SK（Kind=SecretWrap），`ring.Upsert`，`RecordAudit(access_key_rotate)`，持久化写回配置；返回 `{ak, wrapped_secret(b64), kind: secret_wrap, wrap_key_ak, expires_at}` |
| `POST /api/access_keys` body `{ak, mesh, ttl?}` | **新增 key**（携带稳定 AK+Owner，为未来跨 AK 轮换铺路——列表可达、独立 TTL） |
| `GET /api/access_keys` | 列表（每条含 wrap 密文：`{ak, mesh, status, created_at, expires_at, wrapped_secret}`——明文 SK 永不下发；无对应 SK 无法解密） |
| `POST /api/access_keys/{ak}/expire` body `{until}` | 设过期（`until` 为 RFC3339；空 = 永不过期）；过期后 Lookup 自动失效；**用于双 AK 并存窗口的裁切** |
| `DELETE /api/access_keys/{ak}` | 删除 key（幂等）；仅 ring 内存在该 AK |

全部 `RecordAudit`（action=access_key_rotate/add/expire/delete）。**调用方必须是该 key 的合法持有者**
（authMiddleware 验签通过即证明持 SK，自然满足"只有持当前有效 SK 者能操凭据"）。

### 7.5 持久化写回配置（跨重启存活）

- `rotate`/`expire`/`delete` 后 **COW 更新 cfgPtr.AccessKeys**（与 PUT /api/config 同模式：
  `cfg := *h.cfgPtr.Load()` 浅拷贝 → 改 AccessKeys → Store），并**写回 YAML**。
- 写回路径：ring 是权威表 → 把 ring.Snapshot() 序列化成 `[]AccessKeyConfig` → 写回到配置 YAML 路径。
  - 需要配置里记录 YAML 路径（`cfg.ConfigFile`，从 viper `ConfigFileUsed()` 取，若无文件只有 flag/env，则**记录 ring 变更但不写盘**，提示配置后重启持久化）。
  - 写 YAML 原子替换（临时文件 + rename），失败 `RecordAudit(access_key_persist_error)` + 不丢内存态。
- **SIGHUP 重载 access_keys**（顺手补）：`handleSighup` 对比新配置 AccessKeys 与当前 ring——
  变更时 `ring.Replace(...)`（覆盖为文件权威）。同步更新 CLAUDE.md「SIGHUP 不重载 access_keys」的说明。

## 8. sclient 命令

扩展 `cmd/sclient/access_key.go`，新增子命令（各走 SproxySig 签名）：

| 命令 | 行为 |
|------|------|
| `rotate [--mesh M] [--ttl 30d]` | 调 `POST /api/access_keys/rotate`，用**本端当前 SK** 解密返回的 wrapped_secret 得到新 SK，`config set access_key_secret <newSK>` + 打印新旧凭据摘要 |
| `list [--mesh M]` | 调 `GET /api/access_keys`，只解密**自己**的 key（按 AK 匹配本端 SK）的 wrapped_secret 显示，其余显示 masked（无对应 SK 不解密） |
| `add [--mesh M] [--ttl 30d]` | 调 `POST /api/access_keys` 新增 key（AK 由服务端生成或 sclient generateAccessKeyPair 生成+提交） |
| `expire <ak> [--until <RFC3339>]` | 调 expire 端点 |
| `delete <ak>` | 调 DELETE 端点 |

- `pkg/client/accesskey.go`（新）领域 API `RotateAccessKey/AddAccessKey/ListAccessKeys/ExpireAccessKey/DeleteAccessKey`（走 coreRequest 签名）。
- `generateAccessKeyPair` 逻辑可复用（AK 生成格式不变；新增 key 场景 sclient 本地生成 AK/SK 提交，rotate 场景服务端生成新 SK）。

## 9. config 与验证

- `pkg/server/config.go` — `AccessKeyConfig` 不变（仍是 Key/Secret/MeshID 三字段）。
- TDD 测试：
  - `pkg/accesskey/ring_test.go`：Upsert 校验/重复覆盖/Delete 幂等/Expire 设/消/Lookup 过期不快照快照排序/Len；并发 -race；（注入 now 测过期）。
  - `pkg/accesskey/wrap_test.go`：wrap key 派生确定性/不同 salt 不同 key/AES-GCM 往返/篡改失败。
  - `pkg/server/`：auth 查 ring（新 key 验签成功、旧 key 过渡、过期 key 401）；rotate 端点黑盒
    （active key 签名成功 → 返回 wrapped 可解 → 新 SK 立即访问 → expire → delete）；双 SK 共存；
    SIGHUP 重载 access_keys 生效；持久化写回 YAML。
  - `pkg/tunnel/hub/`：Authenticator 共享 ring（新 key 注册成功、过期 key 拒绝、无 SetAccessKeys 仍需）。
  - e2e：真实二进制 rotate → 新 SK 访问 → hub 注册。
- 验证：`go test -race ./pkg/accesskey/... ./pkg/server/... ./pkg/tunnel/hub/... ./cmd/sproxy/... ./cmd/sclient/...` + `make lint`（含子 module）+ `make build-all` + `make test-all` + `make check-loopback`。

## 10. 范围外（后续项）

- **AK 轮换**（owner 延续数据身份）——本轮只轮 SK。
- 密钥持久化 keyring 文件化（脱离配置 YAML 的独立凭据存储 + 权限管控）。
- SK 到期告警/自动过期提醒（日志/metrics 层面）。
- 管理端点的分页/限流。

## 11. 与现有模式的一致性

| 项目惯例 | 本设计落点 |
|----------|-----------|
| configMu + COW + Store | rotate/expire/delete 后的 cfgPtr 更新 |
| authMiddleware 保护管理端点 | 全部新端点挂 authMiddleware |
| RecordAudit 审计 | access_key_rotate/expire/delete 全部入审计 |
| 纯 stdlib + slog | pkg/accesskey 纯 stdlib，AES-GCM 用 crypto/aes+cipher |
| cmd 薄逻辑 | sclient 只做 flag+IO+解密，领域 API 在 pkg/client |
