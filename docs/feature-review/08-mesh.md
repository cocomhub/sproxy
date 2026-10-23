# 审查：Mesh 组网（虚拟 IP + 发现 + 多跳与安全 + 联邦）

- **批次**：8
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 1

## 发现清单

### [P3] 联邦 peer 无凭据时裸请求（无认证调试 hub 语义需文档化）
- **位置**：`pkg/tunnel/hub/federation.go:279-289`（syncPeer 认证）
- **问题**：peer 未配置 AccessKeySecret 时**不签名裸请求**（注释「目标 hub 为无认证调试模式」）——配置了 SK 但缺 AK 则 fail-closed 报错；完全无凭据才允许不签名。语义正确（凭据缺失即信任调试网络），但文档未显式说明「联邦 peer 无凭据 = 信任无认证网络」的安全边界。
- **建议**：federation.md 补「peer 凭据与信任模型」说明。

## 通过项（无问题面）

### 虚拟 IP（hub 权威分配 + 防注入）
- **注册认证**（`auth.go:79-130`）：AK 存活校验 + `|now−ts| > registerProofMaxAge` 防重放 + nonce 池防重放 + `ComputeRegisterProof(secret, nodeID, ts, nonce)` **constant-time 比对**——节点身份 proof of possession，**防伪造节点注册**。
- **VIP 分配**（`router.go:340-385`）：`allocator.Alloc(mesh, nodeID)` 按 mesh 分配 CGNAT 子网 VIP；瞬态临时节点（disc-/mesh-/p2p- 拨号身份）**跳过 VIP 分配**（防 vipTable 幽灵映射）；分配失败不阻断注册（VIP 是增强寻址非前提）。
- **disc 临时节点防冒充**：`RealNodeID` 匹配 + `RealNodeProof` 验证（per-node secret 证明）——防 `disc-` 身份冒充真实节点。
- **DHT 喂入过滤**：只喂稳定真实节点（瞬态身份不污染发现表/k-bucket）。

### 出口拨号策略（SSRF 防护，见批次 4）
- `ipAllowed`（loopback/私网/链路本地/组播拒绝）+ `NewDialPolicy` 多 IP 任一拒绝 + `NewVirtualIPDialPolicy`（==selfVIP + 端口白名单 + 本机服务宣告才进白名单）——**VIP 地址劫持/SSRF 防绕**。

### 多跳与安全
- via-relay/via-direct 多跳；E2E 加密字节流（X 只透传密文，见批次 5）；Ed25519 pinning。

### 多 hub 联邦（federation.go）
- **认证**：SproxySig 签名（v2 skey-id 必传；配置 SK 缺 AK → fail-closed）；**body 限流**（`maxFederationResponseBytes` 防对端异常放大响应撑爆内存）；空 ID 节点丢弃。
- **去重**：Candidates 跨 peer 按 (mesh, node-id) 结构化 key 去重（防字符串拼接碰撞）。
- **持久化**：候选落盘（异步去抖）。
- **证书池**：`loadCertPool`（TLS peer 可选）。

### 出口应用形态 + P2P
- SOCKS5 出口代理 + UDP 端口映射 + HTTP 代理 + TCP 端口转发 + `p2p --manual` 手工 SDP 信令（无 hub 兜底）。

### 测试
- `router_test.go`（VIP/注册认证）+ `auth_test.go` + `federation_test.go` + e2e；`go test` 全绿。

## 验证方式

- 源码逐路径审查（注册认证 + VIP 分配 + 联邦认证/限流 + 拨号策略）
- `go test -count=1 -timeout 180s -run 'TestHub|TestVIP|TestFederation|TestAuth' ./pkg/tunnel/hub/...` → **ok**
