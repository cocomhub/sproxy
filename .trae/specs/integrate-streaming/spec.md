# 隧道协议集成流式传输 Spec

## Why
当前 `EncryptStream` / `DecryptStream` 已作为独立工具在 `stream.go` 中实现，但未集成到 `Do` 和 `NewHandler` 的实际协议流程中。所有 body 数据仍然通过 `io.ReadAll` 全量读入内存：

- `httpRequestToTunnelRequest` 中 `io.ReadAll(req.Body)` — 客户端请求 body 全量缓冲
- `NewHandler` 中 `io.ReadAll(r.Body)` — 服务端接收全量缓冲
- `NewHandler` 中 `io.ReadAll(resp.Body)` — 服务端目标响应全量缓冲
- `Do` 中 `io.ReadAll(httpResp.Body)` — 客户端响应全量缓冲

这导致大文件传输时内存占用与文件大小成正比，sproxy 和 sclient 双方都面临 OOM 风险。

## What Changes
- 修改帧协议格式：metadata 作为单独加密帧一次性发送，body 通过 `EncryptStream` 流式传输
- `Client.Do` 帧路径：metadata 帧一次性发送，body 使用 `io.Pipe` + `EncryptStream` 流式发送
- `NewHandler` 请求解析：先解析 metadata 帧，body 用 `io.Pipe` + `DecryptStream` 流式解密后作为代理请求 body
- `NewHandler` 响应发送：metadata 帧 + `EncryptStream` 流式加密目标响应
- `Client.Do` 响应解析：metadata 帧 + 返回流式 `*http.Response.Body`（`io.Pipe` + `DecryptStream`）
- 小请求（<4KB）的 JSON 模式保持不变，`DoRaw` 签名和行为不变

## Impact
- Affected specs: `rename-do-streaming`, `add-stdhttp-tunnel-api`
- Affected code: `pkg/tunnel/tunnel.go`, `pkg/tunnel/stream.go`, `pkg/tunnel/example_test.go`

## ADDED Requirements

### Requirement: 帧协议支持流式 body 传输
系统 SHALL 修改帧协议，将 metadata 和 body 分离传输：metadata 作为单独加密帧一次性发送，body 通过 EncryptStream 流式传输。

帧格式变更：
- 旧：`[4B metaLen][encrypted metadata][encrypted body]`（body 全量加密后拼接）
- 新：`[4B metaLen][encrypted metadata][stream chunks...]`（body 流式加密，每块 `[2B chunkLen][nonce|ciphertext|tag]`）

#### Scenario: 客户端发送大文件不 OOM
- **WHEN** 客户端调用 `client.Do(req)` 且 `req.Body` ≥ 4KB
- **THEN** 客户端分块加密并发送 body，不将整个 body 读入内存，发送时内存占用恒定为 chunk size（64KB）

#### Scenario: 服务端接收大文件不 OOM
- **WHEN** 服务端 NewHandler 收到帧协议请求
- **THEN** 服务端解密 metadata 后，通过 DecryptStream 流式解密 body，不将整个 body 读入内存

### Requirement: Do 返回流式 Response.Body
系统 SHALL 使 `Do` 方法返回的 `*http.Response.Body` 支持流式读取，调用方可以逐步消费响应数据，无需等待全部数据到达。

#### Scenario: 客户端流式读取响应
- **WHEN** 用户调用 `client.Do(req)` 获取 `*http.Response` 后使用 `io.Copy(os.Stdout, resp.Body)`
- **THEN** 响应 body 边解密边输出，内存占用恒定，不经全量缓冲

#### Scenario: 客户端中途取消读取
- **WHEN** 用户读取部分响应 body 后调用 `resp.Body.Close()`
- **THEN** 底层 HTTP 连接正确关闭，无 goroutine 泄漏

### Requirement: NewHandler 流式转发
系统 SHALL 使 NewHandler 在帧协议模式下，请求 body 和响应 body 均流式处理，不缓冲完整内容。

#### Scenario: 服务端流式转发大请求
- **WHEN** 服务端收到帧协议请求，body 为 100MB 文件
- **THEN** 服务端边解密边转发到目标 URL，内存占用恒定

#### Scenario: 服务端流式返回大响应
- **WHEN** 目标服务器返回 100MB 响应
- **THEN** 服务端边读取目标响应边加密返回给客户端，内存占用恒定

### Requirement: 向后兼容
系统 SHALL 保持小请求（<4KB）的 JSON 模式不变，保持 `DoRaw` 的签名和行为不变，保持 `encodeFrame` / `decodeFrame` 函数不变（供旧代码使用）。

#### Scenario: 小请求仍走 JSON 模式
- **WHEN** 客户端发送 body < 4KB 的请求
- **THEN** 使用原有 JSON 模式（`DoRaw`），行为与改前完全一致

#### Scenario: DoRaw 行为不变
- **WHEN** 调用 `client.DoRaw(&tunnel.Request{...})`
- **THEN** 行为与改前完全一致，所有现有测试通过