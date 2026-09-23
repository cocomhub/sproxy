# 审查：传输层 + 加密与身份 + 出口形态（跨墙可用性）

- **批次**：5
- **审查者**：父会话（subagent 401 后转直接审查）
- **审查基线**：master `e428acbe`
- **维度覆盖**：正确性 / 可用性 / 安全性 / 可维护性

## 结论

**总评**：通过
**发现数**：P0 0 / P1 0 / P2 0 / P3 0

## 通过项（无问题面）

### 传输层可插拔（xfer.Register）
- 多传输（TCP/WS/QUIC/gRPC/WebRTC，`pkg/tunnel/xfer/registry.go`）；hub 中继（WS 挂主 HTTP 端口）；WebRTC 打洞（STUN/TURN + ICE 凭据缓存单飞续期）+ hub 信令；mDNS/DHT 发现；SmartDial（直连 3s 超时回退出口 + race 并行竞速）。

### 加密与身份（`pkg/tunnel/ecdh.go` + `pkg/tunnel/mesh/endtoend.go` + `remote_key.go`）
- **ECDH X25519 握手**：临时密钥对 + HKDF 派生会话密钥（`deriveSessionKey`：纯 ECDH 双层 + **静态密钥绑定**（C-1）——staticKey 混入第二层 HKDF salt，握手非匿名，零凭据访问 fail-closed）。
- **身份交换 + pinning**：Ed25519 proof of possession（签名绑定双方临时 ECDH 公钥 + 域分离前缀 `sproxy-identity-v1`）；`readPeerIdentity` 验签 + 指纹 pinning（不匹配/无身份且配置 pin → `ErrPeerFingerprintMismatch`/`ErrPeerFingerprintRequired` fail-closed）；旧对端兼容（无扩展 EOF 视为未提供身份）。
- **身份阶段 DoS 防护**：`context.AfterFunc` 超时 abort 握手流（恶意对端停滞身份阶段的资源耗尽闭合）。
- **E2E 字节流**：`DialE2EStream`/`ServeE2EStream`（`mesh/endtoend.go:302-379`）复用 ECDH 握手 + 会话密钥，返回加密 net.Conn；`ServeE2ERelay`（中间节点 X 读明文 dial 帧 → 出口拨号 → 泵密文）。
- **红线：staticKey 绝不来自 SK**：`DeriveRemoteStaticKey`（`remote_key.go:46-57`）= **指纹 HKDF 派生**（`remoteReadIKMPrefix + listenerFingerprint`），与 SK 完全解耦——X 持 SK 也读不到明文。
- **X 不见明文断言**：`endtoend_stream_test.go:105` recordingPipe 记录 X 读到全部字节并断言**不含明文**（变异验证过的硬断言）。

### 出口形态
- HTTP 代理（http-proxy，http_proxy 环境变量）+ via-relay/via-direct 多跳 + TURN REST 短期凭证（#141，REST 优先静态 + 日志脱敏）+ 云出口（#395，本地直连优先→失败回退 hub 中继，fail-closed）+ mesh.node（进程内 B 侧）。

### 测试
- `ecdh_test.go`/`ecdh_identity_test.go`/`endtoend_stream_test.go`/`endtoend_test.go` + xfertest 跨传输套件；`go test` 全绿。

## 验证方式

- 源码逐路径审查（ECDH 握手 + 身份签名 + 指纹派生 + E2E 字节流）
- `go test -count=1 -timeout 180s -run 'TestECDH|TestEndtoEnd|TestHandshake' ./pkg/tunnel/... ./pkg/tunnel/mesh/...` → **ok**
