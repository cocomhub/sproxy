# access-key 驱动 /tunnel 认证重构 —— 最终代码审查报告

- 审查范围：`git diff dfe40cd..HEAD`（commit 269b669 / 36b757c / 047834d，任务 6-9）
- 审查人角色：Go 安全 / 协议审查
- 日期：2026-08-25
- 结论摘要：**无 Critical；4 项 Important；6 项 Minor**。核心安全属性（HMAC proof 绑定 node_id、constant-time 比较、fail-closed、SK 永不上线、bodyValidator (n>0, io.EOF) 修复）均验证正确。

---

## 一、审查重点逐项结论

### 1. 安全

| 问题 | 结论 |
|------|------|
| HMAC proof 是否绑定 node_id 防重放/串用？ | **绑定 node_id，防串用/重放成立（对外部无凭据者）。** `hub.ComputeRegisterProof(skHex, nodeID) = HMAC-SHA256(SK, "sproxy-hub-register/v1\n"+nodeID)`（`pkg/tunnel/hub/auth.go`）。验证时对传入的 `reg.NodeID` 重算并 `subtle.ConstantTimeCompare` 比对。捕获的 proof 换到别的 nodeID 会失败；proof 为静态（无 timestamp/nonce），但传输在 wss（TLS）内，捕获需 TLS 被攻破——可接受，见 Minor-6。 |
| constant-time 比较？ | **是。** AK 查找用 `subtle.ConstantTimeCompare`（`auth.go` 遍历），proof 比对用 `subtle.ConstantTimeCompare`。AK 为公开标识，遍历首中即 break 不构成密钥侧信道。 |
| fail-closed 是否完整？ | **主路径完整。** `Authenticator` accessKeys 为空 → 拒绝全部（`auth.go` `len(a.accessKeys)==0 → ErrInvalidAccessKey`）；生产 `cmd/sproxy/root.go:120` 恒传入 `NewAuthenticator(aks)`（非 nil）。唯一 open 路径是 `hub.NewHubServer(..., nil)`（`router.go:397` 注释"auth 为 nil 时不鉴权"），仅测试使用，见 Minor-7。 |
| access_key_secret 是否只在本地计算、永不上线？ | **是。** 客户端仅用于 `sproxysig.Sign` 与 `hub.ComputeRegisterProof`；`sigRoundTripper` / `AutoRegister` / `runRelayOnce` 均只把 AK + sig/proof 放上线，Secret 不上线。 |
| bodyValidator 的 (n>0, io.EOF) 是否仍有绕过？ | **修复本身正确，但存在非 EOF 读取器的系统性绕过（Important-3，为改动前遗留）。** 修复后：哈希匹配 → 返回 `(n, io.EOF)`（不丢末块）；不匹配 → 返回 `(0, err)`（即使 n>0 也不交付数据）。bodyValidator 自身总是以 EOF 或 error 终止，不会返回 `(n, nil)` 后"静默"结束——"调用方读 (n, nil) 后停止"的绕过对读到 EOF 的调用方不成立。真正的绕过是**调用方根本不读到 EOF**（JSON `json.Decoder`、multipart `ParseMultipartForm`），此时 EOF 才触发的哈希比对永不执行。此为改动前（478399d）遗留，本 diff 未关闭，见 Important-3。 |

### 2. 正确性

| 问题 | 结论 |
|------|------|
| 客户端/服务端密钥派生参数（salt/info/输出长度）是否完全一致？ | **salt、输出长度一致**（两端都调 `pkg/tunnel/tunnel.go:135 DeriveTunnelKey`：salt 固定 `"sproxy-tunnel-key-v1"`、输出 32B、info=meshID）。**唯一不一致风险在 meshID**，见 Important-1。 |
| mesh_id 解析（accessKeyMesh）与服务端配置 mesh_id 是否可能不一致？ | **可能。** 客户端 `pkg/client/client.go:204 accessKeyMesh(ak)` 从 AK 字符串 `sk[-<mesh>]-<16hex>` 提取 mesh；服务端 `pkg/server/auth.go:232` 用**配置** `access_keys[i].mesh_id`。两者无任何关联校验。见 Important-1。 |
| fail-fast 是否覆盖所有入口？ | **主入口（`cmd/sproxy`）覆盖**：`cmd/sproxy/root.go:93-96` 无 access_keys 且 api_keys 未启用 → 拒绝启动。库消费者（`pkg/server` 直连 `RegisterRoutes`）不受约束，但 authMiddleware 无配置时放行、tunnel handler 仍返回 401（fail-closed）。api_keys-only 启动但 hub/tunnel 不可用，见 Minor-8。 |

