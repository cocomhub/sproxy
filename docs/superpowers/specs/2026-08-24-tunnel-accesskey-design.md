---
title: access-key 驱动的 /tunnel 认证与编解码（废除 tunnel_key / relay_token）
status: approved
---

# access-key 驱动的 /tunnel 认证与编解码设计

> 日期：2026-08-24
> 分支：feature/mesh-tunnel
> 状态：**approved**（用户已确认整体设计）

## 1. 背景与问题

`sproxy` 服务端配置 `access_keys`（SproxySig 请求签名）后，**纯 tunnel 场景（`sclient list` 等走 `WithTunnel`）出现 HTTP 400**，服务端日志：

```
time=... level=ERROR msg="解析隧道 metadata 失败" component=tunnel error="decrypt metadata with all keys: decrypt: cipher: message authentication failed"
```

**根因**：`/tunnel` 外层由认证重构后的 `srvMux` 承接，但认证重构引入 `access_keys`/`SproxySig` 后，`sclient`（纯 tunnel 模式 `WithTunnel`）只用 tunnel_key 加密 metadata，**未携带 `access_key`**；服务端接到 `/tunnel` 后，被 `authMiddleware`（access_keys 已配置）或签名中间件拦截/污染 → 解密 metadata 失败 → 400。

**目标**：让 `access_key/access_key_secret` 成为 `/tunnel` 的唯一认证与编解码来源，**废除 `tunnel_key` 与 `relay_token`**，同时保持 mesh 节点注册/信令的 per-node secret 身份语义，并用 SproxySig 扩展能力支持大文件流式（不阻塞 body）。

---

## 2. 已确认的决策点

| # | 决策点 | 结果 | 理由 |
|---|--------|------|------|
| 1 | 编解码密钥来源 | **HKDF 派生**（从 SK + mesh_id + 固定 salt） | 避免直接暴露 SK 为 AES 对称密钥；`golang.org/x/crypto/hkdf` 已在 go.mod 可用 |
| 2 | 老 tunnel_key 兼容 | **彻底废除** | 用 access_keys 单一来源，避免双密钥歧义 |
| 3 | 未配置 access_keys | **启动 fail-fast 报错退出** | 不再支持无认证隧道；杜绝静默默认密钥 |
| 4 | 外层签名 | **双签**：`body_sha256`=metadata 帧 SHA-256 + SproxySig 支持 `UNSIGNED` | 大文件流 body 无法整体哈希；metadata 帧固定小、可哈希；AES-GCM 已保护正文 |
| 5 | relay_token | **废除**，节点注册改 **AK 签名准入**，per-node secret 留存 | 上层 mesh 依赖 per-node secret 做信令身份绑定，注册准入改为 AK/HMAC 证明 |
| 6 | /tunnel 主流程 | **A：authMiddleware 验签 + 派生密钥进 ctx** | 鉴权在前、解密在后、密钥周期短；链路清晰可维护 |

---

## 3. 架构与主流程

### 3.1 数据流图

```
sclient (WithTunnel + access_key/secret)      sproxy 服务端
──────────────────────────────               ─────────────────
构造 metadata 帧                               srvMux (无 tunnel_key)
  [4B len + AES(派生密钥) meta] + body_sha256=SHA256(帧)    │
  → POST /tunnel                              ▼
  Authorization: SproxySig                   authMiddleware (SproxySig 验签)
     ak/ts/exp/nonce/body_sha256/sig            │ nonce 池 + 时间 + HMAC
  body = 帧密文流                              │ AK→SK 查表
                                              ▼
                                          派生 HKDF(SK, mesh) → 隧道密钥
                                              → context
                                              ▼
                                         隧道 handler
                                           ├─ ctx 密钥解密 metadata 帧
                                           ├─ 本地路由到 localMux 业务 handler
                                           └─ 响应密文也用 ctx 派生密钥加密
```

### 3.2 关键组件改动

| 文件 | 改动 |
|------|------|
| `pkg/server/auth.go` | `authMiddleware`：对 `POST /tunnel` 走 SproxySig 验签分支；成功后 `sproxysig.DeriveTunnelKey(sk, meshID)` 派生隧道密钥，`SetTunnelKey(ctx, key)` 存入请求 context；支持 `body_sha256=UNSIGNED` 跳过头校验 |
| `pkg/tunnel/handler_client.go` | `Handler` 改从 ctx 取隧道密钥（`GetTunnelKey`），未取到则拒绝；`ServeHTTP` 用 ctx 密钥解密 metadata 与 body、加密响应 |
| `pkg/server/handlers.go` | `RegisterRoutes`：`POST /tunnel` 单独挂 srvMux，不挂 authMiddleware 的 Bearer 分支；`h.tunnelHandler` 由 ctx 密钥驱动 |
| `pkg/sproxysig/sproxysig.go` | 新增 `UnsignedBody` 语义（`==UnsignedBody` 跳过 body 哈希比对）；新增 `DeriveTunnelKey(sk, mesh)` HKDF 派生 |
| `pkg/tunnel/tunnel.go` | `NewClient`/`WithTunnel` 改为从 client 配置的 AK/SK 派生密钥；`ParseKey`/`GenerateKey` 兼容保留（供 sclient access-key 生成） |
| `pkg/client/client.go` | `WithTunnel` 支持 `access_key/access_key_secret`；`withTunnelKeyDerive` 用与 `GetTunnelKey` 相同的 HKDF 派生；`doRequest` 的 `/tunnel` 请求签名 `body_sha256` 为 metadata 帧哈希 |
| `cmd/sclient/factory.go` | `WithTunnel` 仅接受 access_key/access_key_secret；删除 tunnel_key 读取 |
| `pkg/server/config.go` | 删除 `TunnelKey` 字段；`access_keys` 未配置时启动 fail-fast |
| `pkg/tunnel/hub/*` | 节点注册改 AK 签名准入（废除 relay_token）；信令仍用 per-node secret |
| `cmd/sproxy/root.go` | 移除 tunnel_key 解析/自动生成；启动校验 access_keys 非空 |

