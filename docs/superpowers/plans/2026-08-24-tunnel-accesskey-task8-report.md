# 任务 8 报告：hub 节点注册改 AK/HMAC 准入（废除 relay_token）

## 状态

**DONE**（无 BLOCKED；未 commit，按约定由主流程统一提交）

## 目标完成情况

hub 节点注册（WebSocket `/ws`）准入已从 relay_token 明文对比改为 **SproxySig
AccessKey + HMAC proof**（`ComputeRegisterProof = HMAC-SHA256(SK, "sproxy-hub-register/v1\n"+nodeID)`）。
`relay_token` 已在 pkg/ 与 cmd/ 全部废除（0 残留）。per-node secret 信令认证完全保留。

## 改动文件清单

### hub 包（核心准入改造）
- `pkg/tunnel/hub/auth.go` — 重写：删除 relayToken/ErrInvalidToken；新增 `AccessKey`、
  `ErrInvalidAccessKey`、`ErrInvalidAccessKeyProof`、`RegisterProofV1Context`、
  `ComputeRegisterProof`、`NewAuthenticator([]AccessKey)`、`Authenticate(ak, proof, nodeID)`。
- `pkg/tunnel/hub/router.go` — `RegisterFrame` 新增 `access_key`/`access_key_proof` 字段
  （`token` 保留但标注废弃）；`NewRegisterFrame(nodeID, ak, proof, meta, caps...)` 改签名；
  `RegisterConn` 准入改调 `s.auth.Authenticate(reg.AccessKey, reg.AccessKeyProof, reg.NodeID)`。

### mesh 包
- `pkg/tunnel/mesh/mesh.go` — `AutoRegisterParams` 删除 `RelayToken`；`AutoRegister` 用
  `hub.ComputeRegisterProof(p.AccessKeySecret, nodeID)` 计算 proof，`AccessKeySecret==""`
  时 fail-closed 报错 `"register: access_key_secret 为空，无法计算注册 proof"`。
- `pkg/tunnel/mesh/node.go` — `NodeConfig` 删除 `RelayToken`；`runNodeOnce` 移除赋值。
- `pkg/tunnel/mesh/discovery.go` — `dialPeer` 移除 `RelayToken` 字段。

### cmd/sclient
- `cmd/sclient/relay.go` — 移除 `--token` flag 与 token 参数；改传 access-key/secret，
  `runRelayOnce` 用 `hub.ComputeRegisterProof` 计算 proof；移除明文 ws+token 告警。
- `cmd/sclient/mesh.go` — 移除 `--token`/`--relay-token` flag 与 `MeshRelayToken` 调用。
- `cmd/sclient/mesh_node.go` — 移除 `--token`/`--relay-token` flag 与 `RelayToken` 配置。
- `cmd/sclient/p2p.go` — `p2pFlags` 移除 tok/relayTok 字段、`--token`/`--relay-token` flag
  与 `relayToken()` 方法；注册改走 access-key/secret。
- `cmd/sclient/config.go` — 帮助文案移除 relay_token。

### cmd/sproxy + pkg/server
- `cmd/sproxy/root.go` — `hub.NewAuthenticator` 改为把 `cfg.AccessKeys` 转换为
  `[]hub.AccessKey{Key, Secret}` 传入（不再用 `cfg.Hub.RelayToken`）。
- `pkg/server/config.go` — `HubConfig.RelayToken` 字段删除；`hub.enabled` 的
  relay_token 非空校验删除。

### pkg/client
- `pkg/client/config.go` — `Config.RelayToken` 字段、`HandleConfigShow` 显示、
  `ApplyConfigSet("relay_token")` 支持全部删除。
- `pkg/client/client.go` — `relayToken` 字段、`WithRelayToken`、`RelayToken()` 方法删除。
- `pkg/client/mesh_refresh.go` — `MeshRelayToken` 函数删除。

### 注释清理
- `pkg/server/signaling_handlers.go`、`pkg/server/handlers.go` — relay_token 相关注释改写。

### 测试更新
- `pkg/tunnel/hub/auth_test.go` — 重写为 AK/proof 正/负例 + fail-closed + 多 AK 匹配。
- `pkg/tunnel/hub/router_test.go` — 全部 `NewAuthenticator`/注册帧改 AK/proof。
- `pkg/tunnel/mesh/mesh_test.go` — 全部 `NewAuthenticator`/`AutoRegisterParams`/`NodeConfig`
  改 AK/SK（SK 用 64 hex 合法值）；网关 token 同步；新增 `TestAutoRegister_EmptySecretFailsClosed`。
- `cmd/sclient/relay_test.go`、`mesh_test.go`、`p2p_test.go` — 移除 token/relay-token flag 断言；
  `runRelayWithRetry` 签名更新。
- `pkg/client/config_test.go`、`mesh_refresh_test.go` — 移除 relay_token 断言。
- `pkg/server/config_test.go` — relay_token 校验测试改为「无需 token 即通过」。
- `test/e2e_relay_test.go`、`test/e2e_mesh_node_test.go` — E2E 迁移：sproxy 配置 `access_keys`，
  relay/mesh node 用 `--access-key/--access-key-secret`，hub API 轮询与 `/api/relay/stream`
  原始 socket 请求加 SproxySig 签名。

## 验证结果

### 受影响测试（全部通过）
```
go test -count=1 ./pkg/tunnel/hub/...      → ok
go test -count=1 ./pkg/tunnel/mesh/...     → ok
go test -count=1 ./pkg/client/...          → ok
go test -count=1 ./pkg/server/...          → ok
go test -count=1 ./cmd/sclient/...         → ok
go test -count=1 -race ./pkg/tunnel/hub/...  → ok
go test -count=1 -race ./pkg/tunnel/mesh/... → ok
go test -count=1 ./test/ (relay/mesh-node e2e) → ok
```

### 构建 + Lint
```
go build ./...                 → 通过
golangci-lint run ./pkg/... ./cmd/...  → 0 issues
```

### grep relay_token 结果
```
grep -rn "relay_token\|RelayToken" pkg/ cmd/  → 0 命中
grep -rn "relay-token\|relayToken\|MeshRelayToken\|WithRelayToken" pkg/ cmd/  → 0 命中
```
注：全仓 grep 仅剩 `.claude/worktrees/*`（其他会话的独立 worktree，非本次改动）与
`test/e2e_relay_test.go` 一条说明性注释（test/ 不在完成标准 grep 范围）。

## 疑虑

1. **基础文件服务 E2E 测试（test/ 包）在 HEAD 上已存在失败**：`TestE2E_UploadDelete`、
   `TestE2E_HealthEndpoint` 等 17 个文件服务 e2e 测试在 **改动前（HEAD commit）即失败**——
   sproxy 从任务 6/7 起要求配置 `access_keys`（fail-fast），但 `test/e2e_test.go:startSPROXY`
   仍未配置。已用 `git stash` 验证 HEAD 上 `TestE2E_HealthEndpoint` 同样失败。**非本次任务引入**，
   属于前序任务遗留（任务 8 文件清单不含 `test/e2e_test.go`）。本次任务涉及的
   relay/mesh-node E2E 测试全部通过。
2. **`pkg/sproxysig/sproxysig.go` / `sproxysig_test.go` 有未提交的前序改动**（bodyValidator.Read
   的哈希校验时序修复），继承自进入任务前的工作区状态，非本次任务修改，保留原样。
3. **`MeshSignalToken` 函数保留未删**：仅剩单测引用，属 auth_token 时代的遗留导出函数，
   不含 RelayToken，不在本任务废除范围。
