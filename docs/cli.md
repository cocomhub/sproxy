<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# sclient 命令行参考

sclient 是 sproxy 的配套客户端，基于 cobra + pflag。所有命令均支持
`--config`、`--server`、`--tunnel-key` 等全局参数。

## 全局选项

| 选项 | 默认值 | 说明 |
|---|---|---|
| `--config` | XDG 路径 | 指定客户端配置文件路径 |
| `--server` | `https://127.0.0.1:18083` | sproxy 服务端地址（覆盖 server_url 配置） |
| `--tunnel-key` | (空) | 启用 tunnel 模式；64 位 hex AES-256 密钥 |
| `--no-checksum` | false | 跳过 SHA-256 校验（不推荐） |

## 子命令一览

| 命令 | 用途 |
|---|---|
| [`upload`](#upload) | 上传文件 |
| [`download`](#download) | 下载文件 |
| [`delete`](#delete) | 删除文件 |
| [`mv`](#mv) | 重命名 / 移动 |
| [`stat`](#stat) | 查询文件元信息 |
| [`list`](#list) | 列出文件 |
| [`mkdir`](#mkdir) | 创建目录 |
| [`rmdir`](#rmdir) | 删除目录 |
| [`search`](#search) | 搜索文件 |
| [`batch-delete`](#batch-delete) | 批量删除文件 |
| [`batch-rename`](#batch-rename) | 批量重命名文件 |
| [`cd`](#cd) | 切换当前目录 |
| [`pwd`](#pwd) | 打印当前目录 |
| [`tunnel`](#tunnel) | 通过隧道发送任意 HTTP 请求（`--xfer <name> --hub <addr>` 走 xfer/mux 隧道，启用身份指纹 pinning） |
| [`identity`](#identity) | 节点长时身份密钥管理（Ed25519，供对端指纹 pinning） |
| [`relay`](#relay) | 中继节点：连接到 Hub，转发请求到本地 HTTP 服务 |
| [`genkey`](#genkey) | 生成 tunnel_key |
| [`config`](#config) | 配置管理 |
| [`version`](#version) | 打印版本信息 |

## 当前目录概念

sclient 维护一个**持久化的工作目录**（存于 XDG cache），影响所有以相对路径
传入的子命令。

- `sclient cd sub/dir` → 后续 `upload a.txt` 实际目标是 `sub/dir/a.txt`
- `sclient cd /` → 回到根目录
- `sclient cd ..` → 返回上级
- `sclient pwd` → 打印当前目录
- 使用 `/` 开头的**绝对路径**可以绕过当前目录（例如 `sclient upload /shared/file.txt`）
- 包含 `..` 的相对路径在**客户端**就被拒绝（与服务端 ValidateFilePath 对称），
  无需向服务端发送注定失败的请求

## 子命令详情

### upload

```bash
sclient upload <file1> [file2...]
sclient upload --chunked --concurrency 8 large.bin
```

- 自动判断是否启用分块上传（>100 MiB）
- 文件路径中的目录结构会被保留：`sclient upload dir/file.txt` → 服务端 `dir/file.txt`
- 支持 `--chunked` 强制开启分块、`--chunk-size`、`--concurrency`、`--resume`

### download

```bash
sclient download <filename> [output]
sclient download --chunked --concurrency 8 large.bin
```

- 默认走 `GET /download`（支持标准 Range header）
- `--chunked` 启用并发分块下载（走 `/download/chunk`）
- 不指定 output 时使用原文件名

### delete

```bash
sclient delete <filename>
```

每次仅接受一个参数。删除前会本地计算文件 SHA-256 用于服务端校验。

### mv

```bash
sclient mv <from> <to>
```

- 重命名或移动远端文件
- 先 `Stat(from)` 获取 checksum，再 `Rename(from, to, checksum)`
- 目标父目录不存在时服务端自动创建
- 目标已存在时返回错误

### stat

```bash
sclient stat <filename>
```

输出文件 size、checksum、mod_time。不下载内容。

### list

```bash
sclient list                # 列当前目录
sclient list --subdir dir1  # 列指定子目录
```

### mkdir

```bash
sclient mkdir <dirname>
```

创建子目录（递归），类似 `mkdir -p`。

### rmdir

```bash
sclient rmdir <dirname>
sclient rmdir --force <dirname>
```

非空目录在没有 `--force` 时会有交互式确认提示。

### cd / pwd

见上文"当前目录概念"。

### tunnel

```bash
sclient tunnel <url>
sclient tunnel -X POST -H "Content-Type: application/json" -d '{"k":"v"}' <url>
# xfer/mux 隧道模式（启用身份指纹 pinning，见下）
sclient tunnel --xfer tcp --hub 127.0.0.1:18090 <url>
```

通过加密隧道发送任意 HTTP 请求。可用于调试或转发到其他服务。

**xfer/mux 隧道模式（P1 身份 pinning）**：加 `--xfer <name> --hub <addr>` 走
xfer/mux 隧道（如 `tcp`、`ws`）。该模式在 ECDH 握手时交换 Ed25519 长时身份并做
对端指纹 pin 校验：

- 本端身份：`sclient identity generate` 生成（XDG 目录 `sproxy/identity.json`）；
- 对端 pin：配置 `peer_fingerprints`（`sclient config set peer_fingerprints <fp>`，
  多个逗号分隔；对端指纹取 `sclient identity fingerprint` 带外固化）；
- 需配置 `access_key`/`access_key_secret`（派生隧道密钥使握手执行；缺 key 但配置了
  身份/指纹时 fail-closed 报错，不静默降级）；
- fail-closed：pin 不匹配或对端无身份即拒绝；传统隧道（未加 `--xfer`）与文件直连
  命令不做身份交换，配置 `peer_fingerprints` 会 fail-closed 报错并指引使用 `--xfer`。

> **接线现状（N-3）**：`--xfer` 当前对接的是 **xfer/mux listener**（测试或自定义
> 服务端，如 `sclient relay`/`mesh node` 建立的自定义隧道对端）；**真实 sproxy
> hub/relay/mesh 节点的数据面协议尚不兼容**——服务端 xfer tunnel listener 待后续
> 接线，生产环境请勿把 `--xfer` 指向尚未提供该协议的地址（示例中的
> `127.0.0.1:18090` 仅为示意）。

### identity

```bash
sclient identity generate [--file <path>] [--force]   # 生成并持久化节点身份密钥，打印指纹
sclient identity show [--file <path>]                 # 展示本节点身份指纹与公钥
sclient identity fingerprint [--file <path>]          # 仅打印指纹（供脚本/复制）
```

管理节点长时身份（Ed25519，P1 身份 pinning）。身份文件默认存 XDG 配置目录
`sproxy/identity.json`（`--file` 覆盖）。`generate` 已存在时报错（`--force` 覆盖）；
`show`/`fingerprint` 在文件缺失/损坏时返回非 0 退出码并提示恢复路径。

### relay

```bash
# 作为中继节点连接到 Hub
sclient relay --hub ws://hub.example.com/ws --local http://127.0.0.1:8080 --node-id my-node
```

作为中继节点连接到 Hub，注册自身节点标识，然后等待远程请求并通过隧道转发到本地
HTTP 服务。适用于跨网络服务暴露、集群间请求转发等场景。

**参数：**

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--hub` | `ws://127.0.0.1:18084/ws` | Hub 的 WebSocket 地址 |
| `--local` | `http://127.0.0.1:8080` | 本地 HTTP 服务地址 |
| `--node-id` | 时间戳 | 节点唯一标识 |

**工作原理：**

1. 通过 WebSocket 传输层连接到 Hub
2. 创建 mux 多路复用连接（Listener 角色）
3. 在控制流上发送 NodeID 完成注册
4. 使用 `Tunnel.Serve` 接受中继请求
5. 每个请求通过 `http.Client` 转发到本地 `--local` 地址
6. 响应通过隧道原路返回

### genkey

```bash
sclient genkey
```

打印新的 64 位 hex AES-256 密钥（不写入配置文件）。

### config

```bash
sclient config                       # show（同 show）
sclient config show
sclient config set server_url http://proxy:18083
sclient config set tunnel_key <64hex>
```

### version

```bash
sclient version
```

打印 sclient 版本、配置文件路径、生效的 server / tunnel_key 摘要。

### search

```bash
sclient search <keyword>
```

- 递归搜索文件名中包含 `<keyword>` 的文件（不区分大小写）
- 关键字为空时返回空列表
- 输出格式与 `list` 相同，包含 name、size、checksum、mod_time、is_dir

### batch-delete

```bash
sclient batch-delete <file1> [file2...]
```

- 批量删除多个文件，一次调用减少网络往返
- 每个文件先本地计算 SHA-256 再发送删除请求
- continue-on-error：部分文件删除失败不影响其他文件
- 输出每个文件的操作结果（成功/失败及原因）

### batch-rename

```bash
sclient batch-rename <from1> <to1> [from2 to2...]
```

- 批量重命名/移动多组文件
- 参数必须成对出现：源路径和目标路径交替排列
- 每组操作前自动获取源文件 checksum
- continue-on-error：部分操作失败不影响后续
- 输出每个操作的结果（成功/失败及原因）

## 常见错误排查

| 现象 | 可能原因 |
|---|---|
| `路径包含父级引用 '..'` | 客户端预拦截了不安全路径，去掉 `..` 或用绝对路径 |
| `tunnel error (HTTP 403)` | 服务端 `tunnel_key` 为空 / 启动失败，检查 sproxy 日志 |
| `tunnel error (HTTP 400)` | 隧道密钥与服务端不一致，或网络中间层破坏了请求体 |
| `unauthorized` (401) | SproxySig 签名缺失/非法/过期（服务端配置了 `access_keys` 时），检查 `~/.config/sproxy/sclient.yaml` 的 `access_key`/`access_key_secret` 是否与服务端一致 |
| `源文件 SHA-256 校验失败` | mv 期间本地文件已变，刷新本地 checksum 后重试 |
| `文件已存在但 checksum 不匹配` (409) | 服务端已有同名文件且内容不同，先 mv 或 delete |
