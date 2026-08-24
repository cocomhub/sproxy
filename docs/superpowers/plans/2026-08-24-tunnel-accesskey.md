# access-key 驱动 /tunnel 认证与编解码实现计划（废除 tunnel_key / relay_token）

> **面向 AI 代理的工作者：** subagent-driven-development 或 executing-plans。步骤用 `- [ ]` 语法追踪。

**目标：** 让 `access_key/access_key_secret` 成为 `/tunnel`（纯隧道）的唯一认证与编解码来源，用 HKDF 从 SK 派生隧道密钥，彻底废除 `tunnel_key` 与 `relay_token`，并给 SproxySig 双签（`body_sha256`=metadata 帧 SHA-256 + `UNSIGNED` 跳过 body 校验）。修掉「服务端配 access_keys 后纯 tunnel `sclient list` 报 400」的根因。

**架构：** `authMiddleware` 对 `POST /tunnel` 验 SproxySig（nonce + 时间 + HMAC，body 不阻塞）→ 按 AK 查 SK → `SetTunnelKey(ctx, HKDF(SK, mesh_id))` 派生 32B AES-256 隧道密钥放入请求 context；隧道 handler `Handler.ServeHTTP` 用 `GetTunnelKey(ctx)` 解密 metadata 与 body、加密响应。彻底废除服务端 `tunnel_key`（删配置字段、启动无 access_keys 即 fail-fast）；`sclient` 删除 `tunnel_key` 配置项，`WithTunnel` 从 `access_key_secret` 派生。SproxySig 双签：`body_sha256` 支持 `UNSIGNED`（已存在 `UnsignedBody` + `NewBodyValidator`）通用流式能力 + `/tunnel` 对 metadata 帧做 SHA-256。hub 节点注册从共享 `relay_token` 改为 AK/HMAC 准入证明，per-node secret 留存。

**技术栈：** Go 1.26；sproxy 纯 stdlib + `gopkg.in/yaml.v3`；`golang.org/x/crypto/hkdf`（`golang.org/x/crypto` 已在 go.mod）。

---

## 文件结构

| 动作 | 文件 | 职责 |
|------|------|------|
| 新建 | `pkg/tunnel/context_key.go` | `SetTunnelKey(ctx, []byte)` / `GetTunnelKey(ctx)` context 传递 |
| 修改 | `pkg/tunnel/handler_client.go` | `Handler` 从 ctx 取密钥解密 metadata/body、加密响应；不再依赖全局 primaryKey |
| 修改 | `pkg/tunnel/tunnel_mux.go` | `Tunnel`/`NewTunnel` 改用 caller 传入派生密钥（与 ctx 对齐） |
| 修改 | `pkg/server/auth.go` | `authMiddleware` 对 `/tunnel` 走 SproxySig 验签 + HKDF 派生密钥进 ctx；支持 `UNSIGNED`；新增 `deriveTunnelKey(sk, mesh)` |
| 修改 | `pkg/server/handlers.go` | `POST /tunnel` 单独挂 srvMux（不挂 Bearer）；`RegisterRoutes` 不再传 `TunnelKey`，隧道 handler 从 config access_keys 注入 |
| 修改 | `pkg/server/config.go` | 删 `TunnelKey` 字段；`AccessKeys` 非空校验 |
| 修改 | `cmd/sproxy/root.go` | 删 `resolveTunnelKey` / `--tunnel-key` / `UpdateKey`；启动 `len(cfg.AccessKeys)==0 → error` |
| 修改 | `pkg/client/client.go` | `WithTunnel(ak, sk)` 从 SK 派生 `c.tunnelKey`；`doRequest` 对 `/tunnel` SproxySig body_sha256=metadata 帧哈希；保留 `WithXfer` |
| 修改 | `pkg/client/config.go` | 删 `TunnelKey` 字段 |
| 修改 | `cmd/sclient/internal/clientfactory/factory.go` | `WithTunnel` 从 `access_key/access_key_secret` 派生（删除 tunnel_key 读取） |
| 修改 | `cmd/sclient/config.go` / `output.go` | 删除 tunnel_key 配置帮助/展示 |
| 修改 | `pkg/tunnel/hub/auth.go` | `Authenticator` 从共享 token 改 AK/HMAC 准入（废除 relay_token）；`NewAuthenticator(accessKeys)` |
| 修改 | `pkg/tunnel/hub/router.go` / `mesh.go` | 注册帧 no longer 带 relay_token，改为 AK/HMAC 证明 |
| 修改 | `pkg/tunnel/mesh/mesh.go` | `AutoRegister`/相关 |
| 修改 | 各类测试 | `pkg/server/*_test.go`、`pkg/tunnel/*_test.go`、`pkg/sproxysig/*_test.go`、`cmd/sproxy/root_test.go`、`cmd/sclient/*_test.go` |

