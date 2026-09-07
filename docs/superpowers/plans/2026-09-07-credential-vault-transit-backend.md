# Vault Transit 凭据存储加密后端实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 superpowers:subagent-driven-development（推荐）逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 为 sproxy 凭据静态存储加密新增 HashiCorp Vault Transit 后端：`credential_store.backend: aesgcm|vault` 分支装配，vault 走 Vault Transit 引擎加解密（纯 stdlib HTTP，无 hashicorp SDK），AAD context 绑文件身份，decrypt 结果短 TTL 缓存。

**架构：** `SecureStorer` 接口已是正确抽象（Vault Transit encrypt/decrypt 天然字节进出）。新增 `VaultTransitStorer` 实现（`pkg/accesskey/vault_storer.go`，内置），`BootstrapServerCredentials` 按 `cfg.CredentialStore.Backend` switch 装配（aesgcm=现状路径不变 / vault=新 storer）。三层测试：L1 httptest mock Vault 进 CI；L2/L3 真实 Vault 走 `vault_integration` build tag + `make test-vault`。

**技术栈：** Go 1.26 纯 stdlib（`net/http`/`encoding/json`/`encoding/base64`/`crypto/x509`），无新增三方依赖。

**规格唯一来源：** `docs/superpowers/specs/2026-09-07-credential-vault-transit-backend-design.md`

**BASE：** 8fea0422（origin/master，4C 已合并）。PR 建议：单 PR（backend 分支装配 + VaultStorer + 三层测试）。

---

## 文件结构

- 创建：`pkg/accesskey/vault_storer.go` — `VaultTransitStorer` 实现 `SecureStorer`（纯 stdlib HTTP 调 Vault Transit API；AAD context；decrypt 缓存）
- 创建：`pkg/accesskey/vault_storer_test.go` — L1 httptest mock Vault 单元测试
- 创建：`pkg/accesskey/vault_integration_test.go` — L2/L3 真实 Vault（`//go:build vault_integration`）
- 创建：`pkg/testutil/vaultmock/` — httptest mock Vault server 测试工具（L1 与装配测试共享；仿 `pkg/testutil/mockserver` 模式）
- 修改：`pkg/server/config.go` — `CredentialStoreConfig` 加 `Backend string` + `Vault VaultConfig`；`VaultConfig` 类型；SetDefaults（backend 默认 aesgcm、vault.mount 默认 transit、vault.token_env 默认 VAULT_TOKEN、vault.timeout 默认 10s、vault.cache_ttl 默认 30s）；Validate（backend 枚举 + vault 必填）
- 修改：`pkg/server/handlers.go` — `BootstrapServerCredentials` 按 backend 分支 + `resolveVaultToken`
- 修改：`pkg/server/config_test.go` — Validate 增量表驱动
- 修改：`pkg/server/credential_store_encrypt_test.go` — 装配测试（backend=vault mock Vault；backend=aesgcm 回归）
- 修改：`config.example.yaml`、`docs/config.md` — vault 子段注释
- 修改：`Makefile` — `test-vault` 目标（`go test -tags=vault_integration`）

---

### 任务 1：`VaultTransitStorer` 核心（Encrypt/Decrypt + AAD context）

**文件：**
- 创建：`pkg/accesskey/vault_storer.go`
- 创建：`pkg/accesskey/vault_storer_test.go`

**Vault Transit HTTP API 契约（实现依据）：**

```
POST {addr}/v1/{mount}/encrypt/{key_name}
  X-Vault-Token: <token>
  {"plaintext":"<base64>", "context":"<base64 AAD>"}   → 200 {"data":{"ciphertext":"vault:v1:..."}}

POST {addr}/v1/{mount}/decrypt/{key_name}
  X-Vault-Token: <token>
  {"ciphertext":"vault:v1:...", "context":"<base64 AAD>"} → 200 {"data":{"plaintext":"<base64>"}}
```

- [ ] **步骤 1：编写失败的 VaultTransitStorer 测试（L1 httptest mock Vault）**

