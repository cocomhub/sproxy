# 07 · 空间账本验收 + 「覆盖写不回收」是设计而非泄漏（2026-11-27）

## 一、起因

想给 nightly 加「覆盖写不泄漏空间」的验收（旧内容的分块必须被回收，否则卷会持续膨胀）。
实测**推翻了前提**：等长覆盖后 usage 增长了一个完整文件大小（3 MiB → +3 MiB）。

## 二、为什么这不是泄漏

服务端对「同名不同内容」的策略是**保留旧版本**（`checkExistingFileForInit` 的 versioning 分支）：

| 形态 | 行为 | usage 变化 |
|---|---|---|
| 自动路由 + versioning 开 | **先备份旧版本**（`SaveVersion`）再写新内容 | **+1 个文件大小**（旧版本是独立对象） |
| 自动路由 + versioning 关 | 409 拒绝 | 不变 |
| 显式指名卷 | **恒拒**（AD-4 卷唯一性查重） | 不变 |

旧版本的字节**仍然被引用**（在 version 桶里，可 `sclient versions restore` 恢复）⇒ 按定义
就不是孤儿，不该被回收。回收它要靠 `versioning.retention` 的策略 GC（与「覆盖写」正交）。

**因此「retention GC 真的会删除过期版本」成为本结论的承重墙** —— 若 GC 不生效，卷同样会无限
增长，只是增长得更慢（被 max_versions 截断兜住一部分）。该链路此前**没有任何用例**，
本轮补了 `pkg/server/version_gc_retention_test.go`（见第七节）。

**结论**：我最初设计的断言（覆盖后 usage 不增长）会**强制产品放弃版本备份**，方向错误 ——
按「先确认是不是缺陷」的纪律，改成验收**真正的三个不变量**。

## 三、落地的验收：`TestSpaceAccountingRealVolumes`

| 断言 | 失败意味着什么 |
|---|---|
| 上传后 usage 增长 ≥ 文件大小的 1/2 | 账本根本没记（写不进配额） |
| **幂等重传后 usage 完全不变** | 短路是假的（又写了一遍） |
| **删除后 usage 回到基线 + 文件大小/4 以内** | **账本只增不减** ⇒ 配额被慢慢吃光（最难排查的一类） |

第三条是本轮真正新增的保护：此前没有任何用例观察「删除是否真的把空间还回去」——
内容断言（下载对不对）对此完全无感。

观测量用 `GET /api/volumes` 的 `usage`（容量池/卷级计数，跨存储形态统一）；
**不用** `GET /api/volumes/{name}/orphans`：它要求卷实现 `OrphanAuditor`，本地/可信卷未实现
（501/503），而 usage 任何形态都有。

## 四、过程中踩到的两个坑（都记下来）

1. **孤儿审计是异步任务**：首次/`refresh=1` 返回 **202 `{state:"running"}`**，完成才 200 带 report。
   只取一次会把 202 误判成"不支持"。而且**轮询时不能再带 `refresh=1`** —— 每一轮都会重启任务，
   永远等不到完成（实测卡到 90s 超时）。
2. **显式指名卷不能测覆盖**：`WithVolume(...)` 走 AD-4 卷唯一性查重，同名不同内容**恒拒**（409）。
   覆盖写只在**自动路由**下成立 ⇒ 用例不能指名卷，usage 也只能看全卷之和。

## 五、附带改进（与上一条 PR 的修复呼应）

409 的错误文案现在带可操作指引（`conflictHint`）—— 实测输出：

```
初始化上传失败 (HTTP 409): {"success":false,"message":"目标卷 \"main\" 已存在同名文件"}
（目标路径已有不同内容的文件；请先删除/改名后重传，或由服务端开启 versioning 以允许覆盖）
```

## 六之二、retention GC 验收：`TestVersionGC_RetentionReclaimsOverwrittenContent`

覆盖写 → 旧版本存在 → **保留期内不删** → 保留期到期后删除 → 当前文件仍在。

三段断言各有独立含义：

| 断言 | 若失败意味着 |
|---|---|
| 覆盖后版本数 == 1 | `SaveVersion` 没发生 ⇒ 第五节的前提不成立（那就不是"设计"，是丢了备份） |
| 保留期内版本数仍为 1 | GC **误删**（把"超过才删"实现成"总是删"）⇒ 用户无法恢复 |
| 到期后版本数 == 0 | GC 不生效 ⇒ 卷无限增长（本节的承重墙） |

变异命中：把 `cleanupVersionsByRetention` 调用换成 `entries = entries`（不做保留期清理）
⇒ 「保留期到期后版本应被 GC 删除，仍有 1 个」红。

> 配额归还（version 桶 Scope）另有既有单测 `TestService_ReleaseVersionUsage_ReleasesScope`
> 覆盖，本用例不重复断言。

## 六、P3：`markChunkDone` 不清 `dc.masks` 的脆弱耦合（已加断言）

上一轮报告记的「当前不可达但脆弱」本轮加了三层断言（`hybrid_mask_chunks_test.go`）：

- `TestSubRuns_FullMaskNoLongerMeansComplete`：位图全满**不足以**让 chunk 被跳过（完成判据只认 `Chunks`）；
- `TestFilterChunks_MaskDoesNotHideChunk`：位图存在也不得让未完成 chunk 被过滤；
- `TestMarkChunkDone_DoesNotLeaveFullMaskInMemory`：走**真实 `markChunkDone`**，断言
  `manifest.Blocks` 不再残留该 offset。

> **假绿的教训**：第三条第一版忘了预置 `dc.manifest`（内存镜像）⇒ `markChunkDone` 从磁盘懒加载出
> 空 manifest 再写 Chunks，`Blocks` 本来就是空的 ⇒ 断言的"清理"从未被执行，变异（不清 Blocks）
> 也**不报**。补上 `manifest:` 预置后变异才命中。**测"清理"必须确保被清理的对象真实存在**。
