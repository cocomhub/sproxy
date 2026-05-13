# Tasks
- [x] Task 1: 新增流式帧协议辅助函数 — `tunnel.go`
  - [x] 1.1 实现 `encodeMetadataFrame(key, metadataJSON []byte) ([]byte, error)`：`[4B metaLen][encrypted metadata]`
  - [x] 1.2 实现 `decodeMetadataFrame(r io.Reader, key []byte) (metadataJSON []byte, err error)`：从 Reader 读取 4B 长度 + 加密 metadata，解密返回 JSON
  - [x] 1.3 删除 `encodeFrame` / `decodeFrame` / `frameThreshold` 等废弃代码

- [x] Task 2: 重构 `Client.DoRaw` 为流式 — `tunnel.go`
  - [x] 2.1 内部改为流式帧协议：`encodeMetadataFrame` + `EncryptStream` 发送，`decodeMetadataFrame` + `DecryptStream` 接收
  - [x] 2.2 签名保持不变 `(req *Request) (*Response, error)`，`Response.Body` 仍为 Base64
  - [x] 2.3 移除所有 `io.ReadAll` 调用（响应 body 除外，DoRaw 需要全量收集以返回 Base64）

- [x] Task 3: 重构 `Client.Do` 为全流式 — `tunnel.go`
  - [x] 3.1 不再调用 `DoRaw`，直接走流式帧路径
  - [x] 3.2 `io.Pipe` + `EncryptStream` goroutine 流式加密请求 body（无 body 时加密空流）
  - [x] 3.3 响应 body 通过 `io.Pipe` + `DecryptStream` goroutine 返回流式 `*http.Response.Body`
  - [x] 3.4 移除所有 `io.ReadAll` 调用（帧路径）

- [x] Task 4: 重构 `NewHandler` 为全流式 — `tunnel.go`
  - [x] 4.1 请求解析：`decodeMetadataFrame(r.Body, key)` 解析 metadata，剩余用 `DecryptStream` 流式解密
  - [x] 4.2 响应发送：`encodeMetadataFrame` + `EncryptStream` 流式加密目标响应，无大小判断
  - [x] 4.3 移除 `io.ReadAll(r.Body)` 和 `io.ReadAll(resp.Body)`
  - [x] 4.4 删除 `writeEncryptedResponse`、`httpRequestToTunnelRequest`、`tunnelResponseToHTTPResponse` 等废弃函数

- [x] Task 5: 修复 `stream.go` 中 `uint16` 溢出 bug — `stream.go`
  - [x] 5.1 将 chunk 长度从 `uint16`（2字节）改为 `uint32`（4字节），修复 64KB chunk 时密文长度溢出问题
  - [x] 5.2 更新 `EncryptStream`/`DecryptStream` 注释说明格式变更

- [x] Task 6: 更新示例测试 — `example_test.go`
  - [x] 6.1 `ExampleClient_Do_largeBody` 改为 128KB（2 个完整 chunk），输出 `{"size":131072}`
  - [x] 6.2 新增 `ExampleClient_Do_streamResponse` 验证流式响应读取
  - [x] 6.3 `Example_tamperDetection` 改为发送损坏帧，仍验证 400
  - [x] 6.4 所有已有 Example 测试保持通过

- [x] Task 7: 构建和 vet 验证
  - [x] 7.1 运行 `go build ./...` 成功
  - [x] 7.2 运行 `go vet ./...` 成功
  - [x] 7.3 运行 `go test -run Example ./pkg/tunnel/...` 全部 19 个 Example 通过

# Task Dependencies
- Task 1 → Task 2, Task 3, Task 4（可并行）
- Task 5（独立，修复已有 bug）
- Task 6 depends on Tasks 1-5
- Task 7 depends on Tasks 1-6