`pkg/accesskey/vault_storer_test.go`。先建文件内 mock Vault server（不建共享工具——本任务先内联，任务 4 装配测试需要时抽到 testutil）：

```go
// mockVaultServer 返回一个模拟 Vault Transit encrypt/decrypt 端点的 httptest.Server。
// 它回放固定 JSON：encrypt → {"data":{"ciphertext":"vault:v1:mock"}}，
// decrypt → {"data":{"plaintext": base64(入参密文的镜像)}}。
// 断言入参：X-Vault-Token 头、body 的 plaintext/context/ciphertext 字段。
type mockVaultServer struct {
    t        *testing.T
    token    string
    decryptFn func(ciphertext string) string // 定制 decrypt 行为（如错误场景）
}
func newMockVault(t *testing.T, token string) *mockVaultServer { ... }
func (m *mockVaultServer) URL() string { return m.srv.URL }
func (m *mockVaultServer) Close()      { m.srv.Close() }
```

表驱动核心断言：
- **Encrypt 往返**：`NewVaultTransitStorer(VaultOptions{Addr: mock.URL(), Mount: "transit", KeyName: "sproxy", Token: token, AADPath: "credentials.json"})` → `Encrypt([]byte("secret-data"))` → 返回含 `"vault:v1:"` 前缀密文；mock 断言收到 `X-Vault-Token: <token>` 头 + body plaintext = base64("secret-data") + context = base64("credentials.json")。
- **Decrypt 往返**：`Decrypt([]byte("vault:v1:encrypted"))` → mock decrypt 回 base64(原文) → 返回原文明文。
- **AAD context 语义**：Encrypt 发送的 `context` 字段 == base64(AADPath)；两次同 AADPath 一致。
- **token 请求头**：无 token → 错误（构造时）；token 空串 → error「vault: token 为空」。
- **错误分类**：
  - 非 200 + body `{"errors":["permission denied"]}` → error 含 "permission denied"（403 语义）；
  - 503 + `{"errors":["Vault is sealed"]}` → error 含 "sealed"；
  - 5xx → error 含状态码；
  - Vault 不可达（addr 指向已关闭端口）→ error 含 "vault"（网络）。
- **非 vault:v1: 输入拒绝**：`Decrypt([]byte("plain-json"))` → error（不请求 Vault）。
- **ciphertext 保存格式**：Encrypt 返回的密文 = Vault 响应的 `data.ciphertext` 原样（含版本字段，如 `vault:v1:abc/def`）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/ -run VaultTransitStorer`
预期：FAIL（类型/函数未定义）。

- [ ] **步骤 3：实现 VaultTransitStorer**

`pkg/accesskey/vault_storer.go`：

```go
package accesskey

// VaultTransitStorer 是 SecureStorer 的 HashiCorp Vault Transit 实现：凭据明文经 Vault
// Transit 引擎加解密，密钥永不出 Vault。纯 stdlib net/http，无三方依赖（符合 ext-policy）。
// AAD context 绑 AADPath（文件身份）；decrypt 结果短 TTL 缓存（cacheTTL 0 = 关闭）。
type VaultTransitStorer struct {
    addr     string
    mount    string
    keyName  string
    token    string
    aadPath  string
    client   *http.Client
    mu       sync.Mutex
    cache    map[string]vaultCacheEntry // decrypt 缓存（nil = 关闭）
    cacheTTL time.Duration
}

type vaultCacheEntry struct {
    plaintext []byte
    expires   time.Time
}

type VaultOptions struct {
    Addr    string
    Mount   string // 缺省 "transit"
    KeyName string
    Token   string
    CAFile  string        // 自签 CA（可选，nil → 系统池）
    Timeout time.Duration // 0 → 10s
    AADPath string        // AAD context 绑定（建议调用方传凭据文件相对路径）
    CacheTTL time.Duration // 0 = 关闭缓存
}

var _ SecureStorer = (*VaultTransitStorer)(nil)

