# S3 ListBuckets（卷即桶）设计

> 设计任务 S2a-1。需求源：`.design-ledger/task-s2a.md`。源码核实：`pkg/server/s3_server.go`、`pkg/server/volumes.go`、`pkg/volume/registry/set.go`。

## 背景 / 目标

- **现状**：`GET /s3/` 空 key 且无 `list-type=2` → 400「key 不能为空」；`splitS3Bucket` 已实现「卷即桶」语义（key 首段=卷名，`volSet.ByName` 命中后把卷名经 ctx 传给 `s3TenantFor`，多卷装配下 S3 对象寻址走卷）；`GET /s3/?list-type=2` 已能列对象（但仅默认卷）。
- **目标**：`GET /s3/`（无 list-type）返回 AWS `ListBuckets`（`ListAllMyBucketsResult`）XML，枚举请求者 ACL 可见的**本地**卷名，使 rclone/aws 等 S3 客户端的 ListBuckets → ListObjectsV2 流程可发现桶。
- **明确不做**：CreateBucket / DeleteBucket（双层命名空间；卷集合由装配期/运行时外部卷机制决定，S3 侧不建卷）。

## 组件与接口

- `(h *Handlers) s3ListBuckets(w, r)`：验签（复用 `sigV4Verify`）→ owner=AK → 卷枚举 → XML 输出。
- 纯函数 `h.ownerBuckets(owner string) []string`（卷枚举，可独立单测）：
  - `volSet == nil`（旧单卷装配）→ `["default"]`（与 `s3ListObjectsV2` 的 `Name=default` 对齐，零回归）；
  - 否则 `volume.AllowedVolumes(h.volSet.All(), owner)`（ACL 视图，与 `routeUpload` 同源）∩ `{v: h.volSet.Root(v.Name) != nil}`（外部卷无本地根、S3 不可寻址，不列，避免广告不可用桶），按声明序返回卷名。
- 纯函数 `s3BucketsXML(owner string, buckets []string) string`：`ListAllMyBucketsResult`（Owner.ID=owner + Buckets/Bucket）；卷名经 `xmlEscapeText`（复用现有转义，防 XML 注入，与 `s3ContentsXML` 同规）。省略 `CreationDate`（registry 无卷创建时间；rclone / aws CLI 均容忍缺省）。
- `s3Handler` 路由：在现有 `list-type==2` 分支**之后**、空 key 400 分支**之前**插入：`r.Method == GET && key == "" && 无 list-type && 无 uploads/partNumber/uploadId` → `s3ListBuckets`。HEAD 空 key 保持 200（桶存在性，不动）。

## 数据流

1. S3 客户端 `GET /s3/`（SigV4 签名，Credential=AK；SK=凭据 Ring 中 AK 的 64-hex）。
2. `s3Handler`：`TrimPrefix("/s3/")` → key=""；无 list-type → `s3ListBuckets`。
3. `sigV4Verify`：AK → ring 查 SK → hmac 重算签名（常数时间比对）→ 成功返回 AK（即 owner）。
4. `ownerBuckets(owner)`：ACL 视图 ∩ 本地根卷 → 卷名列表。
5. 响应：`Content-Type: application/xml` + `s3BucketsXML`。

## 错误处理

- 无 Authorization → 401「s3: 未认证」；签名失败/AK 无效/ring nil → 403「s3: 认证失败」（与 `s3Handler`/`s3ListObjectsV2` 完全一致）。
- `volSet == nil` 是合法旧装配形态，**不是错误** → 返回 `default` 桶。
- 非 GET（PUT/DELETE `/s3/`）→ 维持现有空 key 分支 400（不改方法语义）。
- XML 无错误路径；卷名仅来自 Config.Validate 已约束的配置值，仍统一 `xmlEscapeText`（纵深防御）。
- SigV4 canonicalRequest 用 `r.URL.EscapedPath()`（根路径空串），签名侧无需任何改动（与 `s3ListObjectsV2` 同构，客户端签名一致即可）。

## 测试 + 变异点

服务端集成测试（httptest + 构造 SigV4 签名请求，模式对齐现有 s3 测试）：

1. 多卷装配（2 本地卷 + 1 外部卷后端）：`GET /s3/` → 200，XML 含可见本地卷名；**不含**外部卷与 ACL 拒绝卷。
2. 空可见卷（owner 全部卷被 ACL 拒绝）→ `<Buckets></Buckets>`（空列表，非 400）。
3. `volSet == nil`（旧装配路径）→ 单 `Bucket Name=default`。
4. 零回归：`GET /s3/?list-type=2` 仍走 `s3ListObjectsV2`；`GET /s3/` 由 400 → 200（无调用方依赖 400）；HEAD 空 key 仍 200。
5. 未认证 401 / 错签名 403。

变异命中（每个断言先变红再实现，TDD）：

- 去掉 `AllowedVolumes` ACL 过滤 → 测试 1 红（拒绝卷泄漏）。
- 不排除外部卷（`Root(name)==nil` 不过滤）→ 测试 1 红（列出不可寻址桶）。
- 固定返回 `["default"]`（枚举被删）→ 测试 1 红（多卷发现失效）。
- 空列表返回 400 或静态桶 → 测试 2 红。

## 片划分

- **片 1**：红灯测试（上述矩阵）→ 实现 `s3ListBuckets` + `ownerBuckets` + `s3BucketsXML` + 路由分支。
- **片 2**：变异验证（4 处全命中）+ `go test ./pkg/server/... -race` + `make lint` 全绿。
- **片 3**：docs 补 S3 ListBuckets 说明（随代码 PR 带上，不单开纯文档 PR）。

## 风险与零回归

- **零回归保证**：新分支仅在 `GET + 空 key + 无 list-type/分块参数` 时生效（此前恒 400），其余分支（list-type=2、分块上传、HEAD 空 key、普通 key 的 GET/PUT/DELETE）逐分支不变；现有 s3 测试全量回归。
- **风险 1**：严格 S3 客户端可能要求 `CreationDate` 字段——aws CLI / rclone 实测容忍缺省；若未来需补，可取卷根目录 mtime（fallback，单点函数）。
- **风险 2**：无尾斜杠 `/s3` 不命中 `/s3/` 前缀路由（现状如此）——文档注明，不改路由。
- **风险 3**：外部卷不列是设计决策（不可寻址）；未来外部卷 S3 可寻址时仅需改 `ownerBuckets` 单点。
