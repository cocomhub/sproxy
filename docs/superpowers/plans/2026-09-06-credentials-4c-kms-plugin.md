# 接口提取 + 加密静态存储 + KMS 插件（4C）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 补齐 4A 原计划「预留接口」的未落地缺口（`CredentialStorer`/`SecureStorer` 均未实现；
**`Authenticator` 认证面接口已在 4B-1 定义**，见 spec §7.2），并把凭据静态存储升级为可加密
（SecureStorer）可插拔（外部子 module 注册）的架构，提供 KMS 插件骨架。**第一步是纯接口提取
（零行为变化、既有测试零回归）**，再叠加加密实现与插件。

**架构：** core（`pkg/accesskey` + `pkg/server`）只持接口，默认文件实现收敛为默认插件；加密静态存储（SecureStorer 加密实现，master key 派生 + AES-256-GCM 信封）；KMS 插件放独立子 module（仿 `pkg/tunnel/xfer/ext/ws` / `pkg/telemetry/ext/otel`，独立 go.mod + replace，加入根 `go.work`）；**三方 TOTP/QR 库家 = `pkg/accesskey/ext/auth/totp/`**（服务端渲染 QR 等三方依赖进该子 module，主包 go.mod 零新增，spec §10/D7）。所有生成/解析/包裹只准走 `pkg/accesskey` 导出（4A 用户硬约束，见下方前提表 #4）。

**技术栈：** Go 1.26，纯 stdlib + 既有 `golang.org/x/crypto/hkdf`。KMS/三方插件子 module 如需第三方 SDK 只加在**该子 module 的 go.mod**，主包 go.mod 零新增。

**BASE**：`1618835f`（origin/master，4A 已合并；4B-1/4B-2 合并后按最新 master 调整——4C 接口提取依赖 4B-1 的 `Key.Role` 与 `GetKey` 已落地）。

**PR 拆分建议**：**4C-1 = 任务 1**（接口提取，纯重构，独立 PR 最安全——行为不变、零回归可验证）；**4C-2 = 任务 2-3**（加密静态存储 + KMS 插件骨架）。理由：接口提取是可单独审查/合并的原子重构，加密/KMS 依赖接口就位；拆开避免「重构+新功能」混在大 diff（既有大 diff 审查不稳定的教训）。

---

## 0. 4A/4B 已落地前提（执行前必读）

| # | 落地事实 | 对 4C 的含义 |
|---|---|---|
| 1 | AK = `ak-<mesh>-<32hex>`（`GeneratePair`）；SK 条目 ID = `skey-<12hex>`（`GenerateID`/`SkeyIDPrefix`）；v2 `skey-id=` 强制必传 | 4C 接口与插件不得引入任何新的凭据格式/生成 |
| 2 | AK/SK 生成/解析/包裹全收归 `pkg/accesskey`（`GeneratePair`/`ParseMesh`/`IsValidAK`/`RandomHexHex`/`GenerateID`/`WrapContextCredentials`/`WrapContextTOTP`/`DeriveWrapKey`/`DeriveTOTPWrapKey`/`EncryptSecret`/`DecryptSecret`/`EncryptSecretKind`/`DecryptSecretKind`；`RoleUser`/`RoleNode`/`RoleAdmin`/`Key.Role`/`GetKey` 已在 4B-1 落地） | 4C 加密/KMS 插件的调用面只准走这些导出，禁止自行实现 |
| 3 | 具体 `server.CredentialStore`（`<tenant>/meta/credentials.json`，`Load`/`Save` 原子写）是唯一持久化实现 | 4C 第一步：提取 `CredentialStorer` 接口，`CredentialStore` 收敛为**默认文件实现**（行为不变） |
| 4 | `CredentialStorer`/`SecureStorer` 接口**均未实现**；**`Authenticator`/`Principal`/`RingAuthenticator` 已在 4B-1 定义**（`pkg/server/auth.go`，经 `RegisterRoutesOpts.Authenticators` 宿主注入） | **4C 第一步=接口提取（CredentialStorer/SecureStorer）**；**不重复提取认证面**——4C 不定义 `AuthenticatorPlugin`，注册表只管 storer/secure 类 |
| 5 | 注入链：`bootstrapCredentials`/`RegisterRoutesOpts.CredentialStore`/`Handlers.credentialStore`/`BootstrapServerCredentials`（`*CredentialStore` 具体类型） | 接口提取后全部改持接口类型。注：4B-1（U3）起 `bootstrapCredentials` 不再生成首启 anonymous 引导凭据（零凭据启动），接口提取对该行为无影响（该分支已在 4B-1 删除） |
| 6 | `KindTOTPWrap`/`WrapContextTOTP` 已落地（4B-2）；`Kind`/`Status`/`Meta`/`Key{AK,Owner,Role,Entries,TOTPSecret}` 为持久化模型（Role 4B-1、TOTPSecret 4B-2） | 4C 加密静态存储加密的是整个 `credentialsFile`（含 TOTPSecret），不是单条目 |
| 7 | ext 子 module 模式：`pkg/tunnel/xfer/ext/ws`（module + replace `github.com/cocomhub/sproxy => ../../../../..` + init() 注册）+ 根 `go.work` 收录 | 4C KMS 插件仿此结构 |
| 8 | `WrapContextCredentials`/`WrapContextTOTP` 是 wrap context 单一事实源（client/server 别名引用） | 4C 加密 master key 派生如需 context 常量，同法收归 accesskey |
| 9 | **三方 TOTP/QR 库家约定（D7/spec §10）**：服务端渲染 QR 等三方 Go 依赖一律进独立 ext 子 module（`pkg/accesskey/ext/auth/totp/`），主 go.mod 零新增 | 4C 提供 `ext/auth/totp` 骨架（注册到 accesskey 注册表的 SecureStorer/认证辅助），Go 标准库的 `pkg/otp`（4B-2）不进 ext |