func NewVaultTransitStorer(opts VaultOptions) (*VaultTransitStorer, error)
```

要点：
- `NewVaultTransitStorer`：校验 addr 非空（`url.Parse` 确认 http/https scheme）、keyName 非空、token 非空（空 → error「vault: token 为空」）；mount 空 → "transit"；timeout ≤0 → 10s；CAFile 非空 → `os.ReadFile` + `x509.NewCertPool` + `AppendCertsFromPEM`（失败 error）+ `http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}`；否则默认 `http.Client{Timeout}`。缓存 map 初始化（CacheTTL>0 时）。
- `Encrypt(plaintext)`：body `{"plaintext": b64(plaintext), "context": b64(aadPath)}`（json.Marshal map 或 struct）→ POST `{addr}/v1/{mount}/encrypt/{keyName}`，头 `X-Vault-Token: token` + `Content-Type: application/json` → 200 解析 `data.ciphertext` 返回；非 200 → `vaultAPIError` 辅助解析 `{"errors":[...]}`。
- `Decrypt(ciphertext)`：
  1. 前缀检查：`!bytes.HasPrefix(ciphertext, []byte("vault:v1:"))` → error（不请求 Vault）。
  2. 缓存查（cacheTTL>0 且命中未过期）→ 返缓存明文。
  3. POST decrypt（body ciphertext + context）→ 200 解析 `data.plaintext` → base64 解码 → 缓存写入 → 返回。
- `vaultAPIError(method string, resp *http.Response, body []byte)`：解析 `{"errors":["..."]}` → `fmt.Errorf("vault: %s 失败: %s (HTTP %d)", method, errors[0], status)`；503 时若 errors 含 "sealed" 特别标注「Vault sealed，需 unseal」。
- 错误统一 `fmt.Errorf("vault: ...: %w", err)` 风格；请求 URL 用 `s.addr + "/v1/" + s.mount + "/" + op + "/" + s.keyName`（不经 `url` 拼接转义——addr 已在构造校验）。
- `context` 用 `base64.StdEncoding.EncodeToString([]byte(s.aadPath))`（可读 AAD；空 aadPath → 空串，仍发字段或省略——选：空则不传 context 字段，Vault 允许缺省）。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/ -run VaultTransitStorer`
预期：PASS。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/vault_storer.go pkg/accesskey/vault_storer_test.go && git commit -m "feat(accesskey): Vault Transit 凭据存储后端——VaultTransitStorer(Encrypt/Decrypt + AAD context + 错误分类)"
```

---

### 任务 2：decrypt 短 TTL 缓存 + sealed 检测补强

**文件：**
- 修改：`pkg/accesskey/vault_storer.go`
- 修改：`pkg/accesskey/vault_storer_test.go`

- [ ] **步骤 1：编写失败的缓存测试**

`vault_storer_test.go` 追加：
- **缓存命中**：CacheTTL=1h + mock Vault decryptFn 计数 → 两次 `Decrypt` 同密文 → mock 只收到 1 次请求（计数断言），两结果 bytes 相等。
- **缓存过期**：CacheTTL=1ns → 两次 Decrypt → mock 收到 2 次（或注入可调 clock——选简单：TTL 极小 + 短暂 sleep 或构造过期 entry）。
- **不同密文不共享缓存**：ciphertext A/B → mock 收 2 次。
- **Encrypt 不碰缓存**：Encrypt 后 cache 长度不变（或只测 Decrypt 缓存语义）。
- **CacheTTL=0（关闭）**：两次 Decrypt → mock 收 2 次。
- **缓存失败透明降级**：缓存层失败（如内存上限）→ 直查 Vault 仍成功（fail-open 于缓存层）——若实现为简单 map 无上限则不适用，可跳过该断言（设计 AD-5 允许缓存层 fail-open，简单 map 无上限时天然无此路径）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/accesskey/ -run VaultCache`
预期：FAIL（缓存未实现）。

- [ ] **步骤 3：实现 decrypt 缓存**

