# 首次连接安全：认证面插件化 + 角色模型 + TOTP 登录（4B）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 在 4A 已收归的凭据架构（`pkg/accesskey` Ring + `/api/credentials` 管理端点 + 签名 v2 `skey-id=` 强制必传 + `trust` 命令族）之上，落地首次连接安全：**认证面插件化**（Authenticator seam + `Principal`）、**账号级角色模型**（`Key.Role`：user/node/admin）、**默认简单 AK/SK 注册**（4A 语义回归，register 直接下发 SK）、**可强制的 TOTP 登录**（`registration.force_totp`，动态码登录签发 session SK）、`sclient trust login`、Web 注册/登录页（客户端 JS QR）。

**架构：** 依赖 4A 单一事实源 `pkg/accesskey`——本计划**不得再造任何 AK/SK 生成/解析/包裹逻辑**，只在其上扩展（`Role` 类型/`Key.Role`/`Key.TOTPSecret` 字段/`Ring.GetKey`/`Ring.AddRegistration`/`WrapContextTOTP`/`EncryptSecretKind` 均收归 accesskey）。TOTP 验证器独立 `pkg/otp`（RFC 6238，纯 stdlib，主 go.mod 零新增，D7）。**零凭据启动（U3）**：server 不再自动生成 anonymous 引导凭据，register 是唯一用户入口。注册/登录为**公开端点**（不挂 authMiddleware，仿 renew 引导豁免 + 独立限频）；**无 admin 时 register 仅回环可达（U2）**；TOTP 登录签发 session SK（`KindTOTPWrap`，TTL 仅服务端控 + per-AK 失败锁定），随后所有请求带新 `skeyID` 走既有 v2 路径——全链（信令/联邦/发现/sync/隧道/relay）零改动。**简单 AK/SK 账号不需要 login/nonce**（有 SK 直接用，DEC-B）。

**技术栈：** Go 1.26，纯 stdlib（`crypto/hmac`/`crypto/sha1`/`encoding/base32`/`crypto/aes`）+ 既有 `golang.org/x/crypto/hkdf`。核心 go.mod 零新增三方依赖。

**BASE**：`1618835f`（origin/master，4A 已合并）。

**PR 拆分建议（重构后，原 4B-1/4B-2 拆分作废）**：
- **4B-1 = 任务 ①-⑤**（认证 seam + Role 模型 + 默认简单 AK/SK 注册，**无 TOTP**，纯 4A 扩展可独立验证）；
- **4B-2 = 任务 ⑥-⑪**（TOTP 插件：`pkg/otp` + force_totp + register TOTP 分支 + nonce/login + `trust login` + 全链路测试）；
- **4B-3 = 任务 ⑫**（Web 注册/登录页 + 客户端 JS QR + `sig.js` 过时注释清理 + `app.js` 补 `sproxy_access_key_id` sessionStorage）。
理由：认证 seam + 角色模型是无 TOTP 的纯 4A 扩展，拆开独立 PR 最安全；TOTP 是独立插件特性；Web 页接线独立于服务端逻辑。注：`sig.js` **输出已是 `skey-id=`**（对齐 Go），仅注释过时，4B-3 无段名协议风险。

---

## 0. 4A 已落地前提（执行前必读，逐条落实勿混淆）

> 以下事实已 grep 核实于当前代码（HEAD=1618835f）。4B 所有设计必须落在其上。

| # | 4A 落地事实 | 对 4B 的含义 |
|---|---|---|
| 1 | AK = `ak-<mesh>-<32hex>`（16B 标准，`accesskey.GeneratePair`）；legacy `<16hex>` 仅解析兼容（`ParseMesh`/`IsValidAK`）；`AccessKeyPrefix="ak-"` + `AllowedAKPrefixes` 白名单 | 4B 注册产生的 AK 必须走 `GeneratePair`；禁止再造生成 |
| 2 | SK 条目 ID = `skey-<12hex>`（`SkeyIDPrefix`；`GenerateID`/`newEntryID` 唯一实现） | 4B session SK / 简单注册 SK 条目 ID 用同一前缀/生成函数 |
| 3 | 协议段 `skey-id=` v2 **强制必传**（`sproxysig.ParseHeader` 缺段 ErrMalformed→401；`ParseHeaderAllowMissingSkeyID` 仅 renew 引导豁免） | 注册/登录签发 SK+skeyID 后所有请求必须带新 skeyID；注册/登录端点无凭据，仿豁免（不挂 authMiddleware） |
| 4 | AK/SK 生成/解析/包裹全收归 `pkg/accesskey`（`GeneratePair`/`GeneratePairLegacy`/`IsValidAK`/`ParseMesh`/`RandomHexHex`/`GenerateID`/`WrapContextCredentials`/`DeriveWrapKey`/`EncryptSecret`/`DecryptSecret`/`WrappedSecret`） | 4B/4C 不得再造；TOTP wrap 新常量同法收归 accesskey |
| 5 | `access-key` 命令已删；sclient 凭据入口 = `trust` 家族（`renew`/`sk`/`ak`，`NewCmdTrust`） | 4B-2 sclient 侧入口为 **`trust login`**（并入 trust 家族） |
| 6 | **admin 判定 = 遍历存活条目 `Meta.Type=="admin"`**（`getRole`，credentials_handler.go:81，逐请求实时，不缓存） | **4B-1 改为账号级 `Key.Role`（DEC-A）**：`getRole` 改读 `ring.GetKey(ak).Role=="admin"`；不再读 `Meta.Type=="admin"`。`Meta.Type` 仅登录来源 provenance |
| 7 | `/api/credentials` 管理端点已落地（`renew`/`sk` 增删查/`ak add+delete+confirm`；`akAddHandler` 已实现仅 admin 门禁；主 mux + localMux 双注册） | 4B 注册=新增公开 `POST /api/credentials/register` + 复用 Ring/`persistCredentials` 管线 |
| 8 | `registration.disable`（false=允许注册）/`allow_insecure_loopback`/`credential_ttl`（renew 默认 30d）已配置（`RegistrationConfig{Disable}`） | 4B-1 扩展 `Registration.ForceTOTP`（默认 false，DEC-B）；4B-2 扩展 `Registration.SessionTTL`（web session，默认 24h）、`Registration.CliTTL`（cli session，默认 **7d**）与 `Registration.LoginFailLimit`/`LoginFailWindow`（per-AK 锁定，默认 5/15m） |
| 9 | wrap context 单一事实源 `accesskey.WrapContextCredentials`（server `credentialWrapContext` / client `CredentialWrapContextPrefix` 别名） | 4B-2 TOTP wrap context 另定常量 `WrapContextTOTP` 并**同法收归 accesskey** |
| 10 | 全链已接 `AccessKeyID`（`signRequest`/`sigRoundTripper`/信令/联邦/发现/sync/隧道/relay；`WithAccessKeyID`） | 4B-2 登录签发 session SK+skeyID 后，这些链路零改动自动用新凭据 |
| 11 | `KindTOTPWrap` 枚举已预留（未实现）；`DecryptSecret` 现只认 `KindSecretWrap` | 4B-2 在 accesskey 扩展 TOTP 包裹/解开（`EncryptSecretKind`/`DecryptSecretKind`） |
| 12 | `CredentialStorer`/`SecureStorer` 接口**均未实现**（4A 仅具体 `server.CredentialStore`） | 属 4C 第一步（见 4C plan），4B **不引入**这些接口；**`Authenticator` 接口在 4B-1 定义（DEC-C），4C 不再重复提取** |
| 13 | Web UI：`transport.js` **已接** `accessKeyID`（直连/隧道两路径均 `entryID: cfg.accessKeyID`，line 303/423）；`sig.js` **实际输出已为 `skey-id=`**（line 115，与 Go 对齐），但**文件注释仍写 `sk=`**（line 9/27/29/102/103，过时）；`app.js` 仅存 `sproxy_access_key`/`_secret`，**未持久化 `sproxy_access_key_id`** | 4B-3 清理 sig.js 过时注释（输出已对，无需改段名）+ `app.js` 增补 `sproxy_access_key_id` sessionStorage 回填 |
| 14 | 4A `bootstrapCredentials` 在 store 为空且 `credentialTTLEnabled()`（`cfg.CredentialTTL>=0`）时**自动生成首启 anonymous AK/SK**（`bootstrapGenerate`，handlers.go:935/995-1022） | **4B-1 移除该行为（U3，DEC-F）**：`bootstrapCredentials` 改为「载入快照 + 零凭据等待注册」，store 为空时不再生成任何凭据；受影响 4A 测试见任务 ③（`credentialstore_test.go`/`config_test.go`） |

---

## 文件结构总览

**新建**
- `pkg/accesskey/role.go`（或并入 accesskey.go）— `Role` 类型 + 常量（4B-1 任务①）
- `pkg/server/register_handler.go` + `register_handler_test.go` — 注册/nonce/登录端点（4B-1 任务③ + 4B-2 任务⑧⑨）
- `pkg/otp/totp.go` + `totp_test.go` — RFC 6238 TOTP 验证器（4B-2 任务⑥）
- `pkg/accesskey/totp.go` + `totp_test.go` — `WrapContextTOTP` 常量 + `DeriveTOTPWrapKey` + `EncryptSecretKind`/`DecryptSecretKind`（4B-2 任务⑦）
- `pkg/client/accesskey_totp.go`（或并入 accesskey.go）— `RegisterTOTP`/`RequestTOTPNonce`/`LoginTOTP`（4B-2 任务⑩）
- `cmd/sclient/trust_login.go` + `trust_login_test.go` — `trust login`（4B-2 任务⑩）
- `web/static/login.html`（或并入 index.html）+ `web/static/login.js` + `login.test.js`（4B-3 任务⑫）

**修改**
- `pkg/accesskey/accesskey.go` — `Key` 加 `Role Role `json:"role"``（4B-1）与 `TOTPSecret []byte `json:"totp_secret,omitempty"``（4B-2，账号级）
- `pkg/accesskey/ring.go` — `cloneKey` 复制 Role/TOTPSecret + 新增 `GetKey(ak string) (*Key, bool)`（I1）与 `AddRegistration(ak, owner string, sk, totpSecret []byte, role Role, ttl time.Duration) (granted bool, id string, err error)`（I2，原子 admin 判定 + 内联 upsert，M5，R4-I1）
- `pkg/accesskey/wrap.go` — `EncryptSecret`/`DecryptSecret` 拆出 Kind 参数化版本（保持旧签名薄委托，4B-2）
- `pkg/server/auth.go` — `Principal`/`Authenticator`/`RingAuthenticator`/`PrincipalFrom`/`requireRole` + authMiddleware 改走链（4B-1 任务②）
- `pkg/server/handlers.go` — `RegisterRoutesOpts.Authenticators` + `Handlers.authenticators` + 注册 register/nonce/login 路由 + `totpNoncePool`/`registerLimiter`/`totpLimiter`/`loginFailTracker`（复用 `RateLimiter`，D6/U4）+ **`bootstrapCredentials` 移除首启 anonymous 生成**（零凭据启动，U3）
- `pkg/server/credentials_handler.go` — `getRole` 改造读 `Key.Role`（DEC-A）；`akDeleteHandler` 拒绝删除 `Role==admin` 的 AK（400，I6）；`akAddHandler` 支持 `role`（node/user）
- `pkg/server/config.go` — `RegistrationConfig` 加 `ForceTOTP`（4B-1）、`SessionTTL`/`CliTTL`/`LoginFailLimit`/`LoginFailWindow`（4B-2）+ SetDefaults/校验；`config.example.yaml` 同步注释
- `pkg/client/client.go` — `RequestSigner` 接口 + 默认 `ConfigSigner` + `WithRequestSigner`（4B-1 任务④）
- `cmd/sclient/trust.go` — `trust ak add` 加 `--role`（4B-1 任务③）；挂载 `trust login`（4B-2 任务⑩）
- `web/static/app.js` / `web/static/sclient/sig.js` / `web/static/sclient/transport.js`（4B-3）— sessionStorage 回填 `sproxy_access_key_id`、登录页记住最近 AK（S3）、sig.js 过时注释清理、注册/登录页接线
- 测试：`pkg/server/server_auth_test.go`（注册/登录公开豁免断言 + Authenticator 链）、`pkg/client/client_test.go`（ConfigSigner/TOTP 领域 API）、`cmd/sclient/trust_test.go`
- 文档：`docs/superpowers/specs/2026-09-04-...`（§2.3/§7.4 已对齐）、`CLAUDE.md`（trust login 入命令表）

