# 凭据 Store 化 + SK 滚动（4A）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 sproxy 的 SproxySig 凭据从静态 yaml `access_keys` 迁移到 store 化权威表（`pkg/accesskey.Ring`），SK 支持多条目共存与运行时滚动（renew），新增 `/api/credentials` 管理端点与 sclient `trust` 命令，签名协议升级 v2，并为 4B（TOTP 注册/登录）与 4C（KMS/插件）预留钩子。

**架构：** 单一事实源 `pkg/accesskey.Ring`（AK→多 SKEntry，加锁原子集合）。启动从 `storage_root/<tenant>/meta/credentials.json` 载入；authMiddleware / hub.Authenticator / 信令 / tunnel 派生密钥全部查同一 Ring 实例。yaml `access_keys` 彻底移除；首启自动生成 anonymous 凭据；管理端点仿 PUT /api/config（COW + RecordAudit），普通用户操作自己的 SK 条目，admin（4B 前无）可操作全量/AK。签名 v2（`sk=<entryID>` 可选 + canonical 升级）。

**技术栈：** Go 1.26，纯 stdlib + `golang.org/x/crypto/hkdf`（wrap key 派生），`crypto/aes`+`crypto/cipher`（AES-GCM），slog，cobra/pflag（sclient）。核心 go.mod 零新增三方依赖。

**BASE**：`5b42ce53`（feature/credentials-store-sk-rotation）。

---

## 文件结构总览

**新建**
- `pkg/accesskey/accesskey.go` — 类型：`Kind`/`Status`/`Meta`/`SKEntry`/`Key`（AK→多 SK 模型）
- `pkg/accesskey/ring.go` — `Ring` 原子集合（UpsertAK/AddKey/DeleteAK/DeleteKey/ExpireKey/Lookup/CoreEntry/GetEntry/Snapshot/Replace/Len）
- `pkg/accesskey/wrap.go` — 信封加密（HKDF wrap key + AES-GCM 包裹 SK）
- `pkg/accesskey/accesskey_test.go`、`ring_test.go`、`wrap_test.go`
- `pkg/server/credentialstore.go` — `CredentialStore`：`<tenant>/meta/credentials.json` 原子读写（Load/Save）
- `pkg/server/credentials_handler.go` — `/api/credentials/*` 管理端点
- `pkg/client/accesskey.go` — 领域 API（RotateAccessKey/ListAccessKeys/DeleteSK/ExpireSK/AddAK/DeleteAK）
- `cmd/sclient/trust.go`（或拆分 trust_ak.go/trust_sk.go）— `trust` 命令族
- `pkg/server/credentials_handler_test.go`、`credentialstore_test.go`

**修改**
- `pkg/sproxysig/sproxysig.go` — v2（Version="2"、Header+EntryID、ParseHeader 收 `v=2`/`sk=`、Canonical 可含 entryID）
- `pkg/server/auth.go` — 移除 `AccessKeyConfig` 定义与 `cfg.AccessKeys` 遍历 → 查 Ring；`VerifySproxySigMulti`；`tunnelDerivedKey` 改收 SK；`authMiddleware` 兜底逻辑；移除 `allow_no_auth` 全放行，改 `allow_insecure_loopback`
- `pkg/server/config.go` — 移除 `AccessKeys` 字段 + Validate 块；新增 `Registration{Allow bool}`、`AllowInsecureLoopback bool`、`CredentialTTL` 配置
- `pkg/server/handlers.go` — `RegisterRoutesOpts` 加 `CredentialRing`/`CredentialStore`/`CSRF`；装配；注册管理端点（主 mux + localMux）
- `pkg/server/config_api.go` — `access_keys_set` 布尔改查 store 状态
- `pkg/server/xfer_listener.go` — `cfg.AccessKeys[0]` 改 Ring 查询
- `pkg/tunnel/hub/auth.go` — `NewAuthenticator(r *accesskey.Ring)`（替换 `[]AccessKey`）
- `pkg/tunnel/hub/router.go` — fail-closed 兜底（nil→空 ring）
- `cmd/sproxy/root.go` — 装配：store→ring→anonymous 首启 → RegisterRoutes + hub.NewAuthenticator(ring)；移除 `len(cfg.AccessKeys)` fail-fast
- `pkg/client/config.go` — `access_key_id` 可选字段（renew 回填）
- `pkg/client/client.go`/`cmd/sclient/*` 签名路径 — v2 兼容
- 测试：`pkg/server/*_test.go`（~30 处 `cfg.AccessKeys` 迁移到 seed ring）、`pkg/tunnel/hub/*_test.go`（~40 处 NewAuthenticator）、`pkg/tunnel/mesh/mesh_test.go`、`cmd/sproxy/*_test.go`、`cmd/sclient/*_test.go`、`pkg/sproxysig/sproxysig_test.go`
- 文档：`docs/config.md`/`docs/architecture.md`/`config.example.yaml`、`CLAUDE.md`（SIGHUP 重载范围删除 access_keys）

