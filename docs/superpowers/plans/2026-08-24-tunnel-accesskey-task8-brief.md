# 任务 8 简报：hub 节点注册改 AK/HMAC 准入（废除 relay_token）

> 这是你的需求，包含要逐字使用的精确取值。不要读整个计划/设计文档，先读本简报。
> 项目根：`D:\workdir\leon\cocomhub\sproxy`（Windows，Git Bash 可用）。

## 目标

hub 节点注册（WebSocket `/ws`）的准入从 **relay_token 明文对比** 改为 **SproxySig AccessKey 签名准入**：
节点注册帧携带 `AccessKey` + `AccessKeyProof`（HMAC-SHA256 证明持有 SK）。**废除 `relay_token`**。
per-node secret（`CapabilityPerNodeSecret` + REG_OK 下发 + 信令 `X-Node-Secret` 校验）**完全保留**，不做改动。

## 全局约束（必须遵守）

- 所有源码/测试 UTF-8 无 BOM；新文件带 SPDX 头（`// Copyright 2026 The Cocomhub Authors. All rights reserved.` / `// SPDX-License-Identifier: Apache-2.0`）。
- 测试纯标准库（无 testify/gomock）；HTTP/WS 服务监听 `127.0.0.1`；Windows 兼容。
- **lint 0 容忍**：`golangci-lint run ./pkg/... ./cmd/...` 必须 0 issues。
- 中文注释/日志/错误信息，保留协议字段/包名原文。

## 设计决策（逐字采用，勿改）

### HMAC 证明
```go
// 常量（放 pkg/tunnel/hub/auth.go）
const RegisterProofV1Context = "sproxy-hub-register/v1"

// 计算（hub 包导出，客户端双端共用）
// skHex: 64 hex 字符的 SproxySig AccessKeySecret
// nodeID: 注册帧的 NodeID（绑定 node_id，防串用/重放）
// 返回 64 hex 字符
func ComputeRegisterProof(skHex, nodeID string) (string, error) {
	sk, err := hex.DecodeString(skHex)
	if err != nil { return "", fmt.Errorf("compute register proof: %w", err) }
	if len(sk) != 32 { return "", fmt.Errorf("compute register proof: sk must be 32 bytes (64 hex chars)") }
	mac := hmac.New(sha256.New, sk)
	fmt.Fprintf(mac, "%s\n%s", RegisterProofV1Context, nodeID)
	return hex.EncodeToString(mac.Sum(nil)), nil
}
```

### 注册帧（`pkg/tunnel/hub/router.go` RegisterFrame）
```go
type RegisterFrame struct {
	NodeID         string   `json:"node_id"`
	Token          string   `json:"token,omitempty"` // 废弃：不再用于准入（保留字段避免破坏旧客户端 JSON）
	AccessKey      string   `json:"access_key,omitempty"`        // 新增：SproxySig AK
	AccessKeyProof string   `json:"access_key_proof,omitempty"`  // 新增：ComputeRegisterProof 输出
	Meta           Meta     `json:"meta"`
	Capabilities   []string `json:"capabilities,omitempty"`
}
```

### `NewRegisterFrame` 签名（`pkg/tunnel/hub/router.go`）
```go
// 旧：NewRegisterFrame(nodeID, token string, meta Meta, caps ...string) []byte
// 新：NewRegisterFrame(nodeID, ak, proof string, meta Meta, caps ...string) []byte
// 裸 nodeID 退化条件不变（meta 全空 && ak=="" && proof=="" && len(caps)==0 → []byte(nodeID)）
```