`vault_storer.go`：`NewVaultTransitStorer` 内 CacheTTL>0 → `cache: make(map[string]vaultCacheEntry)`；`Decrypt` 前缀检查后先查缓存（`entry, ok := s.cache[string(ciphertext)]; ok && now.Before(entry.expires)` → 返 copy）；未命中 → Vault 请求 → 成功后 `if s.cache != nil { s.cache[key] = entry }`。并发安全：`s.mu.Lock()/Unlock()` 包缓存读写（map 非并发安全）。惰性清理：插入时若 `len(s.cache) > 1000` 清一次过期项（防无界增长）。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/accesskey/ -run "VaultTransitStorer|VaultCache"`
预期：PASS（含任务 1 用例回归——缓存默认开不破坏）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/vault_storer.go pkg/accesskey/vault_storer_test.go && git commit -m "feat(accesskey): Vault decrypt 短 TTL 缓存——命中/过期/关闭 + 惰性清理"
```

---

### 任务 3：config backend 字段 + VaultConfig + Validate + SetDefaults

**文件：**
- 修改：`pkg/server/config.go`
- 修改：`pkg/server/config_test.go`
- 修改：`config.example.yaml`、`docs/config.md`

- [ ] **步骤 1：编写失败的 config 测试**

`config_test.go` 追加 `TestConfig_CredentialStore_Backend` 表驱动：
- **缺省**：`Default()` → `CredentialStore.Backend == "aesgcm"`、`Vault.Mount == "transit"`、`Vault.TokenEnv == "VAULT_TOKEN"`、`Vault.Timeout == 10*time.Second`、`Vault.CacheTTL == 30*time.Second`。
- **backend 合法枚举**：`aesgcm`/`vault`/`""`（空 → 归一 aesgcm）→ Validate 通过（encrypt=false 时）。
- **backend 非法**：`"aws"` → Validate error 含「backend」。
- **backend=vault + encrypt=true 校验**：
  - vault.addr 空 → error「vault.addr」；
  - vault.key_name 空 → error「vault.key_name」；
  - token 源全空（token_file 空 + token_env 环境变量未设）→ error「token」；
  - addr+key_name+token_file 齐 → Validate 通过（token_file 路径可读性留装配层）。
