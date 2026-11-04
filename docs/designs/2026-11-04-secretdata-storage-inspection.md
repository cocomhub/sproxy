# secretdata 真实存储审查能力（`volume trace` / `volume explain`）设计

- 状态：**待评审**（用户要求「设计方案评审通过后再实施」）
- 日期：2026-11-04
- 关联：`pkg/volume/secretdata`（布局事实源）、`pkg/volume`（可选能力）、`pkg/server/ops`、`cmd/sclient`、`pkg/client`
- 评审已确认的三项口径：
  1. CLI 形态 = `sclient volume trace <逻辑路径>` / `sclient volume explain <真实路径>`；
  2. 本批只做「能力包 + HTTP API + CLI」，**Web UI 下一批**（届时必须带 Playwright e2e）；
  3. **默认只读索引**，`--disk` 显式开启磁盘真值对比。

---

## 1. 问题

secretdata 卷的物理布局是**刻意不可读**的：

- 逻辑目录 → 随机命名容器目录（5–30 字符），子容器**物理嵌套**在父容器内；
- 文件 → 一个加密 meta blob（名含 `z` 标记） + N 个加密分块（无标记） + 可选 parity 段；
- 目录 → 一个加密 dir-meta blob（名含 `q` 标记），内含 `dir_id` / `parent_dir_id` / 逻辑名；
- 所有 meta 的 pad、mtime 打散、去重池共享容器进一步抹平了可辨识特征。

后果（用户实际痛点）：**运维/审计完全无法回答「这块磁盘上的这个文件是干什么的」**——
既不能从逻辑路径看到它落在哪些真实文件上，也不能从真实文件反推它属于哪个逻辑对象、
是 meta 还是分块/parity、是否是孤儿（GC 该收的垃圾）。

同类需求在别处已有形态：`/api/volumes/export`（导出）、`volume refresh`（重建索引）、
GC（清理孤儿）。但**没有「解释布局」的只读能力**，而这类能力只能由掌握布局的卷实现提供
（调用方不可能反推 `meta_pad`、blocklet、parity、dedup 共享等事实）。

## 2. 目标与非目标

### 目标

| # | 目标 |
|---|---|
| G1 | **正向**：给定逻辑路径，输出它在真实存储上的完整落点（容器链、meta blob、每个分块、parity、blocklet 数、去重池引用）。 |
| G2 | **反向**：给定真实路径（容器目录 / 单个 blob），输出它的用途与逻辑归属（哪个文件、哪个分块、哪个逻辑目录、是否 parity、是否孤儿）。 |
| G3 | **差异审计**：`--disk` 时对比「索引声明」与「磁盘事实」，给出结构化差异码（缺失 blob、孤儿 blob、大小不符、meta 不可解）。 |
| G4 | 能力由**能力包**提供（`pkg/volume` 可选接口 + secretdata 实现），HTTP/CLI 只做薄封装 ⇒ 远端卷天然支持（服务端执行），不在客户端重实现布局解析。 |
| G5 | 输出同时支持人类可读树与稳定 JSON（`schema_version`），可用退出码做脚本门禁。 |

### 非目标（明确不做）

1. **不解密内容**：只解 meta（名称/尺寸/分块索引等元数据），不读取、不校验、不落地明文。
2. **不做修复动作**（删除孤儿、重建 meta、搬迁容器）：审查只读。修复走既有 `volume refresh` / GC。
3. **不覆盖其它卷型**（本地/加密/外部/封装）：接口按「未实现 = 不支持」处置，P1 只有 secretdata 实现。
4. **不承诺索引与磁盘强一致**：本能力正是用来**暴露**不一致，而不是掩盖它。
5. 不做 Web UI（下一批）。

## 3. 事实基础（均来自现有代码，实施时不得另行假设）

