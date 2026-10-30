# 外部卷（secretdata over 网盘）避免「首次启动全量扫描」设计

> 状态：设计稿（2026-10-30）。触发场景：`baidu`（secretdata over baidupcs）卷内 272 部视频 ≈ 1.2 万 blob。

## 1. 现象与根因

**现象**：WSL 重启后 sproxy 启动耗时 21–28 分钟，且高并发下**直接失败退出**：

```
Error: 封装卷装配失败：enclosure: 封装卷 "baidu" 装配失败:
secretdata backend: 卷 "baidu" FS 构造失败:
secretdata: 列容器 "OypEIkI8i9Y5/..." 失败（拒绝不完整视图挂载）:
  获取目录下的文件列表: 远端服务器返回错误, 代码: 31034
```

**调用链**（`pkg/volume/secretdata` 装配期）：

1. `SecretdataFS.loadIndex` → 列卷根容器（`ListDir`）；
2. `scanAllContainerDirMetas`：**所有容器并行**（此前受 `loadGate = min(16, NumCPU)` 限制 = 16 并发）；
3. 每个容器 `ListDir` → `baidupcs` 适配器内部对目录条目按 30 个/批执行 `BaiduPCS-Go meta` 补齐精确 size（`binary_meta.go: mergeExactSizes`）；
4. 1.2 万 blob ⇒ 数百次外部 CLI 调用 × 16 并发 ⇒ **百度限流 31034**；
5. 任一容器列目录失败 ⇒ fail-closed（拒绝不完整视图挂载，语义正确）⇒ 卷装配失败 ⇒ 进程退出。

**要点**：`volumestate` 目录快照缓存（#896/#915）**不解决**该问题——它在读路径，且保密卷 `Persist=false`（不落盘），进程重启即失效；装配期更是直接走原始 FS。

## 2. 设计目标

| 目标 | 说明 |
| --- | --- |
| G1 启动 O(1) 外部调用 | 启动阶段外部 ListDir 次数与 blob 数**无关**（或仅与目录数相关且可忽略） |
| G2 不降低安全语义 | 「不完整视图」仍 fail-closed（不能静默空视图 → GC 误清扫） |
| G3 首次/异常后自愈 | 快照缺失、版本不符、卷内容被判为脏 ⇒ 自动回退到全量校验（一次） |
| G4 可观测 | 启动耗时、扫描 blob 数、缓存命中/回退原因进 metrics + 日志 |

## 3. 方案对比

| 方案 | 做法 | 优点 | 缺点 |
| --- | --- | --- | --- |
| **A 索引快照（推荐主方案）** | 装配完成后把「容器 → 目录 meta → 文件 meta（rel/hash/size/mtime/charged）」索引**加密落盘**到卷内 `.__index/snapshot-<ver>.enc`；下次启动直接载入，仅对快照做**增量/抽样**校验（目录列举 1 次 + 抽样 N 个 blob 存在性） | 启动 O(1)；复用现有 `Root.OpenFileEncrypted`（卷内 meta，已加密） | 快照可能陈旧（外部直改盘）⇒ 需代际 + 抽样校验兜底 |
| B 两阶段挂载 | 启动只列**目录层**（容器名 + 目录 meta），文件 meta 惰性加载（首次进入该容器时解密并校验） | 无需快照；启动快 | 首次访问大目录仍慢；`hasFileMeta`/GC 需要全集 → 需另设「GC 只对已加载容器」限制，语义变更较大 |
| C 异步校验 | 先挂载（可用）→ 后台全量校验，失败则降级为只读 + 告警 | 启动最快 | 违反 fail-closed：窗口期内用户可读写「不完整视图」，GC 风险高 ✗ |
| D 外部 ListDir 结果持久化 | 单独缓存 baidupcs `ListDir` 结果（页面级），重启直接命中 | 实现局部、收益直接 | 只减少外部调用，不减少装配期内存/解密工作量；仍需遍历全部分块条目 |

**结论**：A 为主（快照 + 增量/抽样校验），B 为 A 落地前的过渡（可选）。C 不采纳（安全语义冲突），D 可作为 A 的补充（减少回源）。

## 4. 方案 A 细节

### 4.1 快照内容（加密落盘）