### 3. 协议一致性

- 注册帧 `access_key` / `access_key_proof` 字段语义：新增，`RegisterFrame` 保留 `token` 字段（`json:"token,omitempty"`）兼容旧客户端 JSON，服务端不再消费 token。`NewRegisterFrame(nodeID, ak, proof, meta, caps...)` 在无 meta/ak/proof/caps 时退化为裸 nodeID（兼容旧 hub）。**旧客户端/旧 hub 与新端互不兼容**（旧客户端只发 token → 新 hub fail-closed 拒绝；新客户端发 ak/proof → 旧 hub 只认 token → 拒绝）——为有意破坏性变更，需服务端/客户端同步升级，见 Minor 部署提示。
- per-node secret（B1）信令未被改动：`buildRegisterAck(info.Secret)`、`validRealNodeProof(per-node-secret, realNodeID, proof)`、disc- 防冒充校验（`router.go:294-310`）均保持。disc- 临时注册**双保险**（AK/HMAC proof + 真实节点 per-node secret proof），防冒充逻辑完整。

### 4. 回归风险

- `relay_token` 相关旧路径已全面清除：`pkg/client/config.go`（`RelayToken` 字段、`WithRelayToken`、`MeshRelayToken`）、`cmd/sclient/{relay,mesh,mesh_node,p2p}.go` 的 `--token/--relay-token` flag、`pkg/tunnel/mesh/*` 的 `RelayToken` 字段、服务端 `HubConfig.RelayToken` 均移除。全局搜索未见遗留引用。
- sclient 无 access-key 时**报错清晰**：`mesh.AutoRegister` 与 `runRelayOnce` 均显式检查 `AccessKeySecret == ""` 并返回 `"access_key_secret 为空，无法计算注册 proof"`（含测试 `TestAutoRegister_EmptySecretFailsClosed`）。

---

## 二、分级发现

### Critical
无。

### Important

#### I-1. 客户端 `accessKeyMesh` 解析与服务端配置 `mesh_id` 可能不一致，且无任何校验
- 位置：`pkg/client/client.go:204`（`accessKeyMesh`）、`pkg/server/auth.go:232`（`DeriveTunnelKey(ak.Secret, ak.MeshID)`）、`pkg/tunnel/tunnel.go:143`（HKDF `info=meshID`）
- 问题：隧道密钥派生参数中只有 `info=meshID` 在两端来源不同——客户端从 AK 字符串（`sk-<mesh>-<hex>`）解析，服务端用 YAML 配置 `access_keys[i].mesh_id`。以下任一情况都会导致两端派生出不同密钥，所有 `/tunnel` 请求解密失败（连接断开）：
  1. 运维手填 `mesh_id` 与 AK 内嵌 mesh 段不一致；
  2. **mesh 名含连字符**（如 `--mesh "prod-eu"` → AK=`sk-prod-eu-<hex>`，`strings.Split(ak, "-")` 得到 4 段，`accessKeyMesh` 返回空串，而服务端 `mesh_id: "prod-eu"`）；
  3. 客户端 AK 用 `--mesh` 生成但服务端未配 `mesh_id`（或反之）。
- 影响：fail-closed（连接失败，无数据泄露），但是一个无启动校验的配置陷阱，排查成本高。
- 建议：二选一——(a) 服务端改用与客户端相同的 `accessKeyMesh(ak.Key)` 从 AK 解析 mesh（单一事实来源，消除配置漂移）；或 (b) `Config.Validate()` 启动时校验 `accessKeys[i].mesh_id == accessKeyMesh(accessKeys[i].key)`（mesh 含连字符时提示限制）。至少补一个"mesh 不一致 → 隧道握手失败"的失败语义测试。

#### I-2. `Config.Validate()` 完全未校验 `access_keys` 条目
- 位置：`pkg/server/config.go:221-255`（`Validate`）
- 问题：`access_keys` 是本次认证体系的核心，但 Validate 未检查：Key/Secret 非空、Secret 为 64 hex（32B）、Key 唯一。配置一个 31 字节 Secret 能正常启动，直到 `/tunnel` 请求才 500（"隧道密钥派生失败"），hub 侧 `ComputeRegisterProof` 也会因 SK 长度错误而拒绝所有注册。
- 建议：在 `Validate()` 中对每个 `AccessKeyConfig` 校验：`key != ""`、`secret` 为 64 hex、`mesh_id` 与 AK 内嵌 mesh 一致（见 I-1）、Key 无重复。