| 事实 | 位置 |
|---|---|
| 卷根容器**唯一**，子容器物理嵌套在父容器目录内；扁平旧布局 fail-closed | `secretdata.go` `createContainerLocked` / `validateNestedContainers` |
| 容器目录名 = `shardseal.RandDirName()`（5–30 位 `[A-Za-z0-9]`，规避 `meta`/`data`/`secret` 前缀） | `cryptox/shardseal/naming.go` |
| 文件名三类：dir-meta（随机段含 `q`）、file-meta（随机段含 `z`）、内容分块（无标记） | `shardseal.ClassifyName` / `NameKind` |
| **分类只能看随机段**：hash 段允许出现 `q`/`z`，`strings.Contains` 判型会误判 | `naming.go` `markInRandSeg` |
| 一个分块 = 一个 blob 文件；块内可有多个 blocklet（视频 GOP 等） | `shardseal.ChunkInfo{FileName, Offset, OrigSize, EncSize, Blocklets[]}` |
| parity 是**独立 blob**，命名与普通分块同形 ⇒ **无法凭名字区分，必须读 meta** | `shardseal.ParityInfo{FileName, EncSize, ChunkCount}` |
| 文件 meta 记录原文件 `Name/Size/SHA256/MTime`，加密落盘 | `shardseal.OriginalInfo` |
| `dataDir` ≠ `dirSeg` ⇒ 分块位于**卷级去重容器**（实验性 `Dedup`，生产未接线） | `metaEntry.dataDir`、`features.go` |
| 索引 `index` 的键 = 逻辑 rel（`a/b.txt`），值含 `dirSeg/metaName/meta/phys` | `secretdata.go` `metaEntry` |
| 索引另有目录三类映射：`dirSegs`（逻辑目录 → 容器）、`dirIDs`（逻辑目录 → dir_id）、`dirParents`（逻辑目录 → 父逻辑目录） | `secretdata.go` 字段声明、`features.go` |
| 目录 meta 记录 `Name/ParentDirID/DirID` ⇒ 逻辑路径需**沿 parent 链重建** | `dirMeta` |

> 由最后两条推出两条实现约束：① 正向只需索引；② 反向「容器 → 逻辑目录」必须解 dir-meta
> 并沿 parent 链上溯；「blob → 分块 vs parity」必须解 file-meta（名字无区别）。

## 4. 能力落位（分层）

```
cmd/sclient volume trace|explain        # CLI：调 HTTP，渲染文本/JSON
        ↓
pkg/client  VolumeTrace / VolumeExplain # 唯一 HTTP 客户端封装
        ↓
GET /api/volumes/{name}/trace|explain   # 装配层：解析卷 → 断言能力 → 调用
        ↓
pkg/server/ops  Inspect(...)            # server 作用域薄编排（不做布局解析）
        ↓
pkg/volume  StorageInspector（可选能力）+ AssertStorageInspector
        ↓
pkg/volume/secretdata  实现             # 唯一布局事实源
```

理由（对照 `AGENTS.md` 能力矩阵与收敛原则）：

- 布局解析若写在 `pkg/server` 或 CLI，就是**第二份布局事实**——必然与写路径漂移（历史 P0 同类）。
- 放 `pkg/volume` 可选能力 ⇒ 所有消费方（HTTP / CLI / 未来 Web UI / ops 工具）共用一份实现。
- 未实现该能力的卷 ⇒ `ok=false`，HTTP 返回 `501 not_inspectable`，**绝不返回空结果**（空结果会被误读为「没有落点」）。

## 5. 契约

### 5.1 Go 能力接口（`pkg/volume`）

```go
// InspectMode 是审查模式：索引（快，默认）或磁盘真值（慢，显式）。
type InspectMode int

const (
    InspectIndex InspectMode = iota // 只读索引（默认）
    InspectDisk                     // 索引 ∪ 磁盘，输出差异
)

// StorageInspector 是「真实存储可解释」能力（只读）。
type StorageInspector interface {
    // Trace 返回逻辑路径 rel 在真实存储上的全部落点。
    // rel 不存在 ⇒ (nil, fs.ErrNotExist)；不影响其它错误语义。
    Trace(ctx context.Context, rel string, mode InspectMode) (*PathTrace, error)
    // Explain 返回真实路径 realRel（卷根相对）的用途与逻辑归属。
    // realRel 不在卷根内 ⇒ ErrOutsideVolume（fail-closed，绝不静默夹取）。
    Explain(ctx context.Context, realRel string, mode InspectMode) (*RealExplanation, error)
}

func AssertStorageInspector(v any) (StorageInspector, bool)  // 类型断言助手（仓内既有模式）
```

数据模型（JSON 字段即对外契约，`schema_version: 1`）：

