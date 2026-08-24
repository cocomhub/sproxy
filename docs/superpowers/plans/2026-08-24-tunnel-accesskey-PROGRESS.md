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

## 进行中

### 任务 8：hub 节点注册改 AK/HMAC 准入（废除 relay_token）
- 见 `2026-08-24-tunnel-accesskey-task8-brief.md`（子代理简报）。
- 关键决策：注册帧 `RegisterFrame` 加 `AccessKey`/`AccessKeyProof`；`AccessKeyProof = hex(HMAC-SHA256(SK, "sproxy-hub-register/v1\n"+nodeID))`；`NewAuthenticator([]hub.AccessKey)`；per-node secret 信令保留。

## 待办

### 任务 9：E2E 验证
- `test/e2e_tunnel_accesskey_test.go`：真实二进制 + access_keys + 纯隧道 list/upload。
- `make test-all && make lint` 全绿。

---

## 验证记录
- 主仓 + ext 子 module 全部测试通过；主仓 golangci-lint 0 issues。
- commit `269b669` pre-commit 钩子（build/vet/gofmt/check-loopback/lint）全绿。
