# HashiCorp Vault Transit 凭据存储加密后端设计

> 日期：2026-09-07
> 关联：4C（PR #163/#164）凭据静态加密已落地（`SecureStorer` 接口 + `AESGCMStorer` + `KMSStorer`/`KMSClient` 接缝）；本设计在其上新增 **Vault Transit 后端**。

## 目标

为 sproxy 凭据静态存储加密新增 **HashiCorp Vault Transit** 后端：凭据明文经 Vault Transit 引擎加解密，**密钥永不出 Vault**、key 轮换集中管理、per-node token 独立签发/吊销。**价值定位 = 运维集中**（一台 Vault 服务 N 个 sproxy 节点，节点被攻破只吊销其 token，无需换全局 key）——威胁模型上防单机磁盘被盗读由 `aesgcm` 已覆盖，Vault 是纵深/运维价值，非当前必需。

## 架构决策

**AD-1：backend 字段 + 分支装配（非抽象层/registry）。** config 加 `credential_store.backend: aesgcm|vault`，`BootstrapServerCredentials` 按 backend switch 装配对应 SecureStorer。每加后端 = 枚举值 + 装配分支 + 一个 SecureStorer 实现——此形态天然是未来 AWS/GCP/多节点的模版，**不需要**泛型 backend 接口 + registry 抽象层（YAGNI：当前仅两后端）。选定方案（用户裁定「先定架构后选后端」→ 方案「backend 字段 + Vault Transit 内置」）。

**AD-2：Vault Transit 引擎直加密切换（非 DEK 信封）。** Transit 的 `encrypt`/`decrypt` 天然匹配 `SecureStorer`（字节进出），整份凭据明文经 Vault 加解密。对比「Vault 作 KMSClient 注入 KMSStorer 包 DEK」：Transit 直加密使**明文不经本地 AES**、key 永驻 Vault，且 key 版本轮换由 Vault 管理（密文自带版本，decrypt 自解）——兑现用户方案选择时「密钥永不出 Vault + 轮换集中」意图。

**AD-3：Vault client 内置主包（纯 stdlib）。** 用 `net/http` 直调 Vault HTTP API，**不引入 `hashicorp/vault/api` SDK**——符合 ext-policy（「ext 唯一正当理由是隔离外部三方库」；stdlib 可达则内置）。Vault HTTP API 简单（两个端点），无 SDK 必要。

**AD-4：AAD context 绑定文件身份。** Transit `encrypt`/`decrypt` 的 `context` 参数（base64，可选的额外认证数据）绑 **file path**（如 `credentials.json` 相对 storage_root 路径）——密文被复制/搬移到另一文件即 decrypt 失败（context 不匹配）。兑现 4C 记录的建议 C（多文件共用 key 需 AAD 防跨租户搬移）。

**AD-5：decrypt 结果短 TTL 缓存。** `VaultTransitStorer` 内缓存解密结果（短 TTL，默认 30s；cache key = file path + Vault key 版本 + ciphertext hash），防同一凭据文件短时间内重复 Load 反复往返 Vault。凭据变更走 Save 路径（无缓存，直接 encrypt）。当前凭据读写低频（Load 每启动、persist 每凭据变更），缓存的收益在未来高频读场景，属前瞻但低成本。缓存失败（内存上限/过期）→ 透明降级直查 Vault（fail-open 于缓存层，fail-closed 于 Vault 层）。

## 配置模型

```yaml
credential_store:
  encrypt: false          # 总开关（保持现状语义）
  backend: aesgcm         # 新增：aesgcm（默认，现状本地 master key）| vault（Vault Transit）
  master_key_file: ""     # aesgcm 专用（现状字段不动）
  vault:                  # vault 专用子段（backend=vault 时必须）
    addr: "https://vault:8200"   # Vault 地址（必须）
    mount: "transit"              # transit engine 挂载路径（默认 "transit"）
    key_name: "sproxy"            # transit 加密 key 名（必须）
    token_file: ""                # token 文件路径（优先）；空则回落 token_env
    token_env: "VAULT_TOKEN"      # token 环境变量名（默认 VAULT_TOKEN）
    ca_file: ""                   # 自签 CA 证书路径（可选，默认系统池）
    timeout: "10s"                # HTTP 超时（默认 10s）
    cache_ttl: "30s"              # decrypt 结果缓存 TTL（默认 30s）
                                  # final ruling：config 层设 0 无效回落 30s（viper 零值歧义——
                                  # 缓存恒默认开、不可显式关）；关闭仅限 VaultOptions 内部传 0（测试）
```