```go
type PathTrace struct {
    Volume   string        `json:"volume"`
    Mode     string        `json:"mode"`            // "index" | "disk"
    Rel      string        `json:"rel"`
    Kind     string        `json:"kind"`            // "file" | "dir"
    Size     int64         `json:"size"`            // 逻辑明文大小
    MTime    int64         `json:"mtime"`
    Meta     *BlobRef      `json:"meta,omitempty"`  // file/dir meta blob
    Chunks   []ChunkRef    `json:"chunks,omitempty"`
    Parity   *BlobRef      `json:"parity,omitempty"`
    Dedup    *DedupRef     `json:"dedup,omitempty"` // dataDir 与容器不同时
    Anomalies []Anomaly    `json:"anomalies,omitempty"`
}

type BlobRef struct {
    RealPath string `json:"real_path"`  // 卷根相对，形如 "k3f9a/2026/xxxx"
    Bytes    int64  `json:"bytes"`      // 磁盘声明大小（index 模式=meta 记录）
    Plain    int64  `json:"plain,omitempty"`
    OnDisk   *bool  `json:"on_disk,omitempty"` // 仅 disk 模式
}

type ChunkRef struct {
    Index    int    `json:"index"`
    Blob     BlobRef `json:"blob"`
    Blocklets int   `json:"blocklets"` // 块内 blocklet 数（>1 = 视频/大块分段）
}

type RealExplanation struct {
    Volume   string       `json:"volume"`
    Mode     string       `json:"mode"`
    RealPath string       `json:"real_path"`
    Kind     string       `json:"kind"` // container|dir_meta|file_meta|chunk|parity|unknown
    Owner    *LogicalRef  `json:"owner,omitempty"`
    Container *ContainerRef `json:"container,omitempty"` // 容器本身或 blob 所在容器
    Evidence []string     `json:"evidence,omitempty"`    // 判定依据（人可核对）
    Anomalies []Anomaly   `json:"anomalies,omitempty"`
    Entries  []RealEntry  `json:"entries,omitempty"`     // 仅 container：目录内逐条用途
}

type LogicalRef struct {
    Rel      string `json:"rel"`       // 逻辑路径（反查不到 ⇒ 空）
    Role     string `json:"role"`      // file | dir
    ChunkIdx *int   `json:"chunk_index,omitempty"`
}

type Anomaly struct {
    Code   string `json:"code"`   // 见 §6 差异码表
    RealPath string `json:"real_path,omitempty"`
    Detail string `json:"detail"`
}
```

### 5.2 HTTP

| 方法 | 路径 | 参数 | 说明 |
|---|---|---|---|
| `GET` | `/api/volumes/{name}/trace` | `path`（必填，逻辑 rel）、`disk`（`0/1`，默认 0） | 正向落点 |
| `GET` | `/api/volumes/{name}/explain` | `real`（必填，卷根相对）、`disk` | 反向解释 |

- 路由类：`ClassReader`（与只读的 `/api/volumes/export` 同级；`Mutating=false`）。
- 错误映射：卷不存在 `404`；该卷不支持审查 `501 {"error":"not_inspectable"}`；路径越界 `403`；
  `real` 不存在且 `disk=1` ⇒ `200` + `kind:"unknown"` + `anomaly:not_found`（审查工具需要解释「为什么这里没东西」，不是 404）。
- 响应体统一 `{"trace": {...}}` / `{"explain": {...}}`，遵守 R54（`internal/httpjson`）。

### 5.3 CLI

```
sclient volume trace   <path> [--volume <v>] [--disk] [--json]
sclient volume explain <real> [--volume <v>] [--disk] [--json] [--depth 1]
```

- `<path>` 走现有相对/绝对路径解析（`state.State.ResolveRemotePath`），`<real>` **不做**逻辑解析，原样传服务端。
- 退出码：`0` 成功且无差异；`3` 成功但发现差异（`--disk` 下缺/多/大小不符）；`1` 参数或服务端错误。
- 文本输出即评审时确认的树形（见下），`--json` 输出 §5.1 结构。

### 5.4 文本输出（正向）

```
photos/2026/a.jpg   12.3 MiB  mtime=2026-11-01 12:00
└─ 卷根容器 k3f9a
   └─ 容器 2026   (逻辑目录 photos/2026, dir_id=9xQ..., parent=2ab...)
      ├─ 文件meta 7xQ...z...  1.5 MiB   [meta_pad=1.2 MiB, 密文 SHA256=ab12…]
      ├─ 分块#0   9ab...      4.0 MiB   明文 4.0 MiB  (blocklets=1)
      ├─ 分块#1   c11...      4.0 MiB   明文 4.0 MiB  (blocklets=1)
      ├─ 分块#2   e07...      4.2 MiB   明文 3.8 MiB  (blocklets=3)
      └─ parity   f5d...      4.0 MiB   k=3
```