---

# 4B-1：认证 seam + Role 模型 + 简单 AK/SK 注册

### 任务 ①：accesskey Role 模型（Role 枚举 / Key.Role / GetKey / AddRegistration / getRole 改造）

**文件：**
- 修改：`pkg/accesskey/accesskey.go`（`type Role` + `RoleUser`/`RoleNode`/`RoleAdmin` 常量 + `Key.Role` 字段）
- 修改：`pkg/accesskey/ring.go`（`cloneKey` 复制 Role + 新增 `GetKey`，I1 + 新增 `AddRegistration`，I2）
- 创建：`pkg/accesskey/role_test.go`
- 修改：`pkg/server/credentials_handler.go`（`getRole` 改读 `ring.GetKey(ak).Role`，DEC-A）

**设计要点（DEC-A）：**
- `type Role string`：`RoleUser Role = "user"`（默认）/ `RoleNode Role = "node"` / `RoleAdmin Role = "admin"`。
- `Key.Role Role `json:"role"``（账号级，持久化进 credentials.json）。旧 credentials.json（4A 无 role 字段）加载时 `Role` 为空串——**`Load`/`Replace`（或 `requireRole`）显式归一为 `RoleUser`**（R3-M4）；dev 阶段无存量，无迁移脚本。
- `Ring.GetKey(ak string) (*Key, bool)`（I1）：读锁内返回 `cloneKey` 深拷贝（含 `Role`）；不存在 → `false`。`getRole`/登录 handler 取整 Key 的唯一访问路径（4A 公开方法只有 `Lookup`/`GetEntry`/`Snapshot`，均拿不到含 Role 的整 Key）。
- `Ring.AddRegistration(ak, owner string, sk, totpSecret []byte, role Role, ttl time.Duration) (granted bool, id string, err error)`（I2，DEC-A/DEC-B，R4-I1）：**方法首部校验「`sk` 与 `totpSecret` 不同时非 nil，双 nil → error」（R3-M2）**。**写锁内** ①写 `Key.Role` 与 `Key.TOTPSecret`；②遍历全部 Key 查 `Key.Role=="admin"`；③无 → 本次授 `RoleAdmin`（**首注册恒 admin，无论入参**）；有 → 本次 `RoleUser`（**非首注册传 `RoleAdmin` 拒绝或降级 `RoleUser`**）；④`sk` 非 nil（简单模式）→ **内联**追加 SK 条目（**`ExpiresAt = now + ttl`，`ttl` 由调用方注入（`pkg/accesskey` 不读 `cfg.CredentialTTL`——core 域不依赖 server 配置，R4-I1）；TOTP 模式无 SK 条目，`ttl` 忽略**；**不可再调 `UpsertAK`/`AddKey`——sync.RWMutex 不可重入，持写锁再调会死锁，M5**）；返回 `granted`（=是否授予 admin）与 `id`（简单模式新建 SK 条目 ID；TOTP 模式无 SK 条目 → 空串）。`owner` 空值默认 = AK 字符串（R2-N4）。
- `getRole` 改造（credentials_handler.go:81）：`h.getRole(ak)` 改读 `ring.GetKey(ak)` 的 `Key.Role`；AK 不存在/ring 为 nil → `"user"`。删除「遍历存活条目 `Meta.Type=="admin"`」逻辑。

- [ ] **步骤 1：编写失败的 Role 模型测试**

`role_test.go` / `ring_test.go` 追加：
- Role 常量枚举：`RoleUser=="user"` / `RoleNode=="node"` / `RoleAdmin=="admin"`。
- `Key.Role` 随 Snapshot/Replace 深拷贝保留（`cloneKey` 复制 Role——先在测试断言）。
- `Ring.GetKey(ak)`（I1）：`AddKey` 过的 AK → 返回 Key 副本（含 `Role`，深拷贝）；修改返回值不影响原 ring；不存在 AK → `false`。
- `AddRegistration`（I2）：**首注册** `granted=true` 且 `GetKey(ak).Role==RoleAdmin`；**第二个注册** `granted=false` 且 `Role==RoleUser`；非首注册传 `RoleAdmin` → 拒绝或降级 user（断言不产生第二个 admin）；`owner` 空 → `GetKey(ak).Owner==ak`（R2-N4）；简单模式 `sk` 非 nil + `ttl` 传参 → 返回 `id` 前缀 `skey-` 且条目 `len(sk)==32`（I3）、**条目 `ExpiresAt≈now+ttl`（R4-I1，用显式 ttl 注入断言，不依赖 server 配置）**；TOTP 模式 `sk=nil` → `id==""` 且无 SK 条目、`TOTPSecret` 已写（**`ttl` 忽略**）；**双 nil（`sk` 与 `totpSecret` 均 nil）→ error（R3-M2）**。
- **R3-M4 归一断言**：构造无 `Role` 字段的旧 `Key`（`Role:""`）→ `Load`/`Replace` 后 `GetKey(ak).Role==RoleUser`（`requireRole(principal, user)` 放行）。
- `getRole` 改造：`getRole(ak)=="admin"` 当且仅当 `GetKey(ak).Role==RoleAdmin`（S4 前置）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/ -run "Role|GetKey|AddRegistration"`
预期：FAIL（符号未定义 / cloneKey 未复制 Role）。

- [ ] **步骤 3：实现 Role 模型**

`accesskey.go`：`Role` 类型 + 常量；`Key` 加 `Role` 字段。`ring.go`：`cloneKey` 复制 `Role`；新增 `GetKey(ak string) (*Key, bool)`（读锁内返回 `cloneKey` 副本，I1）；新增 `AddRegistration`（I2，写锁内原子 admin 判定 + 内联 upsert + 条目追加，M5）。`credentials_handler.go`：`getRole` 改读 `ring.GetKey(ak).Role`。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... -run "Role|GetKey|AddRegistration|Credentials"`
预期：PASS（既有 credentials 测试经 getRole 改造后仍绿——无 admin 时恒 user）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/ pkg/server/credentials_handler.go && git commit -m "feat(accesskey): 账号级角色模型——Role 枚举 + Key.Role + GetKey + AddRegistration(原子 admin 判定) + getRole 改造"
```

---

### 任务 ②：Authenticator/Principal/RingAuthenticator + authMiddleware 链 + 宿主嵌入点

**文件：**
- 修改：`pkg/server/auth.go`（`Principal`/`Authenticator`/`RingAuthenticator`/`PrincipalFrom`/`requireRole` + authMiddleware 改走链）
- 修改：`pkg/server/handlers.go`（`Handlers` 增 `authenticators []Authenticator` + `RegisterRoutesOpts.Authenticators []Authenticator` + 装配默认链）
- 创建：`pkg/server/auth_seam_test.go`

**设计要点（DEC-C）：**
- `Principal`（AK/Owner/Role/Mesh 四字段，见 spec §7.2）+ `Authenticator` 接口（`Name()` / `Authenticate(ctx, r) (*Principal, error)`）。
- `RingAuthenticator`：包装现有 SproxySig v2 验签（`verifySproxySigFromRing`），返回 `Principal{AK: cred.ak, Owner: Key.Owner, Role: string(Key.Role), Mesh: cred.mesh}`；无 Authorization 头或验签失败 → error。
- authMiddleware 改走链：`Handlers.authenticators []Authenticator`（默认 `[RingAuthenticator]`）；遍历任一成功 → `Principal` 入 ctx（新增 `PrincipalFrom(ctx)`）。**api_keys Bearer 保留为链前独立检查**（`cfg.APIKeys.Enabled` → 走既有 `authenticateAPIKey`，不查 store，取更小改动不把 api_keys 实现为 Authenticator）。
- `RegisterRoutesOpts.Authenticators []Authenticator`（**nil → 默认 `[]Authenticator{RingAuthenticator{...}}`；
  **非 nil → replace 默认链**（宿主全权掌控，需含 RingAuthenticator 则自行加入，R3-I2））——**宿主嵌入点**：
  外部项目注入自有实现（映射自有用户/会话 → Principal），**文件操作按 `Principal.AK` 落桶**（宿主把目标
  桶 ID 放入 `Principal.AK`，保持 4A 按 AK 落桶现状零回归，R3-I1）。
- 现有 `ActorFrom`/`MeshFrom`/`EntryIDFrom` ctx 保留（4A 消费方零改动）；**`withActor` 在认证成功路径
  统一写 `Principal.AK`**（RingAuthenticator = `cred.ak`；宿主 = 其放入的桶 ID）。
- **`requireRole(principal *Principal, minRole string) error`** 辅助函数：`user` ≤ `node` ≤ `admin` 顺序判定；`Role` 空值归一 `RoleUser` 后判定（R3-M4）；角色不足 → 403。**仅对挂门禁的文件操作路由生效**——`principal==nil`（未认证且非回环直通）→ 401；**回环直通路径已有合成 Principal（R4-I2），放行**；**非文件路由（healthz/version 等裸路由，不挂 authMiddleware）不拦截**。**文件操作路由组**（upload/download/delete/rename/list/stat/mkdir/rmdir/search/batch/chunk/archive/versions…）要求 `Role∈{user,admin}`（minRole=user）；**mesh/hub 组接线点**（minRole=node）在 spec §13 列后续——本计划至少接文件组，完整 mesh node 用户类型列后续 PR。

- [ ] **步骤 1：编写失败的 Authenticator 链测试**

`auth_seam_test.go`：
- **默认链行为不变**：`RegisterRoutes`（无 `Authenticators`）装配后，SproxySig 验签成功路径行为与 4A 一致（GET /api/files 200、ActorFrom 可读、审计 actor 正确）——**4A 既有测试零回归**。
- **宿主注入自定义 Authenticator（R3-I1）**：注入 fake authenticator（映射固定
  `Principal{AK:"ext-user-1", Owner:"ext-display-name", Role:"user"}`，不验签）→ 请求文件列表 → 200 且
  **按 `AK:"ext-user-1"` 落桶**（断言列表返回 ext-user-1 桶内容；`Principal.Owner` 不参与落桶，仅
  `PrincipalFrom(ctx)` 可读作元数据）；**replace 语义**：注入不含 RingAuthenticator 的链 → 未带
  Authorization 头请求直接由 fake 认证成功（默认链被替换）。
- **零凭据窗口宿主认证（R3-I2）**：ring 空（零凭据启动状态）时注入宿主 Authenticator → 请求文件列表
  → 200（链先跑，`handleNoCredentials` 兜底不前置拦截）。
- **requireRole 门禁**：fake authenticator 返回 `Role:"node"` → 文件组请求 → 403；`Role:"user"`/`"admin"` → 200；`Role:""` → 归一 user 放行（R3-M4）。
- **回环直通合成 Principal（R4-I2）**：零凭据窗口（ring 空 + 无宿主 Authenticator）+ `allow_insecure_loopback=true` + 回环来源 → `GET /api/files` → 200（`handleNoCredentials` 直通路径合成 `Principal{AK:"", Owner:"", Role:"user"}` 写入 ctx，`requireRole(principal, user)` 放行）；非回环来源 → 401；**`PrincipalFrom(ctx).Role=="user"` 断言合成角色**。
- **/tunnel 链认证解密正常（R5-I1）**：SproxySig 验签成功（RingAuthenticator 填充 `Principal.Secret`）→ POST `/tunnel` → 服务端用 `PrincipalFrom(ctx).Secret` 派生隧道密钥 → 隧道请求解密正常（复用 4A 隧道 E2E 测试的加密帧路径）。
- **宿主 Authenticator /tunnel 不派生（R5-I1）**：宿主 fake 认证（`Principal.Secret` 为空）→ POST `/tunnel` → **不派生隧道密钥**（401 或明确错误「无 SproxySig 凭据不建立隧道」）。
- **api_keys 优先**：`APIKeys.Enabled=true` 时 Bearer 路径仍走既有逻辑（Authenticator 链不生效）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run "AuthSeam|Authenticator"`
预期：FAIL（类型/字段未定义）。

