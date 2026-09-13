# 远程访问面（读+写）与 sync/remote 收敛 设计

> 本规格回答三个问题并给出**一次定到位**的目标架构：① 预期将来的**远程写**要求什么；② `pkg/sync*` 与规划的 `pkg/remote` **是否功能重叠、是否需重构**；③ 如何避免因考虑不周而**反复重设计**。
> 上游依据：`2026-09-11-y-cluster-read-design.md`（Y 一期只读 + §11 写批次缝）、`2026-09-12-file-service-extraction-design.md`（D-2 域操作 API）。
> 实施见 `docs/superpowers/plans/2026-09-13-remote-access-architecture.md`。

**结论先行**：**存在重叠，且重叠点恰好落在"单文件远程读写"这一层** —— `pkg/sync` 的 `HTTPTransport` 已经是一份**读+写**的远程文件操作实现，而规划的 `pkg/remote` 要做第二份。修法不是新建抽象，而是**把已有的 `sync.FS` 确立为唯一抽象**，`pkg/remote` 作为它的 **mesh 版实现**；`pkg/sync` 退回纯逻辑。**远程写是这条收敛的触发器，而不是另起一套的理由。**

---

## 1. 实测证据

### 1.1 `sync.FS` 已经是完整的远程读写面

`pkg/sync/entry.go:34-42`：

```go
type FS interface {
	ListDir(ctx, path) ([]Entry, error)              // 读
	Stat(ctx, path) (*Entry, error)                   // 读
	OpenRead(ctx, path) (io.ReadCloser, error)        // 读
	WriteFile(ctx, path, r io.Reader, size, mtime int64) error   // 写
	Rename(ctx, from, to) error                       // 写
	Delete(ctx, path) error                           // 写
	MakeDir(ctx, path) error                          // 写
}
```

已存在的**两个实现**：`LocalFS`（`local_fs.go`，390 行）、`HTTPTransport`（`http_transport.go`，521 行 + 1009 行测试）。
组装在 `pkg/syncexec/executor.go`（`Run` 按 `DirectionPush/Pull` 决定 `srcFS`/`dstFS`，远端侧 `newRemoteTransport` → `sync.NewHTTPTransport`）。

### 1.2 三层职责的实测边界

| 层 | 归属（现状） | 与规划的 `pkg/remote` 关系 |
|---|---|---|
| 编排（枚举/差异/冲突/过滤/并发/进度/配额结算） | `pkg/sync`（`ComputeDiff`/`Decide`/`Engine`）+ `pkg/syncmgr`（任务/持久化）+ `pkg/syncexec`（组装） | **不重叠**（remote 不该碰） |
| **单文件远程读写** | `pkg/sync.HTTPTransport`（**读+写 7 方法全有**） | ⚠️ **重叠**：规划的 `pkg/remote` 做 `List`/`Stat`/`Download` |
| 传输载体 | HTTP 直连（`RemoteConfig{URL,AK/SK}`，需对端可达） | mesh（零信任 + 双向指纹 pin + 卷授权 + 无需直连） | **不是重叠，是两种载体** |

### 1.3 包文档与实现自相矛盾（分层未明确的直接证据）

`pkg/sync/job.go` 包文档：*"提供节点间文件同步/复制的**纯逻辑核心** … **不包含任何网络传输实现**"* —— 而 `http_transport.go`（521 行网络实现）就在同一包内。

### 1.4 写批次的原计划（Y-C §11）

> `mesh_readers` 条目加 `scope: read|write`（一期固定 read）；写路径需要节点可见性协调（谁持有该路径的写权），届时才引入协调机制。

### 1.5 抽取规格开篇的动机（本规格要根治的问题）

> ① 新表面（Y 一期的 `remote_read`，**未来的 `remote_write`**）倾向于**各自重写**上传/下载，而非复用

D-2 原文：**「文件服务暴露域操作 API：HTTP 处理器降为薄适配；新表面（remote_read/write）挂域 API 而非改写请求」**。

### 1.6 现有 B 侧只读面是"过渡形态"

master 的 `pkg/server/remote_read.go`：`delegate` 通过**改写 query + 伪造 actor**（`withActor(ctx, tgt.owner)`）把请求交回 HTTP 处理器 —— 即抽取规格 §9 所述的 A–C 阶段过渡挂载形态；D-2 后应改为**直调域 API**。

