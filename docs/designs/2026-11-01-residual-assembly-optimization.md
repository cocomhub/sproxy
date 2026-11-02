# 剩余装配根的优化设计（P2）

> 状态：设计（待实施）
> 前置：`2026-10-31-policy-object-extraction.md`（P1，已交付）、S1–S15 切片（已交付）
> 作用域：仅服务端（`pkg/server`）；`pkg/cloud`/`pkg/downloader`/`pkg/transfer` 等 Global 包不动

## 1. 现状测量（2026-11-01）

| 指标 | 起点（本系列开始） | 现在 |
|---|---:|---:|
| `pkg/server` 非测试行数 | 42,568 | **29,960**（−29.6%） |
| 服务端子包 | 1 | **16** |
| 子包非测试行数 | 0 | 14,556 |
| `Handlers` 直接字段 | 102 | 12（值嵌入 `*Deps`） |

剩余文件按「装配耦合密度」分三类（`h.` 引用数 / 去重 `h.<字段>` 数 / `Handlers` 方法数 / 纯函数数）：

| 文件 | 行数 | h 引用 | 去重字段 | 判定 |
|---|---:|---:|---:|---|
| `routes.go` | 1,004 | 257 | 136 | 装配本体 |
| `routes_setup.go` | 1,005 | 137 | 91 | 装配本体 |
| `handlers.go` | 738 | 155 | 50 | 装配本体 |
| `files_service.go` | 1,532 | 108 | 33 | 能力面转发（`pkg/files` 适配） |
| `volumes_api.go` | 1,115 | 119 | 46 | 装配本体 |
| `register_handler.go` | 1,230 | 101 | 31 | 装配本体（HTTP 编排） |
| `credentials_handler.go` | 805 | 59 | 10 | 装配本体（HTTP 编排） |
| `remote_read.go` + `remote_write.go` + 两个 listener | **1,074** | 57 | 6+4+2+? | **可切**（跨节点读写协议） |
| `credentials.go` + `credential_rotation.go` + `credentialstorer.go` | 623 | 15 | 3 | **可切**（凭据装配/轮换） |
| `nested_enclosure.go` | 419 | 3 | 1 | **可切**（封装卷翻译/装配） |
| `signaling_handlers.go` | 356 | 2 | 0 | **可切**（信令 broker） |
| `usage_store.go` / `user_volume_store.go` / `cluster_index_sync.go` | 940 | 3 | 0–1 | 可切但语义分散（三个独立 store） |

## 2. 判定准则（可执行，取代"凭感觉切"）

**切片门槛**（三者全满足）：
1. 去重 `h.<字段>` ≤ 8（`Deps` 能压到 ≤8 字段，符合 P1-c 的耦合度量口径）；
2. `h.` 密度 ≤ 80 次/千行（低于装配本体一个数量级）；
3. 有单一领域名（能一句话说清「这个包负责什么」）。

**不切门槛**（任一满足即留装配）：
1. 是路由注册 / 能力面转发（`routes*`、`handlers.go`、`files_service.go`）——切了只会把 `Handlers` 能力面复制一份，耦合上升；
2. 依赖 `ActorFrom`/`sendJSONResponse` 等装配层语义超过 5 处（P1-c 的 cloudtasks 教训：HTTP 语义留在装配，领域核心进包）。

## 3. 三条轨道

### T1（结构）：`pkg/server/remotefs` —— 跨节点读写面

**为什么**：`remote_read.go`(415) + `remote_write.go`(287) + `remote_read_listener.go`(203) + `remote_write_listener.go`(169) = **1,074 行生产代码 + 1,813 行测试**，是一个完整的协议面（B 侧跨节点只读/写 + loopback listener），却混在装配根里；读写两面本应**反向同构**（同一套授权、同一套错误映射），放在一起才能守住这个不变量。

**边界**：
- 进包：4 个生产文件 + 全部 `remote_*_test.go`；
- 留装配：路由注册（`routes*.go` 里的 4 条 POST + 4 条 GET/HEAD 注册）、`ActorFrom`。

**Deps 设计**（≤8 字段）：

```go
type Deps struct {
    Files   Files    // List/Stat/OpenDownload/ReadMeta（域能力，非 HTTP）
    Volumes Volumes  // LocateOwnerFile / VolumeByName / Allowed
    Quota   Quota    // QuotaFor(owner)
    Audit   Audit    // RecordAudit / RecordRemoteWriteDenied
    Meta    Meta     // ReadMetaForRemote / DownloadPathForRemote
    Config  Config   // 指纹/凭据/超时
    Logger  *slog.Logger
}
```

**不变量**（必须写测试锁住）：
1. 只读面物理上只注册 GET/HEAD，写面只注册 4 条 POST（非黑名单法）；
2. 写面只做授权 + 调域 API，**不复制** checksum 门禁/原子改名/版本/配额/文件锁/卷路由；
3. 远程写与本地写共享同一锁池（`pkg/files.FileLocks`），跨节点互斥天然成立；
4. 未装配（无 listener/无凭据）⇒ 全部 no-op，单节点零回归。

**验证**：现有 6 个测试文件整体随包移动，用例数/断言零改动；新增 `deps_test.go` 断言 `NumField() <= 8`。

### T2（收敛）：`internal/httpjson` —— 统一 JSON/错误响应

**为什么**：`sendJSONResponse` 全仓 **337 处**调用；且 `pkg/server/ratelimit` 已因无法反向 import 复制了一份 `writeTooManyRequests`（#988 遗留）。同一语义两份实现是明确缺陷，随切片增多只会更多。

