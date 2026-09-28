# 批次 8 审查：Mesh 组网

> 本批 3 路并发对抗审查：R8.1 虚拟 IP+发现 / R8.2 多跳与安全 / R8.3 联邦+出口形态。
> 基线：master `e428acbe`。产出文件 `08-mesh-*.md`。

## 审查目标功能（roadmap 8.1）

- **R8.1 虚拟 IP + 发现与组网**：hub 权威分配 CGNAT 子网（hub.virtual_subnet）+ VipTable 防注入 +
  REG_OK 下发 VIP + 出口 NAT + 端口白名单；sclient `mesh connect` 支持 VIP 寻址；mDNS/DHT 发现、
  WebRTC 打洞（STUN/TURN）、hub 中继、SmartDial 竞速（直连超时回退出口 + 质量加权）。
- **R8.2 多跳与安全 + E2E 加密**：via-relay/via-direct 多跳、端到端加密字节流（X 只透传密文）、
  Ed25519 指纹 pinning。
- **R8.3 联邦卷 + 多 hub 联邦 + 出口形态 + P2P 打洞**：
  - 联邦卷：mesh 载体只读挂载远端卷（federated 后端）。
  - 多 hub 联邦：FederationClient（跨 hub 节点/路由表交换，peer 周期同步，可持久化）。
  - 出口形态：SOCKS5 出口代理（sclient socks --exit）、UDP 端口映射（udp map --exit --remote）、
    正向 HTTP 代理（http-proxy）、TCP 端口转发（mesh connect/relay）。
  - P2P 手动打洞：sclient p2p --manual 手工 SDP 信令（无 hub 兜底）。

## 关键文件

- R8.1：pkg/tunnel/hub/*.go（VipTable/virtual_subnet/REG_OK）、pkg/tunnel/mesh/*.go（mesh connect）、
  pkg/tunnel/p2p/*.go、pkg/tunnel/dht/*.go、pkg/tunnel/mdns*.go
- R8.2：pkg/tunnel/relay*.go（via-relay/via-direct）、pkg/tunnel/endtoend*.go（E2E 字节流）、
  pkg/tunnel/identity*.go（Ed25519 pinning）
- R8.3：pkg/volume/federated/*.go、pkg/tunnel/hub/federation.go（FederationClient）、
  pkg/socks5/*.go、cmd/sclient/socks.go、cmd/sclient/udp*.go、cmd/sclient/p2p*.go、
  pkg/httpproxy/*.go

## 审查维度与关注点

### 正确性
- VIP 分配一致性（并发注册、释放复用、端口白名单强制）；NAT 出口
- 多跳帧序（via-direct 首帧双读修复是否完整）；E2E 会话密钥生命周期
- 联邦路由交换一致性（节点去重、环路防重）；federated 卷只读语义

### 安全性（本批重点）
- VIP 防注入（VipTable 是否可被客户端伪造）；出口 NAT 源地址伪造
- E2E 红线：staticKey 绝不来自 SK；X 是否真不见明文（透传密文验证）
- pinning 校验点全覆盖（双向）；SOCKS5 CONNECT 目标校验（私网拦截）
- 联邦 peer 认证（跨 hub 节点能否伪造身份）；P2P 手动信令（无 hub 时信任模型）

### 可用性 / 可维护性
- 发现协议超时/节流；多 hub 故障降级
- 测试覆盖（e2e、xfertest、联邦 e2e）

## 输出格式

每路审查产出 `08-mesh-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