- [ ] **步骤 3：实现 Authenticator 链**

**步骤 3（实现，按序）**：
1. **先重构 `verifySproxySigFromRing`（auth.go:232-296，R4-I3）**：签名改为「返回 `(*verifiedCredential, error)`，失败**不写响应**」——现全部失败路径的 `http.Error`（:250/:256/:265/:286/:291）收敛为返回 error；`authMiddleware` 在链全失败后统一写 401。**这是任务②的实现前提**（否则 `RingAuthenticator` 包装它在链前/链中失败时已写 401，后续 authenticator 被短路）。
2. `auth.go`：`Principal`（**含 `Secret []byte` 字段，R5-I1**）/`Authenticator`/`RingAuthenticator`（**成功路径填充 `Principal.Secret = cred.secret`**）/`PrincipalFrom`/`requireRole`；authMiddleware 改走 `h.authenticators` 链——**链先跑**（api_keys 前置分支保留；**不因 ring 空而短路**——`handleNoCredentials` 降为「链全失败且 ring 空」的最终兜底，R3-I2）；**`handleNoCredentials` 回环直通分支合成 `Principal{AK:"", Owner:"", Role:"user"}` 写入 ctx**（R4-I2）；**`setResponseActor(w, Principal.AK)`**（原 auth.go:383 `setResponseActor(w, cred.ak)` 同步改，R4-M1）；**`/tunnel` 分支改读 `PrincipalFrom(ctx).Secret + PrincipalFrom(ctx).Mesh` 派生隧道密钥**（auth.go:393-404 原样保留，`cred.secret` → `Principal.Secret`；`Secret` 空 → 401/明确错误不派生，R5-I1）。
3. `handlers.go`：`Handlers.authenticators` + `RegisterRoutesOpts.Authenticators` + 装配（**nil → 默认 `[RingAuthenticator]`；非 nil → replace**；RingAuthenticator 构造注入 `h.credentialRing`）。
- **链中失败继续尝试测试（R4-I3）**：注入 `[RingAuthenticator, fakeAuth]` 链，请求无 Authorization 头 → 链首 RingAuthenticator 失败返回 error（不写响应）→ 链尾 fake 认证成功 → 200（断言未被前置 401 短路）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/...`
预期：PASS（含 4A 既有 auth/server 测试全绿）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/auth.go pkg/server/handlers.go pkg/server/auth_seam_test.go && git commit -m "feat(server): 认证面插件化——Principal/Authenticator/RingAuthenticator + authMiddleware 链 + RegisterRoutesOpts.Authenticators 宿主注入 + requireRole 门禁"
```

---

### 任务 ③：register 简单模式端点 + loopback 门禁 + requireRole 文件组门禁接线 + trust ak add --role

**文件：**
- 创建：`pkg/server/register_handler.go` + `register_handler_test.go`
- 修改：`pkg/server/handlers.go`（注册路由 + `Handlers` 字段 + 限频装配 + `bootstrapCredentials` 移除首启 anonymous，U3）
- 修改：`pkg/server/credentials_handler.go`（`akAddHandler` 支持 `role`；`akDeleteHandler` 拒绝删除 admin AK，I6）
- 修改：`cmd/sclient/trust.go`（`trust ak add` 加 `--role`）

**端点设计（4B-1 定稿简单模式；TOTP 分支在任务 ⑧）：**

| 端点 | 认证 | 行为 |
|------|------|------|
| `POST /api/credentials/register` body `{owner?}` | **公开**（不挂 authMiddleware，主 mux + localMux 双注册，仿 `/healthz` 层 + 独立限频） | register 是**唯一用户入口（U3，DEC-F）**。**loopback 预检（U2）**：handler 开头判定「ring 无任何 `Key.Role=="admin"` 的 AK 且 非 127.0.0.1/::1 回环来源」→ 403（远程首注册拒绝；有 admin 后跳过，按 `cfg.Registration.Disable` 判定）。`cfg.Registration.Disable` → 403（**运维约束 I4b：`disable=true` 仅适用于已有 admin 的存量部署——无 admin 时回环来源亦 403，系统将永无 admin**）。`accesskey.GeneratePair(nil,"")` 生成 AK（标准 `ak-<32hex>`）+ SK hex；**`ring.AddRegistration(ak, owner, hex.DecodeString(skHex), nil, accesskey.RoleUser, h.credentialTTLFromCfg())`**（I2/DEC-B/R4-I1）：handler 以既有 `credentialTTLFromCfg()`（credentials_handler.go:63，仿 renew 现状）注入 `ttl`，写锁内原子判定首注册 → `RoleAdmin`，追加 SK 条目（`len==32`，I3，**`ExpiresAt = now + ttl`，默认 30d，R3-I3**）→ `granted`/`id`；`persistCredentials()`（失败 → `credential_persist_error` 审计 + 500）；`RecordAudit(credential_register)`。返回 `{ak, owner, admin: granted, sk, skey_id: id}`——**SK 仅此一次明文下发**（打印到 CLI / Web 展示一次，不落日志）。body 经 `MaxBytesReader(MaxCredentialsBodyBytes)` + `drainAndVerifyBody`（M7）。**行为衔接（R3-M6）：本端点将在 4B-2（任务⑧）扩展为 `force_totp` 分支——默认保持简单模式（force_totp=false 时行为不变），部署者勿以本 PR 行为推断最终形态。** |
| 限频 | — | **复用既有 `pkg/server.RateLimiter`**（`ratelimit.go`，per-IP 令牌桶 + 全局滑动窗口 + 429 JSON）：`registerLimiter = NewRateLimiter(5, time.Minute, log.With("component","register_limiter"))`，以 `Middleware` 挂公开端点（D6/M1）。 |

**设计决策：注册 = 新公开端点（非复用 `POST /api/credentials` akAdd）。** 理由：akAdd 是 admin-only（4A 无 admin → 恒 403），无法支持「首 user 免 admin」的首连场景；且注册必须返回明文 SK（akAdd 无此概念）。

**D2 原子性说明**：注册的「查无 admin → 授 admin」必须在 **ring 写锁内**一次完成（`AddRegistration`）——4A `Ring` 的 `Snapshot()`（RLock）与 `AddKey`（Lock）是**两次独立方法调用**，两并发注册会都看到「无 admin」然后双打标。故新增 `AddRegistration`（单方法持写锁完成判定+追加），而不是在 handler 里「先 `Snapshot` 判、再 `UpsertAK`+`AddKey`」。

**requireRole 文件组门禁接线（DEC-C）**：本任务把文件操作路由组（`POST /upload`、`GET /download`、`POST /delete`、`POST /rename`、`GET /api/files`、`POST /mkdir`、`POST /rmdir`、`GET /api/files/search`、`POST /api/batch/*`、分块上传/下载、archive、versions、share 等）包 `requireRole(PrincipalFrom(ctx), RoleUser)` 门禁（4B-1 至少接文件组；mesh/hub 组接线点标注，完整实现列后续 PR）。

**移除 anonymous bootstrap 的测试影响（U3，实现本任务时同步调整 4A 既有测试）**：
- `pkg/server/credentialstore_test.go` — `TestGenerateBootstrapCredential_Format`（及 `:180` 同源断言）直接测 `GenerateBootstrapCredential`（首启 anonymous 凭据生成器），该函数随 U3 移除 → 删除/改写为 register 端点的 AK 形态断言。
- `pkg/server/config_test.go` — `TestConfig_Registration_Defaults` 中「`credential_ttl` 负值 = 显式禁用首启 anonymous」的语义断言：负值语义随 anonymous 引导移除而消失（`CredentialTTL` 收敛为 renew 专用，负值不再有意义），相应分支删除；`credential_ttl` 默认 30d 断言保留。
- 其余测试经 `defaultNoAuthRegOpts`（注入空 Ring）或显式注入 CredentialRing 装配，不依赖首启自动生成，**预期零迁移（R2-N3）**——实现时以实际编译/运行结果为准：若出现依赖 anonymous 引导的其它失败用例，按上述清单同样的原则调整（删除/改写），并同步回本计划。

- [ ] **步骤 1：编写失败的注册黑盒测试**

