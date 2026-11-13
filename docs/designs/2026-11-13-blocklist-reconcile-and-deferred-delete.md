# `block_list` 对账 + 分块 meta 逐块校验 + 延迟删除（2026-11-13）

> 状态：**设计待评审**。依据：`docs/external-dependencies.md` §7.8.5（`block_list` 权威性实测）、
> 现有 `pkg/files/meta`、`pkg/volume/trusted`、`uploadcore` 审计 WAL、`pkg/server/scheduler`。
> 标「已有」= 本仓库已实现（附落点）；标「缺口」= 需新增。

## 0. 需求（用户原话拆解）

| # | 需求 | 现状 |
| --- | --- | --- |
| R1 | 上传后用 **`block_list` 逐块比对**：一致 ⇒ 数据已在云端 | **缺口**（现在用整文件 md5 比对，见 §2） |
| R2 | 一致后**再用现有重传/秒传策略刷新**，但**不强求立刻成功** | 部分已有（`healByRapidRefresh` / `RapidUpload`） |
| R3 | **不一致 ⇒ 必须写审计 WAL，记录成功后才能返回成功** | **缺口**（现在直接报错/失败） |
| R4 | **sproxy 内部独立定时任务**确认一致后才删**sproxy 自己的上传本地副本** | **缺口**（staging 无条件删；chunk 仅按 TTL 删） |
| R5 | 该定时任务**仅当卷底层是 baidu 时启动** | **缺口**（卷后端判据） |
| R6 | **下载时逐块校验** | ✅ **已有**（`meta.VerifyReadSeeker` + `filesMetaPolicy.VerifyDownload`） |
| R7 | **文件 meta 中保存分块 md5/sha256** | ✅ **模型已有**（`meta.ChunkMeta`），需确认百度卷是否落盘 |
| R8 | 百度场景**首次用 `block_list` 对账并保存 meta**，保证秒传刷新后仍知道每块真实 md5 | **缺口**（对账 + 落 meta 未接线） |

## 1. 关键既有能力（可复用，勿重造）

### 1.1 分块校验模型与计算（`pkg/files/meta`）

```go
// 包注释原文：「上传/下载按总哈希 + 分块哈希逐分片校验」
type ChunkMeta struct {
    Index  int    `json:"index"`
    Offset int64  `json:"offset"`
    Size   int64  `json:"size"`
    SHA256 string `json:"sha256"`
    MD5    string `json:"md5,omitempty"`
}
```
- `meta.NewCalculator(size, chunkSize)` + `Calculator.Write/Finish() *FileMeta` — **边写边算**（含 Chunks）
- `meta.ResolveChunkSize(chunkSize, size)` — 分块尺寸策略
- 生产已接线：`pkg/server/files_service.go:1481` 用 `meta.NewCalculator(...)`

### 1.2 下载逐块校验（**已有**）

- `meta.VerifyReadSeeker(r, fm, opts)` — 逐块校验的 `io.ReadSeeker` 包装（`verifyChunkAhead`/`verifyEOF`）
- 钩子：`pkg/files/options.go:603` 的 `VerifyDownload(ctx, root, rel, r)`
- 实现：`pkg/server/files_service.go:1013` 的 `filesMetaPolicy.VerifyDownload`
- ⇒ **R6 已满足**：只需**卷有 FileMeta（含 Chunks）**即自动生效。

### 1.3 上传审计 WAL（**已有，直接支撑 R3**）

`pkg/volume/ext/baidupcs/uploadcore/auditwal.go`：
```go
func OpenAuditRecorder(ctx, svc *audit.Service, volume, targetPath string) (*AuditRecorder, error)
func (r *AuditRecorder) Stage(ctx, stage string, kv map[string]any) error
func (r *AuditRecorder) PartDone(ctx, seq int, md5 string) error   // ← 已按分片记录 md5
func (r *AuditRecorder) MarkDone(ctx) error
+ ResumeScope(...) / ResumeScopeByID(...)                          // 崩溃续传/重放
```
⇒ **R3 的"记录审计 WAL 成功后再返回成功"可直接复用**：把「待复查」写成 `Stage` 事件（含 key/rel/期望分块 md5）。

### 1.4 周期任务调度（**已有，直接支撑 R4/R5**）

`pkg/server/scheduler`：`Scheduler.Register(Task)` / `Start()` / `Stop()` / `SetMaintenanceWindow(...)`。

### 1.5 百度 `block_list` 权威性（§7.8.5 实测）

- 取数：库 **`FilesDirectoriesMeta(path)`**（= `pcs/file?method=meta`）→ `FileDirectory.BlockList`
- **父目录列举不返回 `block_list`**（实测恒空）⇒ **必须走 meta 接口**
- 语义：**按 4 MiB 分块的真实内容 md5 列表**，与本地同块计算**逐块相等**，**上传后 T+0 可用**
- 对照：`md5` 字段在 >4 MiB 时是**分片组合值**（可能非法 33 位含 `j`），**不可信**

## 2. 现状缺口（R1/R2/R3/R4/R5/R8）

