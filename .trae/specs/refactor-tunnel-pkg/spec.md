# Tunnel 功能完整性检查与 pkg 包抽象 Spec

## Why
当前 tunnel（加密转发隧道）功能的核心代码（AES-256-GCM 加解密、协议类型、tunnel handler）分散在 `internal/handlers/` 和 `cmd/sclient/` 两处，存在大量重复。缺少独立的公共 pkg 包导致第三方无法内嵌 tunnel 的服务端或客户端能力。需要将 tunnel 功能抽象为 `pkg/tunnel` 包，消除重复，并完善遗漏的功能点。

## 功能完整性检查结论

### ✅ 已实现
- AES-256-GCM 加/解密（两端各有一份实现）
- tunnel_key 配置项（sproxy config 和 sclient config）
- POST /tunnel 服务端端点（解密→转发→加密响应）
- sclient tunnel 子命令（支持 -X/-H/-d/-i/-v）
- tunnel_key 未配置时服务端返回 403
- 服务端转发失败时返回加密的 502 响应
- tunnel_key 非 64 hex 时 sproxy 启动报错退出

### ❌ 缺失/问题
- **代码重复**：`encryptPayload`/`decryptPayload` 在两处独立实现，需要抽象到公共 pkg
- **协议类型重复**：`tunnelRequest`/`tunnelResponse` 结构体两处各定义一份
- **无可嵌入 server**：第三方无法通过导入包获得一个 `http.Handler` 来在自己的 HTTP server 中挂载 tunnel 功能
- **无可嵌入 client**：第三方无法通过导入包获得 tunnel 客户端函数来直接发送加密请求
- **缺少 key 生成工具函数**：`ParseTunnelKey` 只在 sclient 的 crypto.go 里有，没有在公共位置提供 `GenerateTunnelKey` 便利函数

## What Changes
- 新增 `pkg/tunnel/` 包，提供：
  - 公共协议类型 `Request`/`Response`
  - 公共加密工具 `Encrypt`/`Decrypt`/`ParseKey`/`GenerateKey`
  - 可嵌入的服务端 `Handler(key []byte, client *http.Client) http.Handler`
  - 可嵌入的客户端 `Client` 结构体（含 `Do` 方法）
- **BREAKING**：`internal/handlers/crypto.go` 中的 `encryptPayload`/`decryptPayload` 改为调用 `pkg/tunnel`
- **BREAKING**：`cmd/sclient/crypto.go` 中的加密函数改为调用 `pkg/tunnel`
- `tunnelRequest`/`tunnelResponse` 结构体统一定义在 `pkg/tunnel`，两端均引用

## Impact
- Affected specs: add-encrypted-tunnel（重构已实现的 tunnel 功能）
- Affected code:
  - 新增 `pkg/tunnel/tunnel.go`
  - 修改 `internal/handlers/crypto.go`（删除，逻辑移入 pkg/tunnel）
  - 修改 `internal/handlers/handlers.go`（tunnel handler 使用 pkg/tunnel）
  - 修改 `cmd/sclient/crypto.go`（删除，调用 pkg/tunnel）
  - 修改 `cmd/sclient/client.go`（TunnelRequest 使用 pkg/tunnel.Client）

---

## ADDED Requirements

### Requirement: pkg/tunnel 公共包

系统 SHALL 提供 `pkg/tunnel` 包，module path 为 `github.com/cocomhub/sproxy/pkg/tunnel`，包含以下公共 API：

#### 加密工具

```go
func ParseKey(hexKey string) ([]byte, error)
func GenerateKey() ([]byte, error)
func Encrypt(key, plaintext []byte) ([]byte, error)
func Decrypt(key, data []byte) ([]byte, error)
```

- `ParseKey`：将 64 字符 hex 字符串解析为 32 字节 key，校验长度
- `GenerateKey`：生成随机 32 字节 key（用于生成新密钥的便利函数）
- `Encrypt`/`Decrypt`：AES-256-GCM 加解密，nonce 前缀在密文中

#### 协议类型

```go
type Request struct {
    Method  string            `json:"method"`
    URL     string            `json:"url"`
    Headers map[string]string `json:"headers"`
    Body    string            `json:"body"`
}

type Response struct {
    Status  int               `json:"status"`
    Headers map[string]string `json:"headers"`
    Body    string            `json:"body"`
}
```

#### 可嵌入服务端 Handler

```go
func NewHandler(key []byte, client *http.Client) http.Handler
```

- 返回一个 `http.Handler`，第三方可直接将其注册到任意 `http.ServeMux`
- 内部逻辑：解密请求体 → 解析 `Request` → 发起 HTTP 请求 → 加密 `Response` 返回
- key 为空时返回 403
- 解密失败返回 400
- 代理请求失败时返回加密的 status=502 响应（而非 HTTP 502）

#### 可嵌入客户端 Client

```go
type Client struct {
    Key        []byte
    TunnelURL  string
    HTTPClient *http.Client
}

func NewClient(hexKey, tunnelURL string, timeout time.Duration) (*Client, error)

func (c *Client) Do(req *Request) (*Response, error)
```

- `NewClient`：解析 hexKey，创建 HTTP 客户端
- `Do`：序列化 `Request` → 加密 → POST TunnelURL → 解密 → 返回 `*Response`

#### 辅助函数

```go
func EncodeBody(b []byte) string
func DecodeBody(s string) ([]byte, error)
```

- `EncodeBody`：将 []byte 做 base64 编码，用于填充 Request.Body / Response.Body
- `DecodeBody`：解码对应的 base64 字符串

#### Scenario: 第三方 server 嵌入
- **WHEN** 第三方在自己的 server 中执行 `mux.Handle("/tunnel", tunnel.NewHandler(key, client))`
- **THEN** 该 server 具备完整的 tunnel 解密转发能力，无需额外代码

#### Scenario: 第三方 client 嵌入
- **WHEN** 第三方执行 `c, _ := tunnel.NewClient(hexKey, "http://proxy:18080/tunnel", 30*time.Second)` 然后 `resp, _ := c.Do(&tunnel.Request{Method:"GET", URL:"https://api.example.com/data"})`
- **THEN** 请求被加密后发送到 sproxy，响应解密后返回

### Requirement: sproxy 和 sclient 改用 pkg/tunnel

系统 SHALL 将 sproxy 和 sclient 的 tunnel 实现改为复用 `pkg/tunnel` 包。

#### Scenario: sproxy tunnel handler 复用
- **WHEN** POST /tunnel 请求到达 sproxy
- **THEN** sproxy 调用 `tunnel.NewHandler` 处理，行为与之前完全一致

#### Scenario: sclient TunnelRequest 复用
- **WHEN** 用户执行 `sclient tunnel <url>`
- **THEN** sclient 使用 `tunnel.Client.Do` 发送请求，行为与之前完全一致

### Requirement: sclient 新增 genkey 子命令
系统 SHALL 在 sclient 中新增 `genkey` 子命令，调用 `tunnel.GenerateKey()`，输出一个可直接用于 tunnel_key 配置的 64 字符 hex 字符串，方便用户初次配置。

#### Scenario: 生成密钥
- **WHEN** 用户执行 `sclient genkey`
- **THEN** 输出一行 64 字符 hex 字符串