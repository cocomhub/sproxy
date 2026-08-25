# 进度账本 — 2026-08-24-tunnel-accesskey

> 计划：`docs/superpowers/plans/2026-08-24-tunnel-accesskey.md`
> 设计：`docs/superpowers/specs/2026-08-24-tunnel-accesskey-design.md`
> 分支：`feature/mesh-tunnel`

## 已完成

### 任务 1：sproxysig 双签能力单测（UNSIGNED 透传 + 哈希不匹配报错）
- commit `ac2337f`；补充 `TestBodyValidator_DataAndEOF`（`(n>0, io.EOF)` 不丢数据）于任务 6/7 提交内。

### 任务 2：pkg/tunnel context 密钥传递 SetTunnelKey/GetTunnelKey
- commit `e0533fe`（`pkg/tunnel/context_key.go`）。

### 任务 3：pkg/tunnel DeriveTunnelKey（HKDF）
- commit `4f716c0`；后补 32 字节 SK 长度校验（commit `269b669`）。

### 任务 4：authMiddleware 对 /tunnel 验签 + 派生密钥进 ctx
- commit `abb4107`（`pkg/server/auth.go` tunnelDerivedKey）。

### 任务 5：RegisterRoutes 挂载 /tunnel + handler 从 ctx 取密钥
- commit `dfe40cd`（`pkg/tunnel/handler_client.go` 废除 primaryKey/oldKey，`pkg/server/handlers.go` 挂 authMiddleware + ctx 驱动）。
- 测试迁移：`TestUpdateKey` 改认证驱动语义、`TestTunnelHandler_ReturnsHandler` 注入 ctx 密钥、`TestTunnelInnerRequest_InheritsClientTraceID` 改派生密钥 + UNSIGNED 签名。

### 任务 6：废除 tunnel_key + 无 access_keys fail-fast
- commit `269b669`（认证驱动隧道收尾）：
  - `cmd/sproxy/root.go`：无 access_keys（且 api_keys 未启用）启动拒绝；删除 tunnel_key 解析/SIGHUP UpdateKey；监听后写回实际地址到 cfgPtr（`:0` 测试友好）。
  - `pkg/server/config.go`：Validate 允许无 auth（fail-fast 在 cmd 侧），移除空分支。
  - `cmd/sproxy/root_test.go`：AuthWarning → `TestRunServer_RejectsStartupWithoutAuth`；Signal 测试补 access_keys；删除 resolveTunnelKey 相关测试。
- 修复 `pkg/sproxysig/bodyValidator`：`(n>0, io.EOF)` 时丢弃末次数据的 io.Reader 约定违反 → 并发 chunked multipart 上传偶发 413。

### 任务 7：sclient WithTunnel(ak, sk) + 删 tunnel_key 配置
- commit `269b669`：
  - `pkg/client/client.go`：`WithTunnel(ak, sk)` HKDF 派生 + `sigRoundTripper`（UNSIGNED 签名）；`accessKeyMesh` 从 AK 提取 mesh。
  - `pkg/client/config.go`：删 TunnelKey 字段/配置键；`config_extra_test.go` 删 set_tunnel_key 用例。
  - `cmd/sclient/internal/clientfactory/factory.go`：`WithTunnel(cfg.AccessKey, cfg.AccessKeySecret)`。

## 已完成

### 任务 8：hub 节点注册改 AK/HMAC 准入（废除 relay_token）
- commit `36b757c`：`ComputeRegisterProof = HMAC-SHA256(SK, "sproxy-hub-register/v1\n"+nodeID)`；
  `NewAuthenticator([]hub.AccessKey)`；relay_token 在 pkg/cmd 全废除，per-node secret 信令保留。
- 子代理实现（简报 `2026-08-24-tunnel-accesskey-task8-brief.md`，报告 `...-task8-report.md`）。

### 任务 9：E2E 验证 + 最终审查
- commit `047834d`：17 个基础 E2E 迁移 access_keys（startSPROXY 配 access_keys + signingTransport
  自动签名）；新增 `test/e2e_tunnel_accesskey_test.go` 纯隧道 upload/list E2E（无 tunnel_key）；
  bodyValidator `(n>0, io.EOF)` 哈希即时校验（采纳安全审查）。
- commit `7411311`（最终审查修复 I-1..I-4, M-5/M-7）：`tunnel.AccessKeyMesh` 单一 mesh 解析
  （支持连字符）、`Config.Validate` 校验 access_keys、authMiddleware drain body 触发 EOF 校验、
  config.example.yaml 更新、NewHubServer(nil) fail-closed。
- 最终审查：`2026-08-24-tunnel-accesskey-finalreview.md`——0 Critical，修复 4 Important + 2 Minor。

## 审查修复（全部完成）

### commit be14e98 — M-6/M-8/M-10 + I-3
- M-6 hub 注册 proof 加 ts+nonce（v2 协议防重放）：proof = HMAC(SK, "sproxy-hub-register/v2\n"+nodeID+"\n"+ts+"\n"+nonce)；ts 新鲜度 + nonce 池（复用 sproxysig.NoncePool）。
- M-8 api_keys 保留 + 边界：hub.enabled 时强制 access_keys（条件 fail-fast）。
- M-10 verifySproxySig 返回命中 AK（消除二次遍历/TOCTOU）。
- I-3 body 防篡改响应前拒绝：drainAndVerifyBody + 15 个 JSON 端点 + 2 个 multipart 端点补 EOF 校验；回归测试 TestSproxySig_BodyTamperRejected。

### commit 03d7ba5 — M-9 mesh 独立分表
- 新增 MeshRouteTable 聚合层（每 mesh 独立 RouteTable + nodeMesh 映射）；NodeInfo.Mesh；
  registerNode 按 AK 解析 mesh 分表注册；/api/hub/nodes、stats、services 按请求 AK mesh 过滤；
  relay_stream 跨 mesh 转发 404；信令跨 mesh 403；metrics 按 ctx mesh。
- 跨 mesh 隔离测试全套通过（列表/转发/服务发现/信令）。

## 已知权衡（审查后仍搁置，均非缺陷）
- ~~M-6/M-8/M-9/M-10/I-3 均已修复~~（原搁置项全部落实）。
- 剩余：hub proof 重放需 wss/TLS 被攻破（M-6 已加 ts/nonce 防重放）；api_keys-only 文件面保留（不走隧道/hub，已文档化）。

---

## 验证记录
- 主仓 + ext 子 module 全部测试通过；主仓 golangci-lint 0 issues。
- commit `269b669` pre-commit 钩子（build/vet/gofmt/check-loopback/lint）全绿。