| 缺口 | 落点 |
| --- | --- |
| 上传后校验用**整文件 md5**（不可信）+ 有界轮询 + 秒传自愈 | `baidupcs/storage.go` 的 `verifyOne`/`ensureVerifiedAfterUpload`/`putCheckAfterUpload` |
| 无 `block_list` 对账 | 无 |
| 不一致时**没有留痕重试**（直接失败/返回错误） | 无 |
| sdserver **上传校验通过即删本地** | `internal/task/njavtv/manual_upload.go` 的 `deleteLocal` 分支 |
| 无"仅 baidu 卷才启动"的判据 | 无 |

## 3. 设计

### 3.1 两层判据（**用户明示的语义，务必区分**）

| 判据 | 含义 | 行动 |
| --- | --- | --- |
| **`block_list` 逐块一致** | **内容已在云端** | **对客户端即"上传成功"** ⇒ sdserver 等可据此删自己的原件 |
| **`md5` 对账一致**（md5 字段权威） | 对象的 md5 已收敛 | **sproxy 才删自己的本地副本**；此后可不再依赖本地副本 |
| 其它（md5 未一致 / 对账取不到） | 待收敛 | **sproxy 必须保留本地副本 + 写审计 WAL**；由定时任务**重传 / 重新检查** |

> ⚠️ **`block_list` 一致 ≠ 可以删 sproxy 的副本**。原因：`md5` 未权威时，后续仍需
> **用本地副本算 `md5/sliceMD5/crc32/blockMD5s` 触发秒传刷新**（`RapidUpload`）来收敛 md5；
> 删了副本就失去刷新/重传的源 ⇒ **必须留到 md5 一致**。

### 3.2 sproxy 上传后判定流程（R1/R2/R3）

```
上传完成
  ├─ ① block_list 逐块比对（远端 meta.BlockList ↔ 本地 stagedUpload.blockMD5s，块大小 4 MiB）
  │     ├─ 一致  ⇒ 【内容已在云端】⇒ **可以对外返回成功**（客户端可据此删原件）
  │     └─ 不一致 ⇒ 写审计 WAL（待复查：key/rel/期望分块 md5/实测 block_list）
  │                 **WAL 落盘成功后才返回成功**（不因百度侧抖动误报失败）
  ├─ ② md5 对账（远端 md5 字段权威？）
  │     ├─ 一致  ⇒ **删除 sproxy 本地副本**（staging / 会话 chunk）+ 审计标记 done
  │     └─ 未一致 ⇒ **保留本地副本** + 再触发一次秒传刷新（refreshByRapidUpload，**不强求成功**）
  │                  + WAL 留痕 ⇒ 交给定时任务重传/重新检查
  └─ ③ 全程：**md5 对账一致之前，本地副本一律不删**（硬约束）
```
- 单块（≤4 MiB）：`meta.md5 == 整文件 md5`（§7.8.5 实测）⇒ ①② 可一步完成（快速路径）。

### 3.3 本地副本的延迟清理（R4/R5）——**sproxy 内部机制**

> ⚠️ **范围（用户明示）**：删的是 **sproxy 自己的上传本地副本**，**不是** sdserver 的原件——
> 二者**不是同一个文件**：sdserver 原件在**客户端机器**；sproxy 的副本是它从**请求体**落盘的**服务端暂存**。

**sproxy 侧本地副本清单（实测清点）**：

| # | 副本 | 落点 | 现有清理 | 缺陷 |
| --- | --- | --- | --- | --- |
| ① | per-upload staging | `StorageConfig.TempDir/stage-*`（`baidupcs/storage.go:900`） | `defer cleanup()` **无条件即时删** | 新语义下需**留到 md5 一致** |
| ② | 分块会话 chunk 暂存 | `<默认卷根>/<owner>/chunk/<session>/`（`chunked_store.go:179`） | **仅按 TTL（默认 24h）删** | **未确认云端一致就删** ⇒ 丢续传/重试依据 |
| ③ | 客户端侧临时（SDK） | `%TEMP%/sproxy-client/` | `pkg/tempclean.CleanupStale`（节流） | — |

**内部定时任务（仅 baidu 后端启用）**：
- 周期扫描「待复查」条目 ⇒ 对每条：取远端 `meta.BlockList` **逐块比对本副本** ⇒
  一致 ⇒ 删副本 + 审计 `completed`；不一致/取不到 ⇒ 保留 + **有界退避**（达上限转人工）。
- 判据：卷后端类型（`backend == "baidupcs"`）；**非 baidu 卷保持现状**。
- **复用**：`uploadcore.AuditRecorder`（`Stage`/`MarkDone`）+ `pkg/server/scheduler` +
  **`pkg/tempclean`（唯一清理实现）**；**不新建清理器**。

### 3.4 本地副本占用的配额控制 + 空间不足拒绝上传（用户新增要求）

**已有能力（直接复用，勿重造）**：
- **`pkg/sync` 的 staging 配额**：`StagingQuotaTracker{ReserveUsage(ctx,size) error; ReleaseUsage(size)}`，
  哨兵 **`ErrStagingQuotaExceeded`**（`entry.go:125`）；门卫 `StagingQuotaGateFS`（`WriteFile` 先预留、
  写完释放；未知长度用 `reservingReader` 背压）。
- **已装配**：`cmd/sproxy/baidupcs_sync.go:96` → `syncpkg.WrapStagingQuota(fs, t)`。
- **空间不足 → 拒绝上传**已有先例（**507**）：`chunked_upload.go:547`（"存储空间不足"）、
  `write_ops.go:1293/1542`、`options.go:464`、`dir_charge.go:67/76`、`version.go:368/386`。

