# Tunnel 支持标准库 http.Request / http.Response 接口 Spec

## Why
当前 `tunnel.Client.Do()` 使用自定义的 `tunnel.Request` 和 `tunnel.Response` 结构体，第三方嵌入时需要手动做类型转换（标准库 ↔ tunnel 自定义类型），增加了接入成本和出错概率。直接支持标准库 `*http.Request` 和 `*http.Response` 可以让调用方零学习成本使用隧道，像普通 HTTP 客户端一样编程。

## What Changes
- `Client` 新增 `DoHTTP(req *http.Request) (*http.Response, error)` 方法：接受标准 `*http.Request`，自动转换为隧道协议发送，返回重建的标准 `*http.Response`
- `NewHandler` 内部重构，提取 `requestToTunnelRequest` / `tunnelResponseToHTTP` 等内部转换函数，供 `DoHTTP` 对应的响应重建逻辑复用
- `sclient` 的 `TunnelRequest` 函数接入新方法 `c.DoHTTP()`

## Impact
- Affected specs: `refactor-tunnel-pkg`
- Affected code: `pkg/tunnel/tunnel.go`, `cmd/sclient/client.go`, `pkg/tunnel/example_test.go`

## ADDED Requirements

### Requirement: Client.DoHTTP 接受标准 *http.Request
系统 SHALL 在 `Client` 上提供 `DoHTTP(req *http.Request) (*http.Response, error)` 方法。

#### Scenario: GET 请求成功
- **WHEN** 用户构造 `http.NewRequest("GET", "https://api.example.com/data", nil)` 并调用 `client.DoHTTP(req)`
- **THEN** 系统将请求自动转换为隧道协议加密发送，解密响应后重建标准 `*http.Response` 返回，`resp.StatusCode` 等于目标服务器状态码，`resp.Body` 包含目标响应体

#### Scenario: POST 带 Body 请求
- **WHEN** 用户构造 POST 请求并设置 `req.Body` 和 `req.Header`
- **THEN** 请求 Body 通过 Base64 编码后携带在隧道请求中，目标服务器收到完整 Body 和 Header

#### Scenario: 隧道服务不可达
- **WHEN** 隧道 URL 不可达
- **THEN** `DoHTTP` 返回错误，描述网络错误原因

#### Scenario: 目标返回非 200
- **WHEN** 目标服务器返回 404
- **THEN** `DoHTTP` 返回 `*http.Response` 且 `resp.StatusCode == 404`，`resp.Body` 包含目标返回的响应体

#### Scenario: 解密失败
- **WHEN** 隧道服务返回的密文被篡改或密钥不匹配
- **THEN** `DoHTTP` 返回错误，包含解密失败信息

### Requirement: sclient tunnel 命令接入 DoHTTP
系统 SHALL 将 `cmd/sclient` 的 `TunnelRequest` 函数改为使用 `tunnel.Client.DoHTTP` 方法。

#### Scenario: sclient tunnel 命令正常执行
- **WHEN** 用户执行 `sclient tunnel https://api.example.com/data`
- **THEN** 内部使用 `c.DoHTTP()` 发送标准 `*http.Request`，从 `*http.Response` 读取状态码、响应头和响应体，行为与改前完全一致

### Requirement: 新增 DoHTTP 的 Example 测试
系统 SHALL 在 `pkg/tunnel/example_test.go` 中新增 `ExampleClient_DoHTTP` 可运行示例。

#### Scenario: Example 测试可运行
- **WHEN** 运行 `go test -run Example ./pkg/tunnel/...`
- **THEN** 新增的 `ExampleClient_DoHTTP` 和其他已有 Example 全部通过
