# WebDAV 存储后端（V3 plugin 第一个真实扩展） 设计文档

> **状态：** 已确认（2026-09-17 23:05 用户定案）
> **定案：** ① WebDAV 放 `pkg/volume/webdav/`（格式类似 ext 子包，无新依赖无需 go.mod）；② syncmgr 新增 `kind=volume`（通用本机卷，baidupcs 兼容别名）；③ Web UI backend 列表 API（动态下拉）。
> **前置：** V3 通用卷模型已合并（master `c9cb2d2b`）：volume.Volume{Type/Extra} + registry external + RegisterBackend plugin + baidupcs 系统盘 + 用户卷 + Web/CLI。
> **方向：** 用户确认——WebDAV 先行（零依赖验证 plugin 扩展性承诺），S3 后行。

## 目标

实现 **WebDAV 存储后端**：把任意 WebDAV 服务（Nextcloud / 坚果云 / OwnCloud 等）作为 sproxy 的外部卷——`sync.FS` 7 方法经 WebDAV 协议（RFC 4918）映射，经 V3 plugin（`RegisterBackend("webdav")`）接入系统盘（volumes[]）与用户卷。

**这验证 V3 核心承诺**：「未来新存储类型只 RegisterBackend 注册，不改装配核心」——baidupcs 之后的第一个真实外部后端。

## 现状（重要）

- `pkg/gateway/webdav/` 已有 **服务端网关**（`webdav.Handler`：`sync.FS` → WebDAV 协议暴露，供 curl/rsync 读写 sproxy 卷）——**反向**。
- 本任务做 **客户端后端**（WebDAV 服务 → `sync.FS`）——补足另一方向，两向共享协议知识。

## 核心设计

### 1. WebDAV 客户端（sync.FS 实现）

```
pkg/volume/webdav/（用户定案：放 pkg/volume 下，格式类似 ext 子包；无新依赖无需 go.mod）
  webdav_fs.go    // WebDAVFS：sync.FS 7 方法（HTTP 客户端）
  webdav_fs_test.go

方法映射（RFC 4918）：
  ListDir(path)   → PROPFIND depth=1 → 子条目（name/size/mtime/isdir）
  Stat(path)      → PROPFIND depth=0 → 条目（不存在 → nil）
  OpenRead(path)  → GET → io.ReadCloser
  WriteFile(path, r, size, mtime) → PUT（+ PROPPATCH mtime 可选）
  Rename(from,to) → MOVE
  Delete(path)    → DELETE（幂等：404 → OK）
  MakeDir(path)   → MKCOL（已存在 → 幂等 OK）
```

- HTTP 客户端：`&http.Client{Transport: &http.Transport{}}`（每实例独立连接池——仓库硬规则第 17 条，禁共享 DefaultTransport）
- 认证：Basic Auth（user/password）/ Bearer token（可配）
- 路径：WebDAV 根 URL + 相对路径拼接（URL 转义）

### 2. V3 plugin 接入（RegisterBackend）

```
RegisterBackend("webdav", newWebDAVBackend)

newWebDAVBackend(ctx, v volume.Volume) (registry.ExternalBackend, error)：
  v.Extra 读：url（必填，http(s)://host/webdav 根）、username/password 或 token、local_root（可选中间态）
  → WebDAVFS 构造（HTTP 客户端 + 根 URL + 认证）
  → 包装 ExternalBackend{FS() = WebDAVFS, Close() = 关 idle 连接}
```

- 系统盘：config `volumes[]` `type: webdav` + `extra: {url, username, password, local_root}`
- 用户卷：`POST /api/volumes/user` `type=webdav`（复用现有 API，无额外改动）
- 同步：`sync_remotes[].kind=baidupcs` + volume=卷名？——**kind 与 volume 类型解耦**：syncmgr 工厂查 `Set.External(volume)` 返回 FS（kind=baidupcs 名不副实？——**需确认 kind 语义**：kind 是载体（怎么到对端），volume 是存储（到哪里）；WebDAV 卷与 baidupcs 卷一样经 Set.External，kind 仍是本机卷语义。可新增 `kind=volume`（本机卷通用）或沿用 baidupcs kind + 卷类型决定——**裁决：新增 kind 通用化** 见下）

### 3. syncmgr kind 通用化（可选但推荐）

当前 `kind=baidupcs` 语义 = 「本机网盘卷」——WebDAV 也是本机卷，kind 名不副实。**推荐**：
- 新增 `kind=volume`（通用本机卷）：`remote.volume` 查 Set.External（任意类型卷）
- `kind=baidupcs` 保留为兼容别名（旧配置零迁移）

### 4. 中间态约束（用户硬规则）

WebDAV 的 staging/cache/tmp 落本地（local_root，与 baidupcs 一致）——WriteFile 先落本地 staging 再 PUT；OpenRead GET 后落本地临时文件。

## 实施拆分

| 子任务 | 内容 |
|---|---|
| V1 | WebDAV 客户端（sync.FS 7 方法 + HTTP 客户端 + 认证）+ 单测（httptest 模拟 WebDAV 服务端） |
| V2 | WebDAV backend 插件（RegisterBackend("webdav") + Extra 解析）+ config volumes[] type=webdav 校验 |
| V3 | syncmgr kind 通用化（kind=volume 或保留 baidupcs 别名） |
| V4 | e2e（真实 WebDAV 服务端 httptest + 同步任务 push/pull）+ 用户卷 type=webdav |
| V5 | Web/CLI 支持（已有 UI 下拉写死 ['baidupcs'] → 动态/含 webdav）+ 文档 |

## 影响面

- 新增 `pkg/backend/webdav/`（sync.FS 实现）
- `cmd/sproxy`（RegisterBackend("webdav") 装配）
- `pkg/server/config_validate.go`（volumes[] type=webdav 校验）
- `pkg/syncmgr`（kind 通用化，若做）
- `web/static/app.js`（type 下拉加 webdav，或动态）

## 已确认（用户定案）

1. **包位置**：`pkg/volume/webdav/`（格式类似 ext 子包；无新依赖无需 go.mod）
2. **kind 语义**：新增 `kind=volume`（通用本机卷）；`kind=baidupcs` 保留兼容别名
3. **Web UI 下拉**：服务端 backend 列表 API（GET /api/backends 动态）