### 5.5 文本输出（反向）

```
真实路径 k3f9a/2026/7xQ...z...
用途     file_meta
归属     photos/2026/a.jpg  (file, 12.3 MiB)
容器     k3f9a/2026  逻辑目录 photos/2026 (dir_id=9xQ..., parent=2ab...)
依据     meta Original.Name=a.jpg + 索引反查 hit
```

```
真实路径 k3f9a/2026/   (容器)
用途     container
逻辑目录 photos/2026  (dir_id=9xQ..., parent=2ab...)
条目     dir_meta  9xQ...q...  1.4 MiB   本目录
         file_meta 7xQ...z...  1.5 MiB   → photos/2026/a.jpg
         chunk     9ab...      4.0 MiB   → photos/2026/a.jpg #0
         chunk     f5d...      4.0 MiB   → photos/2026/a.jpg parity
         chunk     000...      4.0 MiB   → (孤儿) 索引无引用
```

## 6. 解析算法

### 6.1 正向 `Trace(rel, mode)`

1. 索引查 `rel`：未命中 ⇒ `fs.ErrNotExist`（目录走 `dirs`/目录容器映射）。
2. 组容器链：由 `dirSeg` 沿**容器父链**拆段（嵌套布局下 `dirSeg` 本身是可拆的路径段序列）。
3. 文件：`meta = index[rel].meta` ⇒ 逐 `Chunks[i]` 生成 `ChunkRef`（`FileName`、`EncSize`、`OrigSize`、`len(Blocklets)`）；`Parity` 单独一项；`dataDir != dirSeg` ⇒ `Dedup{Container, RefCount(srcFromIndex)}`。
4. 目录：输出容器 + dir-meta blob（含 `dir_id`/`parent`）。
5. `mode == InspectDisk`：对每个 `BlobRef` 做一次 `inner.Stat`（**并发上限 8**），填 `OnDisk`、比对大小；不符/缺失 ⇒ `Anomaly`。

### 6.2 反向 `Explain(realRel, mode)`

1. **越界校验**：`path.Clean` + 必须相对、不含 `..`、不以 `/` 开头；任何越界 ⇒ `ErrOutsideVolume`（`403`）。
2. 判定输入形态：
   - 是容器目录（`inner.Stat` 为目录）⇒ 分支 A；
   - 否则按文件名分类（`shardseal.ClassifyName`，**只看随机段**）⇒ 分支 B。
3. 分支 A（容器）：
   1. 用卷 secret 解该容器内 dir-meta（每个容器恰好一个；`ClassifyName==KindDirMeta`）⇒ `Name/ParentDirID/DirID`；
   2. 沿 `parent_dir_id` 链上溯重建逻辑路径：`dirIDs`（逻辑目录 → dir_id）反查得到本目录 rel，
      再逐级 `dirParents[rel]` 上溯到卷根；任一级缺失 ⇒ `parent_unresolved`（不猜测）；
   3. 容器内若出现**多个 dir-meta**（异常），全部列出并标注 `dir_meta_ambiguous`，不静默取第一个；
   4. `ListDir` 该容器 ⇒ 逐条 `RealEntry`：dir-meta=本目录；file-meta=反查索引 `metaName` 命中 → `Owner.Rel`；chunk=反查索引 `FileName` 命中 → `Owner.Rel` + `chunk_index` 或 `parity`；未命中 ⇒ `blob_orphan`。
   5. `mode == InspectDisk` 额外做：索引声明该容器应有但磁盘缺的 blob ⇒ `blob_missing`。
4. 分支 B（blob）：
   1. `KindDirMeta` ⇒ 解 dir-meta，输出其容器与逻辑目录（同 A 的 1–2 步）。
   2. `KindFileMeta` ⇒ 解 meta，`Owner.Rel = 索引反查(container, metaName)`；索引无命中 ⇒ `meta_orphan`（可解但无索引条目，可能是索引未加载/外部写入）。
   3. `KindChunk` ⇒ 反查索引（容器内比对 `Chunks[].FileName` 与 `Parity.FileName`）⇒ 命中输出 `chunk#i` / `parity`；未命中 ⇒ `blob_orphan`（`detail` 说明「索引无引用；GC 会按孤儿处理」，但**不**断言可删——GC 有 re-confirm 逻辑）。
