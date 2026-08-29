# sproxy

轻量的文件上传/下载/删除服务，内置基于 AES-256-GCM 的加密隧道与嵌入式 Web UI；
附带 `sclient` 客户端。支持 WebSocket 持久连接、虚拟流多路复用和星型中继网络。

## 架构

```
应用层: sproxy HTTP 路由 + sclient CLI + FileClient Go SDK
  ├── hub 层: 节点注册 / 路由表 / 中继转发
  ├── tunnel 层: HTTP 请求-响应交换 (Tunnel.Do/Serve)
  ├── mux 层: 虚拟流多路复用 (Stream RWC + 心跳)
  └── xfer 层: 传输层抽象 (Conn Send/Receive)
      ├── HTTP POST (内置，向后兼容)
      ├── WebSocket (xfer/ws，独立子模块)
      └── gRPC / QUIC / ... (可插拔)
```

详细架构说明见 [docs/architecture.md](./docs/architecture.md)。


## 快速开始

- 构建

  - 使用 Makefile（推荐，自动构建 cmd 下所有命令，产物位于 build/bin）

    ```bash
    make build
    ```

  - 使用 Go 直接构建单个命令

    ```bash
    # 构建服务端
    go build -o build/bin/sproxy ./cmd/sproxy
    # 构建客户端
    go build -o build/bin/sclient ./cmd/sclient
    ```

- 运行

  - 使用示例配置启动（服务端可执行文件位于 build/bin）

    ```bash
    ./build/bin/sproxy --config ./config.example.yaml
    ```

  - 覆盖配置中的监听地址与上传目录

    ```bash
    ./build/bin/sproxy --config ./config.example.yaml --addr :18083 --uploads-dir ./uploads
    ```


## 命令行参数

- `--version`：打印版本与构建信息后退出
- `--config <PATH>`：指定 YAML 配置文件路径（默认 `config.yaml`，不存在时使用内置默认值）
- `--addr <ADDR>`：覆盖配置中的监听地址（如 `:18083`）
- `--uploads-dir <DIR>`：覆盖配置中的上传目录路径
- `--tunnel-key <HEX>`：覆盖配置中的隧道密钥（64 位十六进制）


## 关键路由

- `GET /`：自动 301 重定向到 `/ui/`（嵌入式 Web UI）
- `GET /ui/`：Web 文件管理界面
- `GET /healthz`：健康检查，返回 200 OK 与文本 `OK`
- `GET /version`：返回版本与构建时间
- `POST /upload`：表单上传文件，字段名 `file`；需携带头 `X-File-Checksum`（SHA-256，hex）
- `GET /download?filename=<name>`：下载已上传文件，响应头返回 `X-File-Checksum`，**支持标准 Range header**
- `POST /delete?filename=<name>`：删除已上传文件；需携带头 `X-File-Checksum`
- `POST /rename?from=<old>&to=<new>`：重命名 / 移动文件；同样需要 `X-File-Checksum`
- `HEAD /api/files/stat?filename=<name>`：查询单文件元信息（响应头）
- `GET /api/files`：列出已上传文件，返回 `{files: [{name, size, checksum, mod_time, is_dir}, ...]}`
- `POST /tunnel`：AES-256-GCM 加密的 HTTP 请求转发（需配置 `tunnel_key`）


## 详细文档

更完整的参考文档位于 `docs/` 目录：

- [docs/api.md](./docs/api.md)：完整 HTTP API 参考，含请求 / 响应格式与错误码
- [docs/architecture.md](./docs/architecture.md)：分层传输架构设计（xfer / mux / tunnel / hub）
- [docs/tunnel.md](./docs/tunnel.md)：加密隧道协议规范与安全性说明（传统模式）
- [docs/config.md](./docs/config.md)：所有配置字段、优先级、SIGHUP 热重载范围
- [docs/cli.md](./docs/cli.md)：sclient 全部子命令使用说明
- [CHANGELOG.md](./CHANGELOG.md)：版本变更记录


## 配置示例

项目支持从 YAML 载入配置，并可被命令行参数覆盖。常用字段见 `config.example.yaml`。你可以复制该文件为实际的 `config.yaml` 并按需修改。

示例片段：

```yaml
addr: ":18083"
uploads_dir: "./uploads"
tunnel_key: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
server_timeouts:
  read_header: "5s"
  read: "30s"
  write: "30s"
  idle: "60s"
log_level: "info"
log_format: "text"
max_header_bytes: 1048576
```


## 云端离线下载（多任务 / 任务组）

sproxy 服务端可代替客户端从外部 URL 下载文件（云端离线下载），支持单任务、批量任务与任务组：

