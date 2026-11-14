<!--
Copyright 2026 The Cocomhub Authors. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Secret 加密卷设计（shardseal：纯 Go 分块加密）— 归档

> **归档说明**：原文 `docs/designs/2026-10-01-secret-volume.md`（§1–§13 + 附录 A，872 行）已于
> 2026-11-14 归档。能力/配置/用法/安全性见 `docs/secret-volume.md`（现行事实源）；方案反复的
> 高层 WHY 另见 `docs/archive/2026-10-01-secret-volume-decisions.md`。本文件只保留**设计意图 /
> 取舍与拒不采纳的方案 / 外部契约 / 仍有效的判断 / 落地指针**，实现步骤与已进权威文档的
> API/配置细节已移除。
>
> **原文已知瑕疵（归档时保留说明，未沿用）**：原文含**两份内容近似重复的「附录 A：PikPak
> 多账号管理设计」**（旧/新版本并存）；§10「旧卷加载」仍写旧标记 `@`/`-`/`_`，与 §3
> naming-hide 收敛后的 `z`/`q` 标记不一致。本文以下按**收敛后语义**归纳。

## 1. 背景与目标
把「下载 → 分块加密 → 上传任意卷」沉淀为**通用加密卷**能力：
- **secretdata 卷**：封装卷（wrapper），写入自动分块加密、读取自动解密，对上层透明。
- **secrets 卷**：特殊抽象卷，存 secret 文件（底层指向任意卷的 secrets 目录）。
- **纯 Go**：加密用 `golang.org/x/crypto`（项目允许，不进 ext）。
- **分块加密后上传**（非流式）：**边加密边上传**（避免 2 倍空间）。
- **命名无特征 + 目录名保密**：底层卷上所有可见名称都加密随机生成——目录名随机、
  文件名三段 base62 同构；不出现 `meta`/`data` 等结构词；文件 meta 不存路径层级，
  逻辑目录信息放目录 meta 内，支持目录移动后整体依旧可解析。
- 落地指针：算法包 `pkg/cryptox/shardseal`；卷包 `pkg/volume/secrets`、`pkg/volume/secretdata`；
  装配 `cmd/sproxy/secret_register.go`；包文档见两包 `README.md`。

## 2. 目录保密与命名（核心决策：匿名性优先）
**为什么**：最初设想底层分 `data/` / `meta/` 顶层目录——这两个词本身就是结构性标识，等于向底层
宣布「这是加密卷」且目录名编码内容类别，匿名性从根上破功。演进为：
1. **随机命名容器目录**（5-30 位 `[a-z0-9]`，生成时规避 `meta`/`data`/`secret` 等保留词）——
   目录名与内容无关联，底层只看到一堆互不可分的随机目录与随机文件。
2. **父引用解耦**（事实上的关键取舍）：若 meta 存完整逻辑路径，**目录移动**就要改写子树内所有
   文件 meta——成本 O(子树) 且易被底层观察到批量变更。改用目录 meta 只存 `{name, parent_dir_id}`、
   文件名只存 basename ⇒ **目录移动/改名 = 只重写被移动目录自己的一个 meta 文件，子树零改动**；
   代价是挂载期需沿 parent 链重算逻辑路径（**成环须 fail-closed** 防栈溢出）。
3. **naming-hide 匿名性收敛（2026-10-06）**：命名从「三段 16hex + 随机段（54-62 字符）」升级为
   「三段 9 字符 base62 + 两个随机段（**35-43 字符**，三类文件名同分布）」。核心变化：
   - **不再有明文哈希外泄**：原首段=块/文件明文内容哈希、中段=文件原始总校验和（可做内容存在性
     探测）⇒ 现首/末段 = **同一加密 blob 自身** SHA-256 两窗口（自包含、不解密可验密文完整），
     中段 = **HMAC 分组盲签**（无密钥无法反推明文/做内容探测）。
   - **三类文件名视觉同构**：靠**随机段内注入的普通小写字母标记**区分（分块无标记、文件 meta
     注入 `z`、目录 meta 注入 `q`）——**标记是替换随机段内一字符、不增长度**，观察者无法凭
     hex 密度/长度/周期/「某字符从未出现」把 meta 与分块区分开。曾考虑 `-`/`_`/`@` 符号与
     `emeta` 前缀，**均被否决**（特殊符号/短名是新特征）。
   - **meta 大小特征也要隐藏**：meta 加密后仅几百字节，直接落盘会被底层凭**文件大小**一眼区分
     （比文件名更明显）⇒ meta 明文 `[4B jsonLen][metaJSON][rand padding]` 整体加密，padding 到
     分块大小范围下限附近（默认 1MiB–2MiB），使**所有文件 ≥1MiB、meta 与分块同分布**
     （`extra.meta_pad_bytes` 可配，**任何配置下 meta 与分块大小范围必须重叠**，测试锁定）。
   - **首部无指纹**：所有加密文件首部为**固定长度（默认 128B）随机字节 R**，magic/`file` 检测
     全判随机；R 仅首部混淆、**不参与完整性**（跳过后按固定偏移读，篡改 R 不影响解密）。