`register_handler_test.go`（复用 seeded server helper `newTestServerCreds`，M4）：
- **注册成功（简单模式）**：POST `/api/credentials/register`（无 Authorization 头，回环来源）→ 200，返回 `{ak, admin:true, sk, skey_id}`；AK 形态 `ak-<32hex>`（`IsValidAK`）；`sk` 为 64-hex（hex decode 32B，I3）；`skey_id` 前缀 `skey-`；**`GetEntry(ak, skey_id).ExpiresAt≈now+cfg.CredentialTTL`（R3-I3/R4-I1，ring 注入 now 断言——handler 经 `credentialTTLFromCfg()` 传入，服务端配置即 cfg.CredentialTTL）**。
- **SK 立即可用**：用下发的 `sk` + `skey_id` 构造 `WithAccessKey(ak, sk)` + `WithAccessKeyID(skey_id)` 签名 GET `/api/files` → 200（v2 强制必传路径闭合）。
- **loopback 预检（U2）**：无 admin 时非回环来源（注入 `RemoteAddr` 为公网 IP）POST register → 403；回环来源 → 200 `admin:true`；**首个 admin 注册后** 远程来源再注册 → 按 `registration.disable` 判定（200 或 403）。
- **首 user 即 admin（DEC-A）**：注册后（ring 注入）`getRole(ak)=="admin"`；**第二个**注册用户 → `admin:false`、`getRole=="user"`。
- **S4**：`AddRegistration` 返回的 `granted` 与 `getRole(ak)` 结果一致（admin:true ↔ getRole==admin）。
- **D2 并发双 admin**：两 goroutine 并发 POST register（sync.WaitGroup + ring 注入 now 固定）→ 恰一个 `admin:true`、`getRole` 判定全 ring 唯一 admin。
- **registration.disable=true** → 403（构造 cfg 注入）。
- **重复注册**：同一 AK 已存在（构造 ring）→ 400/409（防覆盖既有 AK）。
- **I6 admin AK 不可删**：`DELETE /api/credentials/{ak}`（目标是 `Role==admin` 的 AK，`confirm`+`force` 均齐）→ 400（`TestAdminRole_AKDeleteRejected`）。
- **requireRole 门禁**：`Role==node` 的 AK 签名访问 `GET /api/files` → 403（node 不进文件组）。
- **trust ak add --role node**：admin 签名 POST `/api/credentials` body `{ak, owner, role:"node"}` → 200；新 AK `GetKey().Role==RoleNode`。
- **零凭据启动（U3）**：`bootstrapCredentials` 在 store 为空时**不生成**任何凭据（ring.Len()==0）；启动日志提示「首次注册经回环，将成为 admin」（S2）。
- **无认证 401 面不受影响**：注册端点无 Authorization 头可直达；其余 `/api/credentials/*` 仍 401。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run Register`
预期：FAIL（路由未注册 404 / handler 未定义）。

- [ ] **步骤 3：实现注册 handler + 路由 + 门禁接线**

`register_handler.go` + `pkg/server/handlers.go` + `pkg/server/credentials_handler.go` + `cmd/sclient/trust.go`：
- `registerCredentialHandler`：**loopback 预检（U2）** 开头执行——`hasAdmin` = 遍历 `ring.Snapshot()` 查 `Key.Role=="admin"`（与 `AddRegistration` 判定共享同一 ring 查询语义；预检是读侧防御，权威判定仍在 `AddRegistration` 写锁内，D2）；`!hasAdmin && !isLoopbackRemote(r.RemoteAddr)` → 403「首个 admin 注册仅限回环」。随后按端点表；admin 判定/打标 = `ring.AddRegistration` 返回值（**不再**在 handler 里遍历 Snapshot 判 admin 后自行打标——那是非原子检查，D2）。
- **body 防护（M7）**：register 端点 `r.Body = http.MaxBytesReader(w, r.Body, MaxCredentialsBodyBytes)` + `drainAndVerifyBody(r)`（公开端点更应防大 body DoS / 请求走私；仿 `akDeleteHandler` 既有样板，credentials_handler.go:657-666）。
- `Handlers` 加 `registerLimiter`（复用 `pkg/server.RateLimiter`，`NewRateLimiter(5, time.Minute, log.With("component","register_limiter"))`，D6/M1）。
- `handlers.go`：`srvMux.HandleFunc("POST /api/credentials/register", h.registerLimiter.Middleware(h.registerCredentialHandler))`（**不包 authMiddleware**）+ localMux 同注册。
- `handlers.go`（U3）：`bootstrapCredentials` 移除「`ring.Len()==0 && credentialTTLEnabled()` → 生成首启 anonymous」分支（`bootstrapGenerate`/`generateBootstrapCredential`/`credentialTTLEnabled` 一并删除，`BootstrapServerCredentials` 同改）；改为「载入 store 快照 + 零凭据等待注册」，store 为空时启动日志提示「首次注册经回环，将成为 admin」（S2）。
- **requireRole 文件组门禁（DEC-C）**：文件操作路由组包门禁（任务②定义的 `requireRole(PrincipalFrom(ctx), RoleUser)`），`Role==node` → 403。
- `credentials_handler.go`（I6/DEC-A）：`getRole` 已改读 `Key.Role`（任务①）；**`akDeleteHandler` 定位目标 AK 后若 `Key.Role=="admin"` → 400（「admin 角色 AK 不可删除」，I6）**——防唯一 admin 自删导致系统无 admin、下一次注册者即 admin 的接管竞态；`akAddHandler` body 加 `role` 字段（`node`/`user`，默认 user，**admin AK 不可经此创建**——`role:"admin"` 拒绝/降级）。
- `cmd/sclient/trust.go`：`trust ak add` 加 `--role` flag（`node`/`user`，默认 user；帮助注明「node 用于 mesh 节点账号」）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "Register|AdminRole"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/register_handler.go pkg/server/register_handler_test.go pkg/server/handlers.go pkg/server/credentials_handler.go cmd/sclient/trust.go && git commit -m "feat(server): 公开注册端点（默认简单 AK/SK 模式，首 user 即 admin 原子授予 + 回环门禁）+ requireRole 文件组门禁 + admin AK 不可删"
```

---

### 任务 ④：pkg/client RequestSigner seam（ConfigSigner 默认 + 宿主注入）

**文件：**
- 修改：`pkg/client/client.go`（`RequestSigner` 接口 + 默认 `ConfigSigner` + `WithRequestSigner`）
- 创建：`pkg/client/signer_test.go`

**设计要点（DEC-D）：**
- `type RequestSigner interface { Sign(ctx context.Context, req *http.Request) error }`。
- 默认 `ConfigSigner`：用 config 的 `access_key`/`access_key_secret`/`access_key_id` 签名（当前
  `signRequest`/`sigRoundTripper` 行为，含 `WithAccessKey`/`WithAccessKeyID` 等 option 注入的字段；
  `accessKeySecret==""` 时不签名——公开端点直达）。
- **ConfigSigner 逐条承接现状 `sigRoundTripper` 全部行为（R3-M5）**：
  1. **v2 skey-id 强制**：配置了 `access_key` 但缺 `access_key_id` 且非引导态 → 报错
     「access_key_id 未配置（v2 skey-id 必传）」（client.go:1353/1436 既有语义）；
  2. **renew 引导缺段**：`allowMissingEntryID` 一次性开关放行引导路径（首次 renew 尚无 access_key_id）；
  3. **nonce + body hash（客户端计算）**：每次签名生成 16B nonce + 预计算 `body_sha256`（带 body 预计算
     哈希，UNSIGNED 标记缺省体）；服务端流式校验属既有 authMiddleware 行为，**非 ConfigSigner 承接**（R4-M3）；
  4. **隧道 UNSIGNED body**：/tunnel 路径 body 标记 UNSIGNED（metadata 已加密，不预计算哈希）；
  5. 直连（`signRequest`）与隧道（`sigRoundTripper`）两路径共用同一签名语义。
- `FileClient` 支持注入自定义 Signer：`WithRequestSigner(s RequestSigner)` option——宿主可复用自有凭据源/签名器；注入后 `signRequest`/`sigRoundTripper` **（含 xfer 隧道两路径）** 走该 Signer。
- 默认行为零变化：未注入 `WithRequestSigner` 时 = 现状 ConfigSigner。

- [ ] **步骤 1：编写失败的 Signer seam 测试**

`signer_test.go`（mock HTTP server）：
- **默认行为不变**：无注入时签名头与 4A 一致（断言请求带 `SproxySig` 头 + `skey-id=`）；`accessKeySecret==""` 时不带签名头。
- **注入自定义 Signer**：`WithRequestSigner(fake)` 断言每请求调用 `Sign` 一次；自定义 Signer 添加的头部出现在请求中。
- **直连 + 隧道路径均走注入 Signer（R3-M5）**：直连请求与 `WithTunnel` 隧道路径请求各断言注入的 Signer 被调用（两路径都不得绕过注入）。
- **renew 引导缺段承接**：未配置 `access_key_id` 且 `allowMissingEntryID` 引导态 → 仍放行（不报错）；非引导态 → 报「access_key_id 未配置」。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/client/ -run Signer`
预期：FAIL（类型/option 未定义）。

- [ ] **步骤 3：实现 RequestSigner seam**

`client.go`：接口 + `ConfigSigner` + `WithRequestSigner`；`signRequest`/`sigRoundTripper` 分支——注入的 Signer 优先，否则默认 ConfigSigner（逻辑与现状一致）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/client/...`
预期：PASS（4A 既有 client 测试全绿）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/client/client.go pkg/client/signer_test.go && git commit -m "feat(client): RequestSigner seam——ConfigSigner 默认 + WithRequestSigner 宿主注入"
```

---

### 任务 ⑤：4B-1 全链路测试（简单注册 → session 请求 200 / Role 持久化重启）

**文件：** 创建 `pkg/server/role_e2e_test.go`（httptest 黑盒全链路）

- [ ] **步骤 1：编写全链路测试**

- `TestRegisterSimple_FullChain`：注册（拿 ak + sk + skey_id）→ 用 `WithAccessKey(ak, sk)` + `WithAccessKeyID(skey_id)` 的 FileClient 请求 `GET /api/files` → 200（含子目录列表）。
- `TestRegister_FirstIsAdmin` → `getRole==admin`；`TestRegister_ConcurrentFirstAdmin`（并发双注册 → 恰一 admin，D2）。
- `TestAdminRole_PersistAfterRestart`（`store.Save→NewRing+Replace→getRole==admin`——**Role 持久化重启后仍 admin**，DEC-A）。
- **I6**：`TestAdminRole_AKDeleteRejected`（`DELETE /api/credentials/{ak}` 目标是 `Role==admin` 的 AK，`confirm`+`force` 均齐 → 400「admin 角色 AK 不可删除」）。
- **requireRole 门禁**：`Role==node` 访问 `GET /api/files` → 403；`Role==user`/`admin` → 200。
- `TestRegister_Disabled` → 403；`TestRegister_RemoteFirst_403` / `Loopback_200`（U2，e2e 级断言）；注册端点带 `Authorization` 头（错误签名）→ 仍可达（公开豁免断言）。
- **S4**：注册测试同时断言 `AddRegistration` 返回的 `granted` 与 `getRole(ak)` 一致（admin:true ↔ getRole==admin）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run "RegisterSimple|AdminRole"`
预期：FAIL（端到端路径未闭合）。

- [ ] **步骤 3：实现（如任务 ①-④ 已闭合则仅修复缺口）**

跑通缺口后补实现。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "Register|AdminRole"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/role_e2e_test.go && git commit -m "test(server): 4B-1 全链路——简单注册→SK 签名请求 200 / 并发首 admin / Role 持久化重启 / admin AK 不可删"
```

**4B-1 收尾**（全部任务后、PR 前）：

- [ ] 运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/...`
- [ ] 运行：`make lint && make build-all && make test-all && make check-loopback`
- [ ] 手测：零凭据启动（U3）+ 首 admin 经回环注册（U2，简单模式直接下发 SK）+ `allow_insecure_loopback` 兜底（**回环直通合成 Principal 放行本地读取**，R4-I2）；`trust ak add --role node` 创建 node 账号；`trust ak delete` 对 admin AK 返回 400；服务端重启后首个注册用户仍 admin（Role 持久化）；**简单模式注册 SK `ExpiresAt≈now+credential_ttl`（R3-I3）**；**简单模式 SK 过期后无续期路径（R4-M2）**——过期后签名 401、renew 因无有效 SK 失败、login 因无 TOTPSecret 404；恢复按 spec §7.5 指引（停服编辑 store 或重注册迁移旧桶）；**/tunnel 链路（R5-I1）**——SproxySig 认证后 POST /tunnel 隧道解密正常（sclient 隧道/relay 经 session skeyID 零改动）；宿主/无凭据场景 /tunnel 不派生（401）。
- [ ] **PR 描述与手测注明（R3-M6）**：「register 端点行为将在 4B-2（任务⑧）扩展为 `force_totp` 分支——默认保持简单模式（force_totp=false 行为不变）；部署者勿以本 PR 行为推断最终形态」。
- [ ] SDD 流程：review-package → 独立对抗审查 → 定向复审循环（用户 must-fix-all）。