---

## 任务清单

### 任务 1：`sproxysig` 双签能力单测（`UNSIGNED` 透传 + 哈希不匹配报错）

**文件：**
- 修改：`pkg/sproxysig/sproxysig.go`（`NewBodyValidator` 已支持 `UNSIGNED`；补单测）
- 测试：`pkg/sproxysig/sproxysig_test.go`

- [ ] **步骤 1：编写 UNSIGNED body 行为测试**
```go
func TestBodyValidator_Unsigned(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), UnsignedBody)
	b, err := io.ReadAll(r)
	if err != nil || string(b) != "x" {
		t.Fatalf("UNSIGNED 应透传不校验: %v %q", err, b)
	}
}
func TestBodyValidator_Mismatch(t *testing.T) {
	r := NewBodyValidator(strings.NewReader("x"), BodyHash([]byte("y")))
	if _, err := io.ReadAll(r); err == nil {
		t.Fatal("哈希不匹配应收错")
	}
}
```
- [ ] **步骤 2：运行确认**
```bash
cd /d/workdir/leon/cocomhub/sproxy && go test -count=1 -run 'TestBodyValidator_(Unsigned|Mismatch)' ./pkg/sproxysig/...
```
预期：两测试通过（`UnsignedBody`/`NewBodyValidator`/`BodyHash` 已存在）。若 `Mismatch` 报错说明 `NewBodyValidator` 已校验，工作完成。

- [ ] **步骤 3：全量 `go test ./pkg/sproxysig/...` + commit**
```bash
go test -count=1 ./pkg/sproxysig/... && git add pkg/sproxysig && git commit -m "test(sproxysig): UNSIGNED body 透传与哈希不匹配断言"
```

### 任务 2：`pkg/tunnel` context 密钥传递 `SetTunnelKey`/`GetTunnelKey`

**文件：**
- 创建：`pkg/tunnel/context_key.go`
- 测试：`pkg/tunnel/context_key_test.go`

- [ ] **步骤 1：编写失败测试**
```go
package tunnel

import "context"
import "testing"

func TestCtxKey(t *testing.T) {
	ctx := SetTunnelKey(context.Background(), []byte("k"))
	if got := GetTunnelKey(ctx); string(got) != "k" {
		t.Fatalf("GetTunnelKey = %q", got)
	}
	if GetTunnelKey(context.Background()) != nil {
		t.Fatal("背景 ctx 无密钥应为 nil")
	}
}
```
- [ ] **步骤 2：运行确认失败**（`SetTunnelKey` undefined）
- [ ] **步骤 3：实现 context_key.go**
```go
package tunnel

import "context"

type tunnelKeyCtx struct{}

func SetTunnelKey(ctx context.Context, key []byte) context.Context {
	return context.WithValue(ctx, tunnelKeyCtx{}, key)
}
func GetTunnelKey(ctx context.Context) []byte {
	if v, _ := ctx.Value(tunnelKeyCtx{}).([]byte); v != nil {
		return v
	}
	return nil
}
```
- [ ] **步骤 4：验证 + commit**
```
go test -count=1 ./pkg/tunnel/... && git add pkg/tunnel/context_key.go pkg/tunnel/context_key_test.go && git commit -m "feat(tunnel): SetTunnelKey/GetTunnelKey context 传递隧道密钥"
```

