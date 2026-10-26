# 文件元数据 extra 完整方案（读写闭环 + 全卷覆盖）

> 日期：2026-10-26 ｜ 现状基线：master `904f114f9`（#851/#852 已合并）
> 触发：#851 只有「写」无「读」；`meta set` 500（#855 已修路由组清单）；Web UI / 全卷场景未覆盖。

## 1. 现状盘点（已核实）

| 能力 | 现状 | 缺口 |
|---|---|---|
| 写：随上传（单次/分块 init） | ✅ #851：`X-File-Meta-Extra` 头 / init body `meta_extra` | — |
| 写：补录/改 | ✅ `PUT /api/files/meta`（#855 修 500 后可用） | — |
| **读：查看 extra** | ❌ **无任何端点/SDK/CLI/UI** | **核心缺口** |
| 合并语义 | secretdata=`maps.Copy`（**合并**）；trusted=`maps.Copy`（**合并**）；filesMetaPolicy `applyMetaExtra`（**覆盖**，上传路径） | **语义不一致**：上传=覆盖、补录=合并 |
| 覆盖语义 | 无「删键/重置」能力 | 合并无法删除旧键 |
| 全卷 | 加密卷（secretdata）✅ Provider+Updater；本地卷（trusted Wrap）✅；外部卷（baidupcs 裸 FS）⚠️ 无 meta；去重/封装卷 ⚠️ 待核 | 裸外部卷无 sidecar |

### 1.1 读路径现状

- `pkg/server/external_source.go:fileMetaReaderFor` 已有 FileMeta 读取闭包（meta.Provider 探测：secretdata Provider / trusted Wrap 装饰器）——**读侧完整性校验在用，但未暴露查询 API**。
- 各 FS 装饰器（CapacityFS/PrefixFS/davMetaFS/quotaLocalFS/StagingQuotaGateFS/trusted）都实现 `FileMeta` 转发 + `UpdateMetaExtra` 转发（R21 能力链完整）。
- 本地卷（LocalFS）**无** `FileMeta`/`UpdateMetaExtra` 实现——由 `trusted.Wrap` 装饰器提供（写侧恒生成 sidecar）。

### 1.2 写路径的 extra 承载（已核实）

- 单次上传：`pkg/files/write.go` 解析 `X-File-Meta-Extra` → `input.MetaExtra` → `writeTargetSettleCommon` → `writeMetaSidecar`（本地卷 root meta 桶）。
- 分块上传：init body `meta_extra` → `session.MetaExtra`（持久化 session.json）→ complete 后 `writeMetaSidecar`（**仅本地卷目标**）；**外部卷目标**（encrypted/baidupcs）走 `completeExternalTransfer` → `sink.WriteFile(ctx, rel, f, size, mtime)` —— **`UploadSink.WriteFile` 签名无 extra 参数** ⇒ **外部卷分块上传的 meta_extra 被丢弃** ✗。
- 单次上传到外部卷：`externalUploadSink.WriteFile` 同样无 extra ⇒ `trusted.Guard(Wrap)` 写侧生成 meta 但 **不含上传声明的 extra** ✗（applyMetaExtra 只在本地卷 `filesMetaPolicy` 路径）。

### 1.2b 定位缺口（外部卷 meta 读/写实测不可达）

`UpdateMeta`（补录）用 `s.locateForRead(owner, rel, "")` 定位——该函数**只扫本地 `*storage.Root`**
（`volumeFileExists` = `vs.Root(name).Stat`）：

