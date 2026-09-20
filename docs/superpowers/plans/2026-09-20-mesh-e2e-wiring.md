# mesh 端到端加密生产接线（一期：L↔T 字节流形态）实现计划

> **面向 AI 代理的工作者：** 必需子技能：使用 subagent-driven-development（推荐）或 executing-plans 逐任务实现此计划。步骤使用复选框（`- [ ]`）语法来跟踪进度。

**目标：** 把 mesh 端到端加密协议层（已落地 DialE2E/ServeE2ERelay/ServeE2EListener，HTTP 隧道形态）接线到生产数据面（L 直连 T 的 mesh connect 路径），新增字节流形态（DialE2EStream/ServeE2EStream），使 X/hub 中间节点即使持有 SK 也读不到 L↔T 明文。

**架构：** L 侧 mesh.Dial 的 RelayStream/WebRTCStream 返回的裸数据面连接包一层 E2E 字节流隧道（ECDH 握手 + AES-256-GCM 加密）；T 侧 relay.Serve（leaf.go dOK 分支）DetectE2E 读 e2e 标记帧 → ServeE2EStream 解密 → pump 到本地服务。X 中间节点（二期 via-node）用 ServeE2ERelayStream 纯字节泵（透传 e2e 标记，不见明文）。Result.EndToEnd 置位 + 日志/metrics 显式可观测（安全开关生效状态必须可观测，禁静默降级）。

**技术栈：** Go 1.26；AES-256-GCM（pkg/tunnel/stream.go EncryptChunk/DecryptChunk 复用）；ECDH X25519 + Ed25519 身份（pkg/tunnel/ecdh.go PerformHandshakeConn 新增）；mux（外层数据面）。

**规格：** 用户确认语义（2026-09-20）：
1. 版本未发布前的变更不算破坏性变更，不标 BREAKING CHANGE（提交照常 feat/fix）
2. 安全开关必须显式 pinning / 显式开关，生效状态必须可观测（日志/metrics），禁静默降级
3. 字节流形态（DialE2EStream/ServeE2EStream 复用 ECDH 握手 + 会话密钥，返回加密 net.Conn）
4. 无兼容包袱（版本未发布，最佳架构直接设计）
5. 默认启用 + 显式 pinning 防 MITM（无 pin 时 ECDH 防窃听，X 读不到明文）
6. 红线：staticKey 绝不来自 SK（X 持 SK 必须读不到明文）
7. 一期 = L 直连 T（mesh.Dial 路径）；二期 = via-node 多跳 + socks/http-proxy L→X 段

## 全局约束

- TDD：先写红灯测试再实现；测试纯标准库；测试只绑 127.0.0.1
- R14/R18：顶层 Test 默认 t.Parallel()；串行须 // sproxy:serial: 标记；time.Sleep 计数 ≤ 冻结预算（用 pkg/testutil.WaitFor）
- 禁 http.DefaultClient（测试网络客户端隔离）
- UTF-8 without BOM；中文注释/日志/错误信息
- SPDX 头（Copyright 2026 The Cocomhub Authors / Apache-2.0）
- 不标 BREAKING CHANGE（版本未发布）

### 任务 1：协议层字节流形态（DialE2EStream / ServeE2EStream）—— ✅ 完成（提交 09c26171）

**文件：** pkg/tunnel/mesh/endtoend.go（新增 DialE2EStream/ServeE2EStream/e2eStreamConn/ServeE2ERelayStream）+ pkg/tunnel/hub/router.go（DialRequest.E2E）+ pkg/tunnel/ecdh.go（PerformHandshakeConn）+ pkg/tunnel/mesh/endtoend_stream_test.go（新建）

**实现要点（已落地）：**
- DialE2EStream(ctx, outer, addr, opts)：写 e2e dial 帧（[4B len][{"dial":addr,"e2e":true}]）→ PerformHandshakeConn（ECDH + 可选身份/pin）→ 返回加密 net.Conn
- ServeE2EStream(ctx, outer, opts)：读 e2e dial 帧（校验 e2e:true fail-closed）→ 握手 → 返回解密流
- e2eStreamConn：AES-256-GCM 分块加解密（tunnel.StreamEncryptor/Decryptor 单帧 API），net.Conn 透传
- ServeE2ERelayStream(ctx, outer, dialPolicy)：X 侧字节流中继——读 dial 帧 → 出口拨号 → **透传 dial 帧** → pump 密文（二期 via-node）
- PerformHandshakeConn：裸 net.Conn 上 ECDH X25519 + 身份交换（复用 deriveSessionKey/identitySigMessage），Identity 可选（nil = 纯 ECDH）
- validatePeerFingerprintsOptional：空 pin 放行（纯 ECDH），非空校验元素（防 panic）
- 测试：往返 + X 不见明文（recordingPipe）+ pin fail-closed + Identity 可选 + 非 e2e 帧拒绝 + 空 pin fail-closed

### 任务 2：T 侧接线（leaf.go dOK 分支 DetectE2E）

**文件：**
- 修改：`pkg/tunnel/relay/leaf.go`（dOK 分支：e2e:true → 出口拨号 → ServeE2EStream → pump 解密流）
- 修改：`pkg/tunnel/relay/leaf.go` ServeOptions 加 E2EServe 函数注入（避免 relay→mesh 包级环；relay 只依赖 tunnel 包）
- 测试：`pkg/tunnel/relay/leaf_e2e_test.go`（新建）

**步骤：**
- [ ] 写红灯测试（e2e dial 帧 → 解密 → 服务；旧帧 → 兼容）
- [ ] 运行验证失败
- [ ] 实现 leaf.go dOK 分支 DetectE2E（E2EServe 注入）
- [ ] 运行验证通过
- [ ] Commit

### 任务 3：L 侧接线（mesh.Dial 分支 E2E 包层）

**文件：**
- 修改：`pkg/tunnel/mesh/mesh.go`（Dial/DialWithOptions 的 RelayStream/WebRTCStream 分支）
- 修改：`pkg/tunnel/mesh/via_node.go`（via-relay:<X> 候选）
- 测试：`pkg/tunnel/mesh/e2e_wiring_test.go`（新建）

**步骤：**
- [ ] 写红灯测试（E2E 配置时 Dial 返回加密流 + Result.EndToEnd）
- [ ] 运行验证失败
- [ ] 实现 mesh.Dial E2E 包层
- [ ] 运行验证通过
- [ ] Commit

### 任务 4：可观测性 + 文档 + 收尾

**文件：**
- 修改：`pkg/tunnel/mesh/metrics.go`（E2E 计数/标记）
- 修改：`docs/config.md` / `docs/mesh-testing.md`（E2E 接线范围更新）

**步骤：**
- [ ] 加 E2E 可观测（日志/metrics）
- [ ] 文档更新
- [ ] 全量验证（build + 全包测试 + archcheck）
- [ ] pre-commit + 提交
- [ ] PR + CI + 合并（功能维度 squash）