- **backend=aesgcm + encrypt=true 回归**：master_key_file/env 门禁不变（复用既有断言或补）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run TestConfig_CredentialStore_Backend`
预期：FAIL（字段未定义）。

- [ ] **步骤 3：实现 config 扩展**

`pkg/server/config.go`：

```go
// VaultConfig 是 Vault Transit 后端子配置（credential_store.vault 段，backend=vault 时必需）。
type VaultConfig struct {
    Addr      string        `yaml:"addr" mapstructure:"addr"`
    Mount     string        `yaml:"mount" mapstructure:"mount"`         // transit engine 挂载（默认 "transit"）
    KeyName   string        `yaml:"key_name" mapstructure:"key_name"`
    TokenFile string        `yaml:"token_file" mapstructure:"token_file"`
    TokenEnv  string        `yaml:"token_env" mapstructure:"token_env"` // 默认 "VAULT_TOKEN"
    CAFile    string        `yaml:"ca_file" mapstructure:"ca_file"`
    Timeout   time.Duration `yaml:"timeout" mapstructure:"timeout"`     // 默认 10s
    CacheTTL  time.Duration `yaml:"cache_ttl" mapstructure:"cache_ttl"` // 默认 30s；0 = 关
}
```

- `CredentialStoreConfig` 加 `Backend string \`yaml:"backend" mapstructure:"backend"\`` + `Vault VaultConfig \`yaml:"vault" mapstructure:"vault"\``。
- `SetDefaults`（config.go:462 区域）：`if c.CredentialStore.Backend == "" { c.CredentialStore.Backend = "aesgcm" }`；vault 子段：`Mount == "" → "transit"`、`TokenEnv == "" → "VAULT_TOKEN"`、`Timeout <= 0 → 10s`、`CacheTTL == 0 → 30s`（注意 CacheTTL 0 语义冲突——设计 CacheTTL 0=关闭缓存，但默认 30s 需区分「未设」与「显式 0」。裁定：SetDefaults 只在「YAML 未提供」时设 30s，viper 零值无法区分 → 用 `CacheTTL` 默认 30s 恒定，**用户显式设 0 需靠文档说明关闭**——为最小实现，CacheTTL 默认 30s，用户设 0 且 viper 未覆盖时回落 30s。若需真 0=关，加 `*time.Duration` 指针或单独 bool——**本任务选简单：CacheTTL 恒默认 30s，0 无效回落默认**，文档注明「设 0 无效，缓存默认开」。）

  > 修正：让 CacheTTL 支持关闭需区分未设/显式 0。最简：VaultConfig 不加 cache 开关，CacheTTL 默认 30s 恒开；任务 1 VaultOptions.CacheTTL 传 config 值。关闭缓存的诉求（测试/特殊部署）用 `CacheTTL` 指针或后续——本计划选 **CacheTTL time.Duration 默认 30s，config 层不暴露关闭**，VaultOptions.CacheTTL 内部可传 0（单元测试用）。最终：VaultConfig 无 cache_ttl 字段？—— 保留字段但文档注明默认 30s 且恒开配置不可关（YAGNI）。

  **再裁定（final）**：VaultConfig 含 `CacheTTL time.Duration`（yaml `cache_ttl`），SetDefaults `==0 → 30s`。因 viper 零值歧义，**用户无法显式关缓存**（设 0 回落 30s）。文档注明。此简化可接受——缓存 30s 对凭据读写无害（Save 路径不缓存）。

- `Validate`（config.go:826 区域追加）：
  ```go
  switch c.CredentialStore.Backend {
  case "", "aesgcm", "vault": // ok
  default:
      return fmt.Errorf("credential_store.backend=%q 无效，仅允许 aesgcm 或 vault", c.CredentialStore.Backend)
  }
  if c.CredentialStore.Encrypt && c.CredentialStore.Backend == "vault" {
      if c.CredentialStore.Vault.Addr == "" { return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.addr") }
      if c.CredentialStore.Vault.KeyName == "" { return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.key_name") }
      tokEnv := c.CredentialStore.Vault.TokenEnv; if tokEnv == "" { tokEnv = "VAULT_TOKEN" }
      if c.CredentialStore.Vault.TokenFile == "" && os.Getenv(tokEnv) == "" {
          return fmt.Errorf("credential_store.backend=vault 需配置 credential_store.vault.token_file 或环境变量 %s", tokEnv)
      }
  }
  ```

- config.example.yaml：credential_store 段补 backend + vault 子段注释（含 `vault server -dev` 冒烟提示、transit key 创建 `vault secrets enable transit && vault write -f transit/keys/sproxy`、token 获取提示）。
- docs/config.md：同。

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "TestConfig_CredentialStore_Backend|Registration|CredentialTTL"`
预期：PASS（含既有 config 测试回归）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/config.go pkg/server/config_test.go config.example.yaml docs/config.md && git commit -m "feat(server): credential_store.backend 配置——aesgcm|vault 枚举 + VaultConfig 子段 + Validate 门禁"
```

---

### 任务 4：装配分支（BootstrapServerCredentials）+ vaultmock 抽共享 + 装配测试

**文件：**
- 创建：`pkg/testutil/vaultmock/vaultmock.go`（+ `_test.go` 自测可选）
- 修改：`pkg/server/handlers.go`
- 修改：`pkg/server/credential_store_encrypt_test.go`

- [ ] **步骤 1：编写失败的装配测试**