#### I-3. body 哈希校验对 JSON / multipart 端点系统性失效（改动前遗留，本 diff 未关闭）
- 位置：`pkg/server/auth.go:162`（`r.Body = NopCloser(NewBodyValidator(...))`）、`pkg/sproxysig/sproxysig.go:243-260`（仅在 `io.EOF` 时比对哈希）；调用方 `pkg/server/delete_handler.go:143`、`share.go:258`、`rename_handler.go:155`、`cloud_download_handler.go:30`、`upload_handler.go:33` 等。
- 问题：`bodyValidator` 的哈希比对只在读到 `io.EOF` 时执行，而：
  - JSON 端点用 `json.NewDecoder(r.Body).Decode(&req)`——读到 JSON 值结束即返回，**不读到 EOF**；
  - 上传用 `r.ParseMultipartForm(...)`——multipart reader 在 closing boundary 处停止，**不读到 EOF**。
  因此这些端点的请求体哈希比对**永不触发**。签名校验用的是请求头里声明的 `body_sha256`（攻击者不改头、只改 body 时签名照常通过），故活动 MITM（`--insecure` / TLS 被攻破）可篡改 JSON body 而被处理（X-File-Checksum 仅兜底上传文件，JSON 端点无第二道校验）。这直接削弱"body 防篡改"设计意图。
- 说明：该问题在 auth 重构早期提交（478399d，不在本 diff 范围）引入，本 diff 只修复了 `(n>0, io.EOF)` 分支，未触及"不读到 EOF 则永不校验"。
- 建议：在每个消费 body 的 handler 在 `Decode`/`ParseMultipartForm` 之后补 `io.Copy(io.Discard, r.Body)`（强制触发 EOF 校验）；或把 bodyValidator 的校验挂在请求结束钩子；并补"篡改 JSON body 但保留签名头 → 请求被拒"的回归测试。

#### I-4. `config.example.yaml` 过期且 `make run` / 全新部署开箱即挂
- 位置：`config.example.yaml:10-12`（`tunnel_key`）、`:41`（`access_keys` 注释掉）、`:82-86`（`relay_token`）；`cmd/sproxy/root.go:93-96`（fail-fast）
- 问题：示例配置仍文档化已移除的 `tunnel_key` 与 `relay_token`，且 `access_keys` 保持注释。按新 fail-fast，用户直接复制示例配置启动会得到 "拒绝启动：未配置 access_keys（且 api_keys 未启用）"。`make run`（`build/config.yaml` 由用户从示例复制）同样受影响。升级用户（之前无认证运行）将无法启动服务。
- 建议：更新 `config.example.yaml`——提供一组可用的 `access_keys`（含 `sclient access-key create` 生成指引）、删除 `tunnel_key`/`relay_token` 注释、明确标注"必须配置 access_keys 或 api_keys 否则拒绝启动"。

### Minor

#### M-5. `sclient config` 帮助仍列出 `tunnel_key`，但 `ApplyConfigSet` 已移除该键
- 位置：`cmd/sclient/config.go:21`（Long 文案）；`pkg/client/config.go`（`ApplyConfigSet` default → "未知配置键"）
- 问题：帮助文案仍显示 `tunnel_key  隧道密钥 (64 位 hex)`，执行 `sclient config set tunnel_key X` 会报"未知配置键"。文案与实现不一致。
- 建议：从帮助文案移除 `tunnel_key` 行。

#### M-6. hub 注册 proof 为静态（无 timestamp/nonce），可重放
- 位置：`pkg/tunnel/hub/auth.go`（`ComputeRegisterProof`）
- 问题：proof = `HMAC(SK, context+"\n"+nodeID)`，无时间戳/一次性 nonce。捕获一次合法注册帧的节点，可任意时刻以同一 nodeID 重放注册（前提：能捕获帧——需 wss/TLS 被攻破或恶意 hub 运维，后者本已持有 SK）。绑定 nodeID 已排除"换 nodeID 串用"，威胁模型内可接受。
- 建议：若需更强，注册帧加 ts/nonce 并让 `Authenticate` 校验新鲜度（hub 持 SK 即可签发，无需共享额外状态）；当前可留作已知权衡。

#### M-7. `NewHubServer(rt, nil, ...)` 为"开放注册"footgun
- 位置：`pkg/tunnel/hub/router.go:397`（注释"auth 为 nil 时不鉴权"）
- 问题：open-by-default。生产路径恒传 `NewAuthenticator(...)`（fail-closed），但库/测试调用方传 nil 即开放注册。建议把 nil 视为 `NewAuthenticator(nil)`（fail-closed）或在构造时 panic，使"遗漏传 auth"不可能走向开放。