5. 反查实现：只对**目标容器**过滤索引（`dirSeg`/`dataDir` 前缀匹配），O(该容器条目数)；不全区遍历、不建全量反向索引（避免百万条目的内存与陈旧风险）。

### 6.3 差异码表

| Code | 含义 | 产生模式 |
|---|---|---|
| `blob_missing` | 索引/meta 声明某 blob，磁盘不存在 | `--disk` |
| `blob_orphan` | 磁盘有 blob，索引无任何引用 | `--disk` |
| `size_mismatch` | 磁盘大小 ≠ meta 记录（`EncSize`/`postpad`） | `--disk` |
| `meta_unreadable` | meta blob 无法解密/JSON 损坏（secret 错/版本不认识） | 两种 |
| `container_not_indexed` | 容器存在但索引无对应逻辑目录（索引未加载或外部创建） | `--disk` |
| `parent_unresolved` | dir-meta 的 `parent_dir_id` 在索引中找不到 | 两种 |
| `dir_meta_ambiguous` | 同一容器内出现多个 dir-meta（异常布局） | 两种 |
| `not_found` | 输入真实路径在磁盘不存在（解释「这里为什么空」） | `--disk` |
| `dedup_shared` | 分块在去重池容器（共享内容；非异常，仅提示） | 两种 |

> 注意：`blob_orphan` **不等于可安全删除**（GC 有 reconfirm 流程与 crash 窗口）。
> 输出文案必须写「疑似孤儿，删除请走 GC」，避免运维据此直接 `rm`（历史 P0 教训：绕过 Root/卷语义直删）。

## 7. 安全

| 风险 | 处置 |
|---|---|
| 路径穿越（`../../other-owner`） | 反解为卷根相对 + 显式拒绝越界（`ErrOutsideVolume` → 403）；禁止静默 `filepath.Clean` 后继续。 |
| 越权读布局 | 路由 `ClassReader`（与 `/api/volumes/export` 同级）；隧道内层由 `ClassTunnel` 继承同一鉴权；不新增公开面。 |
| 明文泄漏 | **只解 meta，不读内容**；日志只记 rel/blob 名，不记 meta 明文字段；不写任何临时解密文件。 |
| secret 使用 | 走卷装配期已注入的 secret（`Options.Secret`），审查路径不另行读取 secrets 卷、不新增密钥来源。 |
| 输出被当作删除依据 | 文案与 JSON 均标注「只读审计，不构成删除建议」；退出码 3 仅表示差异存在。 |

## 8. 性能与并发

- 默认（index 模式）：O(该文件的 `Chunks` 数)；反向 O(目标容器条目数)；不全区扫描。
- `--disk`：只对**目标容器**（或该文件所在容器）做 `Stat`/`ListDir`，并发上限 8；单容器超 5 万条目时截断并附 `anomaly:truncated`（防一次请求打爆内存）。
- 与写路径并发：审查只读索引快照 + `inner` 读；允许「审查结果 = 某一中间态」。输出需带 `mode` 与（可选）`captured_at`，不承诺强一致。
- 不引入新锁层级：复用 `s.mu` 的读临界区取条目快照后即释放（不在锁内做 IO）。

## 9. 测试与门禁计划（TDD）

### 单测（`pkg/volume/secretdata`，真实临时目录 + 真实 secret，纯标准库）

| 用例 | 断言 |
|---|---|
| `TestTrace_FileMultiChunk` | 3 分块 + 1 parity 的落点路径/大小与写入一致；`blocklets` 数正确 |
| `TestTrace_Dir` | 目录输出容器 + dir-meta；`dir_id` 与写路径一致 |
| `TestTrace_DedupShared` | `dataDir != dirSeg` 时标记 `dedup_shared` 且不把分块算进该文件容器 |
| `TestExplain_Container` | 容器内逐条用途正确（dir_meta/file_meta/chunk/parity）与逻辑归属 |
| `TestExplain_ChunkVsParity` | **仅靠名字无法区分** ⇒ 必须解 meta 才能判 parity（变异：改成按名字判定 ⇒ 红） |
| `TestExplain_Orphan` | 手工造一个无引用 chunk ⇒ `blob_orphan`；文案含「疑似」 |
| `TestExplain_RejectsEscape` | `../`、绝对路径、`a/../../b` ⇒ `ErrOutsideVolume` |
| `TestExplain_NotFound` | `--disk` 下不存在路径 ⇒ `kind=unknown` + `not_found`，不是 404 |
| `TestDisk_SizeMismatch` | 写后截断一个 blob ⇒ `size_mismatch` |
| `TestDisk_BlobMissing` | 删一个 chunk ⇒ `blob_missing` |
| `TestUnsupportedVolume` | 非 secretdata 卷 ⇒ `AssertStorageInspector` false；HTTP 501 |
| `TestJSONSchema_Golden` | 序列化结果与 golden 文件逐字节一致（schema 稳定性） |