- **目录名保密泄漏面审计结论**：底层可观察面（顶层目录名、三类文件名、文件名长度、文件大小、
  文件首部、分块/meta 内容、逻辑目录结构、文件根信息、目录-文件关联）逐项均无明文泄漏。
  已知边界（明确不承诺）：分块/meta 文件名里的**加密 blob SHA-256 窗口**与 **HMAC 组签**属密文
  自校验/盲分组层，不泄露明文内容，保留不改（16hex 明文哈希/总校验已于 naming-hide 移除）。

## 3. 加密格式与 KDF 档位（取舍密集）
- 统一落盘格式：`[R 随机首部][8B 真实密文长 BE][salt][nonce][ciphertext+GCMtag]`（分块与 meta
  同构）；`AES-256-GCM` + 每文件随机 32B 盐 → scrypt 派生文件密钥。
- **KDF 档位化（why）**：secretdata 的 secret 是 **256-bit 高熵随机**（`secrets.go` 生成）⇒
  固定 `2^17`（~202ms，为低熵口令设计的交互式登录档）对高熵密钥**纯属过度防御**——KDF 档位
  **不影响**高熵密钥暴力破解成本（只做域分离/SV 派生）。故注册 **high/standard/low** 三档
  （默认 standard 2^14），各档 = **不同 AlgoVersion + 不同 KDF 派生域** ⇒ 同 secret+salt 跨档
  key 不同、**跨档 fail-closed**；升/降档 = 登记新版本，不破坏既有 blob。
- **loadGate 自适应并发**：派生并发上限不靠人工定，按**注册档位的真实内存**（RFC 7914
  `128×r×N`）反推预算（512MiB），档位越高并发越收窄（防大卷挂载内存爆炸）。
- **mockkdf 测试子包（why）**：测试套件跑真实 scrypt 会让组装层测试白付百毫秒级派生（大卷数百
  文件即分钟级）⇒ 从「极低档真实档」演进到独立子包 `mockkdf`：**HKDF-SHA256 轻量派生（~µs）
  + 仍走真实 AES-GCM**——组装正确性仍真实验证、仅派生轻量化；**生产禁配 mock**，算法可靠性
  始终由真实档维护。内存测算是类型级纯算术、与 KDF 覆盖正交 ⇒ mock 档下发并发不受拖慢。

## 4. meta 设计
- **文件 meta 只存文件根信息**（`original.name` = basename，**不存 rel 路径**）+ 分块索引
  （`chunks[]`：offset/file_name/orig_size/sha256/enc_size/nonce）；目录整体移动后文件仍可解析。
- **目录 meta** 存 `{name, parent_dir_id, dir_id}`（父引用）⇒ 逻辑路径沿 parent 链重算；
  父引用断裂/成环 fail-closed 跳过（防未信任存储上损坏/篡改导致的挂载栈溢出）。
- **旧格式兼容**：从「存完整 `path`」迁到父引用格式时，loadIndex 遇旧格式（明文含 `path` 键）
  **fail-closed**，不静默压平。
- **索引键 = 完整逻辑 rel**，**写路径与重启恢复一致**（重启扫描目录 meta 建 `dir_id→dirMeta`、
  沿链解析路径）；loadIndex 并行化 + `(salt→key)` 派生缓存缓解重复 scrypt。

## 5. 卷模型与寻址
- **secrets 卷**（封装卷）：底层指向**任意卷的 secrets 目录**（含嵌套：target 可为 secretdata 卷）。
  只存普通文件（自身不加密）；本地盘权限 0600；嵌套要求被引用的 secrets 卷**先装配好**（不循环）。
- **secretdata 卷**（封装卷）：`extra.target`（底层卷，支持所有卷）/ `algorithm` / `secret_url` /
  `block_policy` / `temp_dir`。底层无顶层 `data`/`meta` 目录，全随机容器。