### 1.7 写面各 op 的前置要求（决定远程面必须携带什么）

| op | `pkg/files` 现状要求 |
|---|---|
| `upload`（`write.go`） | **必带** `X-File-Checksum`；**可选** `X-File-MTime` |
| `rename`（`rename.go:60,355`） | **必带** checksum |
| `delete`（`delete.go:91-93`） | **必带** checksum |
| `mkdir`/`rmdir`（`dirs.go`） | 无 |

而 `sync.FS` 的签名里**没有 checksum**：
- `WriteFile` → A 侧须**自行计算**（`HTTPTransport` 已如此：spool 后由 `client.Upload` 内部算）
- `Rename` / `Delete` → A 侧须**先 `Stat` 取 checksum** 再携带

⇒ 这是"必须现在定清"的具体项（见 §5.3）。

---

## 2. 根因

"远程访问"被两条**正交轴**切开，而现有实现把两条轴焊在了一起：

```
轴 A：做什么     编排（目录树差异同步） ↕ 单次操作（一个文件的读写）
轴 B：怎么到对端  HTTP 直连（AK/SK + 可达地址） ↕ mesh（指纹 pin + 卷授权 + 免直连）
```

`HTTPTransport` 同时承担「轴 A 的单次操作 + 轴 B 的 HTTP」，`pkg/remote` 计划再做一遍「单次操作 + mesh」⇒ **同一层语义实现两次**。只读时还能容忍；一旦有远程写，**checksum / mtime / 幂等 / 重试 / 错误码 / 授权前置**会立刻分叉成两套。

---

## 3. 目标架构

```
L4 装配      pkg/server            HTTP 面 / 路由 / 配置 / 隧道接线 / 远程面装配
L3 编排      pkg/sync  纯逻辑 + FS 接口 + LocalFS
             pkg/syncmgr  任务生命周期 + RemoteTarget
             pkg/syncexec 组装（LocalFS × 远端 FS 实现）
L2 远程访问  ├─ A 侧 pkg/remote   mesh 建链/缓存 + remote:// 句柄 + 【实现 sync.FS】
             └─ B 侧 /remote/*    只读 listener（已有）+ 写 listener（新增）
                                  只做「授权 → 调 L1 域 API → 审计」
L1 文件域    pkg/files   域操作 API（读写面的非 HTTP 入口） ← D-2
L0 存储域    pkg/storage（+capacity、+registry）
```

### 3.1 责任表（谁做什么 / 谁绝不做什么）

| 组件 | 负责 | **绝不负责** |
|---|---|---|
| `pkg/remote` | mesh 建链与链路缓存（每节点 mux+Tunnel）、`remote://<node>/<vol>/<path>` 解析、指纹 pin、**实现 `sync.FS` 7 方法** | diff / 冲突判定 / 过滤 / 配额结算 / 目录树编排 / 重试策略 |
| `pkg/sync` | 枚举、`ComputeDiff`、`Decide`、`Engine` 编排、**`FS` 接口**、`LocalFS` | **任何网络传输**（`HTTPTransport` 迁出） |
| `pkg/syncmgr` | 任务生命周期、持久化、配额结算、`RemoteTarget` 模型 | 知道对端是 HTTP 还是 mesh（只认 `RemoteTarget.Kind`） |
| B 侧 `/remote/*` | 授权（指纹 pin + 卷 ACL + `scope`）、**调 L1 域 API**、审计 | 自己实现读写（**不许**复制 checksum/原子改名/版本/配额/锁） |
| `pkg/files` | 域操作语义（含 checksum 门禁、mtime、原子改名、版本、配额、文件锁、卷路由） | 知道调用方是 HTTP 还是隧道 |

### 3.2 收敛原则（一句话）

> **`sync.FS` 是唯一的"远程文件操作"抽象；HTTP 与 mesh 只是它 `ReadWriter` 的两种装载方式。**

---

## 4. 已确认的决策（用户确认于 2026-09-13）

| # | 决策 | 结论 |
|---|---|---|
| **A** | 抽象收敛方向 | **`pkg/remote` 实现 `sync.FS`**（不另立 client 接口） |
| **B** | 授权轴 | **`mesh_readers` 条目加 `scope: read\|write`**（Y-C §11 原计划），不新增独立 `mesh_writers` 列表 |
| **C** | 切入顺序 | **先 D-2（域操作 API），再做写批次**（避免先产出一版"改写请求"的写面） |

