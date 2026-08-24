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

## 已知权衡（审查搁置，非缺陷）
- M-6 hub 注册 proof 静态（无 ts/nonce）：捕获需 wss/TLS 被攻破，绑定 nodeID 已排除串用。
- M-8 api_keys-only 启动时 hub//tunnel 不可用（功能死角，非安全洞）。
- M-9 共享 hub 下跨 mesh 元数据可见（mesh 隔离仅在数据面密钥层）。
- M-10 verifySproxySig/tunnelDerivedKey 二次解析头与遍历（理论 TOCTOU，概率极低）。
- I-3 深层修复：JSON/multipart 端点在响应前拒绝篡改 body 需 handler 内校验（当前为响应后检测 + Warn 留痕）。

---

## 验证记录
- 主仓 + ext 子 module 全部测试通过；主仓 golangci-lint 0 issues。
- commit `269b669` pre-commit 钩子（build/vet/gofmt/check-loopback/lint）全绿。