### 任务 3：`pkg/tunnel` `DeriveTunnelKey`（HKDF）

**文件：**
- 修改：`pkg/tunnel/tunnel.go`
- 测试：`pkg/tunnel/tunnel_extra_test.go`

- [ ] **步骤 1：写失败测试**
```go
func TestDeriveTunnelKey(t *testing.T) {
	sk := "2b40d5b60e6792134f07b44b46e2e19fb72f967136868015cb922d720c1aa6f5"
	k1, _ := DeriveTunnelKey(sk, "meshA")
	k2, _ := DeriveTunnelKey(sk, "meshB")
	if len(k1) != 32 || bytes.Equal(k1, k2) {
		t.Fatalf("derived key len=%d equal=%v", len(k1), bytes.Equal(k1, k2))
	}
	k1b, _ := DeriveTunnelKey(sk, "meshA")
	if !bytes.Equal(k1, k1b) {
		t.Fatal("派生必须确定")
	}
	if _, err := DeriveTunnelKey("zz", ""); err == nil {
		t.Fatal("非法 hex 应报错")
	}
}
```
- [ ] **步骤 2：确认失败**（`DeriveTunnelKey` undefined）
- [ ] **步骤 3：实现**（import `golang.org/x/crypto/hkdf`）
```go
// DeriveTunnelKey 从 SproxySig AccessKeySecret（SK，64 hex）派生 32B AES-256 隧道密钥。
// salt 固定字符串提供域分离；info=mesh_id（每 mesh 独立）。两端必须用相同参数。
func DeriveTunnelKey(skHex, meshID string) ([]byte, error) {
	secret, err := hex.DecodeString(skHex)
	if err != nil {
		return nil, fmt.Errorf("derive: invalid sk: %w", err)
	}
	r := hkdf.New(sha256.New, secret, []byte("sproxy-tunnel-key-v1"), []byte(meshID))
	out := make([]byte, 32)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("derive: %w", err)
	}
	return out, nil
}
```
- [ ] **步骤 4：验证 + commit**
```
go test -count=1 ./pkg/tunnel/... && git add pkg/tunnel/tunnel.go pkg/tunnel/tunnel_extra_test.go && git commit -m "feat(tunnel): HKDF 派生隧道密钥 DeriveTunnelKey"
```

### 任务 4：`pkg/server` authMiddleware 对 `/tunnel` 验签 + 派生密钥进 ctx

**文件：**
- 修改：`pkg/server/auth.go`
- 测试：`pkg/server/server_auth_test.go`

- [ ] **步骤 1：实现 `authTunnel` 分支**
```go
// 在 authMiddleware 内部对 r.URL.Path == "/tunnel" 时：
//   先用 h.verifySproxySig(w, r, cfg) 验签（成功才下一步）
//   再 m := AK→SK（沿用 Verify 后 cfg.AccessKeys）
//   key, err := tunnel.DeriveTunnelKey(sk, ak.MeshID); if err -> 500
//   next(w, r.WithContext(tunnel.SetTunnelKey(r.Context(), key)))
```
（传 ctx 的 `next` 签名需把 `r` 换成 `r.WithContext(...)`）
- [ ] **步骤 2：编写集成测试**
```go
// server_auth_test.go
// 1. 带 SproxySig 签名（AK/SK 与配置一致）POST /tunnel（构造 metadata 帧用派生密钥）→ 200
// 2. 不带签名 POST /tunnel → 401
// 3. 错 SK 签名 → 401
```
- [ ] **步骤 3：跑 `go test ./pkg/server/...` 全绿 + commit**

### 任务 5：`pkg/server` RegisterRoutes 挂载 `/tunnel` + handler 从 ctx 取密钥

**文件：**
- 修改：`pkg/server/handlers.go`（`RegisterRoutes` 移除 `TunnelKey`，`POST /tunnel` 挂 `h.tunnelHandler`；`h.tunnelHandler` 由 config access_keys 注入）
- 修改：`pkg/tunnel/handler_client.go`（`Handler` 用 `GetTunnelKey(ctx)` 解密）
- 测试：`pkg/server/integration_test.go`