---

## 5. 必须一次定清的 9 项（本规格的核心价值）

### 5.1 单一抽象

`sync.FS` 为唯一抽象；`pkg/remote` 实现它；`pkg/sync` 只留纯逻辑 + `FS` + `LocalFS`；网络实现（`HTTPTransport`）迁出 `pkg/sync`。
**不定则**：二期加写会另立方法集 ⇒ 与 `FS` 平行的第二套语义永久分叉。

### 5.2 `pkg/remote` / `pkg/sync` 的定位

见 §3.1。**不定则**：`pkg/remote` 逐渐长成第二个 `pkg/sync`。

### 5.3 远程面的 op 集与域 API 映射（含 checksum 缺口）

| 远程 op | 方法+路径 | `sync.FS` | 域 API（D-2） | A 侧需额外做什么 |
|---|---|---|---|---|
| list | `GET /remote/list?vol&path` | `ListDir` | `List(owner, vol, dir)` | — |
| stat | `HEAD /remote/stat?vol&path` | `Stat` | `Stat(owner, vol, path)` | — |
| download | `GET /remote/download?vol&path`（Range） | `OpenRead` | `Open(owner, vol, path)` | — |
| **write** | `POST /remote/write?vol&path`（+`X-File-MTime`、`X-File-Checksum`） | `WriteFile` | `WriteFile(owner, vol, rel, r, size, mtime, expectedCS)` | **spool + 计算 checksum** |
| **rename** | `POST /remote/rename?vol&from&to`（+`X-File-Checksum`） | `Rename` | `RenameIfUnchanged(owner, vol, from, to, expectedCS)` | **先 `Stat` 取 checksum** |
| **delete** | `POST /remote/delete?vol&path`（+`X-File-Checksum`） | `Delete` | `DeleteIfUnchanged(owner, vol, path, expectedCS)` | **先 `Stat` 取 checksum** |
| **mkdir** | `POST /remote/mkdir?vol&path` | `MakeDir` | `Mkdir(owner, vol, path)` | — |

**两条确定性结论**：
1. **checksum 门禁不放松**：`Rename`/`Delete` 的 `FS` 签名没有 checksum，故 A 侧**先 stat 取 checksum 再携带**；B 侧在**同一把文件锁内**校验+执行（`DeleteIfUnchanged`/`RenameIfUnchanged`），使"删除/改名必须校验和"这一不变量在远程面上**语义等价保留**。
2. **`WriteFile` 必须 spool + 哈希**：`FS` 签名无 checksum，而域 API 要求它 ⇒ A 侧落临时文件、边写边算 SHA-256、然后流式提交（`HTTPTransport` 已是此模式，mesh 版照此实现）。副作用（磁盘占用、断点语义）必须在协议层说清（见 §5.4）。

### 5.4 写协议形态

隧道内**单请求流式写**（`size` + `mtime` + `checksum` 随帧/元数据传），B 侧落 `*.partial` → 校验 → **原子 rename**（复用 `pkg/files` 的 `storage.Root.AtomicRename`）。
**不复用** HTTP 分块的三段式会话（`/upload/init|chunk|complete`）——那是 HTTP 请求体上限与可续传的产物。
但 **D-2 的域 API 必须覆盖两者的共同内核**（`WriteFile(...)`），分块会话作为它的上层（临时文件 + 收尾 rename）。
**不定则**：要么把 HTTP 会话模型无谓搬进隧道，要么让域 API 只服务 HTTP、二期再改。

### 5.5 载体成为可替换维度

```go
// pkg/syncmgr
type RemoteKind string
const (RemoteKindDirect RemoteKind = "direct"  // http(s) + AK/SK
       RemoteKindMesh   RemoteKind = "mesh")   // node + vol + 指纹 pin

type RemoteTarget struct {
    Name string
    Kind RemoteKind
    // direct
    URL string; AccessKey, AccessKeySecret, AccessKeyID string
    // mesh
    Node, Volume string; PeerPins []string; Transport string // auto|relay|webrtc
}
```
**不定则**：`syncmgr` 任务模型要改两次（先塞 mesh 字段、再加新字段），配置与持久化格式各迁移一遍。

### 5.6 寻址统一