变异清单（必须命中）：把 `ClassifyName` 换成 `strings.Contains`（红）；把 parity 判定改成「最后一个分块」（红）；去掉越界校验（红）；`--disk` 只查存在不查大小（红）。

### 装配层 / CLI / 门禁

- `pkg/server/ops` 单测：不支持卷 501、越界 403、参数缺失 400、正常 200。
- `pkg/client` 单测：两个方法的 URL/参数/错误映射；沿用每实例独立 `Transport`（禁 `http.DefaultClient`）。
- CLI 单测：文本渲染、`--json` 透传、退出码 0/3/1。
- `test/route_smoke_test.go` 自动覆盖新路由（非 5xx、无 wiring 标记）。
- `route_classes.tsv` 新增两行（`reader`）；`mutating_route_audit.tsv` 不新增（只读）。
- `docs/api.md` 补两节、`docs/cli.md` 补两命令、`AGENTS.md` 能力矩阵加一行、`docs/architecture.md` 加「审查能力」小节。
- R38（分类双锁）/R55（文档）/R54（JSON 单一实现）自动生效；`--disk` 差异码表加进 `docs/api.md`，避免成为「实现细节」。

## 10. 分批与验收

| 批次 | 内容 |
|---|---|
| **P1（本设计实施范围）** | `pkg/volume` 能力接口 + secretdata `Trace/Explain`（index/disk 两模式）+ `pkg/server/ops` + 两条路由 + `pkg/client` + CLI 两命令 + 全部单测/文档/TSV |
| P2（下一批） | Web UI 只读浏览页（逻辑树 + 真实路径注解 + 差异高亮），必须带 Playwright e2e |
| P3（按需） | 其它卷型的能力实现（本地/加密/外部），接口已就位，实现即插即用 |

**验收标准**

1. 对任意 secretdata 文件，`volume trace` 输出的每个真实路径都能在卷根下 `Stat` 到（`--disk` 全绿）；
2. 对任意容器目录，`volume explain` 的条目数与 `ListDir` 一致，且每条都带用途（不允许「未知用途」既无归属又无 anomaly）；
3. 手工制造任一差异（删 blob / 截断 / 造孤儿），`--disk` 必须给出对应差异码且退出码 3；
4. 非 secretdata 卷返回 501 且提示明确（不允许空结果）；
5. `pkg/volume/secretdata` 该能力单测覆盖 ≥90%，`make lint` 0，archcheck 全绿。

## 11. 风险与未决

| 风险 | 缓解 |
|---|---|
| 索引未加载/陈旧时反向结论误导 | 输出始终带 `mode` + `Evidence`；`blob_orphan` 文案标注「疑似」并指向 GC |
| 巨型容器（>5 万条目）响应过大 | 截断 + `truncated` anomaly + 提示用 `--depth`/缩小路径 |
| `dir_meta` 解密依赖 secret 轮换正确性 | 解密失败 ⇒ `meta_unreadable`（不中断整次 explain，逐条标注） |
| 与 GC 语义漂移（GC 有 reconfirm） | 差异码表只描述「索引 vs 磁盘」，不描述「可删除性」；由 GC 决定 |

**待评审确认的默认取舍**（如无异议即按此实施）：

1. 路由类用 `ClassReader`（只读，与 `/api/volumes/export` 同级），而非更严的 `ClassAuth`；
2. `explain` 接受**卷根相对路径**（绝对路径由 CLI 剥前缀），服务端只做越界拒绝；
3. 不实现「删除/修复」子命令（只读审查）；
4. `--disk` 差异时退出码 3（便于脚本门禁），文本模式也照此；
5. P1 仅 secretdata 实现，其它卷型返回 501。