**⚠️ 缺口（本设计要补）**：
| 裸写点 | 问题 |
| --- | --- |
| `baidupcs/storage.go:900` `os.MkdirTemp(s.temp,"stage-*")` + `905 os.OpenFile` | **绕过 FS ⇒ 不被 `StagingQuotaGateFS` 记账** ⇒ 副本占用不受配额约束 |
| `pkg/files/chunked_store.go` 的 chunk/session 写（`os.CreateTemp`/`os.WriteFile`） | 同上 |

**设计**：
1. 为上述两处引入**同一个** `StagingQuotaTracker` 记账（经 `StorageConfig`/注入，**不新建机制**）：
   写前 `ReserveUsage(size)`，删除副本后 `ReleaseUsage(size)`。
2. **保留副本的额度**由该配额池封顶 ⇒ 副本不会无限增长。
3. **无空闲空间时拒绝 sclient 上传请求**：`ReserveUsage` 返回 `ErrStagingQuotaExceeded` ⇒
   HTTP **507 Insufficient Storage**，消息统一为「**空间不足，请稍后重试**」
   （复用既有文案常量 `errMsgStorageQuotaExceeded` / `msgStorageQuotaExceeded`，勿新增重复字面量）。
4. 队列行为沿用 tracker 语义：**排队等待**（受 ctx 约束）+ 超时/拒绝 ⇒ 507。

### 3.5 不复用/不新建清单

| 能力 | 归属 | 说明 |
| --- | --- | --- |
| 分块哈希计算 | `pkg/files/meta`（`Calculator`） | **复用**，勿新建 |
| 下载逐块校验 | `pkg/files/meta.VerifyReadSeeker` + `filesMetaPolicy.VerifyDownload` | **复用**，勿新建 |
| 审计 WAL | `pkg/audit` + `uploadcore.AuditRecorder` | **复用**，勿新建 sink |
| 周期任务 | `pkg/server/scheduler` | **复用**，勿新建 ticker |
| `block_list` 对账 | **新增**（唯一新能力）：`baidupcs` 内一个纯函数 + 上传/定时任务两处调用 | 单一实现 |

## 4. 验收

1. **对账正确性**（e2e，已有工具 `blocklist_authority_e2e_test.go`）：8 MiB / 1 MiB 全新随机内容，
   `meta.BlockList` 与本地分块 md5 **逐块相等**且 **T+0**。
2. **不一致留痕**：构造"本地与远端分块不一致"⇒ 断言 ①返回成功 ②审计 WAL **已有**对应 Stage 记录
   ③**不删**本地文件。变异：去掉 WAL 写入 ⇒ 测试红。
3. **副本延迟清理**：定时任务在"远端一致"前**不删** sproxy 本地副本、一致后**删除**且审计标记 `completed`；
   非 baidu 后端**不注册**该任务。变异：删掉后端判据 ⇒ 红。
4. **下载逐块校验**：篡改远端一个块 ⇒ 下载在**该块**处失败（现状能力回归确认）。
5. **meta 持久化**：上传后读回 `FileMeta.Chunks`，与本地分块哈希一致；秒传刷新后**仍一致**。

## 5. 分期

- **P1（spoxy 内核）**：`block_list` 对账 + 上传后判据改造 + 不一致写 WAL（R1/R2/R3/§3.2）
- **P2（sproxy 内核）**：副本延迟清理定时任务 + 卷后端判据（R4/R5）——**复用** `scheduler` + `tempclean` + 审计 WAL
- **P3**：meta 落盘与下载逐块校验的**端到端确认**（R6/R7 回归 + 缺口补齐）

---

## 6. 对抗式评审（正确性 / 完整性 / 可靠性 / 稳定性 / 边界 / 缺陷）

> 图例：**P0 = 会导致数据丢失或服务不可用**；P1 = 会误判/泄漏；P2 = 边界或可维护性。
> 「已验」= 本轮实测/源码确认；「未验」= 推理待补验证。

### 6.1 已验证事实（支撑设计的前提）

| 事实 | 证据 |
| --- | --- |
| 加密卷上传到百度的单位是 **blob**（`secretdata.uploadBlob`）⇒ `stagedUpload.blockMD5s` 与远端 `block_list` **同为密文块 md5，层次一致可比** | `secretdata/features.go:300`、`copy.go:62` |
| `pkg/audit` 默认 **`SyncAlways`（每行 fsync）**，契约「`Append` 返回 nil ⇒ 已按落盘策略持久化」 ⇒ **「WAL 落盘成功」有强保证** | `audit/journal.go:53/242/318` |
| `block_list` 实测与本地 4 MiB 分块**逐块相等**且 T+0 可用（8 MiB / 1 MiB） | §7.8.5 |
| `md5` 字段**形态合法但 ≠ 整文件 md5**（实测 `a6804cf3…`）⇒ **不可仅凭形态判权威** | §7.8.5 |

### 6.2 P0 缺陷（必须解决才能实施）

