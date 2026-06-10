# sproxy 联邦（多机房）与 sclient 端到端加密中继（P2P Relay）Spec

## Why
当前 sproxy 以单节点文件服务 + 加密隧道为主，在多机房/多网络环境下缺少“节点间安全互通/文件搬运”的一等能力；同时 sclient 之间缺少“无需落盘在服务端”的端到端加密传输路径。

## What Changes
- Phase 1（联邦 / 多机房通道）
  - 增加 sproxy Peer 配置：用“peer 名称 → peer 的 tunnel 入口 URL +（可选）peer 专属 key”描述多机房节点。
  - 增加一组受 Bearer auth 保护的“Peer 管理/访问”API：列出 peer、透传 peer 的文件列表、在本机与 peer 之间进行文件拉取/推送（sproxy↔sproxy）。
  - sclient 增加 `peer` 子命令，调用本机 sproxy 的上述 API，实现跨机房的文件浏览与搬运入口。
- Phase 2（sclient P2P Relay）
  - sproxy 增加 P2P 中继端点：将两个 sclient 的流式连接配对并中继字节流；sproxy 不解密、不落盘。
  - sclient 增加 `p2p` 子命令：对文件流进行端到端加密（复用 pkg/tunnel 的流式加密格式），通过 sproxy 中继完成传输。

## Impact
- Affected specs: `pkg/tunnel` 流式帧协议的复用方式、`pkg/server` 路由与配置加载、`cmd/sclient` CLI 形态。
- Affected code:
  - `pkg/server/config.go`：新增 peers/p2p 配置项（含 viper/env/flag 兼容）
  - `pkg/server/handlers.go`：注册新路由（/api/peers/*, /p2p/*）
  - `cmd/sclient/*`：新增 peer/p2p 子命令
  - （新增）`pkg/peer`：对等端访问封装（基于 `pkg/tunnel.Client`）
  - （新增）`pkg/p2p`：中继会话管理（仅做连接配对与流转发）

## ADDED Requirements
### Requirement: Peer 联邦配置
系统 SHALL 支持在 sproxy 配置中声明 peer 列表，每个 peer 至少包含：
- `name`：唯一标识（用于 URL 路由）
- `tunnel_url`：对端隧道入口（例如 `https://dc2.example.com/tunnel`）
- `tunnel_key`：可选；为空时默认使用本机 `tunnel_key`

#### Scenario: 配置加载成功
- **WHEN** 提供合法的 peers 配置并启动 sproxy
- **THEN** sproxy 启动成功，且 `/api/peers` 可返回 peer 列表

#### Scenario: 配置校验失败
- **WHEN** peers 中存在重复 name、非法 URL、非法 tunnel_key（非 64 hex）
- **THEN** sproxy 启动失败并给出明确错误（指出 peer name 与字段）

### Requirement: Peer 列表 API
系统 SHALL 提供受 `authMiddleware` 保护的端点：
- `GET /api/peers`：返回已配置 peers 的最小信息（name, tunnel_url）

#### Scenario: 未授权访问
- **WHEN** 请求缺少或携带错误的 Bearer token
- **THEN** 返回与现有受保护 API 一致的未授权响应

### Requirement: 远端文件浏览（透传）
系统 SHALL 提供受 `authMiddleware` 保护的端点：
- `GET /api/peers/{name}/files?subdir=...`：通过隧道访问对端的 `GET /api/files?subdir=...` 并原样返回 JSON

#### Scenario: 远端可达
- **WHEN** peer 可达且 tunnel_key 匹配
- **THEN** 返回对端的文件列表 JSON，HTTP 状态码与对端一致（正常为 200）

#### Scenario: 远端不可达或解密失败
- **WHEN** peer 不可达 / TLS 失败 / tunnel_key 不匹配 / 对端返回异常
- **THEN** 返回 502，并在响应体中包含可诊断但不泄露敏感信息的错误摘要

### Requirement: sproxy↔sproxy 文件搬运（拉取/推送）
系统 SHALL 提供受 `authMiddleware` 保护的端点（JSON request/response）：
- `POST /api/peers/{name}/pull`：将对端的一个文件拉取到本机 uploads_dir
- `POST /api/peers/{name}/push`：将本机的一个文件推送到对端 uploads_dir

pull 请求体：
- `filename`：对端文件路径（遵循既有 `ValidateFilePath` 规则）
- `overwrite`：可选；默认 false

push 请求体：
- `filename`：本机文件路径（遵循既有 `ValidateFilePath` 规则）
- `overwrite`：可选；默认 false

#### Scenario: Pull 成功
- **WHEN** 对端存在文件且本机写入成功
- **THEN** 本机 uploads_dir 出现该文件（含中间目录），并能通过本机 `/download?filename=...` 正常下载

#### Scenario: Push 成功
- **WHEN** 本机存在文件且对端写入成功
- **THEN** 对端 `/download?filename=...` 可下载该文件，且 checksum 与本机一致

#### Scenario: 幂等/冲突
- **WHEN** overwrite=false 且目标端已存在同名文件
- **THEN** 以“已存在且 checksum 相同”为幂等成功；checksum 不同则返回冲突错误（409）

### Requirement: sclient Peer 子命令
sclient SHALL 提供面向用户的入口命令（调用本机 sproxy 的 /api/peers API）：
- `sclient peer list`
- `sclient peer ls <peer> [--subdir path]`
- `sclient peer pull <peer> <filename> [--overwrite]`
- `sclient peer push <peer> <filename> [--overwrite]`

#### Scenario: 正常调用
- **WHEN** 用户配置了 `server_url` 与 `auth_token` 且本机 sproxy 配置了 peers
- **THEN** sclient 可完成 peer 列表、远端浏览、pull/push 的调用，并输出清晰的结果信息

### Requirement: P2P Relay 会话（仅中继，不解密）
sproxy SHALL 提供受 `authMiddleware` 保护的流式端点，用于将两个客户端连接配对并中继字节流：
- `POST /p2p/{id}/send`：发送端上传流
- `GET /p2p/{id}/recv`：接收端下载流

约束：
- sproxy SHALL 不解密 payload，不解析帧内容，不写入磁盘
- sproxy SHALL 对会话施加 TTL（可配置），超时后任一侧应收到可识别的错误
- sproxy SHALL 限制并发会话数（可配置），达到上限返回 429

#### Scenario: 接收端先到
- **WHEN** recv 先连接并等待，随后 send 连接并开始发送
- **THEN** recv 端立即开始接收完整字节流，直到 send 端结束或任一侧取消

#### Scenario: 发送端先到
- **WHEN** send 先连接并等待，随后 recv 连接
- **THEN** send 端开始被读取并转发给 recv，直到结束或取消

### Requirement: sclient P2P 子命令（端到端加密）
sclient SHALL 提供 P2P 传输命令，payload 使用端到端加密（复用 `pkg/tunnel` 的流式加密格式），sproxy 仅中继：
- `sclient p2p gen`：生成 `{id, key}`（key 为 64 hex），用于分享给另一端
- `sclient p2p send --id <id> --key <hex> <file>`
- `sclient p2p recv --id <id> --key <hex> [output]`

#### Scenario: 端到端加密传输成功
- **WHEN** send 端用 key 对文件流加密并发送，recv 端用相同 key 解密并写入
- **THEN** 产出文件内容与原文件一致；sproxy 侧不需要也不可能得到明文

## MODIFIED Requirements
无。

## REMOVED Requirements
无。