- **单任务 / 批量**：`POST /api/cloud/download`、`POST /api/cloud/download/batch`，提交后任务进入服务端队列异步下载（客户端轮询 `GET /api/cloud/tasks` 查看进度）。
- **任务组**：`POST /api/cloud/groups` 将多个 URL 作为一个组提交，组级支持状态聚合、取消、恢复、打包归档（`GET/POST/DELETE /api/cloud/groups[/{id}...]`）。
- **断点续传**：下载写入 `.partial` 文件，网络中断/超时后自动重试续传；最终失败的任务保留 `.partial`，可通过 `POST /api/cloud/tasks/{id}/resume`（`{"force":false}` 续传 / `{"force":true}` 重下）恢复。进程重启后 `pending`/`downloading` 任务自动恢复。
- **可靠性**：默认单次尝试超时 30m、响应体空闲超时 1m、最多重试 10 次（间隔 10s）；瞬时错误（网络/5xx/超时）自动重试，4xx/SSRF 等确定性错误不重试；排队中的任务可取消；存储账本按实际大小结算，不泄漏占位空间。
- **Web UI**：云端下载弹窗含任务/组双 Tab、进度条、恢复/取消/打包按钮，输入多行 URL 可"创建组"。
- **CLI**：`sclient cloud-download`（链式）、`submit/wait/fetch/resume` 及 `group/group-list/group-archive/group-cancel/group-resume` 子命令。

相关配置键（见 `config.example.yaml`）：`cloud_max_concurrent`、`cloud_max_batch_urls`、`cloud_sync_threshold`、`cloud_download_timeout`、`cloud_download_idle_timeout`、`cloud_max_retries`、`cloud_retry_delay`、`cloud_task_ttl`、`cloud_failed_task_ttl`、`cloud_download_allow_private`、`cloud_downloader` 等。


## 典型用法

- 查看版本

  ```bash
  ./build/bin/sproxy --version
  ```

- 指定配置文件路径

  ```bash
  ./build/bin/sproxy --config ./config.example.yaml
  ```

- 指定监听地址

  ```bash
  ./build/bin/sproxy --addr :18083
  ```

- 指定上传目录

  ```bash
  ./build/bin/sproxy --uploads-dir ./uploads
  ```

- 指定隧道密钥

  ```bash
  ./build/bin/sproxy --tunnel-key "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
  ```

## Mesh 内网穿透（双重 NAT）

启用 hub 中继后，任意节点可注册并互相寻址；数据面优先 WebRTC 打洞直连、失败回落 hub 中继。

**服务端启用 hub**（`sproxy.yaml`）：
```yaml
hub:
  enabled: true
  relay_token: "<共享密钥>"
  transports:
    ws:
      enabled: true
      path: /ws
```

**节点注册 + 访问**：
```bash
# 中继端（出口节点）：注册 + 宣告本地服务 + 允许出口拨号
sclient relay start --hub wss://hub:18083/ws --token T --node-id relay \
  --service ssh:127.0.0.1:22 --dial-allow

# 本地端（访问方）：连接前自动注册；webrtc 直连优先（需对端同时跑 p2p listen），失败回落中继
sclient mesh connect ssh -l :2222      # 然后 ssh -p 2222 user@127.0.0.1
# 或直接经 hub 中继到目标节点出口
sclient relay dial --node relay --tcp 127.0.0.1:22 -l :2222
```

**云端主动推数据到本地**：`relay dial` 双向可用——本地端先 `relay start` 注册并宣告服务：

```bash
# 本地端（被访问方）：注册 + 宣告 2090 服务 + 允许出站拨号（精确放行宣告地址）
sclient relay start --hub wss://hub:18083/ws --node-id local \
  --token T --insecure --dial-allow --service app:127.0.0.1:2090
```

随后在云端节点执行：

```bash
sclient relay dial --node local --tcp 127.0.0.1:2090 \
  -s https://hub:18083 --auth-token T --insecure
```

即可经 hub 中继写入本地端服务，数据推送由云端发起（无需本地端先发起数据流）。
`--insecure` 仅用于自签证书开发/测试环境，生产应使用真实证书。

## 注意

- 所有超时字段使用 Go 的持续时间语法（例如 `"30s"`、`"5m"`）。
- Checksum 持久化在 `<uploads_dir>/.checksums.json`，由 server 自动维护。
- 历史版本曾包含 `/{host}/{filepath...}` 的 HTTPS 透明转发与 `/bandwidth` 端点，已在重构中移除，定位收敛为文件服务 + 加密隧道。