**语义**：
- `encrypt:false` → 明文（现状，零回归）；
- `encrypt:true + backend:aesgcm`（缺省/空）→ 现状 AESGCMStorer 路径（master_key_file/env），**完全不变**；
- `encrypt:true + backend:vault` → `VaultTransitStorer`（新），忽略 master_key_file，要求 vault 子段完整。

**向后兼容**：`backend` 缺省 `aesgcm`；既有配置（仅 encrypt/master_key_file）行为不变。

## 组件设计

### `pkg/accesskey/vault_storer.go`（新，内置 `package accesskey`）

```go
// VaultTransitStorer 是 SecureStorer 的 HashiCorp Vault Transit 实现：凭据明文经 Vault
// Transit 引擎加解密，密钥永不出 Vault（sproxy 仅持 token 调 API）。无三方依赖（stdlib
// net/http）。AAD context 绑定 file identity；decrypt 结果短 TTL 缓存。
type VaultTransitStorer struct {
    addr    string
    mount   string
    keyName string
    token   string
    aadPath string        // AAD context 绑定的文件身份（如相对 storage_root 路径）
    client  *http.Client
    cache   *decryptCache // 短 TTL 缓存（TTL 0 = 关闭）
}

type VaultOptions struct {
    Addr, Mount, KeyName, Token string
    CAFile   string
    Timeout  time.Duration // 0 → 10s
    AADPath  string        // AAD context 绑定（建议调用方传凭据文件相对路径）
    CacheTTL time.Duration // 0 = 关闭缓存
}

func NewVaultTransitStorer(opts VaultOptions) (*VaultTransitStorer, error)
func (s *VaultTransitStorer) Encrypt(plaintext []byte) ([]byte, error)
func (s *VaultTransitStorer) Decrypt(ciphertext []byte) ([]byte, error)
```

### Vault HTTP API 契约

```
POST {addr}/v1/{mount}/encrypt/{key_name}
  X-Vault-Token: <token>
  {"plaintext":"<base64>", "context":"<base64 AAD>"}   → 200 {"data":{"ciphertext":"vault:v1:..."}}

POST {addr}/v1/{mount}/decrypt/{key_name}
  X-Vault-Token: <token>
  {"ciphertext":"vault:v1:...", "context":"<base64 AAD>"} → 200 {"data":{"plaintext":"<base64>"}}
```

AAD `context` = SHA-256(AADPath) 或直接 base64(AADPath)——设计选 **base64(AADPath)**（可读、调试友好）；decrypt 传同值。

### 错误分类（fail-closed）

| 场景 | 行为 |
|---|---|
| Vault 不可达（网络） | error（含 addr）；不静默明文 |
| Vault 503 + "sealed" | 明确「Vault sealed，需 unseal」错误（非网络误判） |
| Vault 4xx（token 失效 403/无权限/key 不存在 404） | 解析 Vault 错误消息包装，清晰排障方向 |
| Decrypt 输入非 `vault:v1:` 前缀 | 直接报错不请求 Vault（防明文误喂） |
| AAD context 不匹配 | decrypt 返错误（Transit 语义保证） |
| Vault 5xx | 原样包装 |

## 装配

`pkg/server/handlers.go` `BootstrapServerCredentials`：

```go
if cfg.CredentialStore.Encrypt {
    var secure accesskey.SecureStorer
    switch cfg.CredentialStore.Backend {
    case "vault":
        tok, err := resolveVaultToken(cfg.CredentialStore.Vault)  // file→env→error
        v, err := accesskey.NewVaultTransitStorer(accesskey.VaultOptions{
            Addr:    cfg.CredentialStore.Vault.Addr,
            Mount:   cfg.CredentialStore.Vault.Mount,   // 缺省 "transit"
            KeyName: cfg.CredentialStore.Vault.KeyName,
            Token:   tok,
            CAFile:  cfg.CredentialStore.Vault.CAFile,
            Timeout: cfg.CredentialStore.Vault.Timeout, // 缺省 10s
            AADPath: "credentials.json",                 // 相对 storage 的凭据文件身份
            CacheTTL: cfg.CredentialStore.Vault.CacheTTL,
        })
        // ...
        secure = v
    default: // aesgcm（含空 = 向后兼容）
        masterKey, err := resolveCredentialMasterKey(cfg)
        secure = accesskey.AESGCMStorer{Key: masterKey}
    }
    store = accesskey.NewEncryptingStorer(filepath.Join(metaDir, "credentials.json"), secure)
}
```