---

### 任务 1：pkg/accesskey 核心包（数据模型 + Ring + 信封加密）

**文件：**
- 创建：`pkg/accesskey/accesskey.go`
- 创建：`pkg/accesskey/ring.go`
- 创建：`pkg/accesskey/wrap.go`
- 创建：`pkg/accesskey/accesskey_test.go`
- 创建：`pkg/accesskey/ring_test.go`
- 创建：`pkg/accesskey/wrap_test.go`

**设计要点（来自规格 5）：**
- `SKEntry`：`ID string`（`sk-<12hex>`，创建时 `crypto/rand` 生成）、`SK []byte`（32B）、`Kind`、`WrapKeyID`、`CreatedAt`、`ExpiresAt`（零值=永久）、`Status`、`Meta`
- `Meta{ Type string; IP string }`
- `Key{ AK string; Owner string; Entries []SKEntry }`（Mesh 由 `tunnel.AccessKeyMesh(AK)` 派生，**不存字段**避免漂移——但 `pkg/accesskey` 不 import `pkg/tunnel`（避免循环依赖），Mesh 派生由调用方注入，或本包提供纯函数 `ParseMesh(ak string) string` 复制逻辑并注释与 tunnel 一致）
- `Ring{ mu sync.RWMutex; m map[string]*Key; now func() time.Time }`

- [ ] **步骤 1：编写失败的类型与 Ring 测试**

在 `ring_test.go` 用表驱动覆盖：
- 空 ring `Lookup` 返回 `(nil, false)`
- `UpsertAK` 后 `Lookup` 可查到、`CoreEntry` 返回最新已加入条目
- `AddKey` 追加多条 → `Lookup` 返回全部未过期条目；`CoreEntry` 返回最新
- `AddKey` 对不存在 AK 返回错误
- `ExpireKey` 设某条过期（注入 `now` 前进）→ `Lookup` 剔除过期、`CoreEntry` 仍返回未过期者；`GetEntry(ak, id)` 对已过期条目返回错误
- `DeleteKey` 删除某条后 Lookup 不含它、再删同条返回错误（404 语义）
- `DeleteAK` 删除整个 AK → `Lookup` false
- 非法入参：`UpsertAK{AK:""}` 错、`AddKey{SK 非 32B}` 错、`AddKey{ID:""}` 自动生成、重复 `ID` 错
- `Snapshot` 按 AK 排序、深拷贝（改返回切片不影响内部）
- 并发：goroutine 并发 AddKey/Lookup/Expire/Delete 跑 200 轮 `-race` 无竞态

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/...`
预期：FAIL（类型/方法未定义）。

- [ ] **步骤 3：实现类型 + Ring**

`accesskey.go`：
```go
type Kind string  // "plain" | "secret_wrap" | "totp_wrap"(4B 预留枚举值)
type Status string // "active" | "expired" | "disabled"
type Meta struct { Type string; IP string }
type SKEntry struct {
    ID string; SK []byte; Kind Kind; WrapKeyID string
    CreatedAt time.Time; ExpiresAt time.Time; Status Status; Meta Meta
}
type Key struct { AK string; Owner string; Entries []SKEntry }
const EntryIDLen = 12
func newEntryID() string // sk-<12hex> crypto/rand
```
`ring.go`：`NewRing(now ...func() time.Time) *Ring`；方法全加锁；`aliveLocked(e SKEntry) bool` 判 `e.Status!=disabled && e.ExpiresAt.IsZero()||now.Before(e.ExpiresAt)`；`addEntryLocked`（校验 AK 存在/SK 32B/ID 唯一，生成 ID）；`CoreEntry` 返回 alive 且 CreatedAt 最新者。
- 纯 stdlib + `crypto/rand`；slog 允许（可不打日志）。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/...`
预期：PASS。

---

- [ ] **步骤 5：编写失败的信封加密测试（wrap.go）**

`wrap_test.go`：
- `wrapKey(sk []byte, context string)` 确定性：同输入两次结果一致；不同 context 不同 key；`sk` 非 32B 报错
- `EncryptSecret`/`DecryptSecret`（AES-256-GCM, random nonce 前置）：往返成功；密文篡改 → 解密报错（GCM auth 失败）
- context 混用（派生 key 不同）→ 解密失败
- 输出为 `WrappedSecret{ Kind, WrapKeyID, Nonce []byte, Cipher []byte }`，`Decrypt` 校验 Kind 与请求的 wrap context