**设计**：
- `internal/httpjson`（L0，零 `pkg/*` 依赖）：`Send(w, status, v)` / `SendError(w, status, msg)` / `WriteTooManyRequests(w)`；
- 装配层 `sendJSONResponse` 保留为**唯一入口**并委托（或直接替换全部调用点）；
- **棘轮门禁 R54**：非测试源码中裸 `http.Error(` / 自定义 JSON 编码写响应的出现次数只减不增；`internal/httpjson` 与装配层 wrapper 白名单。

**分期**：一期只做 `internal/httpjson` + `ratelimit` 复用 + 门禁基线（不动 337 处）；二期按文件批量迁移（每 PR 一个文件组，棘轮只减）。

**风险**：`sendJSONResponse` 的头部/字节契约必须逐字保持（已有实测断言模式）。

### T3（结构）：`pkg/server/credentials` / `pkg/server/enclosure`

**credentials**（623 行生产 + ~1,100 行测试）：`BootstrapServerCredentials` + 主密钥/Vault token 解析 + 凭据轮换。依赖 `pkg/accesskey`/`pkg/state`/config，`h.` 仅 3 个字段（`logger`/`cfg`/`credStore`）。
**风险**：启动路径，涉及加密主密钥；必须保留 `cmd/sproxy` 的调用面（独立 module）。

**enclosure**（419 行 + 238 行测试）：`TranslateNestedVolume` / `ParseNestedTarget` / `AttachEnclosureVolume` / `DetachEnclosureVolume`。`h.` 仅 1 个字段。
**风险**：`TranslateNestedVolume` 是**用户面语法 → 装配词汇**的翻译点，与 config schema 收敛计划耦合（设计里已注明「先翻译、后收敛」）；切片时不得改变翻译语义。

## 4. 顺序与理由

1. **T1 `remotefs`**：单一切片收益最大（2,887 行）、耦合面最小（4 个字段域）、有现成测试整体搬迁；
2. **T2 `httpjson`**：横切收敛，越早做后续切片越少复制（每新增一个包就多一份响应写的风险）；
3. **T3 `credentials` → `enclosure`**：启动路径与 schema 耦合，放最后，且各自独立 PR。

## 5. 明确不做

| 项 | 理由 |
|---|---|
| 继续切 `routes*`/`handlers.go`/`files_service.go` | 装配本体；切了要复制 `Handlers` 能力面（30+ 依赖），耦合上升 |
| 把 `usage_store`/`user_volume_store`/`cluster_index_sync` 合成一个包 | 三者无共同领域语义，仅为"都是 store"；合成会造出垃圾桶包 |
| 拆 repo / 拆进程 / build tags | 已在前序设计否决（23 个 go.mod 已提供隔离；CI 组合爆炸） |

## 6. 交付判据

- T1 完成后：装配根非测试 ≤ 28,900 行，`pkg/server` 子包 17 个；
- T2 完成后：`sendJSONResponse` 只剩 1 份实现，R54 棘轮基线落盘；
- 每个 PR：`go test ./pkg/server/... ./internal/archcheck/`、`make lint`、`make build-all` 全绿，门禁扫描面同步，文档（`architecture.md`/`gates.md`/`AGENTS.md`）同步。

## 7. 交付结果（2026-11-01）

| 轨道 | PR | 结果 |
|---|---|---|
| T1 `pkg/server/remotefs` | #994 | 1,074 行生产 + 1,813 行测试移出；`Deps` 恒 8 字段（有 `NumField()==8` 断言）；包内 32 例 + 装配层 9 例 |
| T2 一期 `internal/httpjson` | #995 | 收敛 **4 份** JSON 响应实现（装配根 `sendJSONResponse` 328 处、`pkg/files.sendJSON` 146 处、`ratelimit` 429、`cloudtasks.writeJSON`）；门禁 **R54** 基线 11 文件 41 处；顺带修 `pkg/files/runtime.logger()` 未装配时 panic |
| T3-b `pkg/server/enclosure` | #996 | 419 行移出；`h.enclosureSealedAt` → 包级 `SealedAt(set, ...)`；装配级 15 例留装配层、包内 4 例纯函数 |
| T3-a `pkg/server/credentials` | #997 | 458 行进包（store/masterkey/best_credential/bootstrap），装配层 `credentials.go` 433 → 68；启动编排与轮换驱动留装配层；顶层用例数 24 → 24 |

**规模**：`pkg/server` 非测试 **42,568 → 28,125（−33.9%）**；子包 **1 → 19**。

**与原设计的偏差（均为实施中发现，已记录理由）**：

1. T2 一期发现第 **4** 份 JSON 响应实现（`cloudtasks.writeJSON`），设计时只识别出 3 份；
2. T1 的 `federation` 相邻块 `federation_services.go` 是 `h.` 绑定的 HTTP handler，留装配层（设计已预判）；
3. T3-b 的 `volumeFromUserVolume` 移到 `pkg/server/user_volume_store.go` 而非新包——它转换 store 模型 `UserVolume`（装配类型）→ `volume.Volume`，进包即反向依赖；
4. T3-a 按风险收窄为「只切无 `h.` 依赖的纯子集」，启动编排（`bootstrapCredentials(opts)` / `SelfCredential` / 轮换驱动）留装配层。

**仍未做（后续，非阻塞）**：

- **T2 二期**：11 文件 41 处裸 JSON 写迁移到 `httpjson`（R54 棘轮已锁住只减不增）。迁移陷阱：① `json.Encoder` 带尾随换行，`Send` 不带（JSON 解码器无感，字节断言测试会红）；② 序列化失败日志由 per-handler `msgJSONEncode` 变为统一 Warn（丢 handler 名）；
- **剩余装配根 28,125 行**按 §2 判据已到自然边界：`routes*`/`handlers.go`/`files_service.go`/`volumes_api.go` 均为装配本体或能力面转发。