### `Authenticator`（`pkg/tunnel/hub/auth.go` 整体重写）
```go
// AccessKey 是 hub 准入用的 SproxySig 凭据（hub 包自建，勿 import pkg/server）。
type AccessKey struct {
	Key    string
	Secret string
}

// Authenticator 验证节点注册的 SproxySig AccessKey + HMAC proof。
// fail-closed：accessKeys 为空时拒绝所有注册。
type Authenticator struct {
	accessKeys []AccessKey
}

var ErrInvalidAccessKey = errors.New("invalid access key")
var ErrInvalidAccessKeyProof = errors.New("invalid access key proof")

func NewAuthenticator(accessKeys []AccessKey) *Authenticator
// Authenticate(ak, proof, nodeID string) error：
//   1. 空 accessKeys → ErrInvalidAccessKey（fail-closed）
//   2. 遍历按 Key constant-time 匹配；未命中 → ErrInvalidAccessKey
//   3. 命中 → 用 Secret 重算 ComputeRegisterProof(secret, nodeID)，与 proof constant-time 比对；
//      不匹配 → ErrInvalidAccessKeyProof；匹配 → nil
```

### `HubServer.RegisterConn` 准入（`pkg/tunnel/hub/router.go` ~471 行）
```go
if s.auth != nil {
	if err := s.auth.Authenticate(reg.AccessKey, reg.AccessKeyProof, reg.NodeID); err != nil {
		s.logger.Warn("中继节点鉴权失败", "node", reg.NodeID, "error", err)
		_ = sendRegErr("invalid access key")
		return err
	}
}
// reg.AccessKey == "" 时 Authenticate 会因未命中 accessKeys 返回 ErrInvalidAccessKey（fail-closed），
// 无需额外判断。
```

## 文件清单

### 1. `pkg/tunnel/hub/auth.go` — 重写
- 导入：`crypto/hmac`、`crypto/sha256`、`crypto/subtle`、`encoding/hex`、`errors`、`fmt`。
- 删除旧 `relayToken` 逻辑、`ErrInvalidToken`。
- 新增 `AccessKey`、`ErrInvalidAccessKey`、`ErrInvalidAccessKeyProof`、`ComputeRegisterProof`、`RegisterProofV1Context`、`NewAuthenticator([]AccessKey)`、`Authenticate(ak, proof, nodeID)`。

### 2. `pkg/tunnel/hub/router.go`
- `RegisterFrame` 加 `AccessKey`/`AccessKeyProof` 字段（保留 `Token` 字段，加注释"废弃"）。
- `NewRegisterFrame(nodeID, ak, proof string, meta Meta, caps ...string)` 改签名；退化条件更新。
- `RegisterConn` 的 `s.auth.Authenticate(...)` 调用点更新（见上）。
- 检查是否有 `hub.NewAuthenticator` 的其他调用方需要同步（grep `NewAuthenticator(` 全仓）。

### 3. `pkg/tunnel/hub/*_test.go` — 更新认证测试
- 现有 `NewAuthenticator(relayToken)` 的测试全部改为 `NewAuthenticator([]hub.AccessKey{{Key, Secret}})`。
- `Authenticate(token)` → `Authenticate(ak, proof, nodeID)`。
- 保留 fail-closed 测试（空 accessKeys 拒绝）。
- 新增：正确 AK/SK 注册成功、错误 proof 拒绝、未知 AK 拒绝。
- 用 `ComputeRegisterProof` 构造合法 proof。

### 4. `pkg/tunnel/mesh/mesh.go`
- `AutoRegisterParams`：**删除** `RelayToken` 字段（`AccessKey`/`AccessKeySecret` 已存在，保留）。
- `AutoRegister` 内注册帧构造（~246 行）：
  ```go
  // 旧：hub.NewRegisterFrame(nodeID, p.RelayToken, hub.Meta{...}, hub.CapabilityPerNodeSecret)
  // 新：先 ComputeRegisterProof(p.AccessKeySecret, nodeID)；再
  //     hub.NewRegisterFrame(nodeID, p.AccessKey, proof, hub.Meta{...}, hub.CapabilityPerNodeSecret)
  ```
- `RelayToken` 为空时 proof 为空字符串？—— 不。**fail-closed**：`AccessKeySecret == ""` 时返回错误
  `"register: access_key_secret 为空，无法计算注册 proof"`（防止无凭据注册被 hub 静默拒绝后客户端困惑）。