- [ ] **步骤 6：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/...`
预期：FAIL（`wrapKey`/`EncryptSecret` 未定义）。

- [ ] **步骤 7：实现 wrap（信封加密）**

```go
// HKDF-SHA256(secret=sk, salt="sproxy-accesskey-wrap/v1\x00"+context, info=ak)
func wrapKey(sk []byte, context string) ([]byte, error)
type WrappedSecret struct {
    Kind Kind; WrapKeyID string
    Nonce []byte `json:"nonce"`; Cipher []byte `json:"ciphertext"`
}
func EncryptSecret(wrapAK string, sk, wrapKey []byte) (*WrappedSecret, error)
func DecryptSecret(w *WrappedSecret, wrapKey []byte) ([]byte, error)
```
`golang.org/x/crypto/hkdf`（`pkg/telemetry/ext/otel` 已用，属 `golang.org/x/` 自由使用；无新三方依赖）。

- [ ] **步骤 8：运行验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/...`
预期：PASS。

- [ ] **步骤 9：Commit**

```bash
git add pkg/accesskey/ && git commit -m "feat(accesskey): AK→多SK 凭据 Ring + 信封加密(wrap) 核心包"
```

---

### 任务 2：sproxysig 签名 v2

**文件：**
- 修改：`pkg/sproxysig/sproxysig.go`
- 修改：`pkg/sproxysig/sproxysig_test.go`

**协议（来自规格 8）：** Version="2"；Header 增加 `EntryID string`（`sk=` 可选字段）；canonical v2 = v1 段 + 可选 `EntryID` 段（为空则空行）。服务端定位条目：优先 (ak, entryID)；entryID 缺失 → 按 AK 的全部 active 条目 secret 匹配。**客户端不强制携带 entryID**（sclient config 无该字段时缺省）。

- [ ] **步骤 1：编写失败的 v2 测试**

`sproxysig_test.go` 追加：
- `buildHeaderV2` helper：`Header{Version:"2", AK, EntryID:"sk-<id>", TS, Exp, Nonce, BodySHA256}` + `Sign`
- `TestVerify_V2_WithEntryID`：canonical 含 entryID 段，验签通过
- `TestVerify_V2_NoEntryID`：EntryID 空 → 校验用空段，通过
- `TestParseHeader_Version2`：`SproxySig v=2 ak=... sk=sk-abcdef... ts=... exp=... nonce=... body_sha256=... sig=...` 解析正确
- `TestVerify_Reject_VersionMismatch`：明文 v=1 头在校验时被拒（`ErrMalformed`/`ErrVersion`）

- [ ] **步骤 2：运行验证失败**

运行：`go test -count=1 ./pkg/sproxysig/...`
预期：FAIL（Version=1 断言旧、新测试未定义）。

- [ ] **步骤 3：实现 v2**

`sproxysig.go`：
- `const Version = "2"`
- `Header` 加 `EntryID string`；`ParseHeader` 解析 `sk=`（可选）；`v=` 校验 `==Version`（旧测试的 v=1 会失败——见步骤 4 测试迁移）
- `CanonicalHeader`：zanbao 现有 9 段；v2 在 `AK` 后插入 `EntryID` 段（空 entryID 时留空行）。**改造方式：新增 `CanonicalV2(h, includeEntry bool)` 或直接改 `Canonical`**——建议改 `Canonical` 为按 `h.Version` 分支，v1 段序列不变、v2 多一段。
- `Sign`：不变（接收 sk，内部按 h.Version 调对应 canonical）
- `Verify`：不变（按 h.Version 分支）

- [ ] **步骤 4：迁移旧测试 + 验证通过**

