- [ ] peers 配置可通过配置文件加载并完成校验（重复 name / 非法 URL / 非法 key 会阻止启动）
- [ ] `GET /api/peers` 受 auth 保护并返回期望字段
- [ ] `GET /api/peers/{name}/files` 可透传对端文件列表，失败时返回 502 且信息可诊断
- [ ] `POST /api/peers/{name}/pull` 与 `POST /api/peers/{name}/push` 支持流式搬运、幂等与冲突语义（409）
- [ ] `sclient peer` 命令组可通过单测覆盖主要路径（list/ls/pull/push）
- [ ] P2P relay：send/recv 任意顺序到达均可完成传输，会话 TTL 与并发上限生效
- [ ] `sclient p2p` 命令组完成端到端加密传输（send 加密、recv 解密）
- [ ] `go test ./...` 与 `go vet ./...` 通过