- 本地卷 ✅ 命中；
- **外部卷（secretdata/baidupcs/s3）`: `Root` 为 nil、`Set.Tenant` 返 nil（`HasLocalRoot` false）
  ⇒ `locateForRead` **恒未命中** ⇒ `UpdateMeta` 直接返「目标文件不存在或不可定位」✗
  ⇒ **`meta set` 到加密卷/网盘卷根本无法工作**（不只是路由 500）。

正确模型（已有先例）：**外部卷定位必须走 `ExternalVolumeOps`**（`externalVolOps()` 接缝）——
`copyWithinVolume`（pkg/files/copy_ops.go:122）与 `listExternalVolume`（read_ops.go:195）都是
「`HasLocalRoot` 为 false ⇒ 走 `ops.ExternalStat`/`ops.ExternalList`」的双路模型。meta 读/写
必须补齐同构双路。


### 1.3 已知语义矛盾（用户明示：**默认新增而非覆盖**）
- **上传路径**：`applyMetaExtra(fm, extra)` 直接 `fm.Extra = extra`（**覆盖**）——第二次上传同文件会丢弃旧 KV。
- **补录路径**（UpdateMetaExtra）：`maps.Copy`（**合并**）——新增/更新键，保留旧键。
- **用户裁定**：常规默认应**新增（合并）**，覆盖是显式动作。⇒ 上传路径的 `applyMetaExtra` 改合并。

## 2. 目标设计

### 2.1 语义（用户明示 + 补齐）

| 操作 | 语义 | 说明 |
|---|---|---|
| 上传（单次/分块）带 `meta_extra` | **合并**（默认新增，不覆盖旧键） | `applyMetaExtra` 改 `maps.Copy` |
| `meta set --extra`（补录） | **合并** | 现状已合并 ✓ |
| `meta set --extra`（显式覆盖同名键） | **合并的天然行为**（同名键新值覆盖旧值） | 与「新增」同一实现 |
| `meta reset`（清空/删键） | **显式覆盖**：`--replace` 时整表替换；`--delete key` 删单键 | 新增子命令/flag |
| 读：`meta get <file>` | 输出 extra 全部键值（text/json） | 新增 |

**「覆盖」的两种显式形态**：
- `sclient meta set <file> --extra='{...}' --replace`：整表替换（删除旧键）。
- `sclient meta set <file> --delete=key1,key2`：删指定键（保留其余）。

（实现：`PUT /api/files/meta` body 加 `mode: "merge"|"replace"|"delete"` + `keys: [...]`；服务端在 UpdateMetaExtra 层收敛。）

### 2.2 读端点（新）

`GET /api/files/meta?filename=<rel>` → `200 {extra: {k:v}}` / `404`（文件不存在或无 meta）。

- 定位：复用 `UpdateMeta` 的 `locateForRead` → `locateOwnerFile`（全卷路由：本地/加密/外部/封装）。
- 读取：按卷 FS `meta.Provider` 探测（`fileMetaReaderFor` 同链）→ `FileMeta.Extra`（深拷贝防污染）。
- **无 meta 能力/无 sidecar**：返回 `200 {extra: {}}`（读侧零校验语义：sidecar 缺失 = 无 extra，不报错）——与 `warnDegradeDebug` 的「缺失 = 直通」一致。
- 路由：`isFileGroupedRoute` + `isReadOnlyFileRoute`（GET）补成员（#855 教训三处同源）。
- 校验：meta.TrimExtra/ValidateExtra 读侧不做（写侧已校验；读侧只展示）。

### 2.3 SDK（新）

`FileClient.GetFileMeta(ctx, filename) (map[string]any, error)` → GET 端点，解码 `extra`。

### 2.4 CLI（改/新）

- `sclient meta <file>`（现有查询命令）：**默认输出 extra**（text：`extra.<key>: <value>` 每行；json：并入）。
- `sclient meta get <file>`：等同查看（可省略，与主命令合并）。
- `sclient meta set <file> --extra=... [--replace]`：合并（默认）/整表替换（--replace）。
- `sclient meta set <file> --delete=k1,k2`：删键。

### 2.5 Web UI（新，需 Playwright e2e）

- 文件列表/详情：显示 extra（KV 徽章或详情区）。
- 编辑：详情面板 meta 编辑框（JSON textarea + 保存 → PUT merge）。
- 上传：`meta_extra` 可选 JSON 输入框（upload.js → chunkedUpload 已透传 `p.metaExtra`，接 UI）。

### 2.6 全卷覆盖（含外部卷分块上传丢 extra 修复）

**关键缺口修复**：`UploadSink.WriteFile` 签名加 extra 承载（R2 门禁：files 域不直接 import pkg/sync ⇒ 走 ctx）：

```
sink.WriteFile(ctx, rel, f, size, mtime)
→ ctx 经 syncpkg.WithUploadHints{...} 或新 WithMetaExtra(ctx, extra) 携带
→ externalUploadSink.WriteFile 取出 → fs.WriteFile 前经 trusted.Guard(Wrap) 装饰器链
→ trusted.TrustedVolumeFS.WriteFile 的 writeMetaAfter 合并 extra（maps.Copy）
→ secretdata.WriteFile 走流式/内存加密时把 extra 写入 shardseal.Meta.Extra
```

**各卷类型覆盖矩阵**：

| 卷类型 | 读（FileMeta） | 写（上传带 extra） | 写（meta set 补录） |
|---|---|---|---|
| 本地卷（main） | trusted.Wrap FileMeta ✅ | applyMetaExtra（本地路径）✅ 改合并 | UpdateMetaExtra ✅ |
| 加密卷（secretdata over local/网盘） | secretdata Provider ✅ | **UploadSink 链路补 extra**（ctx 承载） | secretdata UpdateMetaExtra ✅ |
| 裸外部卷（baidupcs/s3，无 trusted） | ⚠️ 无 sidecar ⇒ 读返回空 | ⚠️ 无法持久化 extra（无 meta 载体） | ⚠️ ErrMetaUnavailable |
| 封装卷（enclosure） | 底层 FS 转发 ✅ | 底层（secretdata/本地）透传 ✅ | 底层 UpdateMetaExtra ✅ |
| 去重卷（dedup） | 底层 ✅ | 上传 extra 落 meta ✅ | UpdateMetaExtra ✅ |
| 备份/同步（syncexec） | quotaLocalFS 转发 ✅ | — | — |

**裸外部卷（无 trusted）明示**：sidecar 载体缺失 ⇒ 读/补录返回明确「不支持」（fail-closed），不做静默假成功——与 `ErrMetaUnavailable` 语义一致；文档登记该边界。用户需要时再评估「外部卷 meta sidecar 放网盘同目录」方案（独立决策）。

### 2.7 一致性红线（用户 2026-10-07）

- 补录/上传的 extra 写入**不得改变**主文件校验链（TotalSHA256/分块）：合并只改 `fm.Extra` 字段，meta 重算时沿用主文件哈希（trusted 已保证：更新前整文件/轻量判据校验主文件未变）。
- 加密卷 meta 重加密（secretdata.UpdateMetaExtra）沿用原 meta 的 chunks/哈希字段——只改 Extra。

## 3. 实现分期

### 片 1：读闭环（核心缺口）
1. `pkg/files/meta_update.go`：新增 `GetMeta(ctx, GetMetaInput) (map[string]any, error)`（**双路定位**：本地 `locateForRead` / 外部 `externalVolOps().ExternalStat` 判定存在）→ 读 extra。
2. `pkg/files/meta_update.go`：`UpdateMeta` 改**双路定位**（同构）——修「meta set 到外部卷恒不可用」。
3. 读 extra 的双路实现：
   - 本地卷：`FileMetaPolicy` 新增 `ReadMeta(ctx, root, rel) (map[string]any, error)`（读 `meta.MetaPath(rel)` 的 sidecar → `fm.Extra`；sidecar 缺失 → 空 map）。
   - 外部卷：按卷取 FS → `meta.Provider.FileMeta` → `fm.Extra`（`filesVolumeSet` 新增 `MetaReader(volName)`，实现 `metaReaderSource`）。
4. `pkg/server/files_meta_handler.go`：`getFileMeta` handler（GET）。
5. `pkg/server/routes.go`：三处同源补 `GET /api/files/meta`（isFileGroupedRoute + isReadOnlyFileRoute + HandleFunc）。
6. `pkg/client/upload_meta_extra.go`：`GetFileMeta(ctx, filename)`。
7. `cmd/sclient/meta.go`：主命令输出 extra + 新增 `get` 子命令；`output.go` PrintStat 加 extra 展示。
8. 测试：TDD（服务端 handler / 客户端 SDK / CLI 输出）+ 变异验证。

### 片 2：语义统一（默认合并）
9. `applyMetaExtra` 改 `maps.Copy`（上传路径从覆盖→合并）；补测试（上传带 extra 不丢旧 KV 的场景）。
10. `PUT /api/files/meta` 加 `mode: merge|replace|delete`：`--replace`/`--delete` CLI flag；底层 `UpdateMetaExtra` 拆两能力（`UpdateMetaExtra` 合并 / `ReplaceMetaExtra` 整表 / `DeleteMetaKeys` 删键）或统一入参模式。

### 片 3：外部卷上传 extra 不丢
11. `UploadSink.WriteFile` ctx 承载 extra（新增 `syncpkg.WithMetaExtra(ctx, extra)` + `MetaExtraFrom(ctx)` 能力）→ externalUploadSink 取出 → FS 写侧（trusted/secretdata）合并。
12. `session.MetaExtra` 在 `completeExternalTransfer` 路径透传（当前只本地路径用）。

### 片 4：Web UI（需 Playwright e2e 硬规则）
13. 详情面板显示 extra（KV 徽章/区）；编辑面板（JSON textarea + 保存）；上传 JSON 输入框。
14. `node --test` 纯函数单测 + Playwright e2e（`web/e2e`）+ Makefile `web-test` 登记 + R10 门禁。

## 4. 验证清单

- TDD 红灯先行 + 变异验证（读端点缺失→404 红；合并语义回退覆盖→红；外部卷 extra 丢弃→红）。
- 全卷矩阵用例：本地/加密(over local+网盘)/封装/去重 各写读往返。
- `go test -race` 全绿 + lint 0 + archcheck（R38 路由分类 / R2 / R21 能力链）。
- `make web-test` + Playwright e2e。
- wire drift 三方契约（服务端 DTO ↔ SDK ↔ JS）更新。

## 5. 边界与明示

- **裸外部卷（无 trusted）**：不支持 extra（无 sidecar 载体），读返回空 / 补录 fail-closed；文档登记。
- `maxExtraBytes` 16KiB 总量上限保持；`x_` 前缀自定义扩展继续自由。
- 读侧不做 Trim/Validate（写侧已把关）；Extra 深拷贝防污染卷内存态。
- 兼容旧 meta（无 Extra 字段）：读返回空 map；合并从空起步。

## 6. 参考

- #851（写侧实现）、#855（路由组清单教训）、`pkg/files/meta/extra.go`（schema）、`meta_update.go`（补录）、`external_upload.go`（UploadSink）。