- 旧 `buildHeader` 把 `Version` 从 `"1"` 改为 `"2"`（或全部测试用 v2 头）；`TestParseHeader` 输入改成 v=2。
运行：`go test -count=1 ./pkg/sproxysig/...`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/sproxysig/ && git commit -m "feat(sproxysig): 签名协议升级 v2——可选 sk=<entryID> 段 + canonical 分支"
```

---

### 任务 3：凭据 store 化迁移 —— yaml access_keys 移除 + Ring 装配 + auth 改查

> **本任务是迁移主体**：把凭据权威从 `cfg.AccessKeys` 迁到 `Ring`+`CredentialStore`，一次性拆掉全部静态凭据引用。完成后 `verifySproxySig` 只查 Ring、无 yaml 回退、首启 anonymous。

**文件：**
- 创建：`pkg/server/credentialstore.go` + `credentialstore_test.go`
- 修改：`pkg/server/config.go`（移除 `AccessKeysConfig`/`AccessKeys`/Validate 块，加 `Registration`/`AllowInsecureLoopback`/`CredentialTTL`）
- 修改：`pkg/server/auth.go`（`VerifySproxySigMulti`、`tunnelDerivedKey` 收 SK、`authMiddleware` 兜底）
- 修改：`pkg/server/handlers.go`（`RegisterRoutesOpts`/装配/首启 anonymous）
- 修改：`pkg/server/config_api.go`（`access_keys_set` 改查 store）
- 修改：`pkg/server/xfer_listener.go`（`cfg.AccessKeys[0]` → Ring）
- 修改：`cmd/sproxy/root.go`（装配、移除 fail-fast）
- 修改：`config.example.yaml`、`docs/config.md`
- 迁移测试：`pkg/server/*_test.go` 中 ~30 处 `cfg.AccessKeys = ...`、`cmd/sproxy/xfer_listener*_test.go`

- [ ] **步骤 1：编写失败的 CredentialStore 测试**

`credentialstore_test.go`：
- `NewCredentialStore(root, tenant)` → Save(ring 快照) → Load → 等价还原（AK/条目/SK 字节一致）
- Load 时文件不存在 → 返回空（非错）
- 文件损坏（非法 JSON）→ 返回错误
- 写入原子：Save 后无 `.tmp` 残留；并发 Save 不损坏（`-race`）
- 路径：`<root>/<tenant>/meta/credentials.json`（`MkdirAll` meta）

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run CredentialStore`
预期：FAIL（CredentialStore 未定义）。

- [ ] **步骤 3：实现 CredentialStore**

`credentialstore.go`：
```go
type CredentialStore struct { path string }
func NewCredentialStore(metaDir string) *CredentialStore   // metaDir 由调用方按 tenant 计算
func (s *CredentialStore) Load() ([]accesskey.Key, error)  // 不存在→nil,nil；损坏→err
func (s *CredentialStore) Save(keys []accesskey.Key) error // 临时文件+rename 原子写；MkdirAll
```
- JSON 序列化：`SK` 用 `[]byte`（base64 自动）；`SKEntry.ID`/`Kind`/`Status`/`Meta` 带 json tag。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run CredentialStore`
预期：PASS。

---

- [ ] **步骤 5：编写失败的 auth 重构测试**

改造前先定契约（先改测试再改实现）：
- `pkg/server/server_auth_test.go` 新增：
  - `TestVerifySproxySigMulti_TakesRing`：构造 test server（`newTestServerWithRing`，见步骤 7 helper）→ 用 testAccessKey/SK 签名请求 → 200（原来必须 `cfg.AccessKeys` 填充）
  - `TestVerifySproxySigMulti_ExpiredEntryRejected`：ring 中条目已过期（注入 now）→ 签名被拒 401
  - `TestAuthNoCredentials_LoopbackOnly`：无凭据 server + `AllowInsecureLoopback=false` → 远端请求 `/api/files` 401；`AllowInsecureLoopback=true` → 从 127.0.0.1 的读取 200；`/healthz` 无论恒 200
- 迁移现有：`server_auth_test.go:141,165,191,217` 等 `cfg.AccessKeys=...` 改为经 helper 建 server。

- [ ] **步骤 6：运行验证失败**

预期：大量 `cfg.AccessKeys` 相关的签名/401 语义测试 FAIL（`access_keys` 不再被读）。

- [ ] **步骤 7：实现装配 + auth 改查（含测试 helper）**

`handlers.go`：
- `RegisterRoutesOpts` 加 `CredentialRing *accesskey.Ring`、`CredentialStore *CredentialStore`
- `RegisterRoutes`：若 `CredentialRing==nil` → `NewRing()` 并从 Store.Load 灌入；仍空且 `cfg.CredentialTTL>=0`（未禁首启）→ **生成 anonymous**：`generateAccessKeyPair("")` 风格生成 AK（`sk-<16hex>`）+ SK（32B），`AddKey`（Kind=plain，ExpiresAt=now+CredentialTTL），`Store.Save`，slog.Info 明示「首次启动已生成 anonymous 凭据」。存入 `h.credentialRing`/`h.credentialStore`。
- `h.tenantFor`/`credentialStoreFor(owner)`：按 owner 定位 `<storage_root>/<owner>/meta/` 的 store（复用 tenantRoots 缓存模式）。

`auth.go`：
- 移除 `AccessKeyConfig` 类型与 `cfg.AccessKeys` 读取点。
- `VerifySproxySigMulti(w, r)`：返回 `(ak string, secret []byte, mesh string, ok bool)`。查 ring：`Lookup(ak)` → 对每个 alive 条目 constant-time 比对 secret 匹配（无 entryID 时）或按 entryID `GetEntry`；命中后 `sproxysig.Verify(secret, hdr, ...)` 验签。失败各路径保留 Warn + 401。
- `authMiddleware`：`cfg.APIKeys.Enabled` → Bearer（不变）；否则查 ring（`h.credentialRing.Lookup`）——ring 有该 AK 则验签；**ring 为空（无任何凭据）** → 走 `allow_insecure_loopback` 兜底（见步骤 9）。
- `tunnelDerivedKey(secret []byte, mesh string)`：改收 (secret, mesh)，从命中条目取 SK。

`config.go`：
- 删 `AccessKeys []AccessKeyConfig` 字段、`AccessKeyConfig` 全部引用、Validate 块 580-602。
- 加 `Registration struct{ Disable bool }`（yaml `registration.disable`，默认 `false`=允许注册）、`AllowInsecureLoopback bool`（默认 false）、`CredentialTTL time.Duration`（默认 30d）。

- [ ] **步骤 8：迁移全部测试 + 验证通过**

迁移策略（约 30 处）：
- 给 `newTestServerWithAllRoutes`/`newTestServer` 加闭包注入 ring 的变体，或新增 helper：
  ```go
  func newTestServerWithCreds(t *testing.T, keys ...struct{ak, sk string}) (string, atomic.Pointer[Config])
  ```
  内部建 `accesskey.Ring`、`AddKey`、经 `RegisterRoutesOpts.CredentialRing` 注入。签名不变仍用 `signRequest(testAccessKey, testAccessSecret)`。
- 逐文件把 `cfg.AccessKeys = []AccessKeyConfig{{Key: testAccessKey, Secret: testAccessSecret}}` 替换为对该 helper 的调用（保持原断言不变）。
- `xfer_listener_test.go` 中 `cfg.AccessKeys[0]` 映射断言 → 改为 ring 注入后读 `CoreEntry`。
- `pkg/server/config_test.go:TestConfig_Validate_AccessKeys` → 删除或改为 `Registration`/`CredentialTTL` 校验测试。

运行：`go test -race -count=1 ./pkg/server/...`
预期：全部 PASS。

- [ ] **步骤 9：实现无认证兜底 + config_api 状态**

`auth.go`：
- 无凭据分支：`if h.credentialRing == nil || h.credentialRing.Len()==0` → 若 `!cfg.AllowInsecureLoopback` → 仅放行 `/healthz`/`/version`（其余 401）；若 `AllowInsecureLoopback` → 回环（`net.SplitHostPort(r.RemoteAddr)` 为 127.0.0.1/::1）放行读取（GET/HEAD），否则 401。
`config_api.go`：`AccessKeysSet` 改 `h.credentialRing.Len() > 0`。

- [ ] **步骤 10：迁移 cmd/sproxy 装配 + 验证**

`cmd/sproxy/root.go`：
- 移除 `len(cfg.AccessKeys)==0 && !cfg.APIKeys.Enabled` fail-fast（:108）与 hub 对 access_keys 的依赖（:117）：改为「`ring.Len()==0 && !cfg.APIKeys.Enabled` 时若 `!cfg.Registration.Disable` 且未显式禁首启」→ 仍生成 anonymous 并在日志明确（**新部署必须有可访问凭据**）。
- 装配：`store := credentialstore.NewCredentialStore(metaDir)`（按 default tenant/owner 根），`ring := accesskey.NewRing()`，载入/首启，传给 `RegisterRoutes` + `hub.NewAuthenticator(ring)`。
- 删 `aks := ...` 转换块（:184-186）。
- 迁移 `cmd/sproxy/xfer_listener_test.go`/`integration` 的 AccessKeys 注入。

运行：`go test -race -count=1 ./cmd/sproxy/... ./pkg/server/...`
预期：PASS。

- [ ] **步骤 11：Commit**

```bash
git add pkg/server/ cmd/sproxy/ config.example.yaml docs/ && git commit -m "feat(server): 凭据 store 化——yaml access_keys 移除、Ring 权威表、首启 anonymous、无认证回环兜底"
```

---

### 任务 4：hub.Authenticator 共享 Ring

**文件：**
- 修改：`pkg/tunnel/hub/auth.go`
- 修改：`pkg/tunnel/hub/router.go`（fail-closed 兜底）
- 修改：`pkg/tunnel/hub/auth_test.go`、`router_test.go`、`tcp_server_test.go`、`dht_feed_test.go`（约 30 处）
- 修改：`pkg/tunnel/mesh/mesh_test.go`
- 修改：`pkg/server/hub_tcp_relay_test.go`、`cmd/sclient/relay_tcp_test.go`

- [ ] **步骤 1：编写失败的 Auth 测试（新签名）**

`auth_test.go`：`NewAuthenticator(r *accesskey.Ring)`——把现有 `NewAuthenticator(aks)` 调用改为：
```go
func testRing(aks ...hub.AccessKey) *accesskey.Ring  // helper：建 ring + 为每个 AK AddKey(plain, SK: aks[i].Secret)
```
- 现有「空表 fail-closed / 多 key / 过期」测试语义保持：过期条目经 `ring` 的 now 注入测。
- 新增：共享同一 ring 实例的两个 Authenticator 行为一致（无需 SetAccessKeys）。

- [ ] **步骤 2：运行验证失败**

预期：编译错误（签名不匹配）。

- [ ] **步骤 3：实现 hub 共享 ring**

`hub/auth.go`：
```go
type Authenticator struct { ring *accesskey.Ring; noncePool *sproxysig.NoncePool }
func NewAuthenticator(r *accesskey.Ring) *Authenticator
func (a *Authenticator) Authenticate(ak string, proof, nodeID, ts, nonce string) error {
    entry := a.ring.CoreEntry(ak)      // alive 主条目；nil → fail-closed
    // 既有：ts 窗口、nonce 去重、constant-time 比对 ComputeRegisterProof(secret, ...)
}
```
- `hub.AccessKey` 类型保留（兼容路由/现有 mesh 解析）或删除（若仅测试用）——若删除则 mesh 也改。**保留**，作为 ring 装配的适配输入。

- [ ] **步骤 4：迁移全部测试 + 验证**

- `router_test.go`/`tcp_server_test.go`/`dht_feed_test.go` 等所有 `NewAuthenticator(...)` 调用 → `testRing(...)` helper。
- `router.go:512` fail-closed：`hub.NewAuthenticator(nil)` 改 `NewAuthenticator(accesskey.NewRing())`（空 ring 天然 fail-closed）。

运行：`go test -race -count=1 ./pkg/tunnel/hub/... ./pkg/tunnel/mesh/... ./pkg/server/ ./cmd/sclient/...`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/tunnel/hub/ pkg/tunnel/mesh/ pkg/server/hub_tcp_relay_test.go cmd/sclient/relay_tcp_test.go && git commit -m "feat(tunnel/hub): Authenticator 共享 accesskey.Ring——凭据单一事实源"
```

---

### 任务 5：/api/credentials 管理端点

**文件：**
- 创建：`pkg/server/credentials_handler.go` + `credentials_handler_test.go`
- 修改：`pkg/server/handlers.go`（注册路由）
- 修改：`pkg/server/requestlog.go`/`audit.go` 无需（复用 RecordAudit）

**端点（规格 7.4 定稿）：**

| 端点 | 权限 | 行为 |
|------|------|------|
| `GET /api/credentials` | admin | 全量 AK 列表（每 AK 含活跃 SK 数/摘要，不下发明文 SK） |
| `POST /api/credentials` body `{ak, owner}` | admin | 新增 AK（4B 注册用；4A 无 admin → 403） |
| `POST /api/credentials/{ak}/renew` | 本人 | 追加 SK 条目；TTL 服务端控（CredentialTTL）；wrap 加密新 SK |
| `GET /api/credentials/{ak}/sk` | 本人/admin | 该 AK 的 SK 列表（每条含 `sk_id/created/expires/status`，不相邻调用方 SK 的条目 masked） |
| `POST /api/credentials/{ak}/sk/{skID}/expire` body `{until}` | 本人/admin | 设单条过期 |
| `DELETE /api/credentials/{ak}/sk/{skID}` | 本人/admin | 删除单条 SK（幂等，404 if 不存在） |
| `DELETE /api/credentials/{ak}` body `{confirm, force?}` | admin | 删 AK + 二次确认（confirm==ak 且无活跃 SK 或 force） |

- [ ] **步骤 1：编写失败的黑盒测试**

`credentials_handler_test.go`（用任务 3 的 seeded server）：
- `renew`：本人 AK 签名 POST → 200，返回 `{ak, sk_id, wrapped_secret, kind, wrap_key_ak, expires_at}`；用旧 SK 解 wrap 得新 SK；**新 SK 立即可用**请求 200；旧 SK 仍可用（多 SK 共存）；ring 持久化（Store.Save 被调用——断言 store 文件含新条目）
- `renew` 无 ttl 参数可传：传 `ttl` 字段被忽略（服务端控）
- `GET sk`：本人清单含新条目 `sk_id`/`expires`；非本人 SK 的 wrapped_secret 用自己 wrap key 解密失败
- `DELETE sk/{skID}`：删除后该 SK 签名 401；再删同名 404；audit 记录 `credential_sk_delete`
- `POST expire {until}`：到期后该 SK 401；其他条目仍可用
- `GET /api/credentials`（全量）：non-admin（anonymous）→ 403；admin 不可达（4A 无 admin）→ 403
- `DELETE /api/credentials/{ak}`：non-admin → 403；若 admin 权（mock：ring 里放 role=admin 条目）→ confirm 不匹配 400、有活跃 SK 无 force 400、confirm+force → 200 且 AK 删除、audit `credential_ak_delete`
- 无认证 → 401（authMiddleware 保护）
- `localMux` 可达复用（隧道内层，与 audit/share 同模式——**确认注册到 localMux**）

- [ ] **步骤 2：运行验证失败**

预期：路由未注册（404）或 handler 未定义。

- [ ] **步骤 3：实现 handler + 注册路由**

`credentials_handler.go`：
- `Handlers` 加 `credentialRing *accesskey.Ring`、`credentialStore *CredentialStore`（任务 3 已装配）+ `credentialTTL time.Duration`
- 结构拆分：`renewHandler`/`skListHandler`/`skDeleteHandler`/`skExpireHandler`/`akAddHandler`/`akDeleteHandler`
- `renewHandler`：取 `ActorFrom(ctx)` 的 AK → 生成 32B 新 SK → `wrapKey(旧SK, mesh)` 加密 → `AddKey`（Kind=secret_wrap, WrapKeyID=本 AK, ExpiresAt=now+credentialTTL, Meta{Type:"renew", IP:r.RemoteAddr}）→ `Store.Save(ring.Snapshot())` → RecordAudit → JSON 返回
- `akDeleteHandler`：admin 判 `ring 中该 AK 的 Role==admin`（4A 无 admin → 403）；`confirm` 校验；活跃 SK 若无 force → 400
- admin 判定：`getRole(ak)`（条目字段；4A 无 admin，返回 user）
- 注册：主 mux `h.authMiddleware(...)` + localMux 裸注册（与 audit 同模式）

- [ ] **步骤 4：验证通过 + 补 admin 判定测试**

运行：`go test -race -count=1 ./pkg/server/ -run "Credentials|Credential"`
预期：PASS。补 `TestCredentialsHandler_RoleAdmin`（mock 一个 role=admin 条目验证 admin-only 端点边界）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/credentials_handler.go pkg/server/credentials_handler_test.go pkg/server/handlers.go && git commit -m "feat(server): /api/credentials 管理端点——renew/sk 增删查 + admin 二次确认删 AK + 审计"
```

---

### 任务 6：sclient trust 命令 + pkg/client 领域 API

**文件：**
- 创建：`pkg/client/accesskey.go`
- 创建：`cmd/sclient/trust.go`（`trust ak`/`trust sk` 子命令）
- 修改：`cmd/sclient/access_key.go`（rename → trust；create 移到 trust ak add 或保留）
- 修改：`pkg/client/config.go`（`access_key_id` 可选，renew 回填）
- 修改：`pkg/client/client.go`（v2 签名 + entryID 头）
- 修改：`cmd/sclient/relay_tcp_test.go`、`access_key_test.go`
- 创建：`pkg/client/accesskey_test.go`、`cmd/sclient/trust_test.go`

- [ ] **步骤 1：编写失败的 pkg/client 领域 API 测试**

`accesskey_test.go`（用 mock HTTP server，仿 client_test.go `newMockServer`）：
- `RenewAccessKey`：POST `/api/credentials/{ak}/renew` 带签名 → 解析响应 wrapped_secret，用本端 SK 解 wrap 得到新 SK（断言返回新 SK/新 sk_id）
- `ListAccessKeys`：GET `/api/credentials/{ak}/sk` → 解析，解密自己条目
- `DeleteSK`/`ExpireSK`/`AddAK`/`DeleteAK`：方法签名 + 请求方法/路径断言

- [ ] **步骤 2：运行验证失败**

预期：FAIL（函数未定义）。

- [ ] **步骤 3：实现 pkg/client/accesskey.go**

```go
type AccessKeyClient struct { *FileClient }  // 复用 doRequest 签名路径
func (c *FileClient) RenewAccessKey() (wrapped RenewResponse, err error) // 用当前 SK 解 wrap
func (c *FileClient) ListAccessKeys(ak string) ([]SKInfo, error)
func (c *FileClient) DeleteSK(ak, skID string) error
func (c *FileClient) ExpireSK(ak, skID string, until time.Time) error
func (c *FileClient) AddAK(ak, owner string) error
func (c *FileClient) DeleteAK(ak string, force bool) error
```
- `RenewAccessKey` 内：调用 `doRequest` → 拿 wrapped_secret → `accesskey.DecryptSecret(wrapKey(c.AccessKeySecret()))` → 结果含 `NewSecret`/`SKID`。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/client/...`
预期：PASS。

---

- [ ] **步骤 5：编写失败的 trust 命令测试**

`trust_test.go`（CaptureStdout 模式）：
- `trust renew`：mock 服务返回 wrapped → 命令解出新 SK、打印「已轮换 SK：sk_id=...」、调用 `config set access_key_secret`（断言配置更新）
- `trust sk list`/`sk delete`/`sk expire`/`ak add`/`ak delete`：参数/退出码
- rename：`access-key` 命令仍存在但打 deprecated 提示指向 `trust`；`generateAccessKeyPair` 保留（renew 用）或迁移

- [ ] **步骤 6：实现 trust 命令**

`cmd/sclient/trust.go`：
- `NewCmdTrust`（组）→ `trust ak`（list/add/delete[--force 交互确认 AK 名]）、`trust sk`（list/delete/expire）、`trust renew`
- cobra 结构仿现有 `NewCmdAccessKey`；解密用 `accesskey.DecryptSecret`
- `access_key.go` 的 `access-key` 命令标记 deprecated 并 alias 到 trust；`pkg/client/config.go` 加 `AccessKeyID string`（optional，renew 后 `config set access_key_id`）
- `client.go` 的 `signRequest` 在 v2 下携带 `sk=<entryID>` 头（config 有值时）

- [ ] **步骤 7：验证 + 收尾（web/CI）**

运行：
```bash
go test -race -count=1 ./cmd/sclient/... ./pkg/client/... ./pkg/server/... ./pkg/tunnel/hub/... ./cmd/sproxy/...
make lint        # 全 module 0 issues
make build-all   # 构建全部子 module
make test-all    # 全部子 module 测试
make check-loopback
```
修复见到的任何 lint/构建/test 失败。

- [ ] **步骤 8：Commit**

```bash
git add cmd/sclient/ pkg/client/ && git commit -m "feat(sclient): trust 命令族——renew/sk 管理 + pkg/client 领域 API + 签名 v2"
```

---

## 收尾验证（全部任务后）

- [ ] **步骤 1：全量验证**

```bash
git fetch origin && git rebase origin/master   # 并入最新 master（若有冲突按 feature 语义解决）
go test -race -count=1 ./pkg/accesskey/... ./pkg/sproxysig/... ./pkg/server/... ./pkg/tunnel/hub/... ./pkg/tunnel/mesh/... ./pkg/client/... ./cmd/sproxy/... ./cmd/sclient/...
make lint && make build-all && make test-all && make check-loopback && go vet ./...
```

- [ ] **步骤 2：手测（浏览器/CLI）**

- 首启：无 yaml access_keys → 日志出现 anonymous 凭据；`sclient trust renew` 成功
- `sclient trust ak list`/`sk list`：正确展示；`sk delete` 删后签名失效
- 全量 admin 端点 403（4A 无 admin）
- 签名 v2：新 sclient 正常；无 entryID 时 server entryID 缺省匹配

- [ ] **步骤 3：独立审查 + 修复全部发现（含 Minor）**

SDD 流程：review-package → 独立对抗审查 → 定向复审循环，全部发现修复（用户 must-fix-all）。

- [ ] **步骤 4：PR**

```bash
git push -u origin feature/credentials-store-sk-rotation
gh pr create --title "feat(server): 凭据 store 化 + SK 滚动与管理" --body "…"
```
（title 功能为核心，不含阶段/编号；CI 全绿后用户 squash 合并）

## 自检

- **规格覆盖度**：多 SK 模型（任务 1）✓ / 签名 v2（任务 2）✓ / store 化+anonymous+无认证兜底（任务 3）✓ / hub 共享 ring（任务 4）✓ / 管理端点+admin 二次确认+审计（任务 5）✓ / sclient trust + 领域 API（任务 6）✓ / 服务端控 TTL（任务 5）✓ / 4B 钩子（Kind totp_wrap 枚举、Meta、WrapScheme、CredentialStorer 接口——任务 1/3）✓
- **占位符扫描**：无 TODO/待定；所有步骤含实际代码/命令。
- **类型一致性**：`SKEntry.ID`/`Key.AK`/`Ring.CoreEntry`/`accesskey.WrappedSecret` 跨任务一致。

> 注意：任务 3 的「移除 AccessKeyConfig」会让 `struct` 定义从 auth.go 删除——`config.example.yaml`/`docs` 中的 access_keys 文档同步清理。任务 4 的 hub 测试迁移量大但机械（`testRing` helper）。web 前端 `saveAccessKeys`（transport 侧客户端 AK/SK 录入）不需要改（它是客户端配置，非服务端读取）。