---

# 4B-2：TOTP 插件（pkg/otp + force_totp + register TOTP 分支 + nonce/login + trust login）

### 任务 ⑥：`pkg/otp` TOTP 验证器（RFC 6238，纯 stdlib）

**文件：** 创建 `pkg/otp/totp.go`、`pkg/otp/totp_test.go`

**设计要点（D7）：**
- `GenerateSecret() ([]byte, error)` — 20B crypto/rand（GA 兼容 160-bit 密钥）。
- `func (t *TOTP) Code(now time.Time) (string, error)` — RFC 6238：`HMAC-SHA1(secret, 8B big-endian counter)`，counter = floor(unix/period)，period=30s，动态截断取 31-bit 后 mod 10^6 → 6 位；HMAC 用 `crypto/hmac`+`crypto/sha1`（GA 兼容；GA 默认 SHA1/6位/30s）。
- `func (t *TOTP) Validate(code string, now time.Time, window int) bool` — 校验当前及 ±window 个 period（窗口默认 1）。
- `func (t *TOTP) URI(label, issuer string) string` — `otpauth://totp/<url.PathEscape(label)>?secret=<base32>&issuer=<url.QueryEscape(issuer)>&algorithm=SHA1&digits=6&period=30`；base32 用 `encoding/base32.StdEncoding.WithPadding(base32.NoPadding)`。

- [ ] **步骤 1：编写失败的 TOTP 测试**

`totp_test.go` 表驱动：
- RFC 6238 官方测试向量（SHA1、secret=ASCII 字符串 `12345678901234567890`，**20 字节**，RFC 6238 §B 标准种子 + 时间戳）→ 断言 6 位码（向量表：59s→`287082`、1111111109→`081804`、1111111111→`050471`、1234567890→`005924`、2000000000→`279037`、20000000000→`353130`）。
- `Validate`：正确码 → true；错码 → false；±1 窗口内旧/新码 → true（`now-30s`/`now+30s` 的码）；±2 窗口（window=2）→ true；window=1 时 ±2 → false。
- `GenerateSecret`：长度 20、两次不同。
- `URI`：secret base32 无 padding、含 algorithm/digits/period/issuer/label 段。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/otp/...`
预期：FAIL（包不存在）。

- [ ] **步骤 3：实现 TOTP**

`totp.go`：上述函数。纯 stdlib；错误用 `fmt.Errorf` 包装。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/otp/...`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/otp/ && git commit -m "feat(otp): RFC 6238 TOTP 验证器——GA 兼容(SHA1/30s/6位) 生成/校验/±1窗口 + otpauth URI"
```

---

### 任务 ⑦：`pkg/accesskey` TOTP 扩展（wrap context 收归 + TOTP 包裹 + Key.TOTPSecret）

**文件：**
- 修改：`pkg/accesskey/accesskey.go`（`Key` 加 `TOTPSecret []byte` 字段，账号级）
- 修改：`pkg/accesskey/ring.go`（`cloneKey` 复制 TOTPSecret——`GetKey` 已在任务①实现，本任务补深拷贝字段）
- 创建：`pkg/accesskey/totp.go`（`WrapContextTOTP` + `DeriveTOTPWrapKey`）
- 修改：`pkg/accesskey/wrap.go`（`EncryptSecretKind`/`DecryptSecretKind`，旧签名薄委托）
- 创建：`pkg/accesskey/totp_test.go`、`pkg/accesskey/wrap_test.go`（追加）

**设计要点：**
- `Key` 加 `TOTPSecret []byte `json:"totp_secret,omitempty"``（账号级；`credentialstore` 序列化 `[]byte` 自动 base64，无需改 store；**与 SK 同级明文落盘，M12**）。
- `Ring.GetKey(ak)`（任务①已实现）返回的副本**含 `TOTPSecret`**——`cloneKey` 需复制 TOTPSecret 字节（`append([]byte(nil), ...)`）。登录 handler 按 AK 取 TOTPSecret 的唯一访问路径。
- `const WrapContextTOTP = "sproxy-totp/v1"`（与 `WrapContextCredentials` 同法收归 accesskey；client/server 均引用本常量，禁止字面量漂移）。
- `DeriveTOTPWrapKey(code string, ak, nonce string) ([]byte, error)`：
  `wrapKey(sha256(code), ak, WrapContextTOTP+"#"+nonce)`——内部复用现有 `wrapKey`（HKDF-SHA256），
  动态码经 SHA-256 展为 32B HKDF 输入（输入域 256bit，实际熵仍受 6 位码限制——由 nonce 唯一性 + 限频 + per-AK 锁定 + 短 session TTL 补偿，见 spec §6.3）。
- wrap.go 拆 Kind 参数化（**保持旧签名不变**，旧调用零改动）：
  ```go
  func EncryptSecret(wrapAK string, sk, wrapKey []byte) (*WrappedSecret, error)          // = EncryptSecretKind(KindSecretWrap, ...)
  func EncryptSecretKind(kind Kind, wrapAK string, sk, wrapKey []byte) (*WrappedSecret, error)
  func DecryptSecret(w *WrappedSecret, envelopeWrapKey []byte) ([]byte, error)            // = DecryptSecretKind(w, KindSecretWrap, ...)
  func DecryptSecretKind(w *WrappedSecret, expected Kind, envelopeWrapKey []byte) ([]byte, error)
  ```

- [ ] **步骤 1：编写失败的 TOTP 扩展测试**

`totp_test.go` / `wrap_test.go` 追加：
- `DeriveTOTPWrapKey`：同 (code, ak, nonce) 两次一致；nonce 不同 → key 不同；code 不同 → key 不同；context 与 `WrapContextCredentials` 派生出的不同（防跨 context 复用）。
- `EncryptSecretKind(KindTOTPWrap, ...)` → `WrappedSecret.Kind=="totp_wrap"`；`DecryptSecretKind(w, KindTOTPWrap, key)` 往返成功；`DecryptSecret(w, key)`（默认 secret_wrap）对 KindTOTPWrap 信封报错。
- `Key.TOTPSecret` 字段随 Snapshot/Replace 深拷贝保留（`cloneKey` 需复制 TOTPSecret 字节——先在测试断言）；修改返回值不影响原 ring。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/...`
预期：FAIL（符号未定义 / cloneKey 未复制 TOTPSecret）。

- [ ] **步骤 3：实现 TOTP 扩展**

`accesskey.go`：`Key` 加 `TOTPSecret` 字段；`ring.go` `cloneKey` 复制 `TOTPSecret`（`append([]byte(nil), ...)`）。
`totp.go`：常量 + `DeriveTOTPWrapKey`。`wrap.go`：拆 Kind 参数化，旧函数薄委托。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... -run "AccessKey|Wrap|Credentials"`
预期：PASS（server 复用 `EncryptSecret`/`DecryptSecret` 旧签名，零回归）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/ && git commit -m "feat(accesskey): TOTP wrap 扩展——WrapContextTOTP + DeriveTOTPWrapKey + Kind 参数化包裹 + Key.TOTPSecret"
```

---

### 任务 ⑧：register TOTP 分支 + force_totp 配置 + nonce 端点 + nonce 池

**文件：**
- 修改：`pkg/server/register_handler.go`（register TOTP 分支 + `nonceHandler`）
- 修改：`pkg/server/config.go`（`RegistrationConfig.ForceTOTP`，默认 false，DEC-B）
- 修改：`pkg/server/handlers.go`（`Handlers` 加 `totpNoncePool`/`totpLimiter` + 路由）

**端点设计（本任务定稿 TOTP 分支 + nonce；login 在任务 ⑨）：**

| 端点 | 认证 | 行为 |
|------|------|------|
| `POST /api/credentials/register` body `{owner?}`（**force_totp=true 分支**） | **公开** + 限频 | 与任务③共享 handler：`cfg.Registration.ForceTOTP==true` 时走 TOTP 分支——`accesskey.GeneratePair(nil,"")` 生成 AK（SK 丢弃）；`pkg/otp.GenerateSecret()` 生成 TOTP secret；**`ring.AddRegistration(ak, owner, nil, totpSecret, accesskey.RoleUser, 0)`**（I2/R4-I1：写锁内写 `Key.TOTPSecret` + 原子 admin 判定；**不创建 SK 条目**——用户须经 TOTP 登录拿 session SK，DEC-B；`ttl` 无 SK 条目被忽略）；`persistCredentials()`；`RecordAudit(credential_register)`。返回 `{ak, owner, admin: granted, otpauth_uri, base32_secret}`（**无 sk**；`otpauth_uri`/`base32_secret` 由 `pkg/otp` 提供）。loopback 预检/disable 判定与简单模式共用（U2/I4b）。body 防护同任务③（M7）。 |
| `POST /api/credentials/nonce` body `{}` | **公开** + 限频 | 服务端生成 16B 随机 nonce（`sproxysig.NewNonce()`，现有唯一实现），入 nonce 池 `map[nonce]→{expires_at, ip}`（TTL 60s、**单次使用**、**绑定来源 IP**、池上限 4096、插入/消费时惰性清理过期项，D5），返回 `{nonce, expires_at}`。body 防护同 register（M7）。 |
| 限频 | — | **复用既有 `pkg/server.RateLimiter`**（D6）：`totpLimiter = NewRateLimiter(10, time.Minute, log.With("component","totp_limiter"))`（login+nonce 共用，M1），以 `Middleware` 挂公开端点。 |

**登录 nonce 池不复用 `sproxysig.NoncePool`（M10）**：`sproxysig.NoncePool`（`sproxysig/nonce.go`）是按 `(ak, nonce)` 去重的**签名防重放池**，语义与登录 nonce 池不同——登录 nonce 需**单次消费**（任何登录尝试即删，防同 nonce 爆破）、**绑定来源 IP**、TTL 60s、**无 AK 前缀**、池上限 4096。故本计划新建 `totpNoncePool`，不复用既有池。

**TOTP secret 持久化**：`credentialsFile{Keys []accesskey.Key}`，`Key.TOTPSecret []byte`（base64）随 Save/Load 自动处理，无 store 代码改动。

- [ ] **步骤 1：编写失败的 TOTP 注册 + nonce 黑盒测试**

