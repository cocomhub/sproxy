# S3/WebDAV 直连接入文档（11.9-②）设计

> 日期：2026-09-24 ｜ 状态：头脑风暴（待评审） ｜ 范围：接入文档（docs/integrations/）+ 连通性自检 + 文档门禁
> 只读源码：pkg/server/s3_server.go、pkg/server/dav.go

## 1. 背景与目标

- **现状**：`/s3/`（SigV4 纯标准库验签）与 `/dav/`（RFC 4918，经 pkg/gateway/webdav）端点已完备，但无面向用户/AI Agent 的接入文档——LangChain S3Loader/WebDAV loader、rclone、aws cli 等无法直接上手。
- **目标**：产出接入文档（endpoint/凭据/示例），外部 AI 工具照文档即连；补 `POST /api/integrations/self-test` 连通性自检（可选，默认关）；文档接入既有门禁（docs/cli.md 同款正则校验，防文档漂移）。
- **非目标**：不改端点行为；不新增 S3 网关特性（列表分页/ACL/版本为 roadmap P2 残余，文档如实标注）。

## 2. 组件与接口（文档结构）

新文件 `docs/integrations/s3-webdav.md`（含配置示例，R15 门禁登记）：

```
docs/integrations/s3-webdav.md
├─ 1. 概览与能力边界（端点表 + 不支持项清单）
├─ 2. 凭据（AK = 凭据 Ring AccessKey；SK 64-hex = SecretAccessKey；
│      API 密钥 Bearer 仅 WebDAV；owner 隔离语义）
├─ 3. S3 接入（aws cli / rclone / LangChain S3Loader / 分块上传）
├─ 4. WebDAV 接入（curl / Windows 映射网络驱动器 / rclone webdav / LangChain WebDAV loader）
├─ 5. 多卷 bucket 语义（key 首段 = 卷名）
├─ 6. 常见错误排查（401/403/400/404 对照表）
└─ 7. 安全说明（路径校验防穿越、只读写自己 user 桶、凭据不落日志）
```

### 2.1 端点事实（源码核对）

- **S3**：`/s3/<key>` path-style；空 key + `?list-type=2` → ListObjectsV2（XML）；region 固定 `us-east-1`（客户端须一致）；GET=下载 / PUT=上传（201）/ DELETE=删除（204）/ HEAD=HeadObject；分块：`POST ?uploads` → `PUT ?partNumber&uploadId` → `POST ?uploadId` → `DELETE ?uploadId`（abort）；bucket=已装配卷名（`splitS3Bucket`），否则默认卷。
- **WebDAV**：`/dav/`（authMiddleware 认证）；WebDAV 根 = 请求者 owner 的 user 桶；`?volume=<卷名>` 显式选卷（ACL 校验）；标准 PROPFIND/GET/PUT/MKCOL/DELETE/MOVE/COPY。

### 2.2 示例（写入文档）

```bash
# aws cli（路径风格 + 固定 region）
aws --endpoint-url https://<host>:<port> s3 ls s3://default/ --region us-east-1
# rclone s3 remote：provider=Other，access_key_id=AK，secret_access_key=<SK 64-hex>，
# endpoint=...，force_path_style=true，region=us-east-1
# rclone webdav remote：url=https://<host>:<port>/dav/，vendor=other，user=AK，pass=<SK hex>
# curl WebDAV 列目录：curl -u AK:<SK-hex> https://<host>:<port>/dav/ -X PROPFIND -H 'Depth: 1'
```

- **LangChain S3Loader**（`S3FileLoader`/`S3DirectoryLoader`）：boto3 session 注入 `endpoint_url`（`AWS_ENDPOINT_URL`），credentials = AK/SK hex，region_name=us-east-1，bucket=卷名；示例代码段写入文档。
- **LangChain WebDAV loader**：社区 WebDAV loader（webdavclient3 系）配置 host/username/password 到 `/dav/`；文档注明「标准 RFC 4918，任何 WebDAV 客户端可用」，并给 curl 保底示例。
- 外部工具配置细节以各自 SDK 版本为准——文档标注「以实际版本验证为准」避免误导。

### 2.3 连通性自检（可选端点）

`POST /api/integrations/self-test?type=s3|webdav`（受认证，默认配置关）：以请求者凭据实际执行一次最小操作（s3: PUT 随机 key→HEAD→DELETE；webdav: MKCOL→PROPFIND→DELETE），返回逐步骤结果 JSON。目的：文档第 6 节排错表可自证，不是新增功能面。

## 3. 数据流（文档使用流程）

```
用户/AI Agent 读文档 → 配置凭据（sclient trust 或 api_keys）
  → S3: aws/rclone/LangChain loader 直连 /s3/（SigV4）
  → WebDAV: 文件管理器/loader 挂载 /dav/
  → 排错：对照文档第 6 节状态码表；仍不通 → 自检端点逐步定位
```

## 4. 错误处理（文档排错表）

| 现象 | 根因 | 处置 |
|---|---|---|
| 401 未认证 | 无 Authorization 头 | 配置 AK/SK |
| 403 认证失败 | SigV4 签名错误/凭据无效 | 核对 region=us-east-1、path-style、SK 为 64-hex |
| 400 路径非法/卷不可用 | key 含 `..` 或卷未装配 | 换默认卷或装配卷 |
| 404 文件不存在 | rel 不存在 | 先 list 确认 |
| WebDAV 405/404 | 未 strip 前缀/客户端用 virtual-host | 确认 URL 为 `/dav/` 路径风格 |

## 5. 测试 + 变异点

- **文档门禁**（internal/archcheck 新测试，R15 同款）：断言 docs/integrations/s3-webdav.md 存在且含 `/s3/`、`/dav/`、`us-east-1`、`force_path_style` 关键串（防文档被清空/漂移；**变异：删关键串 → 红**）。
- **自检端点单测**（如做）：mock handler——s3 三步全绿 / 中途失败返回含失败步骤 JSON（**变异：吞失败步骤 → 红**）；未认证 401。
- **真实连通 e2e**（可选片）：构建真实二进制 → aws cli（若本机有）/rclone 直连上传下载往返（CI 有工具则跑，无则跳过并注释）。

## 6. 片划分

| 片 | 内容 | 验收 |
|---|---|---|
| **D1** | docs/integrations/s3-webdav.md 全文 + 门禁测试 | 文档门禁绿 + 变异命中 |
| **D2** | 自检端点（可选）+ 单测 | 单测绿 |
| **D3** | rclone/aws cli 真实连通 e2e（CI 可跑才接） | e2e 绿或注明跳过 |

D2/D3 不阻塞 D1（文档先行，端点照旧）。

## 7. 风险与零回归保证

- 纯文档 + 门禁：不改任何端点代码 → 零回归（除可选 D2 新增端点，默认关）。
- 外部工具兼容风险：SigV4 实现是自研纯标准库，aws cli/rclone 互通性需真实连通验证（D3）兜底；文档注明 region/path-style 硬约束。
- 文档夸大风险：能力边界一节如实列出不支持项（ACL/分页/版本/事件通知），不承诺 roadmap 未落地功能。
- 凭据泄露风险：文档只教「如何配」，不出现真实密钥示例（示例用占位符 `<AK>`/`<SK-HEX>`）。