---

## 文件结构总览

**新建**
- `pkg/accesskey/storer.go` — `CredentialStorer` / `SecureStorer` 接口 + 注册表（`RegisterStorer`/`UnregisterStorer`/`GetStorer`）
- `pkg/accesskey/storer_test.go` — 接口契约 + 注册/反注册
- `pkg/accesskey/encrypting_storer.go` + `encrypting_storer_test.go`（任务 2）— SecureStorer 加密实现
- `pkg/accesskey/ext/kms/go.mod`、`kms.go`、`kms_test.go`（任务 3）— KMS 插件骨架（独立子 module）
- `pkg/accesskey/ext/kms/README.md`（可选）
- `pkg/accesskey/ext/auth/totp/go.mod`、`totp_ext.go`、`totp_ext_test.go`（任务 3 可选扩展）— 三方 TOTP/QR 库家（独立子 module；服务端渲染 QR 等三方依赖进此，主包零新增）

**修改**
- `pkg/server/credentialstore.go` — `CredentialStore` 显式实现 `accesskey.CredentialStorer`（`var _ accesskey.CredentialStorer = (*CredentialStore)(nil)` 编译期断言）
- `pkg/server/handlers.go` — `RegisterRoutesOpts.CredentialStore`/`Handlers.credentialStore` 改接口类型；`bootstrapCredentials` 签名改收接口
- `pkg/server/credentials_handler.go` — `persistCredentials` 走接口（零逻辑变化）
- `pkg/server/config.go`（任务 2）— `credential_store` 配置段（`encrypt: bool`、`master_key_file`）
- `go.work`（任务 3）— 收录 `./pkg/accesskey/ext/kms`（与 `./pkg/accesskey/ext/auth/totp`，若实现）
- 测试：`pkg/server/*_test.go` 无需改（接口提取后行为不变）；`pkg/accesskey` 全量回归

---

### 任务 1：接口提取（纯重构，零行为变化）

**文件：**
- 创建：`pkg/accesskey/storer.go` + `storer_test.go`
- 修改：`pkg/server/credentialstore.go`、`pkg/server/handlers.go`、`pkg/server/credentials_handler.go`

**接口定义（放 `pkg/accesskey`——core 域自包含，只依赖 `accesskey.Key`，server 与 ext 插件均可引用，避免 server→ext 反向依赖）：**

```go
// storer.go

// CredentialStorer 是凭据 Ring 快照的持久化抽象。
type CredentialStorer interface {
    Load() ([]Key, error)          // 文件不存在 → (nil, nil)；损坏 → error（fail-closed）
    Save(keys []Key) error         // 原子写；失败 → error（调用方记 credential_persist_error）
}

// SecureStorer 是静态存储加密抽象（对整份凭据文件的字节级加解密）。
type SecureStorer interface {
    Encrypt(plaintext []byte) ([]byte, error)
    Decrypt(ciphertext []byte) ([]byte, error)
}
```

