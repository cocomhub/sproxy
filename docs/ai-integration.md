# AI 集成指南

> 面向 AI 客户端 / Agent / 自动化流水线接入 sproxy 的完整指南：MCP server（标准工具协议）、
> S3/WebDAV 协议直连（LangChain 等语料读取）、事件流 SSE（文件变更感知）。
> 对应路线图 [roadmap.md](./roadmap.md) §11.9「AI 接入规划」。

## 1. MCP server（11.9-①，已落地）

`sproxy-mcp` 是 MCP（Model Context Protocol）server 二进制：把 sproxy 文件能力暴露为
MCP 工具，供 AI CLI（Claude Code / Codex 等）调用。协议层为手写 JSON-RPC 2.0
（`pkg/mcp`，stdlib），经 FileClient 薄封装调用 sproxy HTTP API（SproxySig 签名，
服务端零改动）。

### 1.1 启动参数

```bash
sproxy-mcp --server https://127.0.0.1:18083 \
  --access-key <AK> --access-key-secret <SK> --access-key-id <skey-id> \
  [--volume docs] [--transport stdio|sse] [--sse-addr :18900]
```

| flag | 说明 |
|------|------|
| `--server` | sproxy 服务端地址（**必填**） |
| `--access-key` / `--access-key-secret` / `--access-key-id` | SproxySig 凭据三要素（全部省略时走服务端免认证场景） |
| `--volume` | 可选卷上下文（缺省 auto） |
| `--transport` | 传输方式：`stdio`（默认，本地 AI CLI）或 `sse`（远程 HTTP） |
| `--sse-addr` | SSE 传输监听地址，默认 `:18900`（`--transport=sse` 时生效） |

### 1.2 stdio 传输（本地 AI CLI 默认）

`--transport=stdio`（默认）：经标准输入/输出按 MCP v1 单行 JSON 帧交换消息，
直接供本地 AI CLI 调用。

```bash
./build/bin/sproxy-mcp --server https://127.0.0.1:18083 \
  --access-key <AK> --access-key-secret <SK> --access-key-id <skey-id>
```

### 1.3 SSE 传输（远程 HTTP）

`--transport=sse`：监听 `--sse-addr`（默认 `:18900`）：

- `GET /sse` — 建立事件流（首帧下发 endpoint `/messages?sessionId=<id>`）
- `POST /messages` — 收 JSON-RPC 请求 → 同分派管线 → 事件流回 message 事件

**Bearer 认证**：SSE 端点复用 `--access-key-secret` 作为 Bearer token（常量时间比较，
未提供/错误统一 401 空 body；SK 为空 = 公开，仅限本地/内网部署形态）。

```bash
./build/bin/sproxy-mcp --server https://127.0.0.1:18083 \
  --access-key <AK> --access-key-secret <SK> --access-key-id <skey-id> \
  --transport=sse --sse-addr :18900
```

MCP 客户端配置示例：

```json
{
  "mcpServers": {
    "sproxy": {
      "type": "sse",
      "url": "http://127.0.0.1:18900/sse",
      "headers": { "Authorization": "Bearer <SK>" }
    }
  }
}
```

### 1.4 工具清单（9 个）

| 工具 | 说明 |
|------|------|
| `read_file` | 读文件内容（有大小上限防 OOM） |
| `write_file` | 写文件（单次直传上限内；超大走分块管线） |
| `list_files` | 列出目录文件 |
| `search` | 按文件名搜索 |
| `stat` | 单文件元信息 |
| `mkdir` | 创建目录 |
| `delete` | 删除文件（需先 `stat` 取 checksum 校验防误删） |
| `share_create` | 创建分享链接 |
| `cloud_download_create` | 创建云端下载任务 |

## 2. S3/WebDAV 直连（11.9-2，LangChain 等读取语料）

AI 框架（LangChain 等）可通过标准 S3 / WebDAV 协议直接读取 sproxy 存储中的语料，
无需走 sproxy 私有 API。

### 2.1 S3 端点（`/s3/`）