`register_handler_test.go` 追加（构造 `cfg.Registration.ForceTOTP=true`）：
- **TOTP 注册成功**：POST register → 200，返回 `{ak, admin:true, otpauth_uri, base32_secret}`（**无 sk 字段**）；AK 形态 `ak-<32hex>`；otpauth URI 含 `secret=<base32>`；`base32_secret` 无 padding；`GetKey(ak).TOTPSecret` 非 nil（20B）。
- **TOTP 注册不建 SK 条目**：`GetKey(ak).Entries` 空（`id` 空串）。
- **首 user 即 admin（TOTP 模式）**：`getRole(ak)=="admin"`；第二个注册 `admin:false`。
- **force_totp=false（默认）回归**：简单模式仍返回 `{sk, skey_id}`（任务③已覆盖）。
- **loopback 预检（U2）**：TOTP 分支同样受回环门禁（远程首注册 403）。
- **nonce 端点**：POST → 200 `{nonce, expires_at}`；两次不同；**池上限**：连发 >4096 后最早 nonce 被淘汰（可选大 TTL 注入验证）；**IP 绑定**：nonce 从 `127.0.0.1` 签发、另一来源 IP 消费（任务⑨ login 验证）→ 拒绝。
- **零凭据窗口 nonce 可达**：无 admin 时 nonce 端点回环可达（TOTP 登录前置步骤）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run "RegisterTOTP|Nonce"`
预期：FAIL（路由未注册 404 / handler 未定义）。

- [ ] **步骤 3：实现 TOTP 注册分支 + nonce handler + 池**

`register_handler.go`：`registerCredentialHandler` 按 `cfg.Registration.ForceTOTP` 分支（TOTP 分支生成 TOTP secret + `AddRegistration(ak, owner, nil, totpSecret, RoleUser, 0)`——`ttl` 忽略，R4-I1；`otpauthURI`/`base32Secret` 由 `pkg/otp` 提供；返回无 sk）。`nonceHandler`：`sproxysig.NewNonce()` 生成 + 入池（记录 `normalizeRemoteIP(r.RemoteAddr)` 来源 IP，`ratelimit.go:178`）+ 返回。`config.go`：`RegistrationConfig` 加 `ForceTOTP`（yaml `force_totp`，默认 false，DEC-B）+ SetDefaults/Validate。`handlers.go`：`Handlers` 加 `totpNoncePool`（`map[string]totpNonceEntry{expiresAt time.Time; ip string}` + mutex；单次消费、IP 绑定、**上限 4096**、插入/消费时惰性清理过期项，D5）、`totpLimiter`（`NewRateLimiter(10, time.Minute, log.With("component","totp_limiter"))`，D6/M1）；`srvMux.HandleFunc("POST /api/credentials/nonce", h.totpLimiter.Middleware(h.nonceHandler))` + localMux（公开，不包 authMiddleware）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "Register|Nonce"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/register_handler.go pkg/server/register_handler_test.go pkg/server/handlers.go pkg/server/config.go && git commit -m "feat(server): TOTP 注册分支（force_totp）+ nonce 端点（单次/IP 绑定/池上限 4096）+ 独立限频"
```

---

### 任务 ⑨：服务端登录端点（TOTP 校验 + session SK 签发 + 服务端控 TTL + per-AK 锁定）

**文件：** 修改 `pkg/server/register_handler.go`（登录 handler）、`pkg/server/handlers.go`（`Handlers` 加 `loginFailTracker` 字段，U4）、`pkg/server/config.go`（`SessionTTL`/`CliTTL`/`LoginFailLimit`/`LoginFailWindow`）、`pkg/server/register_handler_test.go`

**端点设计：**

| 端点 | 认证 | 行为 |
|------|------|------|
| `POST /api/credentials/login` body `{ak, nonce, code, login_type?}`（`login_type`=`web` 缺省 / `cli`，仅选服务端控 TTL，非 TTL 覆盖；未知值 → 400，M8） | **公开** + 限频 | ① **per-AK 锁定预检（U4）**：该 AK 在 `loginFailTracker` 锁定窗口内 → 一律 401（含正确动态码）；② nonce 消费（存在+未过期+未用过+**来源 IP 匹配** → 删除消费；**失败也消费**——防同 nonce 爆破，D5；**未知 nonce（池中不存在）→ 401 不计失败**；**池中存在但无效（重放/已消费/过期/IP 不匹配）→ 记失败并 401/400**，R2-N2）；③ `ring.GetKey(ak)` 取 `Key.TOTPSecret`（无 → 404，I1/M16）；④ `pkg/otp.Validate(code, now, 1)` 失败 → 记失败并 401；⑤ `wrapKey = accesskey.DeriveTOTPWrapKey(code, ak, nonce)`；⑥ 生成 session SK（`hex.DecodeString(accesskey.RandomHexHex(32))`，I3 沿用 renew 事实源模式）；⑦ `sessionTTL = cfg.Registration.SessionTTL`（web）/ `cfg.Registration.CliTTL`（cli）；⑧ `AddKey(ak, sessionSK, WithKind(KindTOTPWrap), WithWrapKeyID(""), WithExpiresAt(now+sessionTTL), WithMeta(Meta{Type:"login", IP}))`（**WrapKeyID 置空**——wrap 来自 TOTP code+nonce 而非 SK 间包裹，M13）+ session 条目惰性修剪（M11）；⑨ `persistCredentials()`；⑩ `envelope = EncryptSecretKind(KindTOTPWrap, ak, sessionSK, wrapKey)`；⑪ `RecordAudit(credential_login)`。返回 `{ak, session_skey_id, session_expires_at, wrapped_session_secret}`（snake_case，M2）。成功 → 清零该 AK 失败计数（U4）。body 经 `MaxBytesReader(MaxCredentialsBodyBytes)` + `drainAndVerifyBody` + 白名单解析（M7）。 |

**服务端控 TTL（D3）**：`sessionTTL` 按 `login_type` 选 `cfg.Registration.SessionTTL`（web，默认 24h）或 `cfg.Registration.CliTTL`（cli，默认 **7d**）。客户端 body 白名单无 ttl 字段——传了忽略；`login_type` 仅选择服务端控 TTL，不构成 TTL 覆盖；未知 `login_type` → 400（M8）。

**安全约束（spec §6.3 + D5 + U4/M9）**：nonce 单次使用 + 60s TTL + IP 绑定 + 池上限 4096 + 惰性清理；登录失败（TOTP 错）**同 nonce 已被消费**（防同 nonce 爆破）+ login 端点限频（`totpLimiter` 10/min per-IP → 429）+ **per-AK 失败锁定（U4）**（连续 5 次失败锁 15 分钟，锁定期内该 AK 一律 401，不随 IP 变化失效——防分布式 botnet 爆破）。**失败计数语义（R2-N2）**：**未知 nonce（池中不存在）不计失败**（防攻击者用随机垃圾 nonce 低成本锁定任意 AK 的 DoS）；**池中存在但无效（重放/已消费/过期/IP 不匹配）与 TOTP 错计失败**（防同 nonce 爆破 + 防乱猜动态码）。`code` 参与 wrap key 派生且响应只传 wrapped。**威胁模型如实表述（M9）**：截获 wrapped 响应的攻击者**离线爆破 6 位码不受 30s 服务端窗口限制**（码在签发时刻有效即可）；实际保护 = TLS 边界（截获需破 TLS）+ session TTL（web 24h / cli 7d）+ 爆破 1M 空间的成本 + per-AK 锁定。

- [ ] **步骤 1：编写失败的登录黑盒测试**

`register_handler_test.go` 追加（构造 `cfg.Registration.ForceTOTP=true`）：
- **登录成功**：注册拿 `base32_secret` → 测试内用 `pkg/otp` 复算 code（模拟 GA）→ nonce → login → 200 `{ak, session_skey_id, session_expires_at, wrapped_session_secret}`（M2）；`session_skey_id` 前缀 `skey-`；用 `DeriveTOTPWrapKey(code, ak, nonce)` + `DecryptSecretKind(w, KindTOTPWrap, key)` 解出 32B session SK 且 `len==32`（I3）。
- **错误动态码** → 401。
- **per-AK 锁定（U4）**：同一 AK 连续 5 次错误动态码 → 第 6 次登录（**含正确动态码**）→ 401（锁定窗口内）；ring 注入 now 前进超过 `LoginFailWindow` → 解锁，可登录成功。
- **登录对无 TOTPSecret 的 AK**（构造 ring 中无 TOTPSecret 的 AK）→ 404（M16）。
- **nonce 重放**：同一 nonce 二次 login → 拒绝（401/400）。
- **nonce 过期**：注入过期时间 → 拒绝。
- **nonce IP 不匹配（D5）**：nonce 从 `127.0.0.1` 签发、从另一来源 IP 消费 → 401（注入 `RemoteAddr` 变化）。
- **垃圾 nonce 不计失败（R2-N2）**：同一 AK 携带池中不存在的 nonce 连发多条 login → 均 401 但不累计失败计数；随后正确 nonce + 正确 code 登录成功（未被误锁）。
- **nonce 池登录侧淘汰（M16）**：插入过期 nonce 后 login 消费触发惰性清理（池大小收缩断言）。
- **TTL 服务端控 + 分场景（D3）**：`login_type=web`（缺省）断言 `session_expires_at == now+cfg.Registration.SessionTTL`；`login_type=cli` 断言 `== now+cfg.Registration.CliTTL`；客户端 body 带 `ttl` 字段被忽略；session 条目在 `ExpiresAt` 后签名 → 401（ring 注入 now 前进）。
- **login_type 未知值 → 400（M8/M16）**。
- **session skeyID 立即可用**：用解出的 session SK + skeyID 签名 GET `/api/credentials/<ak>/sk` → 200（v2 强制必传路径）。
- **session 条目惰性修剪（M11）**：多次登录后该 AK 过期 session 条目被清除（仅保留存活条目）。
- **简单模式无 login**：`force_totp=false` 注册的 AK（无 TOTPSecret）登录 → 404（M16）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run Login`
预期：FAIL（handler 未定义）。

- [ ] **步骤 3：实现登录 handler**

`loginCredentialHandler`：按上面端点表。**per-AK 锁定状态（U4）**：`Handlers` 加 `loginFailTracker`
（`map[ak]→{failCount int; lockedUntil time.Time}` + mutex；**map 上限 1024 条 + 惰性清理（R2-N1）**
——插入时若已达上限，先修剪 `lockedUntil` 已过期的条目，仍满则淘汰最早插入条目，防 map 无界增长）。
**失败计数语义（R2-N2）**：**未知 nonce（池中不存在）→ 401 不计失败**（防随机垃圾 nonce 低成本锁定
任意 AK 的 DoS）；**重放/已消费/过期/IP 不匹配 nonce（池中存在但无效）与 TOTP 错** → `failCount++`，
达 `cfg.Registration.LoginFailLimit`（默认 5）→ `lockedUntil = now + cfg.Registration.LoginFailWindow`
（默认 15m）并清零；锁定窗口内该 AK 登录一律 401（含正确动态码，不随 IP 变化失效）；登录成功清零计数。
TOTP secret 取用 **`ring.GetKey(ak)`**（I1，读 `Key.TOTPSecret`；AK 不存在/无 TOTPSecret → 404）。
session SK 生成用 **`hex.DecodeString(accesskey.RandomHexHex(32))`**（I3，沿用 renew 既有事实源模式
credentials_handler.go:231-238，`len==32`）。**session 条目惰性修剪（M11）**：追加 session 条目前，
对该 AK 修剪 `now≥ExpiresAt` 的条目，**保留 `ExpiresAt` 零值条目**（永不过期条目不参与修剪，R3-M1；
`Replace` 加载快照时同样执行）——防 `Key.Entries` 只增不减。body 白名单解析（M7）：
只读 `ak`/`nonce`/`code`/`login_type` 四字段（未知字段忽略），`login_type` 未知值 → 400（M8）。
错误映射：锁定窗口内 → 401；未知 nonce → 401（不计失败）；重放/已消费/过期/IP 不匹配 nonce 与 TOTP
错 → 401（并记失败）；AK 无 TOTP secret → 404；持久化失败 → `credential_persist_error` + 500。
`config.go`：`RegistrationConfig` 加 `SessionTTL`（yaml `session_ttl`，默认 24h）、`CliTTL`（yaml
`cli_ttl`，默认 **7d**）、`LoginFailLimit`（yaml `login_fail_limit`，默认 5）与 `LoginFailWindow`（yaml
`login_fail_window`，默认 15m，U4），`SetDefaults`/`Validate` 同步；`config.example.yaml` 的
`registration` 段注释补 `session_ttl`/`cli_ttl`/`login_fail_limit`/`login_fail_window`。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "Login|Register"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/register_handler.go pkg/server/register_handler_test.go pkg/server/handlers.go pkg/server/config.go && git commit -m "feat(server): TOTP 登录端点——nonce 单次 + 动态码校验 + session SK(KindTOTPWrap) 签发 + 服务端控 TTL + per-AK 锁定"
```