**P0-1｜`block_list` 是"远端声明"，不是独立验证 ⇒ 假一致风险**
- 对账完全依赖百度返回的 `block_list`。若其返回**过期/缓存/错误**值，会判定"已在云端"⇒ 后续按 md5 判据删副本 ⇒ **失去唯一本地源**。
- **缓解（必须）**：① 删副本前**必须叠加第二判据**（设计已要求 md5 对账 ⇒ 保留此约束）；② 定时任务在删副本前做**抽样读回校验**（`RangeRead` 读回 1 个块算 md5）；③ 记录 block_list 快照进审计，便于事后取证。
- 现状：设计只写了"两层判据"，**未写"读回抽样"** ⇒ 补。

**P0-2｜`md5` 对账可能**永不成立** ⇒ 副本永久保留 ⇒ 配额耗尽 ⇒ 507 拒绝所有上传（**服务不可用**）**
- 实测：8 MiB 文件上传后 **90 s 内 md5 未权威**；另一次 `RapidUpload` **命中=true 但 30 s 内仍未权威**。
  ⇒ 若该状态**长期不收敛**（多块文件是常态），则按设计**副本永不删** ⇒ 磁盘/配额被填满 ⇒ **所有新上传被 507 拒绝**。
- 这是本设计**最严重的稳定性缺陷**（自我放大：越积越满 ⇒ 越拒绝）。
- **缓解（必须，且需用户裁定优先级）**：
  1. **回收上限**：副本保留**不超过 N 小时**（可配置，默认保守值）；超限 ⇒ **强制清理 + 告警**
     （理由：`block_list` 已证明云端有该内容 ⇒ 数据安全；代价是丧失"刷新 md5"的能力）；
  2. **配额压力优先清理**：配额达到阈值时，**优先清理"block_list 已一致"的副本**（与"必须 md5 一致才删"冲突 ⇒ **需用户裁定**）；
  3. **指标/告警**：`副本保留时长`、`待复查条目数`、`配额使用率` ⇒ 必须可观测，否则会静默恶化。

**P0-3｜「WAL 落盘成功后才返回成功」与「`block_list` 一致即客户端成功」语义冲突**
- 若 WAL 写失败 ⇒ 按现设计返回**失败** ⇒ 客户端**白重传**（尽管内容已在云端）。
- **正确语义（应改）**：**客户端成功判据 = `block_list` 一致**（与 WAL 无关）；
  WAL 只是 **sproxy 内部清理的依据**，写失败 ⇒ **仍返回成功** + `ERROR` 日志 + 指标，
  并由定时任务**扫描本地副本目录**兜底（**不依赖 WAL 记录**，避免"留了副本但无人知道"）。
- ⇒ 设计需把"WAL 落盘成功后才 return nil"改写为**上述语义**。

### 6.3 P1 缺陷（会误判或泄漏）

**P1-1｜md5 权威判据未精确化（歧义）**
- 必须写成 `strings.EqualFold(meta.MD5, st.md5)`（**值相等**），**不得**只看形态（32 hex）。
  实测反例：`a6804cf3…` 形态合法但 ≠ 整文件 md5。
- 单块（≤4 MiB）时 `meta.MD5 == 整文件 md5`（实测）⇒ 单块走快速路径。

**P1-2｜✅【已验证 2026-11-13】大文件的 `block_list` 块大小仍为 4 MiB**
- 实测四档（`blocklist_authority_e2e_test.go`，全新随机内容 + 唯一文件名 + 断言非秒传）：

  | 大小 | 远端 `block_list` | 本地 4 MiB 块 | 逐块相等 | 全部合法 32hex | 可用 |
  | --- | --- | --- | --- | --- | --- |
  | 1 MiB | 1 | 1 | ✅ | ✅ | T+0 |
  | 8 MiB | 2 | 2 | ✅ | ✅ | T+0 |
  | **100 MiB** | **25** | **25** | ✅ | ✅ | T+0 |
  | **512 MiB** | **128** | **128** | ✅ | ✅ | T+0 |

- ⇒ **块大小恒为 4 MiB、无阈值变化**（512 MiB 全程 84 s）⇒ 与本地 `uploadBlockSize=4MiB` 同构，**对账判据成立**。
- 残余风险：>512 MiB 未验（若后续在超大盘场景使用，建议补 1–2 GiB 档）。

**P1-3｜✅【已验证 2026-11-13】`block_list` 的顺序与合法性（同上四档实测）**
- 逐块比较**依赖顺序**（块 md5 标识内容片段，乱序即无意义）。若百度不保证顺序 ⇒ 假不一致。
- 另：若某元素为空/非法/`"0"` ⇒ 必须判「**不可判定**」并走"保留副本"分支，**绝不可**视为一致。
- **补**：明确比较函数 `equalBlockLists(remote, local) (ok, decidable bool)`。

**P1-4｜多实例并发 ⇒ 定时任务重复执行**
- 多 sproxy 实例共享存储时，各实例的定时任务会同时复查/删除同一副本 ⇒ 竞争。
- **补**：复用 **`pkg/leader`（选主）** 或 **`pkg/server/filelock`**（跨进程文件锁）；**不新建**锁机制。

**P1-5｜定时任务与新上传的竞争（误删新副本）**
- 复查某副本期间，新上传可能写入**同名 rel** 的新副本 ⇒ 按 rel 删除会**误删新副本**。
- **补**：副本身份以 **(rel, blob/内容 md5, mtime)** 三元组配对；删除前后各做一次快照比对（CAS 语义）。