- **路径风格**：`/s3/<key>`（path-style）；`region` 固定 `us-east-1`（客户端必须一致）。
- **认证**：AWS SigV4（`Authorization: AWS4-HMAC-SHA256`）；AccessKey = sproxy 凭据 AK，
  SecretAccessKey = 对应 SK（64-hex）；owner 隔离（AK 的 owner 即请求者，路径相对其 user 桶）。
- **端点**：GET=下载 / PUT=上传（201）/ DELETE=删除（204）/ HEAD=HeadObject；
  `GET /s3/?list-type=2`=ListObjectsV2（XML）；`GET /s3/`=ListBuckets。
- **多卷**：key 首段为已装配卷名（`/s3/<vol>/<key>`），否则默认卷。
- **分块上传**：`POST ?uploads` → `PUT ?partNumber&uploadId` → `POST ?uploadId`（complete）→
  `DELETE ?uploadId`（abort）。

**LangChain S3Loader 配置示例**（`S3FileLoader` / `S3DirectoryLoader`，boto3 session 注入
endpoint_url）：

```python
import boto3
from langchain_community.document_loaders import S3DirectoryLoader, S3FileLoader

session = boto3.Session(
    aws_access_key_id="<AK>",
    aws_secret_access_key="<SK-HEX>",   # sproxy 凭据 SK（64-hex）
    region_name="us-east-1",            # 兼容端点固定 region，必须一致
)

# bucket = 卷名（多卷时 key 首段为卷名，缺省默认卷）
loader = S3DirectoryLoader(
    bucket="default",
    prefix="corpus/",
    endpoint_url="https://<host>:<port>",   # sproxy 地址（不经 /s3/ 前缀）
    aws_access_key_id="<AK>",
    aws_secret_access_key="<SK-HEX>",
    region_name="us-east-1",
)
docs = loader.load()
```

**aws cli 直连示例**：

```bash
aws --endpoint-url https://<host>:<port> \
  s3 ls s3://default/corpus/ --region us-east-1
```

> 注：SigV4 验签为自研纯标准库实现；外部工具配置细节以各自 SDK 版本为准，region/path-style
> 是硬约束（region 必须 `us-east-1`、必须 path-style 而非 virtual-host）。

### 2.2 WebDAV 端点（`/dav/`）

- **启用**：配置 `webdav.enabled: true`（默认 false 不装配）。
- **认证**：经 `authMiddleware`（SproxySig 或 api_keys Bearer，见 §4）。
- **根**：WebDAV 根 = 请求者 owner 的 user 桶；`?volume=<卷名>` 显式选卷（ACL 校验）。
- **协议**：标准 RFC 4918（PROPFIND / GET / PUT / MKCOL / DELETE / MOVE / COPY），
  任意 WebDAV 客户端可用。

**LangChain WebDAV loader 配置示例**（社区 loader，webdavclient3 系；标准 RFC 4918，
以实际版本为准）：

```python
from langchain_community.document_loaders import WebDAVLoader  # 或社区等价实现

loader = WebDAVLoader(
    host="https://<host>:<port>/dav/",   # sproxy /dav/ 端点
    username="<AK>",
    password="<SK-HEX>",
)
docs = loader.load()
```

**curl 保底示例**：

```bash
# 列目录（PROPFIND, Depth: 1）
curl -u <AK>:<SK-HEX> -X PROPFIND -H 'Depth: 1' https://<host>:<port>/dav/

# 上传
curl -u <AK>:<SK-HEX> -T ./corpus.txt https://<host>:<port>/dav/corpus.txt

# 下载
curl -u <AK>:<SK-HEX> -O https://<host>:<port>/dav/corpus.txt
```

## 3. 事件流 AI 流水线（11.9-3）

AI agent / 自动化流水线经 `GET /api/events` SSE 感知文件变更（新增 → 触发处理），
实现事件驱动的 AI 处理流水线。路由注册：`pkg/server/routes.go:574`（隧道内层裸注册，
隧道加密即认证）与 `:889`（主 mux，经 `authMiddleware`）。

### 3.1 端点与事件格式

```
GET /api/events?owner=<owner>
```

- 响应：`Content-Type: text/event-stream`；每事件 `id:<cursor>\ndata:<json>\n\n`，
  data JSON = `{cursor, action, owner, rel, size?}`（`rel` 相对 user 桶路径）。
