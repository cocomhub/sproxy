<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sproxy 加密隧道协议

`pkg/tunnel` 提供基于 AES-256-GCM 的端到端加密信道，使 sclient 可以穿越不可信
网络（公网、HTTP 代理、CDN）访问 sproxy 的所有文件 API。

## 密钥

- 32 字节（256 位）AES 密钥
- 配置文件中使用 64 位十六进制字符表示（`a-f` / `0-9`）
- 生成：`sclient genkey` 或 `tunnel.GenerateKey()`
- 服务端：`tunnel_key` 配置项 → 启动时 viper 加载，留空时自动生成并回写
- 客户端：`tunnel_key` 配置项（默认路径 XDG，详见 [config.md](./config.md)）
- 长度与 hex 格式校验由 `tunnel.ParseKey` 统一负责，启动失败说明密钥格式不对

> 密钥泄漏即视同失去保密性；切勿提交到 git、不要写进截图、不要走非可信信道。

## 帧协议

所有 `POST /tunnel` 请求 / 响应共享同一帧格式：

```
[4B big-endian metaLen][encrypted metadata][stream chunks ...]
```

其中：

- `metaLen`：metadata 帧（包含 nonce + ciphertext + tag）的总长度。
  **上限 `MaxMetadataBytes = 1 MiB`**；超过直接 400 退回，防止远程 OOM 攻击。
- `encrypted metadata`：`Encrypt(key, metadataJSON)`，明文是 JSON：
  - 请求：`{"method": "...", "url": "...", "headers": {...}}`
  - 响应：`{"proto": "HTTP/1.1", "status": 200, "headers": {...}, "content_length": -1}`
- `stream chunks`：body 的流式加密，逐块 `[2B chunkLen][nonce|ciphertext|tag]`，
  默认 64 KiB / 块。每块独立加密，可以边接收边解密、边发送边加密，内存占用恒定。

### 加密参数

- 算法：AES-256-GCM
- nonce：每次加密随机生成 12 字节
- nonce 排列：拼接在密文前（`nonce || ciphertext || tag`），由 `Decrypt` 自动提取
- 同一明文每次加密的密文不同（nonce 随机），且 GCM 提供完整性验证（tag 校验失败 → `Decrypt` 报错）

## 两种路由模式

`pkg/tunnel.NewHandler` 与 `pkg/tunnel.NewLocalHandler` 决定 `POST /tunnel` 收到
请求后如何处理：

| 模式 | 适用 |
|---|---|
| **外部转发**（`NewHandler`） | 请求 URL 是绝对 URL（如 `https://api.example.com/x`），解密后通过 `http.Client` 转发到目标 |
| **本地路由**（`NewLocalHandler`） | 请求 URL 是相对路径（如 `/upload`），不走外部网络，直接在本进程内路由到 sproxy 自己的文件 handler |

sproxy 默认使用 `NewLocalHandler(tunnelKey, localMux, ...)`，让 sclient 通过隧道
直接调用 sproxy 自身的 `/upload` / `/download` / `/api/files` 等路由。

> 注意：本地路由模式下 handler 的 panic 不会让整个隧道 goroutine 阻塞——
> dispatchLocal 用 `defer + recover()` 兜底，保证响应 metadata channel 一定被关闭。

## 客户端使用

Go SDK 调用：

```go
client, _ := tunnel.NewClient(
    "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
    "http://proxy:18083/tunnel",
    30 * time.Second,
    nil,
)
req, _ := http.NewRequest("GET", "/api/files", nil)
resp, _ := client.Do(req)
defer resp.Body.Close()
io.Copy(os.Stdout, resp.Body)
```

`tunnel.Client.Do` 是标准库风格客户端：

- 接受标准 `*http.Request`
- 内部组装帧、流式加密上传、流式解密下载
- 返回标准 `*http.Response`，`Body` 是流式 Reader
- 内存占用与 body 大小无关（仅 1 块加密缓冲）

`pkg/client.FileClient` 在 `WithTunnel(...)` 选项下会自动用 `tunnel.Client` 代替
普通 HTTP 调用，使 `Upload` / `Download` / `Rename` / `Stat` 等方法都走加密信道。

## 错误处理

| 现象 | 含义 |
|---|---|
| HTTP 403 | 服务端 `tunnel_key` 为空，明确拒绝隧道请求 |
| HTTP 400 | metadata 帧损坏（长度过大 / 解密失败 / JSON 错误） |
| HTTP 500 | metadata 编码出错（极少见） |
| `Decrypt` 报 error | 密钥不匹配 / 密文被篡改 / 帧截断 |

## 安全性要点

- **完整性**：GCM tag 保证密文不被篡改，单字节翻转即触发 `Decrypt` 失败。
- **抗重放**：每帧使用随机 nonce + 一次性 stream，不保留服务器端状态，因此对单帧重放无防御；
  但每个 sclient 请求都会触发新的 nonce / chunk，重放整个会话会复用相同密文 → 服务端解密成功
  并产生新的副作用。如果担心重放攻击，请在前置 reverse proxy 层加 TLS + 时间戳鉴权。
- **明文 fallback**：sproxy 的文件 API 路径（`/upload` / `/download` / ...）默认通过明文 HTTP 提供。
  如果客户端绕开 `POST /tunnel` 直接调用文件 API，则没有加密。要强制全加密，
  应在前置 reverse proxy 启用 TLS（`tls.enabled: true`）+ `auth_token` 限制访问。
