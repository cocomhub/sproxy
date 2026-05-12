# Tasks
- [x] Task 1: 在 `Client` 上新增 `DoHTTP(req *http.Request) (*http.Response, error)` 方法
  - [x] 1.1 实现 `httpRequestToTunnelRequest(req *http.Request) (*Request, error)` 内部转换函数：将标准 `*http.Request` 转为 `tunnel.Request`（读取 Body 后 Base64 编码，提取 Headers）
  - [x] 1.2 实现 `tunnelResponseToHTTPResponse(resp *Response) *http.Response` 内部转换函数：将 `tunnel.Response` 重建为标准 `*http.Response`（Body 为 `io.NopCloser` 包装解码后的字节）
  - [x] 1.3 实现 `DoHTTP` 方法：调用内部转换函数 → 调用 `c.Do()` → 转换响应 → 返回标准 `*http.Response`

- [x] Task 2: 调整 `cmd/sclient/client.go` 的 `TunnelRequest` 函数接入 `c.DoHTTP()`
  - [x] 2.1 使用 `http.NewRequest` 构造标准请求
  - [x] 2.2 调用 `c.DoHTTP(req)` 替代原有的 `c.Do()`
  - [x] 2.3 从 `*http.Response` 中读取 StatusCode / Headers / Body，行为与改前一致

- [x] Task 3: 新增 `ExampleClient_DoHTTP` 示例测试到 `pkg/tunnel/example_test.go`
  - [x] 3.1 添加 end-to-end 示例：GET 请求含响应头验证
  - [x] 3.2 运行 `go test -run Example ./pkg/tunnel/...` 确保所有 Example 通过

- [x] Task 4: 构建和 vet 验证
  - [x] 4.1 运行 `go build ./...` 和 `go vet ./...` 确保无编译错误

# Task Dependencies
- Task 2 depends on Task 1
- Task 3 depends on Task 1
- Task 4 depends on Tasks 1-3