---

### 任务 ⑩：`sclient trust login` + `pkg/client` TOTP 领域 API

**文件：**
- 修改：`pkg/client/accesskey.go`（追加 TOTP 领域 API）
- 创建：`cmd/sclient/trust_login.go` + `cmd/sclient/trust_login_test.go`
- 修改：`cmd/sclient/trust.go`（`NewCmdTrust` 挂 `trust login`）
- 修改：`pkg/client/accesskey_test.go`、`cmd/sclient/trust_test.go`（追加）

**设计要点：**
- `pkg/client` 领域 API（**显式无凭据客户端（M14）**——`trust login` 的 RPC 从配置读出的
  `access_key`/`access_key_secret`/`access_key_id` 三字段全部清空再发 register/nonce/login，避免带
  过期签名头；`ConfigSigner` 在 `accessKeySecret==""` 时不签名，直达公开端点）：
  ```go
  type TOTPRegisterResult struct {
      AK           string `json:"ak"`
      Owner        string `json:"owner"`
      Admin        bool   `json:"admin"`
      OTPAuthURI   string `json:"otpauth_uri"`
      Base32Secret string `json:"base32_secret"`
  }
  func (c *FileClient) RegisterTOTP(ctx context.Context, owner string) (*TOTPRegisterResult, error)
  type TOTPNonce struct {
      Nonce     string    `json:"nonce"`
      ExpiresAt time.Time `json:"expires_at"`
  }
  func (c *FileClient) RequestTOTPNonce(ctx context.Context) (*TOTPNonce, error)
  type TOTPLoginResult struct {
      AK               string    `json:"ak"`
      SessionSkeyID    string    `json:"session_skey_id"`    // 响应字段 snake_case（M2）；Go 字段可驼峰
      SessionExpiresAt time.Time `json:"session_expires_at"`
      SessionSK        []byte    // 解密后的明文 session SK（仅本端，不上线）
  }
  func (c *FileClient) LoginTOTP(ctx context.Context, ak, nonce, code, loginType string) (*TOTPLoginResult, error)
  ```
  `LoginTOTP` 内：POST body 带 `login_type`（web/cli，D3）→ 拿 `wrapped_session_secret` → `accesskey.DeriveTOTPWrapKey(code, ak, nonce)` + `DecryptSecretKind(w, KindTOTPWrap, key)` 解 session SK。
- `trust login` 命令流程（薄逻辑：flag + IO + 回填；**回填走 `login_type=cli`**，D3）：
  1. 无 `access_key` 配置（或 `--register`）→ 调 `RegisterTOTP`：打印 `ak`、`base32_secret`（唯一展示，不落日志）、`otpauth_uri`；提示「已加入 Authenticator 后输入 6 位动态码」；响应 `admin:true` → 额外提示「您是首个注册用户，将成为 admin」（S2）。
  2. 已注册 → 直接提示输入动态码；本地无配置 AK 时可用 `--ak <AK>` 手动指定（M6）。
  3. `RequestTOTPNonce` → `LoginTOTP(ak, nonce, code, "cli")` → 解 session SK。
  4. **回填前覆盖确认（D4）**：`cfg.AccessKeySecret` 非空（已有凭据，可能来自 renew 的长命 SK）→ 打印提示「将覆盖现有凭据；长期运行 daemon 建议 `trust renew`」并交互确认（输入 y/N），`--overwrite` 跳过确认；未确认 → 非零退出（仿 `errDeleteAKNotConfirmed` 语义）。
  5. 回填 `config set access_key <ak>` / `access_key_secret <hex(sessionSK)>` / `access_key_id <session_skeyID>`（沿用 `trust renew` 的 `SaveConfig` 回填模式）。
  - flags：`--owner`（帮助注明「owner 影响文件桶归属，默认=AK」，S1）、`--register`（强制走注册）、`--overwrite`（跳过覆盖确认）、`--ak`（已注册但本地无配置 AK 时手动指定，M6）。

- [ ] **步骤 1：编写失败的 pkg/client TOTP 领域 API 测试**

`accesskey_test.go` 追加（mock HTTP server）：
- `RegisterTOTP`：POST `/api/credentials/register` → 解析 `{ak, admin, otpauth_uri, base32_secret}`。
- `LoginTOTP`：mock 返回 wrapped（测试内预构造 `EncryptSecretKind(KindTOTPWrap,...)` 信封）→ 解出与构造一致的 session SK；断言请求体 `login_type` 透传（web/cli）；断言响应 `session_skey_id`（snake_case，M2）被正确解析。
- `RequestTOTPNonce`：POST `/api/credentials/nonce` → 解析 nonce。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/client/ -run TOTP`
预期：FAIL（函数未定义）。

- [ ] **步骤 3：实现 pkg/client TOTP 领域 API**

按上面签名；RPC 构造**无凭据客户端**（M14：发送前清空 `access_key`/`access_key_secret`/`access_key_id` 三字段）；`doJSON` 复用（无凭据时无 Authorization 头）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/client/ -run TOTP`
预期：PASS。

---

- [ ] **步骤 5：编写失败的 trust login 命令测试**

`trust_login_test.go`（CaptureStdout/Stdin 注入模式）：
- `trust login`（mock RPC：register+nonce+login 依次）→ 打印 ak/base32 提示 → 输入 code → 断言 `config set` 三项（access_key/access_key_secret/access_key_id）被调用且值 = 解出的 session 凭据；断言 login 请求 `login_type=cli`（D3）；断言 RPC 请求**无 Authorization 头**（M14 无凭据客户端）。
- `trust login --register` 强制注册分支；stdin EOF（无输入）→ 非零退出（仿 `errDeleteAKNotConfirmed` 语义）。
- **`--ak` 手动指定（M6）**：无配置 AK 时 `--ak <AK>` 走 login 分支（不触发注册），mock 断言请求体 ak 正确。
- **S2 首 admin 提示**：mock register 返回 `admin:true` → 输出含「您是首个注册用户，将成为 admin」。
- **D4 覆盖确认**：预置非空 `access_key_secret` → 输入 `n` → 中止非零退出、config 未被覆盖；输入 `y` → 回填成功；`--overwrite` → 跳过提示直接回填。

- [ ] **步骤 6：运行验证失败**

运行：`go test -race -count=1 ./cmd/sclient/ -run TrustLogin`
预期：FAIL。

- [ ] **步骤 7：实现 trust login**

`trust_login.go`：`newCmdTrustLogin` 按上面流程（含 `--ak`/`--owner`/`--register`/`--overwrite` flags；
`--owner` 帮助注明「owner 影响文件桶归属，默认=AK」（S1）；覆盖确认分支 D4；`login_type=cli` D3；
RPC 用无凭据客户端 M14）；回填用 `cfgSvc.LoadConfig` + `client.SaveConfig(cfg, *cfgFile)`（`trust renew` 同款）。`trust.go` 的 `NewCmdTrust` 追加挂载。

- [ ] **步骤 8：验证通过 + 收尾**

运行：
```bash
go test -race -count=1 ./pkg/otp/... ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/...
make lint && make build-all && make test-all && make check-loopback
```
修复任何 lint/构建/test 失败。

- [ ] **步骤 9：Commit**

```bash
git add cmd/sclient/trust_login.go cmd/sclient/trust_login_test.go cmd/sclient/trust.go pkg/client/accesskey.go pkg/client/accesskey_test.go && git commit -m "feat(sclient): trust login——GA 密钥录入→注册/登录→解 session SK 回填 access_key 三件套"
```

---

### 任务 ⑪：4B-2 全链路验证（TOTP 注册→登录→session 请求 200 / 错误路径）

**文件：** 创建 `pkg/server/totp_e2e_test.go`（httptest 黑盒全链路）

- [ ] **步骤 1：编写全链路测试**

- `TestTOTPLogin_FullChain`：注册（拿 ak + base32_secret）→ `pkg/otp` 复算 code → nonce → login（拿 session skeyID + wrapped）→ 解出 session SK → 用 `WithAccessKey(ak, hex(sessionSK))` + `WithAccessKeyID(skeyID)` 的 FileClient 请求 `GET /api/files` → 200（含子目录列表）。
- `TestTOTPLogin_WrongCode` → 401；`TestTOTPLogin_NonceReplay` → 拒绝；`TestRegisterTOTP_Disabled` → 403；`TestRegisterTOTP_FirstIsAdmin` → `getRole==admin`；`TestRegisterTOTP_ConcurrentFirstAdmin`（并发双注册 → 恰一 admin，D2）；`TestSessionExpiry_Request401`（ring now 注入前进到 ExpiresAt 后 → 签名 401）。
- **M16**：`TestLogin_NoTOTPSecret_404`（无 TOTPSecret 的 AK 登录 → 404）；`TestAdminRole_TOTPPersistAfterRestart`（`store.Save→NewRing+Replace→getRole==admin`——Role 持久化重启后仍 admin；**且用重启后 `Key.TOTPSecret` 复算 code 走完整登录端点 → 200**——TOTP secret 持久化闭环断言，R2-N5）；`TestLogin_UnknownLoginType_400`。
- **S4**：注册测试同时断言 `AddRegistration` 返回的 `granted` 与 `getRole(ak)` 结果一致（admin:true ↔ getRole==admin）。
- 注册端点带 `Authorization` 头（错误签名）→ 仍可达（公开豁免断言）；远程来源首注册（无 admin）→ 403、回环 → 200（U2，e2e 级断言）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run TOTP`
预期：FAIL（端到端路径未闭合）。

- [ ] **步骤 3：实现（如任务 ⑧/⑨/⑩ 已闭合则仅修复缺口）**

跑通缺口后补实现。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "TOTP|Register|Login"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/totp_e2e_test.go && git commit -m "test(server): TOTP 注册→登录→session 全链路——200/401/重放/过期/禁用 覆盖"
```

---

# 4B-3：Web 注册/登录页 + 客户端 JS QR

### 任务 ⑫：Web 注册/登录页 + 客户端 JS QR + sig.js 注释清理 + access_key_id 回填（独立 PR）