**P1-6｜chunk 桶可能位于**目标卷（baidu）**上 ⇒ "本地副本"定义不清晰**
- `chunked_store` 的 `volTenantRoots`（`chunked_store.go:191`）支持**非默认卷**的租户根
  ⇒ 分块会话的 chunk 可能落在 **baidu 卷**上（即在云端）。此时"删本地副本"语义与"云端数据"重合，
  需明确**只对真正的本机暂存（默认卷根）**执行延迟清理。

### 6.4 P2 缺陷（边界与可维护性）

| # | 边界/缺陷 | 处置 |
| --- | --- | --- |
| P2-1 | **0 字节文件**：`block_list` 可能为空 | 空列表 ⇒ 视为「已一致」（0 字节无语义内容）+ 单独用例锁定 |
| P2-2 | **重复上传同一内容** ⇒ 多副本 ⇒ 配额翻倍 | 以内容身份去重（同 md5 只保留一份） |
| P2-3 | **改名/复制**后 rel 变化 ⇒ 副本与 rel 失联 | 副本身份绑定**内容 md5**而非 rel |
| P2-4 | **配额死锁**：配额满 ∧ md5 未一致 ⇒ 不能删（设计要求）也不能留（无空间） | 必须有优先级规则 ⇒ **请求用户裁定**（见 6.5） |
| P2-5 | `stagedUpload` 的 `defer cleanup()` 改为"条件删除" ⇒ **临时文件生命周期变长** | 必须确保 `TempDir` 所在盘**与配额池是同一资源**，否则"记账了但占的是另一块盘" ⇒ 记账失真 |
| P2-6 | WAL 体积随待复查条目增长 | 复用 `pkg/audit` 的 `ArchiveSealed`/`SegmentArchiver` 归档；条目数设上限 |
| P2-7 | 定时任务**失败/卡死** | 超时 + 有界退避 + 指标（设计已提，补"达上限转人工"的可观测点） |

### 6.5 需要用户裁定的两个关键决策

**D-1｜"删副本"的判据能否等价替换？**
- 现设计：**必须等百度 `md5` 字段权威**（用户明示）。
- 但 `md5` 收敛**不可控**（实测 90 s 未权威；`RapidUpload` 命中后 30 s 仍未权威）⇒ **P0-2**。
- **建议的等价判据**：**`block_list` 逐块一致 + 分块 meta 已落盘且读回验证通过**
  ⇒ 已能独立证明"云端内容 == 本地内容"（逐块可证）⇒ **无需依赖百度 md5 收敛**。
- 若能接受 ⇒ **P0-2 与 P1-5 大幅缓解**，WAL/定时任务的必要性也下降（仅剩"配额清理"职责）。
- **请裁定**：(a) 坚持"md5 一致才删"（则必须同时接受 6.2 的回收上限）；(b) 接受等价判据。

**D-2｜配额压力能否优先清理"`block_list` 已一致"的副本？**
- 若不能 ⇒ 配额满时**无法自愈**（P2-4 死锁）⇒ 只能靠 507 拖到 md5 收敛。
- **请裁定**：是否允许"**云端已确认有该内容**"的副本在配额压力下**优先清理**（数据安全，仅丧失刷新 md5 的能力）。

### 6.6 结论

- **设计方向正确**（`block_list` 作权威内容判据有实测支撑；复用既有 meta/审计/调度/配额能力）。
- **当前不能直接实施**：存在 **3 个 P0**（假一致风险、md5 永不收敛导致服务不可用、客户端成功语义冲突）
  与 **6 个 P1**（判据歧义、未验证的大文件块大小/顺序、并发与竞争、chunk 桶归属）。
- **实施前置条件**：① 裁定 D-1/D-2；② 补验 **P1-2**（大文件块大小）与 **P1-3**（顺序/合法性）；
  ③ 明确 P0-3 的成功语义；④ 设计回收上限与监控指标。

---

## 7. 定稿修订（依据用户补充的运行经验与既有能力）

### 7.1 用户补充的关键运行事实（削弱 P0-2）

> 「**每次检查如果不一致就进行两次上传保证命中秒传，5 min 后再检查大概率就刷新了；
> 目前没遇到多次上传还一直不收敛的 case**。」

⇒ **P0-2（md5 永不收敛 ⇒ 副本永久保留 ⇒ 配额耗尽）在实际操作中概率很低**。
本设计据此把 P0-2 从"必须设计回收上限"降级为"**必须有兜底上限 + 指标**"（有备无患，但不作为主要路径）。

**重试策略（用户给定，落到执行器）**：
```
检查不一致 ⇒ 连续上传 2 次（第 2 次几乎必为秒传命中 ⇒ 触发 md5 刷新）
          ⇒ 等 5 min（= issues 默认 RetryBackoff）
          ⇒ 再次对账
          ⇒ 仍不一致 ⇒ 交给 issues 的退避重试（默认 5min × MaxAttempts=5）⇒ 达上限转 needs_human
```

### 7.2 ★ 不新建 WAL 队列 —— **复用 `pkg/server/issues`**（既有能力，已完整覆盖）