`remote://<node>/<vol>/<path>` 作为**句柄**（Y-C AD-4 原话），`pkg/syncmgr.Job` 的 `Src`/`Dst` 与 sclient/WebUI 使用**同一字符串**；`RemoteTarget` 只描述"怎么到对端"，`remote://` 描述"到对端的哪个卷的哪个路径"。
**不定则**：sync 用 URL、remote 用句柄、CLI 再有第三种写法，三者互相翻译。

### 5.7 授权轴

`mesh_readers` 条目加 `scope`：

```yaml
volumes:
  - name: main
    acl:
      mesh_readers:
        - node: nodeA
          fingerprint: 3f2a…9c
          owner: alice
          scope: read          # read（缺省，向后兼容）| write
```
- **读不隐含写、写不隐含读**（`scope: write` 必须显式声明；若要读写则写两条或 `scope: rw`，实现上取 `read`/`write`/`rw` 三值，缺省 `read` = 零回归）；
- **owner 绑定语义不变**：仍是**节点级代理信任**（"我信任节点 A 这个 mesh 成员，允许它访问我指定的 (卷, owner) 命名空间"），不引入用户级联邦；
- 三重约束照旧 fail-closed：`(node, fingerprint, owner, scope)` 命中 **且** `owner` 过本卷 ACL（`Authorize`）。

**不定则**：若先做独立 `mesh_writers` 列表，将来加 `scope` 要二次迁移配置 + 代码 + 文档。

### 5.8 远程写的并发与一致性

- **不引入分布式锁 / 版本向量 / 一致性协商**；
- 沿用 Y-C 的「**单一属主写**」约定：写批次只允许**代理写**（A 代 B 写 B 拥有的 owner 命名空间）；
- 跨节点与本地写的互斥由 **B 侧既有文件级锁**保证（`pkg/files` 的 `FileLocks`：远程写与本地写**共享同一锁池** ⇒ 天然互斥）——这是"远程面复用域 API"的直接收益；
- **冲突判定留在上层 `pkg/sync`（`ConflictPolicy`）**，B 侧不判冲突。

**不定则**：Y-C §11 的"谁持有写权"被悬置，二期可能被迫引入分布式协调（大改）。

### 5.9 B 侧远程面**只做授权 + 调域 API**

只读面现状是"改写请求 + 伪造 actor"（过渡形态）。D-2 之后：只读面切到域 API，写面从第一天就走域 API。
**不定则**：远程写会复制一份写路径（checksum / 原子改名 / 版本 / 配额 / 锁）——**正是抽取规格开篇要根治的问题**。

---

## 6. 分期路线

| 期 | 内容 | 依赖 | 兼容/回退 |
|---|---|---|---|
| **P0｜Y-C 一期（不缩水）** | `feature/y-read-transport` rebase → 合并（T4 传输原语 + T5 B 侧 loopback listener）→ T6 `pkg/remote` 只读 | — | 只读白名单（AD-7）不变；**T4 的短写/截断修复是写批次的安全前置** |
| **P1｜抽象收敛（零行为变更）** | ① `sync.FS` 确立为唯一抽象；② `HTTPTransport`/`LocalFS` 归属理顺（网络实现移出 `pkg/sync`）；③ `pkg/remote` 暴露 `NewFS(...) sync.FS`：**先实现 3 读方法，写 4 方法返回 `ErrUnsupported`**；④ `syncmgr` 引入 `RemoteTarget{Kind}`（`direct` 为唯一实现） | P0 | 纯新增；HTTP 路径与配置不变 |
| **P2｜D-2 域操作 API** | `pkg/files` 读写面域化（`List`/`Stat`/`Open`/`WriteFile`/`RenameIfUnchanged`/`DeleteIfUnchanged`/`Mkdir`/`Rmdir`）；HTTP 处理器降为薄适配；**B 侧只读面从"改写请求"切到域 API** | P1 | HTTP 契约零改动由四条机械核对 + e2e 钉住 |
| **P3｜写批次（Y 二期）** | `scope` 授权 + B 侧**写 listener**（独立服务名 + 独立路由白名单）+ A 侧 `pkg/remote` 实现 4 个写方法 + `syncexec` 支持 `remote://` 目标 | P2 | 读服务**物理上**仍只注册 GET/HEAD（AD-7 非黑名单法） |
| **P4｜收敛** | `direct` 降级为"另一种 `RemoteTarget.Kind`"，保留兼容、不再新增能力 | P3 | 旧配置继续可跑 |

