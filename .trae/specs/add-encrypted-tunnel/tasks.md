# Tasks

- [x] Task 1: sproxy 端 — 扩展 Config 支持 tunnel_key
  - [x] 在 `config.Config` 中新增 `TunnelKey string yaml:"tunnel_key"` 字段
  - [x] 在 `cmd/sproxy/main.go` 中加载配置后校验 tunnel_key：非空时必须为 64 字符 hex，否则打印错误退出
  - [x] 将 tunnel_key 解析为 `[]byte` 传入 handlers

- [x] Task 2: sproxy 端 — 实现加密工具函数
  - [x] 在 `internal/handlers/` 下新增 `crypto.go`，实现 `encryptPayload(key, plainJSON []byte) ([]byte, error)` 和 `decryptPayload(key, data []byte) ([]byte, error)`
  - [x] 使用 `crypto/aes`、`crypto/cipher`、`crypto/rand` 标准库实现 AES-256-GCM
  - [x] encryptPayload：生成 12 字节随机 nonce，加密，返回 `nonce + ciphertext`
  - [x] decryptPayload：取前 12 字节为 nonce，剩余为 ciphertext，解密返回明文

- [x] Task 3: sproxy 端 — 实现 POST /tunnel 端点
  - [x] 在 `internal/handlers/handlers.go` 中新增 `tunnel` 方法
  - [x] 仅接受 POST 方法，否则返回 405
  - [x] tunnel_key 为空时返回 403
  - [x] 读取请求体，调用 decryptPayload 解密
  - [x] 解析 JSON 载荷（method, url, headers, body）
  - [x] 构造 http.Request 发往目标 URL
  - [x] 读取目标响应，base64 编码响应体
  - [x] 构造响应 JSON（status, headers, body），调用 encryptPayload 加密后返回
  - [x] 在 `RegisterRoutes` 中注册 `POST /tunnel` 路由

- [x] Task 4: sclient 端 — 扩展 Config 支持 tunnel 配置
  - [x] 在 `SclientConfig` 中新增 `TunnelKey string yaml:"tunnel_key"` 和 `TunnelEndpoint string yaml:"tunnel_endpoint"` 字段
  - [x] `DefaultConfig()` 中 TunnelEndpoint 默认值为 `/tunnel`
  - [x] `HandleConfigShow()` 中显示 tunnel_key（掩码显示，仅显示前 4 位和后 4 位）和 tunnel_endpoint
  - [x] `HandleConfigSet()` 中支持 tunnel_key（校验 64 字符 hex）和 tunnel_endpoint

- [x] Task 5: sclient 端 — 实现加密工具函数
  - [x] 在 `cmd/sclient/` 下新增 `crypto.go`，实现 `encryptPayload` 和 `decryptPayload`（与 sproxy 端逻辑一致）
  - [x] 实现 `parseTunnelKey(hexKey string) ([]byte, error)` 将 hex 字符串转为 32 字节 key

- [x] Task 6: sclient 端 — 实现 tunnel 子命令
  - [x] 在 `cmd/sclient/client.go` 中新增 `TunnelRequest` 函数
  - [x] 构造请求载荷 JSON（method, url, headers, body base64）
  - [x] 调用 encryptPayload 加密
  - [x] POST 到 `server_url + tunnel_endpoint`
  - [x] 解密响应，输出 status、headers（-i 时）、body
  - [x] 在 `cmd/sclient/main.go` 中新增 `tunnel` 子命令路由
  - [x] 支持选项：`-X METHOD`（默认 GET）、`-H "Header: Value"`（可重复）、`-d @file|-d "body"`、`-i`（显示响应头）、`-v`（详细模式）
  - [x] 更新 `printHelp()` 添加 tunnel 命令说明

- [x] Task 7: 验证构建
  - [x] 运行 `go build ./...` 确保全项目编译通过
  - [x] 运行 `go vet ./...` 确保无 vet 警告

# Task Dependencies
- Task 2 依赖 Task 1（crypto 函数需要 key）
- Task 3 依赖 Task 1、Task 2（tunnel handler 需要 config 和 crypto）
- Task 5 依赖 Task 4（sclient crypto 需要 key 解析）
- Task 6 依赖 Task 4、Task 5（tunnel 命令需要 config 和 crypto）
- Task 7 依赖 Task 1-6
- Task 1 和 Task 4 可并行
- Task 2 和 Task 5 可并行（在各自依赖完成后）