| 需求 | `pkg/server/issues` 既有实现 | 结论 |
| --- | --- | --- |
| 自动重试 + 定时 | `Manager.Due(now)` / `RunDue(ctx, now, execs map[Kind]RetryFunc)` | 复用 |
| **退避 + 上限 + 转人工** | `RetryBackoff`（默认 **5 min**）、`MaxAttempts`（默认 **5**）、`markNeedsHuman` | 复用（与用户"5 min 后再检查"**完全吻合**） |
| 持久化 / 重启恢复 | `persistLocked()` / `LoadError()` / `store.go` | 复用（**= WAL 的完备替代**） |
| **人工触发重新检查/重传** | `POST /api/issues/resolve`、`POST /api/issues/scan`、`GET /api/issues`、`GET /api/issues/detail` | 复用 |
| 运维界面 | `web/static/issues.js`（列表含详细失败原因与最近失败） | 复用 |
| 种类 | 已有 `Kind` 枚举（含 **`KindUploadFailed="upload_failed"`**） | 复用/新增一个 Kind |

⇒ **原设计的"审计 WAL 队列"作废**，改为：**登记/重试/转人工/人工触发全部走 issues**。

### 7.3 WAL 写入失败怎么办（用户提问）—— 明确语义

**分层（用户口径：内存优先，WAL 只为重启恢复，目的是降低"丢问题处理"的风险）**：

| 层 | 角色 | 失败时的行为 |
| --- | --- | --- |
| **内存（主）** | 唯一的**即时状态**：待复查集合、重试计数、下次到期时间 | **不丢**（进程存活期内一定可重试） |
| **持久化（次）** | **仅用于跨重启恢复**（= issues 的 `persistLocked` / `store`） | **失败不影响客户端成功**，不影响内存重试；仅"重启后少一条待复查"（风险降低但非零） |
| **兜底（补）** | **启动时扫描本地副本目录**，把"有副本但无对应已确认记录"的孤儿**补登记为 issues** | 把"持久化丢失"的残留重新纳入处理 |

**对外语义（已确认）**：**`block_list` 一致 ⇒ 客户端即成功**；持久化/重试**只影响 sproxy 内部清理**，
**绝不**因为"持久化失败"给客户端返回失败（否则客户端白重传）。

### 7.4 人工介入闭环（用户提问：能否接入既有流程）—— **能，且是同一个机制**

- **自动**：`RunDue` 按退避重试 ⇒ 成功 `resolved`；达上限 ⇒ **`needs_human`**。
- **人工**：
  - 查看：`GET /api/issues`（列表，含 detail/最近失败）/ Web UI 事项页；
  - **触发重新检查**：`POST /api/issues/scan`（立即触发一轮扫描）；
  - **处置**：`POST /api/issues/resolve`（记录说明，供审计）——**需新增一个"立即重试单条"的动作**，
    复用既有 resolve 路由与权限模型（auth 运维面），**不新建 API 面**。
- ⇒ **无需新建任何"人工流程"**；只新增一个 `Kind` + 一个 `RetryFunc` + （可选）一个"立即重试"动作。

### 7.5 修订后的缺陷状态

| 原编号 | 状态 |
| --- | --- |
| P0-1（假一致） | **仍需缓解**：删副本前叠加第二判据 + 抽样读回（保留） |
| P0-2（永不收敛⇒配额耗尽） | **降级**：用户实测"两次上传+5min 后大概率刷新"；仍需**上限兜底 + 指标**（不阻塞实施） |
| P0-3（成功语义冲突） | **已裁定**：`block_list` 一致 ⇒ 客户端成功；持久化失败不得导致客户端失败 |
| P1-4（多实例重复执行） | **需确认 issues 的 `RunDue` 是否已有选主**；若无 ⇒ 复用 `pkg/leader`（勿新建） |
| 其余 P1/P2 | **保持**（尤其 P1-2 大文件块大小、P1-3 顺序/合法性，仍须补验） |

### 7.6 修订后的分期（更小、更贴合既有能力）

| 期 | 内容 |
| --- | --- |
| **P1** | `block_list` 对账（本地 blob 分块 md5 ↔ 远端 `meta.BlockList`）+ `md5` 对账（`EqualFold`）+ **落分块 meta** |
| **P2** | **复用 issues**：新增 `Kind`（如 `blob_md5_unconverged`）+ `RetryFunc`（**两次上传 → 5min 复查**）+ 启动兜底扫描；**副本延迟清理**（md5 一致才删；仅 baidu 后端） |
| **P3** | 裸写纳入 staging 配额记账 + 507 拒绝 + 人工"立即重试"动作 + 指标（保留时长/待复查数/配额率） |

### 7.7 源码核实（本轮）：issues 的语义与接线**完全匹配**，机制零新建

| 核实点 | 源码事实 |
| --- | --- |
| **内存优先** | `Manager.Report`（`issues.go:174`）：**先写内存** `m.items[it.ID] = &cp`，**之后**才 `m.persistLocked()` |
| **持久化失败不阻塞** | `persistLocked`（`issues.go:412`）：`Store.Save` 失败**仅记 `m.loadErr`**，不返回错误、不阻断调用方 ✅（正是"内存为主、WAL 只为重启恢复"） |
| **不可重试即转人工** | `Report`：`Retryable=false` ⇒ 直接 `StatusNeedsHuman`；`true` ⇒ `StatusOpen` + `NextRetryAt = now + RetryBackoff`（默认 5 min） |
| **既有调度已接线** | `cmd/sproxy/root.go:1246` → `h.RegisterIssuesRetryTask(issuesExecs)`；`pkg/server/issues_schedule.go:40` |
| **扩展点唯一** | `pkg/server/issues_executors.go:73` 的 `NewIssuesExecutors`（`Kind → RetryFunc` 表）；未覆盖的 Kind 会被 `RunDue` 转 `needs_human`（**fail-safe**） |
| **多实例无选主** | `issues` 内仅进程内 `m.mu`；跨实例需复用 **`pkg/leader`**（P1-4 成立，**勿新建**） |