> **认证面接口不在此定义**：`Authenticator`/`Principal`/`RingAuthenticator` 已在 4B-1 定义
> （`pkg/server/auth.go`，spec §7.2），宿主经 `RegisterRoutesOpts.Authenticators` 注入。
> 4C 注册表只管 storer/secure 类；三方认证实现（如平台登录）如需要挂接点，后续基于 4B-1 的
> `Authenticator` 接口单独设计，不在本计划范围。

**注册表（仿 `pkg/tunnel/xfer.Register`）：**
```go
// plugin.go
var registry = struct{ sync.RWMutex; m map[string]any }{m: map[string]any{}}
func RegisterStorer(name string, v any) error   // 重名 → 错误
func UnregisterStorer(name string)              // 测试/反注册
func GetStorer[T any](name string) (T, bool)    // 泛型取 storer
```

**收敛默认实现：**
- `server.CredentialStore` 加编译期断言 `var _ accesskey.CredentialStorer = (*CredentialStore)(nil)`——Load/Save 签名已匹配（`Load() ([]accesskey.Key, error)` / `Save(keys []accesskey.Key) error`），零改动。
- `SecureStorer` 明文默认实现 `accesskey.PlainStorer`（`Encrypt`=原样、`Decrypt`=原样）——供未开启加密时装配。

**注入链改接口：**
- `RegisterRoutesOpts.CredentialStore *CredentialStore` → `accesskey.CredentialStorer`
- `Handlers.credentialStore *CredentialStore` → `accesskey.CredentialStorer`
- `bootstrapCredentials(opts)` 内 `store.Load()`/`store.Save(...)` 走接口（调用方不变）
- `BootstrapServerCredentials` 返回类型同步

- [ ] **步骤 1：编写失败的接口契约测试**

`storer_test.go`：
- `PlainStorer`：Encrypt==Decrypt==原样往返。
- 注册表：`RegisterStorer("plain", PlainStorer{})` → `GetStorer` 命中；重名 `RegisterStorer` → 错误；`UnregisterStorer` 后 `GetStorer` 不命中。
- 编译期断言：`var _ accesskey.CredentialStorer = (*server.CredentialStore)(nil)`（放 `pkg/server/credentialstore_test.go` 追加）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/ -run Storer`
预期：FAIL（接口/注册表未定义）。

- [ ] **步骤 3：实现接口 + 注册表 + 注入链改造**

按上面定义。`RegisterRoutesOpts.CredentialStore` 改接口类型后，既有测试注入 `*CredentialStore` 的调用点自动满足（`*CredentialStore` 实现接口）——**零测试迁移**。

- [ ] **步骤 4：全量回归（零回归是验收标准）**

运行：
```bash
go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/... ./pkg/tunnel/hub/...
make lint && make build-all && make check-loopback
```
预期：全部 PASS 且与改造前一致（纯重构）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/storer.go pkg/accesskey/plugin.go pkg/accesskey/storer_test.go pkg/accesskey/plugin_test.go pkg/server/credentialstore.go pkg/server/credentialstore_test.go pkg/server/handlers.go pkg/server/credentials_handler.go && git commit -m "refactor(accesskey): 接口提取——CredentialStorer/SecureStorer + storer 注册表 + 注入链改持接口（零行为变化）"
```

---

### 任务 2：加密静态存储（SecureStorer 加密实现）

**文件：**
- 创建：`pkg/accesskey/encrypting_storer.go` + `encrypting_storer_test.go`
- 修改：`pkg/server/config.go`（`credential_store` 配置段）、`pkg/server/credentialstore.go`（装配加密 storer）、`pkg/server/handlers.go`（`bootstrapCredentials` 按配置包 SecureStorer）
- 修改：`pkg/server/credentialstore_test.go`（加密开/关往返）

**设计要点：**
- `EncryptingStorer`：包装 `CredentialStorer` + `SecureStorer`——`Save` 时 `SecureStorer.Encrypt(data)` 再落盘；`Load` 时读盘后 `Decrypt`。
- master key 派生：`accesskey` 新增 `DeriveMasterKey(passphrase []byte, salt []byte) ([]byte, error)`（HKDF-SHA256，收归单一事实源）；key 来源：配置文件路径 `credential_store.master_key_file`（base64 32B）或环境变量（`SPROXY_CREDENTIAL_MASTER_KEY`）。
- 信封：AES-256-GCM，nonce 前置密文（复用 `accesskey` 包裹模式）。
- 配置：`credential_store.encrypt: false`（默认关——开启后既有明文 credentials.json 需迁移：`Load` 检测明文（非 GCM 结构）→ 拒绝或提示迁移；4C 提供 `--encrypt-migrate` 一次性迁移命令，属可选扩展）。
- 调用面纪律：`EncryptingStorer` 只调 `accesskey` 导出（`DeriveMasterKey`/`EncryptSecret`/`DecryptSecret` 或同源 AES-GCM 辅助）。