`credential_store_encrypt_test.go` 追加 `TestBootstrap_VaultBackend`：
- 抽共享 `pkg/testutil/vaultmock`：从任务 1 的内联 mock 泛化——`vaultmock.NewServer(t, opts)` 可配置 encrypt/decrypt 响应 + 请求计数 + 记录收到的 token/context。
- 装配 vault：cfg `{Encrypt: true, Backend: "vault", Vault: {Addr: mock.URL(), KeyName: "sproxy", TokenFile: <temp file 写 token>}}` → `BootstrapServerCredentials` → 返回 store 非 nil；`store.Save(keys)` → mock Vault 收到 encrypt 请求（含 token 头 + context=base64("credentials.json")）→ 落盘为 `vault:v1:` 密文；`store.Load()` → mock 回 base64 → 还原 keys（含 Role/TOTPSecret）。
- **token_file 优先**：temp 文件写 tokenA + env 设 tokenB → 装配用 tokenA（mock 断言请求头）。
- **token 全无**：file 空 + env 未设 → Bootstrap 返 error（fail-fast）。
- **aesgcm 回归**：`Backend: "aesgcm"`（或空）→ 行为与现状一致（复用既有 TestBootstrap_Encrypt 类断言或补一条 backend 空 = aesgcm）。

- [ ] **步骤 2：运行验证失败**

运行：`go test -race -count=1 ./pkg/server/ -run TestBootstrap_VaultBackend`
预期：FAIL（backend 分支未实现 / vaultmock 不存在）。

- [ ] **步骤 3：实现装配分支**

`pkg/server/handlers.go` `BootstrapServerCredentials`：

```go
if cfg.CredentialStore.Encrypt {
    var secure accesskey.SecureStorer
    switch cfg.CredentialStore.Backend {
    case "vault":
        tok, err := resolveVaultToken(cfg.CredentialStore.Vault)
        if err != nil { return nil, nil, err }
        v, err := accesskey.NewVaultTransitStorer(accesskey.VaultOptions{
            Addr:     cfg.CredentialStore.Vault.Addr,
            Mount:    cfg.CredentialStore.Vault.Mount,   // SetDefaults 已填 "transit"
            KeyName:  cfg.CredentialStore.Vault.KeyName,
            Token:    tok,
            CAFile:   cfg.CredentialStore.Vault.CAFile,
            Timeout:  cfg.CredentialStore.Vault.Timeout, // SetDefaults 已填 10s
            AADPath:  "credentials.json",
            CacheTTL: cfg.CredentialStore.Vault.CacheTTL, // SetDefaults 已填 30s
        })
        if err != nil { return nil, nil, err }
        secure = v
    default: // aesgcm（含空）
        masterKey, err := resolveCredentialMasterKey(cfg)
        if err != nil { return nil, nil, err }
        secure = accesskey.AESGCMStorer{Key: masterKey}
    }
    store = accesskey.NewEncryptingStorer(filepath.Join(metaDir, "credentials.json"), secure)
    logger.Info("凭据静态存储加密已启用", "backend", cfg.CredentialStore.Backend)
}
```

新增 `resolveVaultToken(vc VaultConfig) (string, error)`：
```go
func resolveVaultToken(vc VaultConfig) (string, error) {
    if vc.TokenFile != "" {
        data, err := os.ReadFile(vc.TokenFile)
        if err != nil { return "", fmt.Errorf("读取 vault token 文件失败: %w", err) }
        tok := strings.TrimSpace(string(data))
        if tok != "" { return tok, nil }
    }
    envName := vc.TokenEnv; if envName == "" { envName = "VAULT_TOKEN" }
    if tok := os.Getenv(envName); tok != "" { return tok, nil }
    return "", fmt.Errorf("credential_store.backend=vault 需配置 token_file 或环境变量 %s", envName)
}
```