⇒ **落地改动面（极小）**：
1. 新增一个 `Kind`（如 `KindBlobMD5Unconverged = "blob_md5_unconverged"`；或优先复用 `KindUploadFailed`）；
2. 在 `NewIssuesExecutors` 增加一个 `RetryFunc`：**连续上传 2 次（保证命中秒传）→ 等 5 min → 重新对账**
   → 一致 ⇒ 返回 nil（`RunDue` 会 `markResolved`）；不一致 ⇒ 返回错误（走退避，达上限转 `needs_human`）；
3. 上传路径在「`md5` 未对账一致」时 `Report` 该事项（**不影响**返回给客户端的成功）；
4. （可选）复用 `POST /api/issues/resolve` / `/scan` 实现人工"立即触发重新检查并重传"。

### 7.8 ④「副本条件保留」的精确实施方案（code-anchored，2026-11-13 复核）

> 面向 PR #1143 的下一片；已按源码复核，**不是推断**。

**现状（源码事实）**：
- `pkg/volume/ext/baidupcs/storage.go:946` `stageUpload(ctx, r, key) (*stagedUpload, error)`：
  创建 `sub := os.MkdirTemp(s.temp, "stage-*")`，定义 `cleanup := func(){ tmp.Close(); os.RemoveAll(sub) }`，
  **但该 closure 只在错误路径被调用，成功路径不返回给调用方**
  ⇒ **副本目录实际留在 `s.temp`（TempDir）下**，当前由 **`pkg/tempclean` 按 mtime 老化清理**。
- `*stagedUpload` 只携带 `tmpPath`（= `<TempDir>/stage-*/<base>`）。

**⇒ ④ 的形态是「登记 + 保护期」，而不是「改成不删」**：

| 步骤 | 落点 | 要点 |
| --- | --- | --- |
| 1. 产出"待复查"结论 | `putCheckAfterUpload` / `verifyAfterUpload` / `checkAfterUploadUnreliableMD5` | 现返回 `(meta, done, err)`；需**显式表达**「`block_list` 一致 **且** md5 未权威」（建议新增结果枚举或 sentinel，**不要**用布尔拼装） |
| 2. 上报事项 | `Storage` 侧注入 `issues` 上报口（窄接口，避免 `baidupcs` 反向依赖 `pkg/server`） | `Kind=blob_md5_unconverged`；`Path`=**副本路径**；`Detail`=rel；`Volume`=卷名 |
| 3. **保护期** | `pkg/tempclean` 与登记路径的协作 | 副本若留在 `TempDir` 会被按龄清掉 ⇒ 二选一：① 登记时把副本**移动到稳定目录**（如 `<TempDir>/reconcile-pending/<hash>`）并让 tempclean 跳过该子目录；② 让 tempclean 接受"已登记路径集合"（需注入）。**必须显式选择并测试** |
| 4. 仅 baidu 后端启用 | 装配层（卷后端判据） | 非 baidu 卷不上报、不保护（无此可见性语义） |
| 5. 启动兜底 | 启动时扫描 `reconcile-pending/` | 孤儿副本补登记（**不依赖** issues 持久化是否成功） |

**必须覆盖的测试（TDD + 变异）**：
1. 「`block_list` 一致 && md5 未权威」⇒ ①上报事项（断言 `Path`/`Detail`/`Volume`）②副本**在保护期内不被清理**
   ——变异：去掉登记 ⇒ 红；去掉保护 ⇒ 红（用 tempclean 老化后断言副本仍存在）
2. md5 权威 ⇒ 副本**被清理**，且**不上报** ——变异：反转判据 ⇒ 红
3. `RetryFunc` 收敛 ⇒ `resolved` 且副本清理；连续不一致 ⇒ 退避至 `needs_human` ——变异：无退避 ⇒ 红
4. 非 baidu 后端 ⇒ **不上报/不保护** ——变异：删后端判据 ⇒ 红
5. 启动兜底扫描补登记 ——变异：去掉扫描 ⇒ 红

**风险**：③ 若把副本移到稳定目录，需确认**不落在卷内**（否则会走 `pkg/files` 用户内容路径/占配额语义），
并确保与 `StagingQuotaTracker` 记账的资源**同一盘**（否则"记账了但占的是另一块盘"）。

### 7.9 实施后订正（2026-11-13，含真机 e2e 实测）

**④ 的触发条件比预期罕见**（真机实测）：
`Put` 的既有后校验循环（重传 + rapidupload 秒传刷新 + ETag 复核）**通常会在返回前把远端 md5 收敛**，
此时 `verifyAfterUpload` 走 `ETag == 本地 md5` 分支 ⇒ 判定"已收敛" ⇒ **正确地不做保留**。
真机 e2e（`reconcile_pending_e2e_test.go`）实测：5 MiB 随机内容上传后 `etag == 本地 md5` ⇒ 未触发保留（符合预期）。