#### M-8. api_keys-only 服务可启动，但 hub 与 /tunnel 均不可用
- 位置：`cmd/sproxy/root.go:94`（fail-fast 只判 `len(AccessKeys)==0 && !APIKeys.Enabled`）、`pkg/server/handlers.go:294`（`/tunnel` 走 authMiddleware，api_keys 路径不设置派生隧道密钥 → 401）
- 问题：`api_keys.enabled=true` 但 `access_keys` 为空时服务能启动，但 hub 注册全部被拒（空 aks fail-closed），`/tunnel` 全部 401。功能死角而非安全洞。
- 建议：文档注明"hub.enabled 或隧道使用需要 access_keys"；或 fail-fast 在 `Hub.Enabled` 时要求 access_keys 非空。

#### M-9. 共享 hub 下跨 mesh 元数据可见
- 位置：`pkg/server/handlers.go`（`GET /api/hub/nodes`）、hub 注册准入
- 问题：任一合法 AK 可列出 hub 上全部节点/服务（含他 mesh）。mesh 隔离仅体现在隧道密钥层（密钥不同 → 跨 mesh 数据面解密失败 fail-closed），元数据面不隔离。为既有设计权衡，非本 diff 引入。
- 建议：如需硬隔离，`/api/hub/nodes` 与 hub 路由按 `accessKeys[i].mesh_id` 过滤。

#### M-10. `verifySproxySig` 与 `tunnelDerivedKey` 重复解析头并二次遍历 access_keys
- 位置：`pkg/server/auth.go:133-164` 与 `:225-236`
- 问题：`/tunnel` 请求验签后再次 `ParseHeader` + 遍历 `AccessKeys` 派生密钥。冗余且存在理论 TOCTOU（两次遍历之间 SIGHUP 热更 config 导致 AK 消失 → 合法请求 500），概率极低。
- 建议：`verifySproxySig` 直接返回命中的 `*AccessKeyConfig` 供后续派生，避免二次遍历。

---

## 三、已核实的正面安全属性（本轮审查确认无误）

1. **hub 注册准入**：`Authenticate(ak, proof, nodeID)` 三要素齐全，空 accessKeys fail-closed；proof 绑定 nodeID；constant-time 比较；单测覆盖"错误 proof / 未知 AK / proof 换 nodeID / 空 accessKeys / 多 AK"（`pkg/tunnel/hub/auth_test.go`）。
2. **SK 永不上线**：客户端仅本地计算签名与 proof；`sigRoundTripper` 外层 `/tunnel` 请求仅带 `Authorization` 头（`body_sha256=UNSIGNED`），body 完整性由隧道 AES-GCM + AAD 承担，签名只作认证——设计自洽。
3. **`/tunnel` 双重保护**：外层 authMiddleware 验签（fail-closed，未验签 401）+ 内层 tunnel handler `GetTunnelKey(ctx)` 为空即 401；nonce 防重放池在 `pkg/server/handlers.go:93` 全局启用。
4. **bodyValidator (n>0, io.EOF) 修复正确**：匹配 → `(n, io.EOF)` 交付末块不丢数据；不匹配 → `(0, err)` 不交付不可信数据。`io.ReadAll`/`io.Copy` 等标准读取器均正确处理 `(n, io.EOF)`。新增测试覆盖"末块+EOF 不丢"与"末块+EOF 且哈希不匹配立即报错"（`pkg/sproxysig/sproxysig_test.go`）。
5. **fail-fast**：`cmd/sproxy/root.go:93-96` 在启动监听前执行，有单测（`cmd/sproxy/root_test.go:249 TestRunServer_RejectsStartupWithoutAuth`）。
6. **disc- 临时身份防冒充**：`router.go:294-310` 的 real_node_id + per-node secret HMAC 证明校验未受影响，与 AK/HMAC 准入叠加为双保险。

---

## 四、建议修复顺序

1. **I-1 / I-2**（配置一致性 + 校验）——成本低，消除隧道"谜之失效"与坏配置运行时 500，建议立即做。
2. **I-3**（body 哈希校验失效）——补 `io.Copy(io.Discard, r.Body)` 或请求级校验钩子 + 回归测试；虽为遗留，但直接关系"body 防篡改"安全承诺。
3. **I-4**（示例配置）——文档/部署一致性，升级用户直接受益。
4. Minor 项按需处理（M-5/M-7 最低成本优先）。