- **⚠️ 已废止的旧 secrets 模型（2026-10-15 per-owner 重构）**：设计时点为「全局默认 secrets 卷 +
  `secrets://default/` 省略寻址 + `extra.default_secret` 选取」。**现行实现 = per-owner 默认桶**
  （`<storage_root>/<owner>/meta/secrets/`，免声明、首次使用自动派生、`/api/secrets` 恒作用于请求者
  owner，无全局默认卷）。原文该段保留为历史记录，现行语义见 `docs/secret-volume.md` §3.9/§4。
- **覆盖写原子化（审查 F-2）**：曾「先删旧后传新」⇒ 中途失败**永久丢失旧版本**。改为：新随机名
  先传全部分块 + 新 meta → 全部成功 → 切换索引 + best-effort 删旧；中途失败 → 删新留旧。
- **逻辑路径与索引键一致性（审查 F-1）**：写路径与重启恢复必须同源（否则改名/移动后路径漂移）。
- **MakeDir 不转发明文目录到底层**（旧行为 `inner.MakeDir(rel)` 会向底层泄漏逻辑目录名）。
- **Imp-1 空目录语义**：删目录内最后文件时连目录 meta + 注册一并删（彻底成空、重启不复现）并
  递归回收成空祖先；根容器恒保留。

## 6. 删除与 GC（决策）
**从「墓碑 + 后台 GC」转为「即时物理删」**：后台 GC 是并发正确性泥潭——同一套「判定孤儿再清理」
路径**三次**引入数据丢失竞态（误删存活分块 / 挂载期瞬时读故障误判存活 / epoch 窗口内已提交的写被
清理）。加密卷默认形态**不应依赖「能删错数据的后台 GC」保持正确**。改为 Delete 一次持锁删完
meta + 全部分块（先删 meta 再删分块——meta 删失败则分块全在，可重建完整文件，不产生幽灵），**删即
释放、重启不复现**；GC 降级为**可选维护工具**（默认 `gc_interval=0` 不启动），仅供远程卷/多进程
共享场景显式启用作孤儿兜底。（决策详述见配套 decisions 归档。）

## 7. 可插拔分块算法（plugin）
- 接口 `BlockPlanner(origSize) ([]Block, error)`；默认 `random`（1MB-200MB 随机）。
- `video-keyframe`（关键帧边界 + 大小综合分片）：`shardseal.RegisterBlockletMode`（基于
  `pkg/plugin.Registry`，KeyframeIndexer 来自 `pkg/cryptox/ext/keyframe`），secretdata 写路径
  按文件名自动选型、未命中回落默认 fixed。
- **段级随机读（非逐字节流式）**：`OpenRangeRead` 按 meta 定位块 → **只解密含目标区间的 blocklet
  段**（视频关键帧对齐，seek 不整文件解密）；**注意实现边界**：整段解密后一次性返回 ⇒ 大区间
  会放大 RAM = 区间大小；HTTP 层加密卷 Range 已接线（一期：`syncpkg.RangeSeeker` 把
  RangeReader+size 适配 `io.ReadSeeker` 喂 `http.ServeContent`，明文逻辑坐标，命中返回 206）。

## 8. 架构演进预留（决策：可平滑演进是硬前提）
- **AlgoVersion 注册表 = 平滑演进的唯一保证**：任何未来语义（压缩/去重/多副本/纠删码）若改变
  派生/解密路径，**登记新 `AlgoVersion`**（KDF 派生域随之分离，**版本不明文进 blob**）；
  解密按注册表「升序试派生」定位，新增版本仅注册即生效；未知版本 **fail-closed**。
  所有预留字段/类型都位于密文内 + AAD 内、`omitempty` 不膨胀。
- **12 项审计预留**（字段/常量 → 方向）：去重引用（RefCount+Ref 块类型）· 乐观锁（BaseVersion，
  多进程 CAS）· 墓碑+GC（可选默认关）· usage 记账 · 溯源（WriterID/SourceURL/ExportedFrom）·
  版本保留（VersionSeq/Supersedes）· 流式（blocklet 索引，已实现关键帧段级）· 多副本
  （XOR parity 块类型）· 纠删码（后续 AlgoVersion）· 访问计数（AccessCount/LastAccess）·
  meta 签名（HMAC 子域）· 版本时钟（VClock）；横向另有 压缩 / KeyID / Extra。
