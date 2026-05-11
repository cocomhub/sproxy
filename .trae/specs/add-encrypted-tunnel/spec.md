# sproxy 加密转发隧道 Spec

## Why
当前 transfer 路由 `/{host}/{filepath...}` 将目标主机和路径以明文形式暴露在 URL 中，且请求/响应体在 sproxy 与客户端之间明文传输。当 sproxy 通过非 TLS 通道（如内网 HTTP）部署时，中间人可以获知转发目标和内容，存在信息泄露风险。需要增加加密转发方案：隐藏路由目标信息、加密传输内容。

## What Changes
- **sproxy**：新增 `POST /tunnel` 端点，接收 AES-256-GCM 加密的请求载荷，解密后转发到真实目标，将响应加密后返回
- **sproxy config**：新增 `tunnel_key` 配置项（256-bit hex key），为空时禁用 tunnel 功能
- **sclient config**：新增 `tunnel_key` 和 `tunnel_endpoint` 配置项
- **sclient**：新增 `tunnel` 子命令，构造加密请求通过 sproxy tunnel 转发
- 原有 `/{host}/{filepath...}` 明文路由**保持不变**，不影响现有功能

## Impact
- Affected specs: implement-sclient（sclient 新增 tunnel 子命令和配置项）
- Affected code: `config/config.go`、`internal/handlers/handlers.go`、`cmd/sclient/main.go`、`cmd/sclient/client.go`、`cmd/sclient/config.go`

---

## ADDED Requirements

### Requirement: Tunnel 加密协议
系统 SHALL 使用 AES-256-GCM 作为加密算法，共享密钥由配置项 `tunnel_key` 提供（64 字符 hex 编码的 32 字节 key）。

#### Scenario: 加密请求载荷
- **WHEN** sclient 构造 tunnel 请求
- **THEN** 生成 12 字节随机 nonce，用 AES-256-GCM 加密 JSON 载荷，将 nonce+ciphertext 作为 POST /tunnel 的请求体发送

#### Scenario: 解密并转发
- **WHEN** sproxy 收到 POST /tunnel 请求
- **THEN** 从请求体读取 nonce（前 12 字节）和 ciphertext，用 AES-256-GCM 解密得到 JSON 载荷，构造 HTTP 请求发往目标，将目标响应加密后返回

#### Scenario: 解密失败
- **WHEN** sproxy 解密失败（key 不匹配或数据损坏）
- **THEN** 返回 400 Bad Request，不泄露错误细节

### Requirement: Tunnel 请求载荷格式
解密后的请求载荷 SHALL 为 JSON 格式：
```json
{
  "method": "GET",
  "url": "https://example.com/path?query=1",
  "headers": {"Accept": "application/json", "Authorization": "Bearer xxx"},
  "body": "<base64_encoded_body_or_empty_string>"
}
```

#### Scenario: GET 请求
- **WHEN** 载荷 method=GET 且 body 为空
- **THEN** sproxy 构造 GET 请求发往 url，无请求体

#### Scenario: POST 请求含 body
- **WHEN** 载荷 method=POST 且 body 非空
- **THEN** sproxy 解码 base64 body，构造 POST 请求发送

### Requirement: Tunnel 响应格式
sproxy 返回的加密响应解密后 SHALL 为 JSON 格式：
```json
{
  "status": 200,
  "headers": {"Content-Type": "application/json"},
  "body": "<base64_encoded_response_body>"
}
```

#### Scenario: 正常响应
- **WHEN** 目标返回 200
- **THEN** sproxy 将状态码、响应头、base64 编码的响应体加密后返回

#### Scenario: 目标不可达
- **WHEN** 目标连接失败
- **THEN** sproxy 返回 status=502, body 包含错误描述

### Requirement: sproxy Tunnel 端点
系统 SHALL 在 sproxy 中注册 `POST /tunnel` 端点。