⇒ **④ 真正会触发的场景**：一致性循环**用尽后仍未权威**，例如
① `PutHints.DeferVerify`（批量延迟校验路径：上传后跳过即时复核）；
② 重传达上限（`ErrTransient`）前的中间态；
③ 百度侧长时间不刷新（用户称"目前未遇到"，但存在）。

**实现要点（已在代码中落实）**：
- 保留判据**同源**（`markReconcileIfNeeded`：`!md5Converged(meta, st)`），两处调用（block_list 命中分支 + `MD5Unreliable` 分支），避免漂移。
- ⚠️ 实施中曾出现**两次空转**，均已修：
  1. `block_list` 对账命中即**提前 return** ⇒ 走不到"md5 非权威"那段 ⇒ 副本被删、重试无源可传
     ⇒ 修法：命中分支**也**调用 `markReconcileIfNeeded`（内容已确认在云端但 md5 未权威 ⇒ 保留 + 上报，**对客户端仍返回成功**）。
  2. 事项 `Path` 若存**随机副本路径** ⇒ 事项 ID（Kind+Volume+Path 派生）每次都变 ⇒ 重复事项堆积
     ⇒ 修法：`Path` 存**稳定的 rel**，副本由执行器按 `(volume, rel)` 从 pending 集合**反查**。
- 副本保护期：保留在 `<TempDir>/sproxy-reconcile-pending/`（**不在** `pkg/tempclean` 已知前缀内 ⇒ 不会被按龄误清）；
  由重试任务在终态（收敛/转人工）清理；**启动兜底扫描**补登记孤儿（不依赖 issues 持久化）。

**验证**：
- 单测：`reconcile_pending_test.go` 6 例（保留/上报/扫描/越界防护/nil reporter/重试收敛清理）
  + `issues_executors_blobmd5_test.go` 2 例 + `blob_reconcile_wiring_test.go` 3 例；
- **变异命中 4 处**：不可判定当通过 / 去掉接线 / md5 只看形态 / 忽略保留标记 / 去掉越界防护 / 去掉 skipped 统计；
- 全量回归：`pkg/server` 51.5 s、`pkg/volume/ext/baidupcs` 289.7 s、`cmd/sproxy` 全绿；`gofmt`/`goimports`/`go vet` 干净。

### 7.10 配额缺口核实与方案 A 落地（2026-11-13）

**订正（我先前表述不准确）**：
- ❌「`baidupcs` 的 `stage-*` 裸写未被配额记账」——**不准确**。普通上传的本地峰值**已被 FS 层预留覆盖**：
  `pkg/server/volumes.go:765` `syncpkg.ApplyStagingQuota(fs, h.stagingQuotaTrackerFor(owner))`
  （HTTP 上传 / 转存 / 备份 / 同步推送同一装配形状）。
- ✅ **真实缺口 = "④ 保留的待复查副本"**：保留发生在 `Storage.Put` **返回之后**（FS 层的预留已释放），
  副本继续占盘却**无账**。

**架构约束（决定了方案形态）**：
| 事实 | 影响 |
| --- | --- |
| `h.StagingQuotaTrackerFor` 只在 `RegisterRoutes` **之后**可用 | 而 `baidupcs.Storage` 在**卷装配期**构造 ⇒ 构造参数注入不可行 |
| tracker 是**按 owner** 的，`Storage` 是**按卷**的（不感知 owner） | Storage 内无法选出 owner 作用域 |

**方案 A（用户裁定）**：为「待复查副本」设**专用配额作用域**（保留 owner 名 `pending-reconcile`
⇒ 独立 `staging/<该名>` Scope），装配后经 `SetStagingQuota` 注入；保留 ⇒ `ReserveUsage(字节数)`，
清理 ⇒ `ReleaseUsage(同值)`。
- **分账**：与"用户上传在途"分开计量（同一磁盘两本账，但语义清晰：一份是"请求级在途"，一份是"系统级待复查"）；
  实际占用因"原子保留/清理"而**不重叠**，不会重复计量同一份 Peak。
- **字节数随副本落盘**（`pending.json` 记 `size`）⇒ **进程重启后仍能正确释放**，避免额度泄漏。
- **磁盘将满时**：`ReserveUsage` 失败 ⇒ **不保留**（连原目录一起清掉，不让未受管占用累积），
  **上传仍成功**（内容已在云端）⇒ 上报时不带副本路径，重试执行器判"副本不存在"转人工（Detail 写明配额不足）。
- **对外拒绝语义**：新上传的「空间不足 ⇒ 507 稍后重试」由既有 FS 层闸 + 各 handler 的 507 映射承担
  （`chunked_upload.go:547`、`write_ops.go:1293/1542`、`options.go:464`、`dir_charge.go:67/76`、`version.go:368/386`），
  本片**未改动**该面。

**验证**：baidupcs 侧 2 例（按字节记账 + 清理释放；配额不足 ⇒ 不保留且不影响上传成功）+
装配层 1 例（各卷注入**同一个**专用 tracker）；**变异命中 3 处**（保留不预留 / 清理不释放 / register 不收集可注入能力）。