`resolveVaultToken`：token_file 读文件 trim → 空则 token_env 环境变量 → 仍空 error fail-fast。

**装配失败 fail-fast**：vault 连不上/认证失败在启动期 return error（cmd/sproxy 拒绝启动），不与明文路径混。

## 验证：三层测试结构（用户裁定）

| 层 | 手段 | 覆盖 |
|---|---|---|
| L1 单元 | httptest 假 Vault | 请求/响应 JSON 解析、base64/AAD context 编解码、错误分类（403/404/503-sealed/5xx）、decrypt 缓存命中/过期/失效、非 vault:v1: 输入拒绝、ca_file 加载 |
| L2 契约 | 真 Vault dev mode | 真实 API 行为（encrypt→decrypt 往返）、AAD context 语义（同 AAD 成功 / 异 AAD 解密失败，**key 需 derived=true**——非 derived key 忽略 context）、密文格式（vault:v1: 前缀、版本字段） |
| L3 行为 | 真 Vault + 状态操作 | key 轮换（rotate 后 latest_version 递增 + 旧密文仍可解）、权限拒绝（受限 policy token → Decrypt permission denied） |

- **L2/L3 用真实 Vault（用户 2026-09-07 调整后）**：`vault_integration_test.go` **无 build tag**、运行时检测 `VAULT_ADDR`（默认 http://127.0.0.1:8200）可达性——不可达 `t.Skip`（本地无 Vault / CI 非 ubuntu-vault job 自动跳过不失败）。docker 容器（`scripts/test-vault.sh`）/ CI ubuntu `services.vault` 起真实 Vault（镜像钉 minor 如 `hashicorp/vault:1.18`）时自动实跑 L2/L3。L1（httptest mock）进常规 CI 恒跑。
- **requireVault 就绪语义（审查 M8 定稿）**：健康检查 `GET /v1/sys/health`——**200 视为就绪**；网络不可达 / 429（standby）/501（未初始化）/503（sealed）一律视为不可用 → `t.Skip`。
- L3-seal 不做自动化（真实 seal 锁 dev 实例、需 unseal key 恢复，环境破坏风险）——sealed 错误分类已由 L1 mock（503 sealed 用例）覆盖；文档说明默认跳过。

## 范围外（后续独立规划）

- **对等多节点存储**：用户方向「先做 Vault，然后做对等扩展」——多节点经 mesh 互访/统一命名空间存储是本设计之后的方向，Vault 作为其中心凭据组件到时自然延伸。不在本设计范围。
- **AWS KMS backend**（Role ARN）：backend 模版就绪后按需加（AWS SDK → ext）。本设计不含。
- **SecureStorer 接口引入 ctx**：真实 Vault 网络往返，后续评估接口级 ctx 穿透（当前 http.Client timeout 已提供超时边界）。

## 文件清单

- 新建：`pkg/accesskey/vault_storer.go`、`pkg/accesskey/vault_storer_test.go`（L1 httptest）
- 新建：`pkg/accesskey/vault_integration_test.go`（**无 build tag**，运行时检测 VAULT_ADDR 可达性，不可达 `t.Skip`）、`scripts/test-vault.sh`（docker 自动起容器）
- 修改：`pkg/server/config.go`（`CredentialStoreConfig` 加 Backend + Vault 子段 + Validate/SetDefaults）、`pkg/server/handlers.go`（Bootstrap 分支 + resolveVaultToken）、`pkg/server/config_test.go`（Validate 增量）、`pkg/server/credential_store_encrypt_test.go`（装配）
- 文档/CI：`config.example.yaml`、`docs/config.md`、Makefile（test-vault 目标）、`.github/workflows/ci.yml`（ubuntu test job + vault service；windows 拆独立 job 仅 L1）