- [ ] **步骤 1：Handler 解密改用 ctx 取密钥（行为保持）**
- [ ] **步骤 2：RegisterRoutes 改（不再传 TunnelKey）**
- [ ] **步骤 3：`go test ./pkg/server/... ./pkg/tunnel/...` + lint + commit**

### 任务 6：`pkg/server/config.go` + `cmd/sproxy/root.go` 废除 tunnel_key + 无 access_keys fail-fast

**文件：**
- 修改：`pkg/server/config.go`（删 `TunnelKey` 字段）
- 修改：`cmd/sproxy/root.go`（删 `resolveTunnelKey` / `--tunnel-key` / `UpdateKey`；启动 `len(cfg.AccessKeys)==0 → error`）
- 测试：`cmd/sproxy/root_test.go`、`pkg/server/*_test.go` 全量更新

- [ ] **步骤 1：删字段/flag/resolveTunnelKey/启动 fail-fast**
- [ ] **步骤 2：全量 `go build ./pkg/... ./cmd/...`（修引用）**
- [ ] **步骤 3：更新测试 + 全量 `go test` + lint + commit**

### 任务 7：`sclient` 侧 `WithTunnel(ak, sk)` + 删 tunnel_key 配置

**文件：**
- 修改：`pkg/client/client.go`（`WithTunnel(ak, sk)`；`doRequest` 对 `/tunnel` SproxySig=metadata 帧哈希）
- 修改：`pkg/client/config.go`（删 `TunnelKey`）
- 修改：`cmd/sclient/internal/clientfactory/factory.go`（`WithTunnel` 从 access_key/secret 派生；删除 tunnel_key 读取）
- 测试：`pkg/client/*_test.go`、`cmd/sclient/internal/clientfactory/factory_test.go`、`cmd/sclient/*_test.go`

- [ ] **步骤 1：`WithTunnel(ak, sk)` 签名变更 + HKDF 派生**
- [ ] **步骤 2：`WithXfer` 语义保持**
- [ ] **步骤 3：全量更新调用点 + 测试 + lint + commit**

### 任务 8：hub 节点注册改 AK/HMAC 准入（废除 relay_token）

**文件：**
- 修改：`pkg/tunnel/hub/auth.go`、`pkg/tunnel/hub/router.go`、`pkg/tunnel/mesh/mesh.go`、`pkg/tunnel/hub/register_ack.go`
- 测试：`pkg/tunnel/hub/*_test.go`、`pkg/tunnel/mesh/*_test.go`、`pkg/tunnel/webrtc_signal_e2e_test.go`

- [ ] **步骤 1：Auth 改 AK 准入（`NewAuthenticator(accessKeys)`；HMAC 证明替换 relay_token）**
- [ ] **步骤 2：AutoRegister/mesh/p2p/relay start 改（去 RelayToken，加 AK/HMAC proof）**
- [ ] **步骤 3：测试 + lint + commit**

### 任务 9：E2E 验证

- [ ] `test/e2e_tunnel_accesskey_test.go`：真实二进制 + access_keys + 纯隧道 `list`/`upload` 走通（metadata 帧哈希签名，无 tunnel_key）。
- [ ] `make test-all && make lint` 全绿。

---

## 自检

- 规格覆盖：任务 1/2/3 ↔ 决策 1/3/4（HKDF/双签/ctx）；任务 4/5 ↔ 决策 6（主流程 A）；任务 6 ↔ 决策 2/3（废除 tunnel_key/fail-fast）；任务 7 ↔ 决策 1/2（sclient 派生）；任务 8 ↔ 决策 5（relay_token 废除 + AK 准入 + per-node secret 留存）。
- 类型一致性：`DeriveTunnelKey`、`SetTunnelKey`/`GetTunnelKey`、`WithTunnel(ak, sk)`、`NewAuthenticator(accessKeys)` 同名跨任务一致。
- 无占位符：所有代码步骤有实际内容。