### 5. `pkg/tunnel/mesh/node.go` + `pkg/tunnel/mesh/discovery.go`
- `MeshNodeConfig.RelayToken` 字段删除（node.go:41-42）。
- 所有 `RelayToken:` 赋值点移除；`runNodeOnce` 等用 `AccessKeySecret` 派生 proof 的地方改用 `ComputeRegisterProof`。
- `AutoRegisterParams{... RelayToken: ...}` → 传 `AccessKey`/`AccessKeySecret`（已有）。

### 6. `cmd/sclient/relay.go`、`mesh.go`、`mesh_node.go`、`p2p.go`
- 移除 `--token` / `--relay-token` flag（若存在）；`relayToken` 变量删除。
- 注册/拨号处改为传 `cfg.AccessKey` / `cfg.AccessKeySecret`（sclient 配置已有 `access_key`/`access_key_secret`）。
- **flag 回落**：`--access-key`/`--access-key-secret` 已存在（其他命令用），无需新增 flag，只用现有配置。
- 更新命令行帮助文案（`--token` → 说明用 `--access-key`/`--access-key-secret`）。

### 7. `cmd/sclient/config.go`
- `RelayToken` 配置项：sclient Config 的 `RelayToken` 字段（如果存在）删除或标注废弃。检查 `relay_token` 是否仍被 mesh 读取（CLAUDE.md 说 `relay_token` 是通用 mesh 配置键）——**本次删除**，相关 `config set relay_token` 支持移除。

### 8. `cmd/sproxy/root.go` + `pkg/server/config.go`
- `pkg/server/config.go`：`HubConfig.RelayToken` 字段删除；`c.Hub.Enabled && c.Hub.RelayToken == ""` 校验（~248 行）删除（hub 准入改由顶层 access_keys 提供）。
- `cmd/sproxy/root.go` ~114 行：
  ```go
  // 旧：hub.NewAuthenticator(cfg.Hub.RelayToken)
  // 新：把 cfg.AccessKeys 转换为 []hub.AccessKey{Key, Secret}，再 hub.NewAuthenticator(aks)
  ```
- `pkg/server/config.go` 的 HubConfig 中若 RelayToken 在别处校验/引用，一并清理。grep `RelayToken` 全仓确认无残留。

### 9. `pkg/server/signaling_handlers.go`
- 若引用 `RelayToken`（`rt` 校验信令时）——检查：信令认证用 per-node secret（`X-Node-Secret`），**不改**。仅确认没有 relay_token 残留（grep `relay_token`/`RelayToken`）。有则清理。

### 10. `pkg/client/config.go`（sclient 的 client 包配置）
- 若 `Config` 有 `RelayToken` 字段，删除（grep 确认）。

### 11. 测试更新
- `pkg/tunnel/mesh/mesh_test.go`、`pkg/tunnel/webrtc_signal_e2e_test.go`、`pkg/tunnel/p2p*_test.go`、`cmd/sclient/p2p_test.go` 等所有 `RelayToken` 引用改为 AccessKey/AccessKeySecret。
- 测试里构造合法 proof：`hub.ComputeRegisterProof(skHex, nodeID)`。

## 测试要求
- 每个改动包跑 `go test -count=1 ./pkg/tunnel/hub/... ./pkg/tunnel/mesh/... ./cmd/sclient/...` 全过。
- 新增覆盖：hub Authenticate 正/负例（正确 AK、错误 proof、未知 AK、空 accessKeys fail-closed）；mesh AutoRegister 无 SK 报错。
- 最后 `go build ./...` + `golangci-lint run ./pkg/... ./cmd/...` 0 issues。

## 完成标准
- `grep -rn "relay_token\|RelayToken" pkg/ cmd/` 除 **PROGRESS/设计/计划文档** 外 0 残留（注意 `cmd/sproxy/sproxy.exe`/`sproxy.yaml` 是构建产物，忽略）。
- 全部受影响测试通过 + lint 0 issues。

## 报告
- 报告写到 `docs/superpowers/plans/2026-08-24-tunnel-accesskey-task8-report.md`。
- 回传：状态（DONE/BLOCKED）、commit hash、`go test` 一行小结、`grep relay_token` 结果、疑虑（如有）。