`pkg/testutil/vaultmock/vaultmock.go`（抽共享，仿 mockserver 模式）：
```go
package vaultmock
// Server 是模拟 Vault Transit encrypt/decrypt 端点的 httptest.Server（L1/装配测试共享）。
// 可配置响应；记录收到的 token / context / plaintext / ciphertext；请求计数。
type Server struct { ... }
func NewServer(t *testing.T, opts Options) *Server
type Options struct {
    Token     string
    DecryptTo []byte // decrypt 返回的明文（nil → 回显请求 ciphertext 的镜像）
}
func (s *Server) URL() string
func (s *Server) EncryptCount() int
func (s *Server) LastToken() string
func (s *Server) LastContext() string
func (s *Server) LastPlaintextB64() string
func (s *Server) SetDecryptError(status int, msg string)
```

- [ ] **步骤 4：运行验证通过**

运行：`go test -race -count=1 ./pkg/server/ -run "TestBootstrap|CredentialStoreEncrypt"` + `go test -race -count=1 ./pkg/accesskey/ ./pkg/testutil/...`
预期：PASS（aesgcm 回归 + vault 装配新用例 + 既有 credentialstore 加密测试全绿）。

- [ ] **步骤 5：Commit**

```bash
git add pkg/server/handlers.go pkg/server/credential_store_encrypt_test.go pkg/testutil/vaultmock/ && git commit -m "feat(server): Bootstrap 按 backend 分支装配——vault=resolveVaultToken+NewVaultTransitStorer / aesgcm 回归 + vaultmock 测试工具"
```

---

### 任务 5：L2/L3 真实 Vault 集成测试 + make test-vault

**文件：**
- 创建：`pkg/accesskey/vault_integration_test.go`（`//go:build vault_integration`）
- 修改：`Makefile`

- [ ] **步骤 1：编写 L2/L3 集成测试**

`pkg/accesskey/vault_integration_test.go`（`//go:build vault_integration`）：

```go
// 真实 Vault 集成测试（L2 契约 / L3 行为）。需本地 Vault dev mode：
//   docker run --rm -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=root hashicorp/vault
// 或 vault server -dev -dev-root-token-id=root。运行：make test-vault
package accesskey_test

// 环境变量：VAULT_ADDR（默认 http://127.0.0.1:8200）、VAULT_TOKEN（默认 root）、
// VAULT_SKIP 置任意值跳过（CI 无 Vault 时防御）。
```

用例：
- **L2 契约-往返**：test helper 建 transit key（若不存在 `vault secrets enable transit` + `vault write -f transit/keys/<name>`，通过 http API 或直接假设已配置——选：test 用 HTTP API 建 key，若 400 key 已存在则忽略）→ Encrypt 任意明文 → 密文含 `vault:v1:` 前缀 → Decrypt 还原。
- **L2-AAD context**：Encrypt with AADPath=A → Decrypt with AADPath=A 成功；Decrypt with AADPath=B → 失败（Transit context 不匹配语义）。
- **L3-key 轮换**：`POST /v1/transit/keys/<name>/rotate` → 旧密文 Decrypt 仍成功（Vault 按版本自解）。
- **L3-权限拒绝**：建受限 policy token（仅 encrypt 无 decrypt 或反之）→ 越权操作 → error 含「permission denied」。
- **L3-seal**：真实 seal/unseal 需要 unseal key——dev mode 单 key 可 `POST /v1/sys/seal` 用 root token → 随后 Encrypt → error 含「sealed」→ `POST /v1/sys/unseal`（dev root key 空）恢复。（seal/unseal 测试风险：若环境非 dev 会破坏——**选：L3-seal 用单独新建的 Vault 实例或标记 skip-if-not-dev**；简化：只测「mock 503-sealed 已在 L1 覆盖」，真实 seal 用 `VAULT_SEAL_TEST=1` 门控，默认跳过。**最终：L3 覆盖轮换 + 权限拒绝；seal/unseal 用环境变量门控的独立子测试，默认 skip，因真实 seal 有环境破坏风险。**）
- 前置：`VAULT_ADDR` 不可达 → `t.Skip`（防 CI 意外跑）。

- [ ] **步骤 2：确认默认跳过**

运行：`go test -race -count=1 ./pkg/accesskey/`（无 tag）→ vault_integration_test.go 不编译（build tag），现有测试绿。