- **多副本 = XOR parity（k-of-k+1 纯 stdlib）→ Reed-Solomon 后续**：parity 冗余 1/k、计算 O(k)
  异或、纯 stdlib；弱点是仅覆盖单块。RS 覆盖多块但引依赖/复杂度 ⇒ parity 先行、RS 经新
  AlgoVersion 后续（k-of-k+1 是 RS 的 n=m=1 特例）。
- **已实现但实验性清单（方案 A 2026-10-03）**：Dedup **预留/未接线**（装配层不解析 `extra.dedup`，
  生产不可达）· Erasure **实验性默认关** · 后台 GC **可选默认关** · 乐观锁 **多进程预留 API** ·
  墓碑+GC **可选默认关**。**共同收敛原则**：已实现但未生产验证的能力一律「标注实验性 + 默认关」，
  不进入「默认正确性」承诺；可承接更完整能力的标预留。默认配置形态应简单、正确性自足
  （无后台依赖、无未验证能力参与）。
- **对照类似实现**：gocryptfs（流式/thin，借鉴随机访问按需解密）· restic（内容寻址去重 +
  压缩，借鉴 §Ref/Compressed）· age（多接收者公钥，借鉴多 key 互读）· Tahoe-LAFS（RS 纠删码，
  对应 §RS 后续）。

## 9. 附录 A（原文）：PikPak 多账号管理（会话文件池）
- **背景**：免费账号下载带宽 ~1.15MB/s（账号级，多并发不加速）、**20GB/日 下载配额**；大文件
  单账号一天仅能下 1 个 ⇒ 需多账号轮换接力。分享转存到个人网盘**不占下载配额**（只占存储）。
- **实测机制**：CLI 会话单 `.credentials.json`（access_token + refresh_token，过期自动 refresh）；
  `PIKPAK_TOKEN` env 只覆盖 access_token、**无 refresh**；**CLI 硬编码 `~/.pikpak`**（HOME/
  USERPROFILE/config-dir 均无效）⇒ 会话文件池须切写该固定路径；REST `drive/v1/about` 有 storage
  与任务数、**带宽 20GB/日 不可查** ⇒ 本地记录。
- **设计（方案 A，用户确认）**：会话文件池每账号存完整 credentials JSON（含 refresh_token，CLI
  自动刷新、无 captcha）；同进程内 CLI 调用**串行**；本地按日累计配额、按剩余配额选账号（round-robin
  起点 + 失败冷却）；凭据存加密卷（每账号一个 secret 文件，复用 shardseal）。**首期单账号整文件下载**，
  多账号分块并行下载为后续片。
- 落地指针：`pkg/volume/ext/pikpak/account.go`（Account/AccountPool）+ `cmd/sproxy/pikpak_cmd.go`。

## 10. 附录 A（原文）：fMP4 与「边缓冲边播」决策
- **fMP4（fragmented MP4）关键帧解析双实现**（`pkg/media`）：ffprobe 优先（天然支持 fMP4，不降级）；
  go-mp4 兜底（遇 fMP4 无全局 stss → **降级 fixed**，能播但 seek 不优化）。观测：slog + Prometheus
  metric `sproxy_keyframe_fmp4_total`（**口径注意**：计数的是「检测到 fMP4 特征的文件」而非「被降级
  的」，解读时留意）。**决策触发器**：fMP4 占比上升或出现**纯 Go 硬约束部署**（不可装 ffmpeg）时，
  才评估切 `mp4ff`；「ffmpeg 可部署」下现有方案更胜（无需重写测试基建）。
- **「边缓冲边播」诚实边界**：解密侧**段级随机读**已实现（seek 只解目标段）；HTTP Range 接线于
  2026-10-05 一期落地（`syncpkg.RangeSeeker` → `http.ServeContent` 返回 206 + Content-Range，
  顶点测试 `pkg/volume/secretdata/range_serve_http_test.go`）。

## 11. 已知限制（如实记录，不承诺）
- **非流式加密**：写路径当前 `io.ReadAll` 全文 → 分块加密（峰值 1× 文件，不写临时源）；未做
  逐块流式（blocklet 双层规划 + meta 名三段锚定要求先有整文件内容哈希）；大文件内存由
  `Options.MaxFileBytes`（`extra.max_file_bytes`）上限兜底 fail-closed，完整流式留后续。
- meta padding / 目录名等匿名性约束由命名测试锁定（长度同分布、hex 密度 <45%、meta padding
  范围与分块重叠、无 `meta`/`data` 保留词等）；实现演进不得破坏这些不变量。