- [ ] **步骤 1：编写失败的加密存储测试**

`encrypting_storer_test.go`：
- `EncryptingStorer` roundtrip：`Save` → 落盘字节 ≠ 明文（含 `"keys"` 字样不出现）；`Load` 还原与 Save 前一致（`[]accesskey.Key` 等价，含 `Role` 与 `TOTPSecret` 字段）。
- tamper：改一个密文字节 → `Load` 报错（GCM auth 失败）。
- master key 错 → `Load` 报错；key 派生确定性（同 passphrase+salt 两次一致、salt 不同不同）。
- 明文模式（`PlainStorer`）往返 = 现状（回归）。
- 配置装配：`credential_store.encrypt=false` → 用 `PlainStorer`；`true` → `EncryptingStorer`（注入 mock SecureStorer 断言 Save 前调 Encrypt）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/ -run EncryptingStorer`
预期：FAIL（未定义）。

- [ ] **步骤 3：实现加密存储**

按上面要点。`server.CredentialStore` 保持纯文件读写（不含加密）；`bootstrapCredentials` 装配时按配置把 `CredentialStorer` 包成 `EncryptingStorer`（`SecureStorer` = 加密实现或 `PlainStorer`）。

- [ ] **步骤 4：验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/... ./pkg/server/ -run "Credential|Storer"`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/encrypting_storer.go pkg/accesskey/encrypting_storer_test.go pkg/server/config.go pkg/server/credentialstore.go pkg/server/handlers.go && git commit -m "feat(accesskey): 加密静态存储——SecureStorer 加密实现 + master key HKDF 派生 + 装配开关"
```

---

### 任务 3：外部插件注册 + KMS 插件骨架（独立子 module）+ ext/auth/totp 骨架

**文件：**
- 创建：`pkg/accesskey/ext/kms/go.mod`、`pkg/accesskey/ext/kms/kms.go`、`pkg/accesskey/ext/kms/kms_test.go`
- 创建：`pkg/accesskey/ext/auth/totp/go.mod`、`totp_ext.go`、`totp_ext_test.go`（三方 TOTP/QR 库家骨架）
- 修改：`go.work`（收录 `./pkg/accesskey/ext/kms` 与 `./pkg/accesskey/ext/auth/totp`）
- 修改（core）：`pkg/accesskey/plugin.go`（注册表已就绪，任务 1）——如需按类型细分注册可加 `RegisterSecureStorer`/`RegisterCredentialStorer` 便捷函数

**KMS 插件骨架（`pkg/accesskey/ext/kms`）：**
- go.mod：`module github.com/cocomhub/sproxy/pkg/accesskey/ext/kms` + `require github.com/cocomhub/sproxy v0.0.0` + `replace github.com/cocomhub/sproxy => ../../../..`（仿 `pkg/tunnel/xfer/ext/ws`；KMS SDK 依赖只加本 module，主包 go.mod 零新增）。
- `KMSStorer` 实现 `accesskey.SecureStorer`：信封模式——本地随机 DEK（data encryption key）加密数据（AES-256-GCM，调 `accesskey` 导出），DEK 本身经可插拔 KMS 客户端加密（`KMSClient` 接口：`EncryptDEK`/`DecryptDEK`）。KMS 客户端默认实现返回 `ErrNotConfigured`（骨架，AWS/GCP 适配后续填）。
- `init()` 注册到 accesskey 注册表（`accesskey.RegisterStorer("kms", ...)`）。
- 不 import 服务端（只依赖 `pkg/accesskey`）。
- 测试：`KMSStorer` roundtrip（mock KMSClient）；未配置 KMS → 明确错误；注册/反注册；主包不 import ext（用 `go list` 断言）。

**ext/auth/totp 骨架（`pkg/accesskey/ext/auth/totp`，三方库家）：**
- 定位：**服务端渲染 QR 等三方 TOTP/QR Go 依赖的唯一落点**（spec §10/D7）。Go 标准库 TOTP（`pkg/otp`，4B-2）在主包，不进此 module。
- 骨架：`init()` 注册到 accesskey 注册表（如注册一个「服务端 QR 渲染」SecureStorer/辅助函数；默认实现返回哨兵错误提示未配置三方依赖）；后续按需填三方库（如 `github.com/skip2/go-qrcode` 等）只加本 module 的 go.mod。
- 不 import 服务端（只依赖 `pkg/accesskey` 与自身三方依赖）。

- [ ] **步骤 1：编写失败的 KMS 插件测试**

`kms_test.go`（子 module 内）：
- `KMSStorer` + mock `KMSClient`：`Encrypt`→`Decrypt` 往返；KMS 加密的 DEK 在密文中可寻址（断言格式）；篡改密文 → 解密报错。
- 未配置 KMS（默认 client）→ `Encrypt` 返回 `ErrNotConfigured`。
- 注册到 accesskey 注册表 → `GetStorer` 命中；`UnregisterStorer` 后不命中。

`totp_ext_test.go`（子 module 内，可选骨架断言）：
- 默认（未配置三方依赖）→ 辅助函数返回哨兵错误；注册/反注册。

- [ ] **步骤 2：运行验证失败**

运行：`cd pkg/accesskey/ext/kms && go test -race -count=1 ./...`
预期：FAIL（包/函数未定义；go.work 未收录）。

- [ ] **步骤 3：实现 KMS 插件骨架 + ext/auth/totp 骨架 + go.work 收录**

按上面要点。`go.work` 的 `use` 追加 `./pkg/accesskey/ext/kms`（与 `./pkg/accesskey/ext/auth/totp`，若实现）。

- [ ] **步骤 4：验证通过 + 全量回归**

运行：
```bash
cd pkg/accesskey/ext/kms && go test -race -count=1 ./...
cd ../../.. && go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/...
make lint && make build-all && make test-all
go list -deps ./pkg/accesskey | grep -q ext/kms && echo "主包意外依赖 KMS（应无输出）"   # 断言主包不依赖 ext
```

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/ext/kms/ pkg/accesskey/ext/auth/totp/ go.work && git commit -m "feat(accesskey/ext): KMS 插件骨架（SecureStorer 信封实现 + 可插拔 KMS 客户端）+ ext/auth/totp 三方库家骨架 + 注册表接入"
```