- [ ] **步骤 3：Makefile test-vault 目标**

Makefile 加：

```makefile
# 真实 Vault 集成测试（需本地 Vault dev mode：docker run --rm -p 8200:8200 \
# -e VAULT_DEV_ROOT_TOKEN_ID=root hashicorp/vault）
.PHONY: test-vault
test-vault:
	cd pkg/accesskey && go test -race -count=1 -tags=vault_integration ./...
```

- [ ] **步骤 4：验证（有本地 Vault 时）**

运行：`make test-vault`（本地 Vault dev mode 存在时）
预期：PASS（L2 往返/AAD + L3 轮换/权限）。若本地无 Vault → 说明需先起 docker。

- [ ] **步骤 5：Commit**

```bash
git add pkg/accesskey/vault_integration_test.go Makefile && git commit -m "test(accesskey): Vault L2/L3 集成测试——往返/AAD/轮换/权限 (vault_integration tag) + make test-vault"
```

---

## 收尾验证（全部任务后）

- [ ] **步骤 1：全量验证**

```bash
git fetch origin && git rebase origin/master
go test -race -count=1 ./pkg/accesskey/... ./pkg/server/... ./pkg/client/... ./cmd/sclient/... ./cmd/sproxy/...
make lint && make build-all && make check-loopback && go vet ./...
```

- [ ] **步骤 2：手测（真实 Vault 冒烟，L2/L3 实跑）**

- 起 `docker run --rm -p 8200:8200 -e VAULT_DEV_ROOT_TOKEN_ID=root hashicorp/vault`；
- `vault secrets enable transit` + `vault write -f transit/keys/sproxy`；
- config：`credential_store: {encrypt: true, backend: vault, vault: {addr: http://127.0.0.1:8200, key_name: sproxy, token: root}}`；
- 启动 sproxy → 日志「凭据静态存储加密已启用 backend=vault」→ register 一个用户 → 检查 `credentials.json` 为 `vault:v1:` 密文 → 重启 sproxy → 凭据还原（list/签名 200）；
- 改 Vault key 轮换 `vault write -f transit/keys/sproxy/rotate` → 重启仍可解（L3）；
- 错误 token → 启动 fail-fast。

- [ ] **步骤 3：独立审查 + 修复全部发现（含 Minor）**

SDD 流程：review-package → 独立对抗审查 → 定向复审循环（用户 must-fix-all）。

- [ ] **步骤 4：PR**

```bash
git push -u origin feature/credential-vault-transit
gh pr create --title "feat(server): Vault Transit 凭据存储加密后端——credential_store.backend 分支 + AAD context + 三层测试" --body "…"
```

（title 功能核心，不含阶段编号。）

---

## 自检

- **规格覆盖度**：AD-1 backend 字段（任务 3）+ 装配分支（任务 4）✓ / AD-2 Transit 直加密（任务 1）✓ / AD-3 纯 stdlib（任务 1 import 清单）✓ / AD-4 AAD context（任务 1 Encrypt/Decrypt context 字段 + AADPath）✓ / AD-5 decrypt 缓存（任务 2）✓ / 三层测试 L1（任务 1-2 mock + 任务 4 vaultmock 抽共享）/ L2-L3（任务 5）✓ / 错误分类 sealed/403/404/5xx/前缀拒绝（任务 1）✓ / config Validate + SetDefaults + 文档（任务 3）✓。
- **占位符扫描**：无 TODO/待定；每任务含实际代码/测试。L3 seal 测试「环境破坏风险」已裁定为 env 门控默认 skip（非占位——明确行为）。
- **类型一致性**：`VaultTransitStorer`/`VaultOptions{Addr,Mount,KeyName,Token,CAFile,Timeout,AADPath,CacheTTL}`/`VaultConfig{Addr,Mount,KeyName,TokenFile,TokenEnv,CAFile,Timeout,CacheTTL}`/`resolveVaultToken` 跨任务一致；`credential_store.backend` yaml 键一致。