```jsonc
{
  "version": 1,            // 快照格式版本
  "gen": 42,               // 卷代际（每次写操作 +1，随写路径持久化）
  "algo": "...",           // KDF/分块算法档位（不匹配即作废）
  "containers": [          // 容器 → 逻辑路径 + 目录 meta 摘要
    {"c":"<container>","rel":"jav/makers/...","dir_id":"...","blob_len":123}
  ],
  "files": [               // 文件 meta 摘要（不含明文内容）
    {"c":"<container>","rel":"...","size":123,"hash":"...","mtime":"...","charged":123}
  ],
  "dir_phys_total": 12345, // 目录 meta 足迹（容量记账）
  "created_at": "..."
}
```

- 写入位置：默认卷 `<storage_root>/meta/volume-index/<volume>.enc`（**非**外部卷根，避免污染网盘目录结构）；如要求与卷同生命周期则放卷内 `.__index/`，需评估 baidupcs 对隐藏目录的处理。
- 加密：复用 `storage.Root.OpenFileEncrypted` / `pkg/volume/secretdata` 的卷密钥（不可明文落盘）。

### 4.2 启动流程（快照命中）

1. 读快照 → 校验 `version`/`algo`/卷根指纹（`gen` 与账本对账）；
2. **一次** `ListDir` 卷根（必须，用于确认卷可达 + 容器集合未变）；
   - 容器集合与快照一致（数量 + 名称集合）；不一致 ⇒ 回退全量（G3）；
3. **抽样校验**：随机 N（默认 16，可配 `secretdata.index_sample`）个文件 → `Stat` 存在性 + size 比对；
   - 任一失败 ⇒ 回退全量（一次），并记录 metrics；
4. 载入索引（跳过解密全部文件 meta —— 这是主要省时点）；
5. 后台（可选）渐进校验剩余条目，仅告警不阻塞。

### 4.3 快照失效条件（必须回退全量）

- 文件不存在/解密失败/格式版本不符/算法档位不符；
- 卷根 `ListDir` 失败或容器集合变化；
- 抽样校验失败；
- 写路径（WriteFile/Delete/Rename）后 `gen` 递增 → 下一次启动按 `gen` 判断（相等才可复用；不等则增量：仅重扫「变更日志」记录的容器 —— 若变更日志不可用则全量）；
- **显式** `sclient volume verify --full` 强制全量。

### 4.4 与本次限流的正交修复（已落地）

- 装配期容器列目录并发上限 `maxContainerScanParallel = 4`（此前 16）；
- `listInner` 对非 `ErrNotExist` 错误退避重试 3 次（5s/20s），仅在重试耗尽后 fail-closed；
- 这两项把「必然失败」变成「慢但成功」，是方案 A 上线前的止血。

## 5. 验收标准

| 场景 | 期望 |
| --- | --- |
| 首次启动（无快照，1.2 万 blob） | 全量扫描一次，生成快照；耗时记录到日志/metrics |
| 二次启动（快照命中、卷未变） | 外部调用 = 1 次 `ListDir` + N 次抽样；启动 < 60s；日志 `index snapshot hit` |
| 外部直改盘（新增/删除文件） | 抽样或容器集合校验发现 ⇒ 回退全量（一次）⇒ 新快照；不出现「文件消失」/GC 误清扫 |
| 快照损坏/算法档位变化 | 回退全量，告警 + metric `index_snapshot_invalid` |
| 写入后重启 | `gen` 不等 ⇒ 走增量（变更加日志）或全量；不得复用陈旧索引 |

## 6. 实施拆解

1. **S1** 快照读写 + 版本/算法/gen 校验（含加密落盘），无行为变更（仅生成，不消费）；
2. **S2** 启动消费快照（命中路径）+ 抽样校验 + 回退全量（默认开启，配置 `secretdata.index_snapshot: true`）；
3. **S3** 写路径 `gen` 递增 + 增量重扫（可选，先只做「gen 不等则全量」）；
4. **S4** 可观测（metrics/日志）+ 文档（`docs/architecture.md`、运营手册）；
5. **S5** 门禁：新增 `internal/archcheck` 规则 —— 装配期外部 `ListDir` 调用必须经 `listInner`（限并发 + 重试）。

**风险**：快照陈旧导致「索引与磁盘不一致」——由抽样校验 + 容器集合校验 + `gen` 兜底；任何一条不满足即回退全量（保守优先）。
