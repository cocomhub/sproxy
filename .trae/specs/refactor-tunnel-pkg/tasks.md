# Tasks

- [ ] Task 1: 创建 pkg/tunnel/tunnel.go — 公共包核心实现
  - [ ] 创建 `pkg/tunnel/tunnel.go`，package 为 `tunnel`
  - [ ] 实现 `ParseKey(hexKey string) ([]byte, error)`：hex 解析 + 长度校验（32 字节）
  - [ ] 实现 `GenerateKey() ([]byte, error)`：生成 32 字节随机 key
  - [ ] 实现 `Encrypt(key, plaintext []byte) ([]byte, error)`：AES-256-GCM 加密，随机 nonce 前缀
  - [ ] 实现 `Decrypt(key, data []byte) ([]byte, error)`：AES-256-GCM 解密
  - [ ] 定义 `Request` 结构体（method/url/headers/body，json tag）
  - [ ] 定义 `Response` 结构体（status/headers/body，json tag）
  - [ ] 实现 `EncodeBody(b []byte) string`（base64 编码）
  - [ ] 实现 `DecodeBody(s string) ([]byte, error)`（base64 解码）
  - [ ] 实现 `NewHandler(key []byte, client *http.Client) http.Handler`：可嵌入服务端 handler
  - [ ] 实现 `Client` 结构体和 `NewClient(hexKey, tunnelURL string, timeout time.Duration) (*Client, error)`
  - [ ] 实现 `(c *Client) Do(req *Request) (*Response, error)`：加密请求 + 解密响应

- [ ] Task 2: 重构 internal/handlers — 改用 pkg/tunnel
  - [ ] 在 `internal/handlers/handlers.go` 的 tunnel handler 中，将 `tunnel.NewHandler(h.tunnelKey, h.client)` 作为 `http.Handler` 注册到路由，替换原有的 `h.tunnel` 方法
  - [ ] 删除 `internal/handlers/crypto.go`（加密逻辑已移入 pkg/tunnel）
  - [ ] 删除 `handlers.go` 中的 `tunnelRequest`/`tunnelResponse` 结构体定义
  - [ ] 删除 `handlers.go` 中的 `h.tunnel` 方法实现

- [ ] Task 3: 重构 cmd/sclient — 改用 pkg/tunnel
  - [ ] 删除 `cmd/sclient/crypto.go`（加密逻辑改用 pkg/tunnel）
  - [ ] 修改 `cmd/sclient/client.go`：
    - 删除 `tunnelRequest`/`tunnelResponse` 结构体定义
    - `TunnelRequest` 函数改用 `tunnel.NewClient` + `tunnel.Client.Do`
  - [ ] 修改 `cmd/sclient/config.go` 中的 `parseTunnelKey` 改调用 `tunnel.ParseKey`（或直接删除，改为 import）
  - [ ] 在 `cmd/sclient/main.go` 中新增 `genkey` 子命令：调用 `tunnel.GenerateKey()`，输出 64 字符 hex 字符串
  - [ ] 更新 `printHelp()` 添加 `genkey` 命令说明

- [ ] Task 4: 验证构建
  - [ ] 运行 `go build ./...` 确保全项目编译通过
  - [ ] 运行 `go vet ./...` 确保无 vet 警告

# Task Dependencies
- Task 2 依赖 Task 1（handlers 需要 pkg/tunnel 包存在）
- Task 3 依赖 Task 1（sclient 需要 pkg/tunnel 包存在）
- Task 2 和 Task 3 可并行（在 Task 1 完成后）
- Task 4 依赖 Task 1-3