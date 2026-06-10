# Tasks

- [ ] Task 1（Phase 1）: 配置扩展 — peers
  - [ ] 在 `pkg/server/config.go` 增加 peers 配置结构体与字段（含 yaml tag）
  - [ ] 在 viper 加载路径中支持 peers（配置文件 + 环境变量前缀 SPROXY_ + flag 兼容策略按现有约定）
  - [ ] 增加配置校验：peer name 唯一、tunnel_url 合法、tunnel_key（若非空）为 64 hex
  - [ ] 为配置解析/校验补充表驱动测试（优先单测）

- [ ] Task 2（Phase 1）: 新增 pkg/peer — 对端访问封装（基于 pkg/tunnel.Client）
  - [ ] 定义 `Peer`（name, tunnelURL, keyHex/derived）
  - [ ] 定义 `Client`：提供 `ListFiles(subdir)`、`PullFile(filename)`、`PushFile(filename)` 所需的最小能力
  - [ ] 单测：URL 拼接、错误包装、对端返回非 200 的处理

- [ ] Task 3（Phase 1）: sproxy 端新增 /api/peers 路由与 handlers
  - [ ] `GET /api/peers`：返回 peers 列表（受 authMiddleware 保护）
  - [ ] `GET /api/peers/{name}/files`：隧道透传对端 `/api/files`
  - [ ] `POST /api/peers/{name}/pull`：从对端下载并落盘到本机 uploads_dir（流式）
  - [ ] `POST /api/peers/{name}/push`：从本机读取并流式上传到对端
  - [ ] 测试：使用 `httptest` 搭建对端 sproxy（含 /tunnel + 本地路由）进行端到端 handler 测试
  - [ ] 测试覆盖冲突（409）、幂等（checksum 相同）、不可达（502）

- [ ] Task 4（Phase 1）: sclient peer 子命令
  - [ ] 新增 `peer` 命令组（cobra）
  - [ ] 实现 `peer list/ls/pull/push`
  - [ ] 单测：cobra 命令执行（不依赖真实网络，使用 httptest）

- [ ] Task 5（Phase 2）: 新增 pkg/p2p — 会话配对与中继
  - [ ] 设计 `Manager`：维护 `{id -> session}`，支持 send/recv 任意先后到达
  - [ ] 会话 TTL 与并发上限（默认值在 config 中给出）
  - [ ] 单测：send/recv 双向配对、超时、取消、并发上限

- [ ] Task 6（Phase 2）: sproxy 端新增 /p2p 路由与 handlers
  - [ ] 注册 `POST /p2p/{id}/send` 与 `GET /p2p/{id}/recv`（受 authMiddleware 保护）
  - [ ] 流式转发：不缓冲全量数据，任一侧断开可正确释放资源
  - [ ] 端到端测试：用 httptest + 两个 client goroutine 传输随机数据并比对一致

- [ ] Task 7（Phase 2）: sclient p2p 子命令（端到端加密）
  - [ ] 新增 `p2p` 命令组与 `gen/send/recv`
  - [ ] send：读取文件 → `tunnel.EncryptStream` → POST 到 `/p2p/{id}/send`
  - [ ] recv：GET `/p2p/{id}/recv` → `tunnel.DecryptStream` → 写入 output
  - [ ] 单测：使用 httptest 的 p2p handler 进行加密传输校验

- [ ] Task 8: 验证
  - [ ] `go test ./...`
  - [ ] `go vet ./...`

# Task Dependencies
- Task 2 依赖 Task 1（peer 配置与 key 策略确定）
- Task 3 依赖 Task 1、Task 2
- Task 4 依赖 Task 3
- Task 6 依赖 Task 5
- Task 7 依赖 Task 6
- Task 8 依赖 Task 1-7

