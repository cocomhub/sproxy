# 批次 5 审查：跨墙可用性

> 本批 3 路并发对抗审查：R5.1 传输+发现+选路 / R5.2 加密与身份 / R5.3 出口形态。
> 基线：master `e428acbe`。产出文件 `05-crosswall-*.md`。

## 审查目标功能（roadmap 5.1）

- **R5.1 可用性**：多传输层可插拔（TCP/WS/QUIC/gRPC/WebRTC，`xfer.Register`）；hub 中继
  （WS 挂主 HTTP 端口）穿透 NAT；WebRTC 打洞（STUN/TURN + ICE 凭据缓存单飞续期）+ hub 信令；
  mDNS/DHT 发现；mesh 多路径自动选路（SmartDial：直连 3s 超时回退出口，race 并行竞速）。
- **R5.2 加密与身份**：AES-256-GCM 隧道 + ECDH(X25519) 会话密钥（前向保密）+ 公开指纹派生
  静态密钥防降级 + Ed25519 身份双向指纹 pinning（`sclient identity`/`--peer-pins`）；端到端加密
  字节流（L⇄T 应用层 E2E，X 只透传密文）；mTLS 客户端证书。
- **R5.3 出口形态**：正向 HTTP 代理（http-proxy，http_proxy 环境变量开箱即用）；via-relay/via-direct
  多跳；TURN REST 短期凭证（#141，REST 优先静态、日志脱敏）；云端下载经 mesh 出口（#395，
  cloud_download_exit_node，本地直连优先→失败回退出口，fail-closed）；服务端进程内 mesh 节点
  （mesh.node，B 侧角色）。

## 关键文件

- R5.1：pkg/tunnel/xfer/*.go（xfer.Register + 各传输）、pkg/tunnel/hub/*.go（中继）、
  pkg/tunnel/p2p/*.go（WebRTC/STUN/TURN）、pkg/tunnel/dht/*.go、pkg/tunnel/mdns*.go、
  pkg/tunnel/mesh/*.go（SmartDial）
- R5.2：pkg/tunnel/tunnel.go（AES-GCM）、pkg/tunnel/endtoend*.go（ECDH/E2E）、
  pkg/sproxysig/*.go（Ed25519 pinning）、pkg/tunnel/identity*.go
- R5.3：pkg/httpproxy/*.go、pkg/server/cloud_exit*.go（#395）、pkg/tunnel/mesh/*.go（RunNode）、
  cmd/sclient/http_proxy*.go、cmd/sclient/turn*.go

## 审查维度与关注点

### 正确性
- SmartDial 竞速结果一致性（胜出连接归属、败者资源释放）；WebRTC ICE 状态机
- TURN REST 凭证 HMAC 计算与过期；E2E 握手帧序
- 云出口回退路径（本地直连失败后出口是否真正接管、超时）

### 安全性（本批重点）
- ECDH 会话密钥派生正确性（前向保密）；staticKey 绝不来自 SK（红线）
- pinning 校验点覆盖（双向指纹）；mTLS 证书链校验
- TURN REST 日志脱敏（凭证不落日志）；http-proxy 的 CONNECT 目标校验
- 出口拨号策略（私网/loopback 拒绝、DNS 重绑定防绕）

### 可用性 / 可维护性
- 传输降级路径（ws→quic→tcp 失败时行为）；错误信息
- 发现协议超时/节流；测试覆盖（xfertest 套件、e2e）

## 输出格式

每路审查产出 `05-crosswall-<名字>.md`，按 `README.md` 模板。证据 + 分级 P0-P3 + 通过项。