> **前提缺陷（按 1618835f 实际代码核实）**：`sig.js` 的**输出已是 `skey-id=`**（line 115，与 Go 对齐），但**注释仍写 `sk=`**（line 9/27/29/102/103，过时）；`transport.js` 已接 `accessKeyID`（line 303/423，两路径透传）；**`app.js` 未持久化 `sproxy_access_key_id`** → Web 配凭据后直连/隧道签名在 v2 强制必传下会 401。本任务一并修复。

**文件：**
- 修改：`web/static/sclient/sig.js`（**清理过时注释** `sk=` → `skey-id=`；输出段名已对，无需改逻辑；canonical 不变；对齐 `pkg/sproxysig.SignAndFormat`）
- 修改：`web/static/sclient/transport.js`（`accessKeyID` 已支持，确认直连/隧道两路径透传——既有断言）
- 修改：`web/static/app.js`（sessionStorage 键 `sproxy_access_key_id` 存取，`saveAccessKeys` 同步）
- 创建：`web/static/login.html`（或并入 index.html 模态）+ `web/static/login.js`（注册表单/动态码输入/QR 渲染）
- 创建：`web/static/login.test.js`（`make web-test` 纳入）
- 修改：`web/static/sclient/sig.test.js`（skey-id 向量）

**流程：**
1. 注册页（`force_totp=true` 模式）：输入 owner → POST `/api/credentials/register` → 展示 `base32_secret` 文本 + otpauth URI + **客户端 JS QR**。**QR 走客户端渲染（D7）**：vendored 轻量 JS QR 编码器进 `web/static/`（如内嵌 qrcode.min.js 或自写最小编码器，非 Go 依赖），GA 扫码从浏览器渲染；同时保留「手动输入密钥」路径（CLI 场景打印 base32 供手动输入 GA）。
2. 登录页：输入 AK + 6 位动态码 → `RequestTOTPNonce` → `LoginTOTP(..., "web")`（D3：浏览器场景走 `login_type=web`，服务端按 `SessionTTL` 24h 发短 session SK）→ 用 `sclientCrypto` 做 `DeriveTOTPWrapKey` 等价派生（JS 端新增 totp wrap key 派生，对齐 `pkg/accesskey`）+ 解密（`crypto.subtle` AES-GCM）→ 得 session SK/skeyID → 存 `sessionStorage`（`sproxy_access_key`/`sproxy_access_key_secret`/`sproxy_access_key_id`）。**登录页 sessionStorage 记住最近 AK（S3）**：登录成功后写入 `sproxy_last_ak`，下次打开自动回填 AK 输入框，减少手输。
3. 后续请求：`transport.js` 已接 `cfg.accessKeyID`，签名自动带 `skey-id=`（输出段名已就绪）。

**验证：** `make web-test`（login.test.js + sig.test.js 新向量）+ 浏览器手测（chrome-devtools）：真实注册→GA（模拟器或测试内 pkg/otp 复算）→登录→列表刷新 200；console 无 error。

- [ ] **步骤 1：编写失败的登录页 JS 测试**（`login.test.js`：注册表单提交 → register 调用；QR 渲染断言；登录表单 → nonce+login 调用；sessionStorage 回填三键 + `sproxy_last_ak`）
- [ ] **步骤 2：运行验证失败**：`make web-test` → FAIL
- [ ] **步骤 3：实现**（sig.js 注释清理 + app.js `sproxy_access_key_id` 回填 + login.js/login.html + vendored JS QR）
- [ ] **步骤 4：验证通过**：`make web-test` → PASS
- [ ] **步骤 5：Commit**

```bash
git add web/static/ && git commit -m "feat(web): 注册/登录页 + 客户端 JS QR + sproxy_access_key_id 回填 + sig.js 注释清理"
```

---

## 收尾验证（全部任务后）

- [ ] **步骤 1：全量验证**

```bash
git fetch origin && git rebase origin/master   # 并入最新 master（若有冲突按 feature 语义解决）
go test -race -count=1 ./pkg/otp/... ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/... ./pkg/tunnel/hub/...
make lint && make build-all && make test-all && make check-loopback && go vet ./...
make web-test
```

- [ ] **步骤 2：手测（CLI + 浏览器）**

- **零凭据启动（U3）**：store 为空启动 → 日志提示「首次注册经回环，将成为 admin」（S2），不打印任何 AK/SK；ring 空、无自动生成凭据。
- **首注册仅回环（U2）**：远程 `sclient trust login`（首 admin 未注册时）→ register 403；本地回环（同机或 SSH 隧道）注册 → 成功为 admin；有 admin 后远程注册恢复正常（受 `registration.disable` 控制）。
- 首启（无凭据、registration.disable=false、force_totp=false）：本地注册 → 直接下发 SK → 回填 → `sclient list` 200。
- 首启（force_totp=true）：`sclient trust login` 全流程（GA 录入 base32 → 动态码 → session SK 回填）→ `sclient list` 200。
- 注册端点在 `registration.disable=true` 下 403；第二个注册用户非 admin。
- 浏览器：注册→扫码/手动密钥→登录→列表 200；session SK 过期后重登。
- 全链凭据：`sclient trust login` 后信令/relay/mesh 用 session skeyID 正常（零改动断言）。
- I6：`trust ak delete` 对 admin 角色 AK 返回 400；服务端重启后首个注册用户仍 admin（Role 持久化）。
- D3/D4/U4：`trust login`（cli）后 `trust sk list` 显示 session 条目 `ExpiresAt≈now+cli_ttl`（**7d**）；已有凭据时 `trust login` 提示覆盖确认、`n` 中止；同一 AK 连续输错 5 次动态码后第 6 次（含正确码）仍 401，窗口过后恢复。

- [ ] **步骤 3：独立审查 + 修复全部发现（含 Minor）**

SDD 流程：review-package → 独立对抗审查 → 定向复审循环（用户 must-fix-all）。4B-1 / 4B-2 / 4B-3 分别 PR。

- [ ] **步骤 4：PR**

```bash
# 4B-1
git push -u origin feature/credentials-4b-totp-login
gh pr create --title "feat(server): 认证面插件化 + 角色模型 + 默认简单 AK/SK 注册——Authenticator seam + Key.Role + register 回环门禁" --body "…"
# 4B-2 / 4B-3 各拉独立分支，title 聚焦 TOTP 插件 / Web 注册登录页。
```
（title 功能为核心，不含阶段编号。）

## 自检

- **规格覆盖度**：Role 模型 + `GetKey`/`AddRegistration` + getRole 改造（任务①，DEC-A/I1/I2）✓ / Authenticator 链 + 宿主注入 + requireRole（任务②，DEC-C）✓ / register 简单模式 + 回环门禁 + 零凭据启动 + admin AK 不可删（任务③，U2/U3/I6）✓ / ConfigSigner seam（任务④，DEC-D）✓ / 4B-1 全链路（任务⑤，含 Role 持久化重启）✓ / TOTP 验证器（任务⑥，D7）✓ / wrap 收归 accesskey + TOTP 包裹（任务⑦）✓ / register TOTP 分支 + force_totp + nonce 池（任务⑧，DEC-B/D5）✓ / 登录 + session SK + 分场景服务端控 TTL + per-AK 锁定（任务⑨，D3/U4）✓ / trust login + 领域 API（任务⑩，D4/M6/M14）✓ / 4B-2 全链路测试（任务⑪，含 R2-N5）✓ / Web 页 + JS QR + access_key_id 回填（任务⑫，D7/S3）✓。
- **设计缺口闭环**：DEC-A Role 三分 + admin 判定读 Key.Role + 首注册原子授 admin + akDelete 拒绝 admin AK ✓ / DEC-B 双模式注册（简单 AK/SK + force_totp）+ AddRegistration 新签名 ✓ / DEC-C Principal/Authenticator/RingAuthenticator + RegisterRoutesOpts.Authenticators（**非 nil replace、链先跑**）+ requireRole ✓ / DEC-D RequestSigner seam（**ConfigSigner 承接 sigRoundTripper 全行为**）✓ / DEC-E pkg/otp stdlib + 客户端 JS QR + 三方库进 ext ✓ / DEC-F 零凭据启动 + 回环首 admin ✓。既有已定稿细节（U1 cli 7d / U2 / U3 / U4 / I1-I7 / M1-M16 / S1-S4 / R2-N1..N5）按新结构全部迁移保留。
- **round-3 修复闭环（R3-I1/I2/I3 + M1-M6 + S1）**：文件按 `Principal.AK` 落桶（任务②，R3-I1）✓ / 链先跑 + replace 语义 + handleNoCredentials 最终兜底（任务②，R3-I2）✓ / 简单模式 SK `ExpiresAt=now+ttl`（调用方注入）+ admin 恢复指引（任务①③，R3-I3+S1）✓ / 修剪措辞对齐（任务⑨，R3-M1）✓ / AddRegistration 双 nil 校验（任务①，R3-M2）✓ / force_totp 切换语义（spec §11，R3-M3）✓ / Role 空值归一（任务①，R3-M4）✓ / ConfigSigner 承接 + xfer 注入测试（任务④，R3-M5）✓ / 4B-1 PR 衔接注明（任务③收尾，R3-M6）✓。
- **round-4 修复闭环（R4-I1/I2/I3 + M1-M3）**：AddRegistration 加 `ttl time.Duration` 参数（handler 经 `credentialTTLFromCfg()` 注入，pkg/accesskey 不读 server 配置；任务①③，R4-I1）✓ / 回环直通合成最小 `Principal{AK:"",Owner:"",Role:"user"}` 放行 requireRole（任务② + spec §7.2/§7.6，R4-I2）✓ / 先重构 `verifySproxySigFromRing` 为「失败返回 error 不写响应」+ `setResponseActor` 改 `Principal.AK`（任务②，R4-I3+M1）✓ / 简单模式 SK 过期无续期路径 + 迁移旧桶（spec §7.5 + 4B-1 收尾手测，R4-M2）✓ / ConfigSigner 承接项删除服务端 `NewBodyValidator`（spec §9.1 + 任务④，R4-M3）✓。
- **round-5 修复闭环（R5-I1 + M1）**：`Principal` 增 `Secret []byte` 字段 + authMiddleware `/tunnel` 派生改读 `Principal.Secret + Principal.Mesh`（RingAuthenticator 填充、宿主留空不派生；spec §7.2 + 任务②，R5-I1）✓ / `/tunnel` 链认证解密正常 + 宿主不派生测试 + 4B-1 收尾手测 ✓ / spec §2.3 正文 AddRegistration 签名补 `ttl`（6 参，R5-M1）✓。
- **4A 复用断言**：全计划零新增 AK/SK 生成/解析；SK 生成沿用既有事实源模式 `hex.DecodeString(accesskey.RandomHexHex(32))`（credentials_handler.go:231-238，I3）；只调 `accesskey`/`pkg/otp` 导出。
- **占位符扫描**：无 TODO/待定；所有步骤含实际代码/命令。（mesh/hub requireRole 组接线点在 spec §13 列后续，非本计划占位。）
- **类型一致性**：`RoleUser`/`RoleNode`/`RoleAdmin`/`Key.Role`/`session_skey_id`（snake_case）/`KindTOTPWrap`/`WrapContextTOTP`/`DeriveTOTPWrapKey`/`EncryptSecretKind`/`GetKey`/`AddRegistration(ak, owner, sk, totpSecret, role, ttl)`（R4-I1）/`login_type`/`force_totp`/`login_fail_limit`/`login_fail_window` 跨任务一致。