> **P1 是防返工的关键动作**：代价极小（写方法先返回 `ErrUnsupported`），却把接口固定下来，使 P3 不必改任何已定接口。

---

## 7. 安全模型（写批次）

| 约束 | 内容 | fail 方向 |
|---|---|---|
| 传输认证 | 隧道握手 + **双向 Ed25519 指纹 pin**（A 侧 `peerPins[node]` 为空即拒，**不 TOFU**） | 拒 |
| 节点身份 | B 侧由**已认证对端指纹**反查 `(node, owner, scope)` 绑定；**owner 绝不由请求方指定** | 拒 |
| 卷访问 | 命中条目**且** `owner` 过本卷 `ACL.Authorize` | 拒 |
| 能力 | `scope` 必须显式含 `write` 才允许写 op | 拒 |
| 路由 | 读 listener 只注册 `GET`/`HEAD`；写 listener 只注册写 op —— **写 handler 从不出现在读路由表上** | 拒 |
| 前置校验 | `rename`/`delete` 保留 checksum 门禁（B 侧同锁内校验） | 拒 |
| 审计 | 每个 op 记 `mesh_read` / `mesh_write` + `volume` + `path` + `status` + `mesh` 节点 | 必记 |

---

## 8. 兼容与回退

- **HTTP 契约零改动**：P2 期间由四条机械核对（`test/` 零改动、用例名守恒、路由表逐条一致、分层门禁）+ e2e 钉住。
- **配置向后兼容**：`scope` 缺省 `read` ⇒ 旧配置语义不变。
- **`direct` 通路保留**：P4 只是把它降为 `RemoteTarget.Kind` 的一个取值，旧 `sync_remotes` 配置继续可用。
- **回退粒度 = 每期一个 PR**：任一期可独立回退，不影响已完成期。

## 9. 风险

| 风险 | 评级 | 处置 |
|---|---|---|
| **P2 规模**（`pkg/files` 17 个 HTTP 形状方法域化） | 高 | 切片推进（读面 → 写面 → 目录/批量），每片 HTTP 契约零改动 + 机械核对 |
| 安全面任一步 fail-open | 高 | 三重约束照 `AuthorizeMeshRead` 模板实现；每步新增拒绝路径用例 |
| 隧道长传输短写/截断/超时 | 中 | **P0 先把 T4 的修复并入 master**（`c7f1d239` 短写截断、`2fb40512` 关闭顺序） |
| `HTTPTransport` 迁出牵动 1009 行测试 | 中 | 迁移与测试同步搬（用例名守恒）；先加 `syncfs` 转发再删旧（可按 `checksum.Reader` 先例） |
| spool 造成的 A 侧磁盘占用与 `WriteFile` 断点语义 | 中 | 协议层明确"单请求流式 + 失败即整体重试"，**不承诺**跨请求断点（分块会话留给 HTTP 面） |

## 10. 不做（记录理由）

- ❌ 分布式锁 / 版本向量 / 一致性协商（单属主 + B 侧本地文件锁已足够）。
- ❌ B 侧冲突判定（`ConflictPolicy` 属上层 `pkg/sync`）。
- ❌ 透明网关（Y-C §11 列为二期候选，非写批次前置）。
- ❌ 元数据目录服务（Y-C AD-4 显式寻址正是为不依赖它）。
- ❌ 复用 HTTP 分块三段式会话到隧道（见 §5.4）。
- ❌ 让 `pkg/sync` 继续持有网络实现（见 §1.3）。

## 11. 自检

- **占位符扫描**：无"待定/TODO"。§5 的 9 项均为**已定结论**（A/B/C 经用户确认，其余由本规格给出单值选择）。
- **内部一致性**：§3.1 责任表与 §5.1/5.2 的定位一致；§5.3 的 op 表与 §7 安全模型逐行对应；§6 分期与 §5 各项决策一一落位。
- **范围检查**：本规格只覆盖"远程访问面 + sync/remote 收敛 + D-2 前置"；不覆盖 Y-C 一期自身的实现细节（见其计划）。
- **歧义检查**：已写死三处易歧义点——① `scope` 三值 `read|write|rw` 且缺省 `read`；② `Rename`/`Delete` 走 `*IfUnchanged` 保留 checksum 不变量；③ 隧道写为单请求流式、不承诺跨请求断点。
