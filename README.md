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
      ├── TCP (内置，xfer/internal/tcp；含 tcp+tls)
      ├── WebSocket (xfer/ext/ws，独立子模块)
      └── QUIC / gRPC / WebRTC (xfer/ext/*，可插拔)
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

  - 覆盖配置中的监听地址与存储根目录

    ```bash
    ./build/bin/sproxy --config ./config.example.yaml --addr :18083 --storage-root ./storage
    ```


## 命令行参数

- `--version`：打印版本与构建信息后退出
- `--config <PATH>`：指定 YAML 配置文件路径（默认 `config.yaml`，不存在时使用内置默认值）
- `--addr <ADDR>`：覆盖配置中的监听地址（如 `:18083`）
- `--storage-root <DIR>`：覆盖配置中的存储根目录路径
- `--no-tls`：禁用 TLS（覆盖 `tls.enabled` 配置）
- `--allow-no-auth`：允许无认证启动（仅限本地回环调试，生产勿用）
- `dav [remote://node/vol[/path]]`：本地 WebDAV 代理子命令（`--listen` 指定监听地址，默认 `127.0.0.1:8080`）

> 隧道密钥无需配置：服务端按凭据 Ring 条目的 SK 经 HKDF 派生（详见 [docs/config.md](./docs/config.md)）。


## 关键路由

- `GET /`：自动 301 重定向到 `/ui/`（嵌入式 Web UI）
- `GET /ui/`：Web 文件管理界面
- `GET /livez`：存活探针（liveness），纯进程存活检查，返回 200 OK 与文本 `OK`
- `GET /readyz`：就绪探针（readiness），per-tenant UploadStore 全部健康才 200，否则 503
- `GET /healthz`：健康检查（兼容别名，语义等同 `/readyz`），返回 200 OK 与文本 `OK`
- `GET /version`：返回版本与构建时间
- `GET /api/audit/export`：导出审计日志（JSON 数组，按时间正序；支持 `action`/`actor`/`after_ts` 过滤）
- `POST /upload`：表单上传文件，字段名 `file`；需携带头 `X-File-Checksum`（SHA-256，hex）
- `GET /download?filename=<name>`：下载已上传文件，响应头返回 `X-File-Checksum`，**支持标准 Range header**
- `POST /delete?filename=<name>`：删除已上传文件；需携带头 `X-File-Checksum`
- `POST /rename?from=<old>&to=<new>`：重命名 / 移动文件；同样需要 `X-File-Checksum`
- `HEAD /api/files/stat?filename=<name>`：查询单文件元信息（响应头）
- `GET /api/files`：列出已上传文件，返回 `{files: [{name, size, checksum, mod_time, is_dir}, ...]}`
- `POST /tunnel`：AES-256-GCM 加密的 HTTP 请求转发（需带 SproxySig 凭据：AK/SK 在服务端凭据 Ring 登记；yaml `access_keys` 已随凭据 store 化移除，登记与轮换见 `sclient trust` / `POST /api/credentials/register`）

- **Web UI 隧道**：`web/static/sclient/` 领域库驱动页面，其经端口 `POST /tunnel`（外层 SproxySig、内层 AES-256-GCM）或直连（按配置）访问文件 API；`web.tunnel` 服务端开关（`/api/config` 下发 `web_tunnel`，默认 `true`）控制默认模式，页面「走隧道（调试）」checkbox 可即时切换并持久化（localStorage）。浏览器未填入 AK/SK（无法派生隧道密钥）时强制回落直连；服务端 `access_keys_set` 仅用于配置面板展示。


## 详细文档

更完整的参考文档位于 `docs/` 目录：

- [docs/api.md](./docs/api.md)：完整 HTTP API 参考，含请求 / 响应格式与错误码
- [docs/architecture.md](./docs/architecture.md)：分层传输架构设计（xfer / mux / tunnel / hub）
- [docs/tunnel.md](./docs/tunnel.md)：加密隧道协议规范与安全性说明（传统模式）
- [docs/config.md](./docs/config.md)：所有配置字段、优先级、SIGHUP 热重载范围、备份/恢复
- [docs/cli.md](./docs/cli.md)：sclient 全部子命令使用说明
- [CHANGELOG.md](./CHANGELOG.md)：版本变更记录


## 配置示例

项目支持从 YAML 载入配置，并可被命令行参数覆盖。常用字段见 `config.example.yaml`。你可以复制该文件为实际的 `config.yaml` 并按需修改。

示例片段：

```yaml
addr: ":18083"
storage_root: "./storage"
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

- 指定存储根目录

  ```bash
  ./build/bin/sproxy --storage-root ./storage
  ```

- WebDAV 网关（任意工具直接访问远端卷）

  把 `remote://<node>/<vol>[/<path>]` 远端卷暴露为本地 WebDAV 端点，curl / rsync / 文件管理器 / 编辑器可直接读写：

  ```bash
  ./build/bin/sproxy dav --listen 127.0.0.1:8080 remote://nodeA/main
  # 另一终端：
  curl -X PUT http://127.0.0.1:8080/hello.txt -d world
  curl http://127.0.0.1:8080/hello.txt        # → world
  rsync -av ./local/ dav://127.0.0.1:8080/    # rsync 需带 rsync:// 前缀模块映射，或用 curl/编辑器直连
  ```

  子路径起点：`remote://nodeA/main/subdir` 把 WebDAV 根对准卷内 `subdir`。

  凭据复用主配置（`--config`）的 `mesh.hub_url` / `mesh.access_key` / `mesh.access_key_secret`；
  hub 地址为空时指向本机 HTTP 面。

- 隧道密钥无需配置（已废除）

  服务端按凭据 Ring 条目的 SK 经 HKDF 派生隧道密钥；旧的 `--tunnel-key` / `tunnel_key` 已移除，
  配置该键仅历史兼容（见 [docs/config.md](./docs/config.md)）。

## Mesh 内网穿透（双重 NAT）

启用 hub 中继后，任意节点可注册并互相寻址；数据面优先 WebRTC 打洞直连、失败回落 hub 中继。

**服务端启用 hub**（`sproxy.yaml`）：
```yaml
hub:
  enabled: true
  # relay_token 已废除：注册准入由凭据 Ring 的 SproxySig AK+HMAC proof 提供
  transports:
    ws:
      enabled: true
      path: /ws
```

**节点注册 + 访问**（凭据用全局 `--access-key`/`--access-key-secret`，与服务端凭据 Ring 登记的 AK/SK 一致）：
```bash
# 中继端（出口节点）：注册 + 宣告本地服务 + 允许出口拨号
sclient relay start --hub wss://hub:18083/ws --node-id relay \
  --access-key <AK> --access-key-secret <SK> \
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
  --access-key <AK> --access-key-secret <SK> --insecure \
  --dial-allow --service app:127.0.0.1:2090
```

随后在云端节点执行：

```bash
sclient relay dial --node local --tcp 127.0.0.1:2090 \
  -s https://hub:18083 --access-key <AK> --access-key-secret <SK> --insecure
```

即可经 hub 中继写入本地端服务，数据推送由云端发起（无需本地端先发起数据流）。
`--insecure` 仅用于自签证书开发/测试环境，生产应使用真实证书。

## 注意

- 所有超时字段使用 Go 的持续时间语法（例如 `"30s"`、`"5m"`）。
- Checksum 持久化在 `<storage_root>/<tenant>/meta/checksums.json`，由 server 自动维护。
- 历史版本曾包含 `/{host}/{filepath...}` 的 HTTPS 透明转发与 `/bandwidth` 端点，已在重构中移除，定位收敛为文件服务 + 加密隧道。

## 参与贡献

- 贡献指南：[CONTRIBUTING.md](CONTRIBUTING.md)（开发环境、测试要求、提交与门禁规范）
- 安全问题：请走**私下**渠道，见 [SECURITY.md](SECURITY.md)——**不要**开公开 Issue
- 发布流程：[RELEASING.md](RELEASING.md)（CHANGELOG 由 release-please 从提交信息生成，勿手工维护）

## 许可

Apache License 2.0，见 [LICENSE](LICENSE)。