#### Scenario: tunnel_key 未配置
- **WHEN** sproxy 配置中 tunnel_key 为空
- **THEN** POST /tunnel 返回 403 Forbidden

#### Scenario: 请求方法非 POST
- **WHEN** 使用 GET 等其他方法请求 /tunnel
- **THEN** 返回 405 Method Not Allowed

#### Scenario: 请求体为空
- **WHEN** POST /tunnel 请求体为空
- **THEN** 返回 400 Bad Request

### Requirement: sproxy Config 扩展
系统 SHALL 在 `config.Config` 中新增字段：
- `TunnelKey string yaml:"tunnel_key"` — AES-256-GCM 共享密钥（64 字符 hex）

#### Scenario: tunnel_key 合法
- **WHEN** tunnel_key 为 64 字符 hex 字符串
- **THEN** 解析为 32 字节 key，启用 tunnel 功能

#### Scenario: tunnel_key 格式错误
- **WHEN** tunnel_key 非 64 字符 hex
- **THEN** sproxy 启动时打印警告后退出

### Requirement: sclient tunnel 子命令
系统 SHALL 在 sclient 中新增 `tunnel` 子命令，用法：
```
sclient tunnel <url> [-X METHOD] [-H "Header: Value"] [-d @file|-d "body"]
```

#### Scenario: 基本 GET 请求
- **WHEN** 用户执行 `sclient tunnel https://api.example.com/data`
- **THEN** 加密载荷并通过 POST /tunnel 发送，解密响应并输出

#### Scenario: POST 请求含 JSON body
- **WHEN** 用户执行 `sclient tunnel -X POST -H "Content-Type: application/json" -d '{"key":"val"}' https://api.example.com/echo`
- **THEN** 发送携带 JSON body 的加密 POST 请求，输出解密后的响应

#### Scenario: 从文件读取 body
- **WHEN** 用户执行 `sclient tunnel -X PUT -d @data.json https://api.example.com/upload`
- **THEN** 读取 data.json 内容作为请求体

#### Scenario: 输出响应头
- **WHEN** 用户使用 `-i` 选项
- **THEN** 输出解密后的响应头和响应体

#### Scenario: 详细模式
- **WHEN** 用户使用 `-v` 选项
- **THEN** 输出加密前的请求载荷和解密后的完整响应信息

### Requirement: sclient Config 扩展
系统 SHALL 在 `SclientConfig` 中新增字段：
- `TunnelKey string yaml:"tunnel_key"` — AES-256-GCM 共享密钥（64 字符 hex）
- `TunnelEndpoint string yaml:"tunnel_endpoint"` — Tunnel 端点路径（默认 `/tunnel`）

#### Scenario: config show 显示 tunnel 配置
- **WHEN** 用户执行 `sclient config show`
- **THEN** 打印 tunnel_key 和 tunnel_endpoint（tunnel_key 显示为掩码）

#### Scenario: config set tunnel_key
- **WHEN** 用户执行 `sclient config set tunnel_key <64_char_hex>`
- **THEN** 校验合法性后保存

#### Scenario: tunnel_key 未配置
- **WHEN** 用户执行 tunnel 命令但 tunnel_key 为空
- **THEN** 提示用户先配置 tunnel_key

### Requirement: 加密工具函数（在 handlers 和 sclient 之间共享）
系统 SHALL 提供 `encryptPayload` 和 `decryptPayload` 函数：
- `encryptPayload(key []byte, plainJSON []byte) ([]byte, error)` — 生成随机 nonce，AES-256-GCM 加密，返回 nonce+ciphertext
- `decryptPayload(key []byte, data []byte) ([]byte, error)` — 从 data 提取 nonce（前 12 字节），AES-256-GCM 解密，返回明文

这些函数需要分别在 sproxy 和 sclient 两端各实现一份（无法共享 package，因为 sclient 是独立 main package）。

#### Scenario: 加密解密往返
- **WHEN** 用 encryptPayload 加密，再用 decryptPayload 解密
- **THEN** 原始明文可完全恢复