- 动作覆盖：`upload` / `rename` / `delete` / `mkdir` / `rmdir`（文件写路径）、
  `version`（版本恢复/删除，restore 时 `size`=恢复后文件大小，delete 时 `size`=0）、
  `share`（分享链接创建，`size`=分享文件大小；载荷**不含 token/password**）。
- `owner` 参数可选：缺省取请求认证 actor（隧道模式需显式传）。
- **重连回放**：`Last-Event-ID` 头从指定游标之后回放（游标单调递增，缓冲容量
  1000 条/owner；落后过多滚出缓冲则回放失败——客户端应全量刷新兜底）。
- 订阅者慢消费（chan 满）会被断开，客户端重连可回放。

### 3.2 curl 示例

```bash
curl -N -H 'Authorization: Bearer <API-KEY>' \
  https://<host>:<port>/api/events?owner=alice
# 输出示例：
# : connected
#
# id: 42
# data: {"cursor":42,"action":"upload","owner":"alice","rel":"corpus/doc1.txt","size":2048}
#
```

### 3.3 python SSE 客户端示例（监听文件变更 → 触发处理）

```python
import json
import requests

headers = {"Authorization": "Bearer <API-KEY>"}
r = requests.get(
    "https://<host>:<port>/api/events",
    headers=headers,
    stream=True,
)
last_id = None
for line in r.iter_lines():
    if line is None:
        continue
    text = line.decode("utf-8")
    if text.startswith("id:"):
        last_id = text[3:].strip()
    elif text.startswith("data:"):
        ev = json.loads(text[5:])
        print(f"[{ev['action']}] {ev['rel']} (size={ev.get('size', '-')})")
        # 示例：新增语料 → 触发下游处理（向量化 / 通知 LLM）
        if ev["action"] == "upload":
            process_new_document(ev["rel"])
# 断线重连：带上 Last-Event-ID 回放断点后的事件
r = requests.get(
    "https://<host>:<port>/api/events",
    headers={**headers, "Last-Event-ID": str(last_id)} if last_id else headers,
    stream=True,
)
```

## 4. 认证说明

sproxy 主 mux 支持两种认证模式（除 `/livez` `/readyz` `/healthz` `/version` `/ui/`
`POST /tunnel` 之外的所有路由都要求认证）：

### 4.1 SproxySig 请求签名（默认，凭据 Ring）

当服务端凭据 Ring 非空时（首启自动登记 anonymous 凭据，见 `sclient trust` /
`/api/credentials`），请求头携带 `Authorization: SproxySig v=2 ...`（AccessKey /
AccessKeySecret + HMAC-SHA256 请求签名；AK/SK 由 `sclient trust ak add` 生成注册，
`sclient trust renew` 轮换 SK）。

```bash
curl -H 'Authorization: SproxySig v=2 <完整签名>' https://<host>:<port>/api/files
```

- 命令行访问：用 `sclient`（自动签名）或 `sproxy-mcp --access-key ... --access-key-secret ...`
- 程序访问：用 `pkg/client` FileClient（`client.WithAccessKey(ak, sk)`）

### 4.2 Bearer API key（多用户模式）

配置 `api_keys.enabled: true` + `api_keys.keys`（`key` + `permission`：
`read` 只读 / `write` 读写），请求头携带 `Authorization: Bearer <key>`：

```yaml
api_keys:
  enabled: true
  keys:
    - key: <API-KEY>
      permission: read   # 只读（GET/HEAD）
    - key: <API-KEY-2>
      permission: write  # 读写（全部操作）
```

```bash
curl -H 'Authorization: Bearer <API-KEY>' https://<host>:<port>/api/files
```

> 注意：`api_keys` 与凭据 Ring（SproxySig）是**互斥优先**的独立特性——`api_keys.enabled`
> 时 Ring 仍装配（hub 准入与隧道派生仍需 AK/SK），但主 mux 认证链优先走 Bearer 分支。
> WebDAV（`/dav/`）两种凭据均可；S3（`/s3/`）固定走 SigV4（AK/SK）。
