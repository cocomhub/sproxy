# sproxy

轻量的 HTTP 文件代理与上传/下载服务。提供将请求转发到下游 HTTPS 服务的能力，同时内置基础的文件上传、下载与删除接口，并暴露带宽监控、健康检查与版本信息等运维接口。


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


## 关键路由

- `GET /healthz`：健康检查，返回 200 OK 与文本 `OK`
- `GET /version`：返回版本与构建时间
- `GET /bandwidth`：返回当前下行带宽（KB/s，整型文本）
- `POST /upload`：表单上传文件，字段名 `file`；需携带头 `X-File-MD5`
- `GET /download?filename=<name>`：下载已上传文件
- `POST /delete?filename=<name>`：删除已上传文件；需携带头 `X-File-MD5`
- `/* 代理转发 */ /{host}/{filepath...}`：将请求转发至 `https://{host}/{filepath...}`；若配置了 `allowed_hosts`，仅允许名单内主机


## 配置示例

项目支持从 YAML 载入配置，并可被命令行参数覆盖。常用字段见 `config.example.yaml`。你可以复制该文件为实际的 `config.yaml` 并按需修改。

示例片段：

```yaml
addr: ":18083"
uploads_dir: "./uploads"
tunnel_key: "1234567890abcdef1234567890abcdef1234567890abcdef1234567890abcdef"
allowed_hosts:
  - "example.com"
  - "example.org:443"
client_timeout: "30s"
server_timeouts:
  read_header: "5s"
  read: "30s"
  write: "30s"
  idle: "60s"
log_level: "info"
log_format: "text"
max_header_bytes: 1048576
```


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

## 注意

- 当 `allowed_hosts` 留空时，代理将允许访问任意主机；建议在生产环境中显式配置允许列表。
- 所有超时字段使用 Go 的持续时间语法（例如 `"30s"`、`"5m"`）。