---

## 收尾验证（全部任务后）

- [ ] **步骤 1：全量验证**

```bash
git fetch origin && git rebase origin/master
go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/... ./pkg/tunnel/hub/...
make lint && make build-all && make test-all && make check-loopback && go vet ./...
cd pkg/accesskey/ext/kms && go test -race -count=1 ./...
```

- [ ] **步骤 2：手测**

- 默认明文：启动 → 既有 credentials.json 正常读写（回归，含 Role/TOTPSecret 字段往返）。
- 开启加密（`credential_store.encrypt: true` + master key）→ 落盘为密文 → 重启 Load 还原；改一字节 → 启动拒绝（fail-closed）。
- KMS 插件：`go run` 一个引用 `ext/kms` 的小程序 → `KMSStorer` 注册 + 未配置报错符合预期。
- ext/auth/totp：`go run` 一个引用 `ext/auth/totp` 的小程序 → 默认哨兵错误符合预期（未配置三方依赖）。

- [ ] **步骤 3：独立审查 + 修复全部发现（含 Minor）**

SDD 流程：review-package → 独立对抗审查 → 定向复审循环（用户 must-fix-all）。4C-1（任务 1）与 4C-2（任务 2-3）分别 PR。

- [ ] **步骤 4：PR**

```bash
git push -u origin feature/credentials-4c-kms-plugin
gh pr create --title "refactor(accesskey): 凭据存储接口化——CredentialStorer/SecureStorer + 加密静态存储 + KMS 插件骨架" --body "…"
```

## 自检

- **规格覆盖度**：接口提取（任务 1，含 CredentialStorer/SecureStorer；**认证面 Authenticator 已在 4B-1 定义，不重复提取**）✓ / 加密静态存储（任务 2，覆盖含 Role/TOTPSecret 的整份 credentialsFile）✓ / 外部插件注册 + KMS 骨架 + ext/auth/totp 三方库家（任务 3）✓ / 注入链改接口（任务 1）✓。
- **4A/4B 复用断言**：接口与插件只调 `pkg/accesskey` 导出；主包 go.mod 零新增；ext 独立 go.mod；`pkg/otp`（stdlib TOTP）留在主包不进 ext。
- **占位符扫描**：`KMSClient` 骨架的默认实现返回哨兵错误（非占位/panic）；ext/auth/totp 默认实现返回哨兵错误（非占位/panic）；所有步骤含实际代码/命令。
- **类型一致性**：`CredentialStorer`/`SecureStorer`/`EncryptingStorer`/`KMSStorer`/`Authenticator`（4B-1）跨任务一致。