### 3.3 编解码密钥派生（HKDF）

```go
// 密钥派生：HKDF-SHA256(ikm=SK_hex, salt=固定 salt, info=mesh_id)
// 每个 mesh 的 SK 派生独立隧道密钥；未提供 mesh_id 用 "" 派生（默认 mesh）。
func DeriveTunnelKey(sk, mesh string) ([]byte, error) {
    secret, err := hex.DecodeString(sk)
    if err != nil { return nil, err }
    salt := []byte("sproxy-tunnel-key-v1")
    r := hkdf.New(sha256.New, secret, salt, []byte(mesh))
    out := make([]byte, 32)
    if _, err := io.ReadFull(r, out); err != nil { return nil, err }
    return out, nil
}
```

- salt 固定（公开）：HKDF 不需要秘密盐，固定字符串提供域分离（防止跨版本/跨上下文复用）；
- info 用 `mesh_id`（每个 mesh 独立密钥，多 mesh 互不串扰）；
- 输出 32 字节 = AES-256 密钥，直接用于 `AADMeta`/`AADStream` 加解密。

### 3.4 SproxySig 双签（流式 body 处理）

- **metadata 帧哈希**：客户端把整个 `[4B len + AES 密文]` 的 SHA-256 作为 `body_sha256` 放入签名头；服务端 `NewBodyValidator` 流式累加哈希、EOF 比对（正确帧校验通过，无阻塞）；
- **UNSIGNED 能力**：`body_sha256 == UnsignedBody` 时服务端不比对 body 哈希（大文件流 / 隧道长连接；AES-GCM 已保证正文完整性）——sclient 可选用；
- 两条路径都不影响隧道 body 流式（不整体哈希、不阻塞）。

### 3.5 废除 tunnel_key / relay_token 的迁移

- 服务端：删除 `tunnel_key` 配置字段；启动时 `len(cfg.AccessKeys) == 0` → `fmt.Errorf("拒绝启动：未配置 access_keys")`，**fail-fast**；
- `sclient`：`tunnel_key` 配置项删除，`WithTunnel` 从 `access_key_secret` 派生；
- hub 注册：`relay_token` 字段废弃，节点注册改用 AK 签名准入（HMAC 证明持有 SK），per-node secret 由服务端注册响应下发并继续用于信令。

---

## 4. 测试计划

| 测试 | 位置 | 验证点 |
|------|------|--------|
| `TestTunnel_AccessKeyAuth` | `pkg/server/integration_test.go` | `POST /tunnel` 带 SproxySig 签名，可访问文件 API（list/upload）；缺签名 401；错 AK 401 |
| `TestTunnel_DerivedKeyDecrypt` | `pkg/tunnel/*_test.go` | 客户端由 SK 派生密钥加密，服务端由同一 SK 派生密钥解密成功 |
| `TestSproxySig_UnsignedBody` | `pkg/sproxysig/sproxysig_test.go` | `UNSIGNED` 跳过 body 哈希比对；普通哈希路径仍校验 |
| `TestRegisterRoutes_NoAccessKeys_Fails` | `cmd/sproxy/root_test.go` | 未配置 access_keys 启动报错 |
| E2E | `test/e2e_tunnel_accesskey_test.go` | sclient 纯隧道 + access_keys，`list` 成功；无 tunnel_key |
| mesh 注册迁移 | `pkg/tunnel/hub/*_test.go` | 节点 AK 签名准入、per-node secret 留存、信令正常 |

---

## 5. 兼容与风险

### 兼容性
- **不兼容旧 tunnel_key**：老配置必须迁移到 `access_keys`；服务端启动 fail-fast 保证不会再跑无认证模式（开发模式需显式配置 access_keys）。
- **domain 分离**：AAD 标签不变（`tunnel:meta:v1` / `tunnel:stream:v1`），但派生密钥不同→旧密文无法解密（预期）。
- 保留 `ParseKey`/`GenerateKey`（供 `sclient access-key create` 生成 SK 素材），但不再有全局隧道密钥。

### 风险
1. **客户端/服务端派生不一致**：两端必须用完全相同的 HKDF 参数（salt/info/输出长度）——用共享语料测试锁死；
2. **SproxySig `UNSIGNED` 滥用**（大 body 不哈希）：保留签名对 header/path/query 覆盖，AES-GCM 兜底；仅用于隧道。
3. **mesh 迁移面大**：relay_token 在 `hub.Register`/`NewAuthenticator`/sclient 多处引用，需一并改 AK 签名准入。

---

## 6. 已完成/待办

- [x] 用户确认整体设计（2026-08-24）
- [x] 写实现计划（writing-plans，`2026-08-24-tunnel-accesskey.md`）
- [x] 任务 1–9 全部实现（commit `eb9efdd..047834d`，分支 `feature/mesh-tunnel`）
  - 任务 1–5：sproxysig 双签 / ctx 密钥 / DeriveTunnelKey / authMiddleware / handler ctx
  - 任务 6–7：废除 tunnel_key + fail-fast / sclient WithTunnel 派生（`269b669`）
  - 任务 8：hub 节点注册 AK/HMAC 准入（`36b757c`）
  - 任务 9：E2E 迁移 access_keys + 纯隧道 E2E + bodyValidator 哈希即时校验（`047834d`）
- [x] 全量验证：`make test-all` 所有子 module 通过；`make lint` 0 issues；E2E 全绿
